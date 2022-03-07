/*
Copyright 2020 The Flux authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/fluxcd/pkg/apis/meta"
	"github.com/fluxcd/pkg/runtime/conditions"
	helper "github.com/fluxcd/pkg/runtime/controller"
	"github.com/fluxcd/pkg/runtime/events"
	"github.com/fluxcd/pkg/runtime/patch"
	"github.com/fluxcd/pkg/runtime/predicates"
	sourcev1 "github.com/fluxcd/source-controller/api/v1beta2"
	serror "github.com/fluxcd/source-controller/internal/error"
	sreconcile "github.com/fluxcd/source-controller/internal/reconcile"
	"github.com/fluxcd/source-controller/internal/reconcile/summarize"
	"github.com/fluxcd/source-controller/pkg/registry"
	"github.com/fluxcd/source-controller/pkg/sourceignore"
	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	kuberecorder "k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	oras "oras.land/oras-go/pkg/registry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ociRepoReadyConditions contains all the conditions information needed
// for OCIRepository Ready status conditions summary calculation.
var ociRepoReadyConditions = summarize.Conditions{
	Target: meta.ReadyCondition,
	Owned: []string{
		sourcev1.FetchFailedCondition,
		sourcev1.ArtifactOutdatedCondition,
		meta.ReadyCondition,
		meta.ReconcilingCondition,
		meta.StalledCondition,
	},
	Summarize: []string{
		sourcev1.SourceVerifiedCondition,
		sourcev1.FetchFailedCondition,
		sourcev1.ArtifactOutdatedCondition,
		meta.StalledCondition,
		meta.ReconcilingCondition,
	},
	NegativePolarity: []string{
		sourcev1.FetchFailedCondition,
		sourcev1.ArtifactOutdatedCondition,
		meta.StalledCondition,
		meta.ReconcilingCondition,
	},
}

// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=ocirepositories,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=ocirepositories/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=ocirepositories/finalizers,verbs=get;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch

// OCIRepositoryReconciler reconciles a OCIRepository object
type OCIRepositoryReconciler struct {
	client.Client
	kuberecorder.EventRecorder
	helper.Metrics

	Storage        *Storage
	ControllerName string

	requeueDependency time.Duration
	RegistryClient    *registry.Client
}

type OCIRepositoryReconcilerOptions struct {
	MaxConcurrentReconciles   int
	DependencyRequeueInterval time.Duration
}

// ociRepoReconcilerFunc is the function type for all the Git repository
// reconciler functions.
type ociRepoReconcilerFunc func(ctx context.Context, repository *sourcev1.OCIRepository, artifact *sourcev1.Artifact, pullResult *registry.PullResult) (sreconcile.Result, error)

func (r *OCIRepositoryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return r.SetupWithManagerAndOptions(mgr, OCIRepositoryReconcilerOptions{})
}

func (r *OCIRepositoryReconciler) SetupWithManagerAndOptions(mgr ctrl.Manager, opts OCIRepositoryReconcilerOptions) error {
	r.requeueDependency = opts.DependencyRequeueInterval

	return ctrl.NewControllerManagedBy(mgr).
		For(&sourcev1.OCIRepository{}, builder.WithPredicates(
			predicate.Or(predicate.GenerationChangedPredicate{}, predicates.ReconcileRequestedPredicate{}),
		)).
		WithOptions(controller.Options{MaxConcurrentReconciles: opts.MaxConcurrentReconciles}).
		Complete(r)
}

func (r *OCIRepositoryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, retErr error) {
	start := time.Now()
	log := ctrl.LoggerFrom(ctx)

	repository := &sourcev1.OCIRepository{}
	if err := r.Get(ctx, req.NamespacedName, repository); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Record suspended status metric
	r.RecordSuspend(ctx, repository, repository.Spec.Suspend)

	// Return early if the object is suspended
	if repository.Spec.Suspend {
		log.Info("reconciliation is suspended for this object")
		return ctrl.Result{}, nil
	}

	// Initialize the patch helper with the current version of the object.
	patchHelper, err := patch.NewHelper(repository, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}

	// recResult stores the abstracted reconcile result.
	var recResult sreconcile.Result

	// Always attempt to patch the object and status after each reconciliation
	// NOTE: The final runtime result and error are set in this block.
	defer func() {
		summarizeHelper := summarize.NewHelper(r.EventRecorder, patchHelper)
		summarizeOpts := []summarize.Option{
			summarize.WithConditions(ociRepoReadyConditions),
			summarize.WithReconcileResult(recResult),
			summarize.WithReconcileError(retErr),
			summarize.WithIgnoreNotFound(),
			summarize.WithProcessors(
				summarize.RecordContextualError,
				summarize.RecordReconcileReq,
			),
			summarize.WithResultBuilder(sreconcile.AlwaysRequeueResultBuilder{RequeueAfter: repository.GetInterval().Duration}),
			summarize.WithPatchFieldOwner(r.ControllerName),
		}
		result, retErr = summarizeHelper.SummarizeAndPatch(ctx, repository, summarizeOpts...)

		// Always record readiness and duration metrics
		r.Metrics.RecordReadiness(ctx, repository)
		r.Metrics.RecordDuration(ctx, repository, start)
	}()

	// Add finalizer first if not exist to avoid the race condition
	// between init and delete
	if !controllerutil.ContainsFinalizer(repository, sourcev1.SourceFinalizer) {
		controllerutil.AddFinalizer(repository, sourcev1.SourceFinalizer)
		recResult = sreconcile.ResultRequeue
		return
	}

	// Examine if the object is under deletion
	if !repository.ObjectMeta.DeletionTimestamp.IsZero() {
		recResult, retErr = r.reconcileDelete(ctx, repository)
		return
	}

	// Reconcile actual object
	reconcilers := []ociRepoReconcilerFunc{
		r.reconcileStorage,
		r.reconcileSource,
		r.reconcileArtifact,
	}
	recResult, retErr = r.reconcile(ctx, repository, reconcilers)
	return
}

func (r *OCIRepositoryReconciler) reconcile(ctx context.Context, repository *sourcev1.OCIRepository, reconcilers []ociRepoReconcilerFunc) (sreconcile.Result, error) {
	if repository.Generation != repository.Status.ObservedGeneration {
		conditions.MarkReconciling(repository, "NewGeneration", "reconciling new object generation (%d)", repository.Generation)
	}

	ociCtx, cancel := context.WithTimeout(ctx, repository.Spec.Timeout.Duration)
	defer cancel()

	var pullResult registry.PullResult
	var artifact sourcev1.Artifact

	// Run the sub-reconcilers and build the result of reconciliation.
	var res sreconcile.Result
	var resErr error
	for _, rec := range reconcilers {
		recResult, err := rec(ociCtx, repository, &artifact, &pullResult)
		// Exit immediately on ResultRequeue.
		if recResult == sreconcile.ResultRequeue {
			return sreconcile.ResultRequeue, nil
		}
		// If an error is received, prioritize the returned results because an
		// error also means immediate requeue.
		if err != nil {
			resErr = err
			res = recResult
			break
		}
		// Prioritize requeue request in the result.
		res = sreconcile.LowestRequeuingResult(res, recResult)
	}
	return res, resErr
}

func (r *OCIRepositoryReconciler) reconcileStorage(ctx context.Context,
	repository *sourcev1.OCIRepository, artifact *sourcev1.Artifact, pullResult *registry.PullResult) (sreconcile.Result, error) {
	// Garbage collect previous advertised artifact(s) from storage
	_ = r.garbageCollect(ctx, repository)

	// Determine if the advertised artifact is still in storage
	if artifact := repository.GetArtifact(); artifact != nil && !r.Storage.ArtifactExist(*artifact) {
		repository.Status.Artifact = nil
		repository.Status.URL = ""
	}

	// Record that we do not have an artifact
	if repository.GetArtifact() == nil {
		conditions.MarkReconciling(repository, "NoArtifact", "no artifact for resource in storage")
		return sreconcile.ResultSuccess, nil
	}

	// Always update URLs to ensure hostname is up-to-date
	// TODO(hidde): we may want to send out an event only if we notice the URL has changed
	r.Storage.SetArtifactURL(repository.GetArtifact())
	repository.Status.URL = r.Storage.SetHostname(repository.Status.URL)

	return sreconcile.ResultSuccess, nil
}

func (r *OCIRepositoryReconciler) reconcileSource(ctx context.Context,
	repository *sourcev1.OCIRepository, artifact *sourcev1.Artifact, pullResult *registry.PullResult) (sreconcile.Result, error) {

	// Login to the registry
	loginOpt, err := r.credentials(ctx, repository)
	if err != nil {
		e := &serror.Event{
			Err:    err,
			Reason: sourcev1.AuthenticationFailedReason,
		}
		conditions.MarkTrue(repository, sourcev1.FetchFailedCondition, sourcev1.AuthenticationFailedReason, e.Err.Error())
		// Return error to the world as observed may change
		return sreconcile.ResultEmpty, e
	}

	hostRegistry := strings.TrimPrefix(strings.Split(repository.Spec.URL, "/")[0], "https://")
	hostRegistry = strings.TrimPrefix(hostRegistry, "http://")
	hostRegistry = strings.TrimPrefix(hostRegistry, "oci://")

	if loginOpt != nil {
		err = r.RegistryClient.Login(hostRegistry, loginOpt)
		if err != nil {
			e := &serror.Event{
				Err:    err,
				Reason: sourcev1.AuthenticationFailedReason,
			}
			conditions.MarkTrue(repository, sourcev1.FetchFailedCondition, sourcev1.AuthenticationFailedReason, e.Err.Error())
			// Return error to the world as observed may change
			return sreconcile.ResultEmpty, e
		}
	}

	ref, err := r.reference(repository)
	if err != nil {
		e := &serror.Event{
			Err:    err,
			Reason: sourcev1.OCIRepositoryOperationFailedReason,
		}
		conditions.MarkTrue(repository, sourcev1.FetchFailedCondition, sourcev1.OCIRepositoryOperationFailedReason, e.Err.Error())
		// Return error to the world as observed may change
		return sreconcile.ResultEmpty, e
	}

	isHelm := strings.Contains(repository.Spec.URL, "oci://")
	if isHelm {
		manifest, err := r.RegistryClient.PullManifest(ref.String())
		if err != nil {
			e := &serror.Event{
				Err:    fmt.Errorf("failed to pull the artifact for reference '%s': %w", ref.String(), err),
				Reason: sourcev1.OCIRepositoryOperationFailedReason,
			}
			conditions.MarkTrue(repository, sourcev1.FetchFailedCondition, sourcev1.OCIRepositoryOperationFailedReason, e.Err.Error())
			// Return error to the world as observed may change
			return sreconcile.ResultEmpty, e
		}

		// Helm chart, return ready condition with helm artifact reference
		r.AnnotatedEventf(repository, map[string]string{
			"revision": manifest.Digest.String(),
		}, corev1.EventTypeNormal, "NewArtifact", "Helm Artifact with ref: %s", ref)

		repository.Status.Artifact = &sourcev1.Artifact{
			URL:      ref.String(),
			Revision: manifest.Digest.String(),
		}

		return sreconcile.ResultSuccess, nil
	}

	// Pull the image
	result, err := r.RegistryClient.Pull(ref.String())
	if err != nil {
		e := &serror.Event{
			Err:    fmt.Errorf("failed to pull the artifact for repository '%s': %w", ref.String(), err),
			Reason: sourcev1.OCIRepositoryOperationFailedReason,
		}
		conditions.MarkTrue(repository, sourcev1.FetchFailedCondition, sourcev1.OCIRepositoryOperationFailedReason, e.Err.Error())
		// Return error to the world as observed may change
		return sreconcile.ResultEmpty, e
	}

	pullResult = result

	revision := fmt.Sprintf("%s@%s", fmt.Sprintf("%s/%s", ref.Registry, ref.Repository), pullResult.Manifest.Digest)

	// Mark observations about the revision on the object.
	if !repository.GetArtifact().HasRevision(revision) {
		message := fmt.Sprintf("new artifact revision '%s'", revision)
		conditions.MarkTrue(repository, sourcev1.ArtifactOutdatedCondition, "NewRevision", message)
		conditions.MarkReconciling(repository, "NewRevision", message)
	}

	conditions.Delete(repository, sourcev1.FetchFailedCondition)

	*artifact = r.Storage.NewArtifactFor(repository.Kind, repository.GetObjectMeta(), revision, fmt.Sprintf("%s.tar.gz", pullResult.Manifest.Digest))

	return sreconcile.ResultSuccess, nil
}

func (r *OCIRepositoryReconciler) reconcileArtifact(ctx context.Context,
	repository *sourcev1.OCIRepository, artifact *sourcev1.Artifact, pullResult *registry.PullResult) (sreconcile.Result, error) {
	// Always restore the Ready condition in case it got removed due to a transient error.
	defer func() {
		if repository.GetArtifact().HasRevision(artifact.Revision) {
			conditions.Delete(repository, sourcev1.ArtifactOutdatedCondition)
			conditions.MarkTrue(repository, meta.ReadyCondition, meta.SucceededReason,
				"stored artifact for revision '%s'", artifact.Revision)
		}
	}()

	if repository.GetArtifact().HasRevision(artifact.Revision) {
		ctrl.LoggerFrom(ctx).Info("artifact up-to-date", "revision", artifact.Revision)
		return sreconcile.ResultSuccess, nil
	}

	// Create artifact dir
	if err := r.Storage.MkdirAll(*artifact); err != nil {
		return sreconcile.ResultEmpty, &serror.Event{
			Err:    fmt.Errorf("failed to create artifact directory: %w", err),
			Reason: sourcev1.StorageOperationFailedReason,
		}
	}

	// Acquire lock.
	unlock, err := r.Storage.Lock(*artifact)
	if err != nil {
		return sreconcile.ResultEmpty, &serror.Event{
			Err:    fmt.Errorf("failed to acquire lock for artifact: %w", err),
			Reason: meta.FailedReason,
		}
	}
	defer unlock()

	// archive artifact and check integrity
	var ignoreDomain []string
	var ps []gitignore.Pattern
	if repository.Spec.Ignore != nil {
		ps = append(ps, sourceignore.ReadPatterns(strings.NewReader(*repository.Spec.Ignore), ignoreDomain)...)
	}

	// We expect the tarball to be a single file
	if pullResult.Layers == nil || len(pullResult.Layers) == 0 {
		return sreconcile.ResultEmpty, &serror.Event{
			Err:    fmt.Errorf("wrong number of layers in the manifest"),
			Reason: meta.FailedReason,
		}
	}

	in, err := uncompressed(pullResult.Layers[0].Data)
	if err != nil {
		return sreconcile.ResultEmpty, &serror.Event{
			Err:    fmt.Errorf("failed to uncompress the archive: %w", err),
			Reason: meta.FailedReason,
		}
	}
	defer in.Close()

	// Archive directory to storage
	if err := r.Storage.ArchiveTar(artifact, tar.NewReader(in), SourceIgnoreFilter(ps, ignoreDomain)); err != nil {
		return sreconcile.ResultEmpty, &serror.Event{
			Err:    fmt.Errorf("unable to archive artifact to storage: %w", err),
			Reason: sourcev1.StorageOperationFailedReason,
		}
	}
	r.AnnotatedEventf(repository, map[string]string{
		"revision": artifact.Revision,
		"checksum": artifact.Checksum,
	}, corev1.EventTypeNormal, "NewArtifact", "fetched artifact with digest %s from '%s'", pullResult.Manifest.Digest[:12], repository.Spec.URL)

	// Record it on the object
	repository.Status.Artifact = artifact.DeepCopy()

	// update latest symlink
	url, err := r.Storage.Symlink(*artifact, "latest.tar.gz")
	if err != nil {
		r.eventLogf(ctx, repository, corev1.EventTypeWarning, sourcev1.StorageOperationFailedReason,
			"failed to update status URL symlink: %s", err)
	}

	if url != "" {
		repository.Status.URL = url
	}
	return sreconcile.ResultSuccess, nil
}

// garbageCollect performs a garbage collection for the given v1beta1.OCIRepository.
// It removes all but the current artifact except for when the
// deletion timestamp is set, which will result in the removal of
// all artifacts for the resource.
func (r *OCIRepositoryReconciler) garbageCollect(ctx context.Context, repository *sourcev1.OCIRepository) error {
	if !repository.DeletionTimestamp.IsZero() {
		if deleted, err := r.Storage.RemoveAll(r.Storage.NewArtifactFor(repository.Kind, repository.GetObjectMeta(), "", "*")); err != nil {
			return &serror.Event{
				Err:    fmt.Errorf("garbage collection for deleted resource failed: %w", err),
				Reason: "GarbageCollectionFailed",
			}
		} else if deleted != "" {
			r.eventLogf(ctx, repository, events.EventTypeTrace, "GarbageCollectionSucceeded",
				"garbage collected artifacts for deleted resource")
		}
		repository.Status.Artifact = nil
		return nil
	}
	if repository.GetArtifact() != nil {
		if deleted, err := r.Storage.RemoveAllButCurrent(*repository.GetArtifact()); err != nil {
			return &serror.Event{
				Err: fmt.Errorf("garbage collection of old artifacts failed: %w", err),
			}
		} else if len(deleted) > 0 {
			r.eventLogf(ctx, repository, events.EventTypeTrace, "GarbageCollectionSucceeded",
				"garbage collected old artifacts")
		}
	}
	return nil
}

// credentials retrieves the credentials from Authentication
func (r *OCIRepositoryReconciler) credentials(ctx context.Context, repository *sourcev1.OCIRepository) (registry.LoginOption, error) {
	auth := repository.Spec.Authentication
	if auth == nil {
		return nil, nil
	}

	pullSecretNames := sets.NewString()
	if auth.SecretRef != nil {
		pullSecretNames.Insert(auth.SecretRef.Name)
	}

	// lookup service account
	serviceAccountName := auth.ServiceAccountName
	if serviceAccountName == "" {
		serviceAccountName = "default"
	}
	serviceAccount := corev1.ServiceAccount{}
	err := r.Get(ctx, types.NamespacedName{Namespace: repository.Namespace, Name: serviceAccountName}, &serviceAccount)
	if err != nil {
		return nil, err
	}
	for _, ips := range serviceAccount.ImagePullSecrets {
		pullSecretNames.Insert(ips.Name)
	}

	// lookup image pull secrets

	for _, imagePullSecretName := range pullSecretNames.List() {
		imagePullSecret := corev1.Secret{}
		err := r.Get(ctx, types.NamespacedName{Namespace: repository.Namespace, Name: imagePullSecretName}, &imagePullSecret)
		if err != nil {
			return nil, err
		}

		// retrieve the username and password from the secret
		if len(imagePullSecret.Data) != 0 {
			login := registry.LoginOptBasicAuth(string(imagePullSecret.Data["username"]), string(imagePullSecret.Data["password"]))
			return login, nil
		}
	}

	return nil, fmt.Errorf("no credentials found")
}

func (r *OCIRepositoryReconciler) reconcileDelete(ctx context.Context, repository *sourcev1.OCIRepository) (sreconcile.Result, error) {
	// Garbage collect the resource's artifacts
	if err := r.garbageCollect(ctx, repository); err != nil {
		// Return the error so we retry the failed garbage collection
		return sreconcile.ResultEmpty, err
	}

	// Remove our finalizer from the list
	controllerutil.RemoveFinalizer(repository, sourcev1.SourceFinalizer)

	// Stop reconciliation as the object is being deleted
	return sreconcile.ResultEmpty, nil
}

func (r *OCIRepositoryReconciler) reference(repository *sourcev1.OCIRepository) (oras.Reference, error) {
	url := strings.TrimPrefix(repository.Spec.URL, fmt.Sprintf("%s://", "oci"))

	ref := repository.Spec.Reference
	if ref == nil {
		return oras.ParseReference(fmt.Sprintf("%s:latest", url))
	}

	if ref.Digest != "" {
		return oras.ParseReference(fmt.Sprintf("%s@%s", url, ref.Digest))
	}

	if ref.SemVer != "" {
		// get a sorted list of tags
		tags, err := r.RegistryClient.Tags(url)
		if err != nil {
			return oras.Reference{}, fmt.Errorf("failed to get tags for %s: %w", url, err)
		}

		c, err := semver.NewConstraint(ref.SemVer)
		if err != nil {
			return oras.Reference{}, err
		}

		var candidates []*semver.Version
		for _, t := range tags {
			v, err := semver.NewVersion(t)
			if err != nil {
				continue
			}

			if c != nil && !c.Check(v) {
				continue
			}

			candidates = append(candidates, v)
		}

		if len(candidates) == 0 {
			return oras.Reference{}, fmt.Errorf("no matching tags were found")
		}

		sort.Sort(sort.Reverse(semver.Collection(candidates)))
		return oras.ParseReference(fmt.Sprintf("%s:%s", url, candidates[0].Original()))
	}

	if ref.Tag != "" {
		return oras.ParseReference(fmt.Sprintf("%s:%s", url, ref.Tag))
	}

	return oras.ParseReference(fmt.Sprintf("%s:latest", url))
}

func (r *OCIRepositoryReconciler) updateStatus(ctx context.Context, req ctrl.Request, newStatus sourcev1.OCIRepositoryStatus) error {
	var repository sourcev1.OCIRepository
	if err := r.Get(ctx, req.NamespacedName, &repository); err != nil {
		return err
	}

	patch := client.MergeFrom(repository.DeepCopy())
	repository.Status = newStatus

	return r.Status().Patch(ctx, &repository, patch)
}

// uncompressed returns the uncompressed artifact.
func uncompressed(data []byte) (io.ReadCloser, error) {
	in := bytes.NewReader(data)
	var uncompressed io.ReadCloser
	var err error
	if contentType := http.DetectContentType(data); contentType == "application/x-gzip" {
		uncompressed, err = gzip.NewReader(in)
		if err != nil {
			return nil, err
		}

		return uncompressed, nil
	}

	// If the artifact is not compressed, we just return the data as a reader.
	return io.NopCloser(in), nil
}

// eventLog records event and logs at the same time. This log is different from
// the debug log in the event recorder in the sense that this is a simple log,
// the event recorder debug log contains complete details about the event.
func (r *OCIRepositoryReconciler) eventLogf(ctx context.Context, obj runtime.Object, eventType string, reason string, messageFmt string, args ...interface{}) {
	msg := fmt.Sprintf(messageFmt, args...)
	// Log and emit event.
	if eventType == corev1.EventTypeWarning {
		ctrl.LoggerFrom(ctx).Error(errors.New(reason), msg)
	} else {
		ctrl.LoggerFrom(ctx).Info(msg)
	}
	r.Eventf(obj, eventType, reason, msg)
}
