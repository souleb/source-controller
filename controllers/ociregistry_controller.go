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
	"context"
	"crypto/x509"
	"fmt"
	"time"

	"github.com/fluxcd/pkg/apis/meta"
	"github.com/fluxcd/pkg/runtime/conditions"
	helper "github.com/fluxcd/pkg/runtime/controller"
	"github.com/fluxcd/pkg/runtime/patch"
	"github.com/fluxcd/pkg/runtime/predicates"
	sourcev1 "github.com/fluxcd/source-controller/api/v1beta2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	kuberecorder "k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

type OCIRegistryReconciler struct {
	client.Client
	kuberecorder.EventRecorder
	helper.Metrics
}

type OCIRegistryReconcilerOptions struct {
	MaxConcurrentReconciles int
}

func (r *OCIRegistryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return r.SetupWithManagerAndOptions(mgr, OCIRegistryReconcilerOptions{})
}

func (r *OCIRegistryReconciler) SetupWithManagerAndOptions(mgr ctrl.Manager, opts OCIRegistryReconcilerOptions) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&sourcev1.OCIRegistry{}, builder.WithPredicates(
			predicate.Or(predicate.GenerationChangedPredicate{}, predicates.ReconcileRequestedPredicate{}),
		)).
		WithOptions(controller.Options{MaxConcurrentReconciles: opts.MaxConcurrentReconciles}).
		Complete(r)
}

// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=ociregistries,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=ociregistries/status,verbs=get;update;patch

func (r *OCIRegistryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, retErr error) {
	start := time.Now()
	log := ctrl.LoggerFrom(ctx)

	registry := &sourcev1.OCIRegistry{}
	if err := r.Get(ctx, req.NamespacedName, registry); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	r.RecordSuspend(ctx, registry, registry.Spec.Suspend)
	// return early if the object is suspended
	if registry.Spec.Suspend {
		log.Info("Reconciliation is suspended for this object")
		return ctrl.Result{}, nil
	}

	patchHelper, err := patch.NewHelper(registry, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}

	defer func() {
		patchOpts := []patch.Option{
			patch.WithOwnedConditions{
				Conditions: []string{
					meta.ReadyCondition,
					meta.ReconcilingCondition,
					meta.StalledCondition,
				},
			},
		}

		if retErr == nil && (result.IsZero() || !result.Requeue) {
			conditions.Delete(registry, meta.ReconcilingCondition)

			patchOpts = append(patchOpts, patch.WithStatusObservedGeneration{})

			readyCondition := conditions.Get(registry, meta.ReadyCondition)
			switch readyCondition.Status {
			case metav1.ConditionFalse:
				// As we are no longer reconciling and the end-state is not ready, the reconciliation has stalled
				conditions.MarkStalled(registry, readyCondition.Reason, readyCondition.Message)
			case metav1.ConditionTrue:
				// As we are no longer reconciling and the end-state is ready, the reconciliation is no longer stalled
				conditions.Delete(registry, meta.StalledCondition)
			}
		}

		if err := patchHelper.Patch(ctx, registry, patchOpts...); err != nil {
			retErr = kerrors.NewAggregate([]error{retErr, err})
		}

		r.Metrics.RecordReadiness(ctx, registry)
		r.Metrics.RecordDuration(ctx, registry, start)

	}()

	return r.reconcile(ctx, registry)
}

func (r *OCIRegistryReconciler) reconcile(ctx context.Context, registry *sourcev1.OCIRegistry) (ctrl.Result, error) {
	// Mark the resource as under reconciliation
	conditions.MarkReconciling(registry, meta.ProgressingReason, "")

	// validate registry spec and credentials
	if err := r.validate(ctx, registry); err != nil {
		conditions.MarkFalse(registry, meta.ReadyCondition, sourcev1.ValidationFailedReason, err.Error())
		return ctrl.Result{}, err
	}

	conditions.MarkTrue(registry, meta.ReadyCondition, meta.SucceededReason, sourcev1.InitializedReason)
	ctrl.LoggerFrom(ctx).Info("Registry initialized")

	return ctrl.Result{}, nil
}

func (r *OCIRegistryReconciler) validate(ctx context.Context, registry *sourcev1.OCIRegistry) error {
	url := registry.Spec.URL
	if registry.Spec.Authentication.SecretRef != nil {
		var secret corev1.Secret
		secretName := types.NamespacedName{Namespace: registry.Namespace, Name: registry.Spec.Authentication.SecretRef.Name}

		if err := r.Get(ctx, secretName, &secret); err != nil {
			return fmt.Errorf("failed to read secret, error: %w", err)
		}

		if _, ok := secret.Data["username"]; !ok {
			return fmt.Errorf("secret %s does not contain a username", secretName.String())
		}

		if _, ok := secret.Data["password"]; !ok {
			return fmt.Errorf("secret %s does not contain a password", secretName.String())
		}
	} else if registry.Spec.Authentication.ServiceAccountName != "" {
		pullSecretNames := sets.NewString()
		var username, password string
		var serviceAccount corev1.ServiceAccount
		saName := types.NamespacedName{Namespace: registry.Namespace, Name: registry.Spec.Authentication.ServiceAccountName}

		err := r.Get(ctx, saName, &serviceAccount)
		if err != nil {
			return fmt.Errorf("failed to read service account, error: %w", err)
		}
		for _, ips := range serviceAccount.ImagePullSecrets {
			pullSecretNames.Insert(ips.Name)
		}

		// lookup image pull secrets

		for _, imagePullSecretName := range pullSecretNames.List() {
			imagePullSecret := corev1.Secret{}
			err := r.Get(ctx, types.NamespacedName{Namespace: registry.Namespace, Name: imagePullSecretName}, &imagePullSecret)
			if err != nil {
				return fmt.Errorf("failed to read imagePullSecret, error: %w", err)
			}

			// retrieve the username and password from the secret
			if len(imagePullSecret.Data) != 0 {
				username = string(imagePullSecret.Data["username"])
				password = string(imagePullSecret.Data["password"])
				if username != "" && password != "" {
					break
				}
			}
		}

		if username == "" || password == "" {
			return fmt.Errorf("service account %s does not contain credentials", serviceAccount.Name)
		}
	}

	if url == "" {
		return fmt.Errorf("no url found in 'spec.URL'")
	}

	var certPool *x509.CertPool
	if registry.Spec.CertSecretRef != nil {
		var secret corev1.Secret
		secretName := types.NamespacedName{Namespace: registry.Namespace, Name: registry.Spec.CertSecretRef.Name}

		if err := r.Get(ctx, secretName, &secret); err != nil {
			return fmt.Errorf("failed to read secret, error: %w", err)
		}

		caFile, ok := secret.Data["caFile"]
		if !ok {
			return fmt.Errorf("no caFile found in secret %s", registry.Spec.CertSecretRef.Name)
		}

		certPool = x509.NewCertPool()
		ok = certPool.AppendCertsFromPEM(caFile)
		if !ok {
			return fmt.Errorf("could not append to cert pool: invalid CA found in %s", registry.Spec.CertSecretRef.Name)
		}
	}

	return nil
}
