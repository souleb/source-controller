/*
Copyright 2022 The Flux authors

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
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	oras "oras.land/oras-go/pkg/registry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ociArtifactReadyConditions contains all the conditions information needed
// for OCIArtifact Ready status conditions summary calculation.
var ociArtifactReadyConditions = summarize.Conditions{
	Target: meta.ReadyCondition,
	Owned: []string{
		sourcev1.FetchFailedCondition,
		sourcev1.StorageOperationFailedCondition,
		sourcev1.ArtifactOutdatedCondition,
		meta.ReadyCondition,
		meta.ReconcilingCondition,
		meta.StalledCondition,
	},
	Summarize: []string{
		sourcev1.SourceVerifiedCondition,
		sourcev1.StorageOperationFailedCondition,
		sourcev1.FetchFailedCondition,
		sourcev1.ArtifactOutdatedCondition,
		meta.StalledCondition,
		meta.ReconcilingCondition,
	},
	NegativePolarity: []string{
		sourcev1.FetchFailedCondition,
		sourcev1.StorageOperationFailedCondition,
		sourcev1.ArtifactOutdatedCondition,
		meta.StalledCondition,
		meta.ReconcilingCondition,
	},
}

// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=ociartifacts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=ociartifacts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=ociartifacts/finalizers,verbs=get;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch

// OCIArtifactReconciler reconciles a OCIArtifact object
type OCIArtifactReconciler struct {
	client.Client
	kuberecorder.EventRecorder
	helper.Metrics

	Storage        *Storage
	ControllerName string

	requeueDependency time.Duration
	RegistryClient    *registry.Client
}

type OCIArtifactReconcilerOptions struct {
	MaxConcurrentReconciles   int
	DependencyRequeueInterval time.Duration
}

// ociArtifactReconcilerFunc is the function type for all the oci Artifacts
// reconciler functions.
type ociArtifactReconcilerFunc func(ctx context.Context, obj *sourcev1.OCIArtifact, artifact *sourcev1.Artifact, pullResult *registry.PullResult) (sreconcile.Result, error)

func (r *OCIArtifactReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return r.SetupWithManagerAndOptions(mgr, OCIArtifactReconcilerOptions{})
}

func (r *OCIArtifactReconciler) SetupWithManagerAndOptions(mgr ctrl.Manager, opts OCIArtifactReconcilerOptions) error {
	r.requeueDependency = opts.DependencyRequeueInterval

	if err := mgr.GetCache().IndexField(context.TODO(), &sourcev1.OCIRegistry{}, sourcev1.OCIRegistryURLIndexKey,
		r.indexOCIRegistryByURL); err != nil {
		return fmt.Errorf("failed setting index fields: %w", err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&sourcev1.OCIArtifact{}, builder.WithPredicates(
			predicate.Or(predicate.GenerationChangedPredicate{}, predicates.ReconcileRequestedPredicate{}),
		)).
		Watches(
			&source.Kind{Type: &sourcev1.OCIRegistry{}},
			handler.EnqueueRequestsFromMapFunc(r.requestsForOCIRegistryChange),
			builder.WithPredicates(SourceRevisionChangePredicate{}),
		).
		WithOptions(controller.Options{MaxConcurrentReconciles: opts.MaxConcurrentReconciles}).
		Complete(r)
}

func (r *OCIArtifactReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, retErr error) {
	start := time.Now()
	log := ctrl.LoggerFrom(ctx)

	obj := &sourcev1.OCIArtifact{}
	if err := r.Get(ctx, req.NamespacedName, obj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Record suspended status metric
	r.RecordSuspend(ctx, obj, obj.Spec.Suspend)

	// Return early if the object is suspended
	if obj.Spec.Suspend {
		log.Info("reconciliation is suspended for this object")
		return ctrl.Result{}, nil
	}

	// Initialize the patch helper with the current version of the object.
	patchHelper, err := patch.NewHelper(obj, r.Client)
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
			summarize.WithConditions(ociArtifactReadyConditions),
			summarize.WithReconcileResult(recResult),
			summarize.WithReconcileError(retErr),
			summarize.WithIgnoreNotFound(),
			summarize.WithProcessors(
				summarize.RecordContextualError,
				summarize.RecordReconcileReq,
			),
			summarize.WithResultBuilder(sreconcile.AlwaysRequeueResultBuilder{RequeueAfter: obj.GetRequeueAfter()}),
			summarize.WithPatchFieldOwner(r.ControllerName),
		}
		result, retErr = summarizeHelper.SummarizeAndPatch(ctx, obj, summarizeOpts...)

		// Always record readiness and duration metrics
		r.Metrics.RecordReadiness(ctx, obj)
		r.Metrics.RecordDuration(ctx, obj, start)
	}()

	// Add finalizer first if not exist to avoid the race condition
	// between init and delete
	if !controllerutil.ContainsFinalizer(obj, sourcev1.SourceFinalizer) {
		controllerutil.AddFinalizer(obj, sourcev1.SourceFinalizer)
		recResult = sreconcile.ResultRequeue
		return
	}

	// Examine if the object is under deletion
	if !obj.ObjectMeta.DeletionTimestamp.IsZero() {
		recResult, retErr = r.reconcileDelete(ctx, obj)
		return
	}

	// Reconcile actual object
	reconcilers := []ociArtifactReconcilerFunc{
		r.reconcileStorage,
		r.reconcileSource,
		r.reconcileArtifact,
	}
	recResult, retErr = r.reconcile(ctx, obj, reconcilers)
	return
}

func (r *OCIArtifactReconciler) reconcile(ctx context.Context, obj *sourcev1.OCIArtifact, reconcilers []ociArtifactReconcilerFunc) (sreconcile.Result, error) {
	if obj.Generation != obj.Status.ObservedGeneration {
		conditions.MarkReconciling(obj, "NewGeneration", "reconciling new object generation (%d)", obj.Generation)
	}

	ociCtx, cancel := context.WithTimeout(ctx, obj.Spec.Timeout.Duration)
	defer cancel()

	var pullResult registry.PullResult
	var artifact sourcev1.Artifact

	// Run the sub-reconcilers and build the result of reconciliation.
	var res sreconcile.Result
	var resErr error
	for _, rec := range reconcilers {
		recResult, err := rec(ociCtx, obj, &artifact, &pullResult)
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

func (r *OCIArtifactReconciler) reconcileStorage(ctx context.Context,
	obj *sourcev1.OCIArtifact, artifact *sourcev1.Artifact, pullResult *registry.PullResult) (sreconcile.Result, error) {
	// Garbage collect previous advertised artifact(s) from storage
	_ = r.garbageCollect(ctx, obj)

	// Determine if the advertised artifact is still in storage
	if artifact := obj.GetArtifact(); artifact != nil && !r.Storage.ArtifactExist(*artifact) {
		obj.Status.Artifact = nil
		obj.Status.URL = ""
	}

	// Record that we do not have an artifact
	if obj.GetArtifact() == nil {
		conditions.MarkReconciling(obj, "NoArtifact", "no artifact for resource in storage")
		return sreconcile.ResultSuccess, nil
	}

	// Always update URLs to ensure hostname is up-to-date
	// TODO(hidde): we may want to send out an event only if we notice the URL has changed
	r.Storage.SetArtifactURL(obj.GetArtifact())
	obj.Status.URL = r.Storage.SetHostname(obj.Status.URL)

	return sreconcile.ResultSuccess, nil
}

func (r *OCIArtifactReconciler) reconcileSource(ctx context.Context,
	obj *sourcev1.OCIArtifact, artifact *sourcev1.Artifact, pullResult *registry.PullResult) (sreconcile.Result, error) {

	// Get the registry object
	registry, err := r.getRegistry(ctx, obj)
	if err != nil {
		e := &serror.Event{
			Err:    fmt.Errorf("failed to get registry: %w", err),
			Reason: "SourceUnavailable",
		}
		conditions.MarkTrue(obj, sourcev1.FetchFailedCondition, "SourceUnavailable", e.Err.Error())
		// Return error to the world as observed may change
		return sreconcile.ResultEmpty, e
	}

	// Login to the registry
	loginOpt, err := r.credentials(ctx, registry)
	if err != nil {
		e := &serror.Event{
			Err:    fmt.Errorf("failed to get credentials: %w", err),
			Reason: sourcev1.AuthenticationFailedReason,
		}
		conditions.MarkTrue(obj, sourcev1.FetchFailedCondition, sourcev1.AuthenticationFailedReason, e.Err.Error())
		// Return error to the world as observed may change
		return sreconcile.ResultEmpty, e
	}

	hostRegistry := normalizeURL(registry.Spec.URL)

	if loginOpt != nil {
		err = r.RegistryClient.Login(hostRegistry, loginOpt)
		if err != nil {
			e := &serror.Event{
				Err:    fmt.Errorf("failed to login to registry: %w", err),
				Reason: sourcev1.AuthenticationFailedReason,
			}
			conditions.MarkTrue(obj, sourcev1.FetchFailedCondition, sourcev1.AuthenticationFailedReason, e.Err.Error())
			// Return error to the world as observed may change
			return sreconcile.ResultEmpty, e
		}
	}

	ref, err := r.reference(obj, registry.Spec.URL)
	if err != nil {
		e := &serror.Event{
			Err:    err,
			Reason: sourcev1.OCIArtifactOperationFailedReason,
		}
		conditions.MarkTrue(obj, sourcev1.FetchFailedCondition, sourcev1.OCIArtifactOperationFailedReason, e.Err.Error())
		// Return error to the world as observed may change
		return sreconcile.ResultEmpty, e
	}

	// Pull the image
	result, err := r.RegistryClient.Pull(ref.String())
	if err != nil {
		e := &serror.Event{
			Err:    fmt.Errorf("failed to pull the artifact for obj '%s': %w", ref.String(), err),
			Reason: sourcev1.OCIArtifactOperationFailedReason,
		}
		conditions.MarkTrue(obj, sourcev1.FetchFailedCondition, sourcev1.OCIArtifactOperationFailedReason, e.Err.Error())
		// Return error to the world as observed may change
		return sreconcile.ResultEmpty, e
	}

	*pullResult = *result
	revision := fmt.Sprintf("%s@%s", fmt.Sprintf("%s/%s", ref.Registry, ref.Repository), pullResult.Manifest.Digest)

	// Mark observations about the revision on the object.
	if !obj.GetArtifact().HasRevision(revision) {
		message := fmt.Sprintf("new artifact revision '%s'", revision)
		conditions.MarkTrue(obj, sourcev1.ArtifactOutdatedCondition, "NewRevision", message)
		conditions.MarkReconciling(obj, "NewRevision", message)
	}

	conditions.Delete(obj, sourcev1.FetchFailedCondition)

	*artifact = r.Storage.NewArtifactFor(obj.Kind, obj.GetObjectMeta(), revision, fmt.Sprintf("%s.tar.gz", pullResult.Manifest.Digest))

	return sreconcile.ResultSuccess, nil
}

func (r *OCIArtifactReconciler) reconcileArtifact(ctx context.Context,
	obj *sourcev1.OCIArtifact, artifact *sourcev1.Artifact, pullResult *registry.PullResult) (sreconcile.Result, error) {
	// Always restore the Ready condition in case it got removed due to a transient error.
	defer func() {
		if obj.GetArtifact().HasRevision(artifact.Revision) {
			conditions.Delete(obj, sourcev1.ArtifactOutdatedCondition)
			conditions.MarkTrue(obj, meta.ReadyCondition, meta.SucceededReason,
				"stored artifact for revision '%s'", artifact.Revision)
		}
	}()

	if obj.GetArtifact().HasRevision(artifact.Revision) {
		ctrl.LoggerFrom(ctx).Info("artifact up-to-date", "revision", artifact.Revision)
		return sreconcile.ResultSuccess, nil
	}

	// Create artifact dir
	if err := r.Storage.MkdirAll(*artifact); err != nil {
		e := &serror.Event{
			Err:    fmt.Errorf("failed to create artifact directory: %w", err),
			Reason: sourcev1.DirCreationFailedReason,
		}
		conditions.MarkTrue(obj, sourcev1.StorageOperationFailedCondition, e.Reason, e.Err.Error())
		return sreconcile.ResultEmpty, e
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
	if obj.Spec.Ignore != nil {
		ps = append(ps, sourceignore.ReadPatterns(strings.NewReader(*obj.Spec.Ignore), ignoreDomain)...)
	}

	// We expect the tarball to be a single file
	if pullResult.Layers == nil || len(pullResult.Layers) == 0 {
		return sreconcile.ResultEmpty, &serror.Event{
			Err:    fmt.Errorf("no layers found in the pulled artifact"),
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
		e := &serror.Event{
			Err:    fmt.Errorf("unable to archive artifact to storage: %s", err),
			Reason: sourcev1.ArchiveOperationFailedReason,
		}
		conditions.MarkTrue(obj, sourcev1.StorageOperationFailedCondition, e.Reason, e.Err.Error())
		return sreconcile.ResultEmpty, e
	}
	r.AnnotatedEventf(obj, map[string]string{
		"revision": artifact.Revision,
		"checksum": artifact.Checksum,
	}, corev1.EventTypeNormal, "NewArtifact", "fetched artifact with ref %s", pullResult.Ref)

	// Record it on the object
	obj.Status.Artifact = artifact.DeepCopy()

	// update latest symlink
	url, err := r.Storage.Symlink(*artifact, "latest.tar.gz")
	if err != nil {
		r.eventLogf(ctx, obj, events.EventTypeTrace, sourcev1.SymlinkUpdateFailedReason,
			"failed to update status URL symlink: %s", err)
	}

	if url != "" {
		obj.Status.URL = url
	}
	return sreconcile.ResultSuccess, nil
}

// garbageCollect performs a garbage collection for the given v1beta1.OCIArtifact.
// It removes all but the current artifact except for when the
// deletion timestamp is set, which will result in the removal of
// all artifacts for the resource.
func (r *OCIArtifactReconciler) garbageCollect(ctx context.Context, obj *sourcev1.OCIArtifact) error {
	if !obj.DeletionTimestamp.IsZero() {
		if deleted, err := r.Storage.RemoveAll(r.Storage.NewArtifactFor(obj.Kind, obj.GetObjectMeta(), "", "*")); err != nil {
			return &serror.Event{
				Err:    fmt.Errorf("garbage collection for deleted resource failed: %w", err),
				Reason: "GarbageCollectionFailed",
			}
		} else if deleted != "" {
			r.eventLogf(ctx, obj, events.EventTypeTrace, "GarbageCollectionSucceeded",
				"garbage collected artifacts for deleted resource")
		}
		obj.Status.Artifact = nil
		return nil
	}
	if obj.GetArtifact() != nil {
		if deleted, err := r.Storage.RemoveAllButCurrent(*obj.GetArtifact()); err != nil {
			return &serror.Event{
				Err: fmt.Errorf("garbage collection of old artifacts failed: %w", err),
			}
		} else if len(deleted) > 0 {
			r.eventLogf(ctx, obj, events.EventTypeTrace, "GarbageCollectionSucceeded",
				"garbage collected old artifacts")
		}
	}
	return nil
}

// credentials retrieves the credentials from Authentication
func (r *OCIArtifactReconciler) credentials(ctx context.Context, obj *sourcev1.OCIRegistry) (registry.LoginOption, error) {
	username, password, err := getRegistryCredentials(ctx, r.Client, obj)
	if err != nil {
		return nil, err
	}

	return registry.LoginOptBasicAuth(username, password), nil
}

func (r *OCIArtifactReconciler) reconcileDelete(ctx context.Context, obj *sourcev1.OCIArtifact) (sreconcile.Result, error) {
	// Garbage collect the resource's artifacts
	if err := r.garbageCollect(ctx, obj); err != nil {
		// Return the error so we retry the failed garbage collection
		return sreconcile.ResultEmpty, err
	}

	// Remove our finalizer from the list
	controllerutil.RemoveFinalizer(obj, sourcev1.SourceFinalizer)

	// Stop reconciliation as the object is being deleted
	return sreconcile.ResultEmpty, nil
}

func (r *OCIArtifactReconciler) reference(obj *sourcev1.OCIArtifact, specURL string) (oras.Reference, error) {
	url := strings.TrimPrefix(specURL, fmt.Sprintf("%s://", "oci"))

	ref := obj.Spec.Reference
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

func (r *OCIArtifactReconciler) updateStatus(ctx context.Context, req ctrl.Request, newStatus sourcev1.OCIArtifactStatus) error {
	var obj sourcev1.OCIArtifact
	if err := r.Get(ctx, req.NamespacedName, &obj); err != nil {
		return err
	}

	patch := client.MergeFrom(obj.DeepCopy())
	obj.Status = newStatus

	return r.Status().Patch(ctx, &obj, patch)
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
func (r *OCIArtifactReconciler) eventLogf(ctx context.Context, obj runtime.Object, eventType string, reason string, messageFmt string, args ...interface{}) {
	msg := fmt.Sprintf(messageFmt, args...)
	// Log and emit event.
	if eventType == corev1.EventTypeWarning {
		ctrl.LoggerFrom(ctx).Error(errors.New(reason), msg)
	} else {
		ctrl.LoggerFrom(ctx).Info(msg)
	}
	r.Eventf(obj, eventType, reason, msg)
}

func (r *OCIArtifactReconciler) indexOCIRegistryByURL(o client.Object) []string {
	reg, ok := o.(*sourcev1.OCIRegistry)
	if !ok {
		panic(fmt.Sprintf("Expected an OCIRegistry, got %T", o))
	}
	u := normalizeURL(reg.Spec.URL)
	if u != "" {
		return []string{u}
	}
	return nil
}

func (r *OCIArtifactReconciler) requestsForOCIRegistryChange(o client.Object) []reconcile.Request {
	reg, ok := o.(*sourcev1.OCIRegistry)
	if !ok {
		panic(fmt.Sprintf("Expected an OCIRegistry, got %T", o))
	}

	ctx := context.Background()
	var list sourcev1.OCIArtifactList
	if err := r.List(ctx, &list, client.MatchingFields{
		sourcev1.SourceIndexKey: fmt.Sprintf("%s/%s", sourcev1.OCIRegistryKind, reg.Name),
	}); err != nil {
		return nil
	}

	var reqs []reconcile.Request
	for _, i := range list.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&i)})
	}
	return reqs
}

func normalizeURL(url string) string {
	u := strings.TrimPrefix(strings.Split(url, "/")[0], "https://")
	u = strings.TrimPrefix(u, "http://")
	u = strings.TrimPrefix(u, "oci://")
	return u
}

// getRegistry returns the v1beta2.OCIRegistry for the given object, or an error describing why the registry could not be
// returned.
func (r *OCIArtifactReconciler) getRegistry(ctx context.Context, obj *sourcev1.OCIArtifact) (*sourcev1.OCIRegistry, error) {
	namespacedName := types.NamespacedName{
		Namespace: obj.GetNamespace(),
		Name:      obj.Spec.OCIRegistryRef.Name,
	}

	var registry sourcev1.OCIRegistry
	if err := r.Client.Get(ctx, namespacedName, &registry); err != nil {
		fmt.Printf("error getting registry %s: %v\n", namespacedName.String(), err)
		return nil, err
	}

	return &registry, nil
}

func getRegistryCredentials(ctx context.Context, r client.Client, obj *sourcev1.OCIRegistry) (string, string, error) {
	auth := obj.Spec.Authentication
	if auth == nil {
		return "", "", nil
	}

	pullSecretNames := sets.NewString()
	if auth.SecretRef != nil {
		pullSecretNames.Insert(auth.SecretRef.Name)
	}

	// lookup service account
	if auth.ServiceAccountName != "" {
		serviceAccount := corev1.ServiceAccount{}
		err := r.Get(ctx, types.NamespacedName{Namespace: obj.GetNamespace(), Name: auth.ServiceAccountName}, &serviceAccount)
		if err != nil {
			return "", "", err
		}
		for _, ips := range serviceAccount.ImagePullSecrets {
			pullSecretNames.Insert(ips.Name)
		}
	}

	// lookup image pull secrets

	for _, imagePullSecretName := range pullSecretNames.List() {
		imagePullSecret := corev1.Secret{}
		err := r.Get(ctx, types.NamespacedName{Namespace: obj.GetNamespace(), Name: imagePullSecretName}, &imagePullSecret)
		if err != nil {
			return "", "", err
		}

		// retrieve the username and password from the secret
		if len(imagePullSecret.Data) != 0 {
			return string(imagePullSecret.Data["username"]), string(imagePullSecret.Data["password"]), nil
		}
	}

	return "", "", fmt.Errorf("no credentials found")
}
