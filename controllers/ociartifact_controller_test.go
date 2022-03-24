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
	"fmt"
	"io/ioutil"
	"testing"

	"github.com/darkowlzz/controller-check/status"
	_ "github.com/distribution/distribution/v3/registry/auth/htpasswd"
	_ "github.com/distribution/distribution/v3/registry/storage/driver/inmemory"
	"github.com/fluxcd/pkg/apis/meta"
	"github.com/fluxcd/pkg/runtime/conditions"
	"github.com/fluxcd/pkg/runtime/patch"
	sourcev1 "github.com/fluxcd/source-controller/api/v1beta2"
	"github.com/fluxcd/source-controller/pkg/registry"
	. "github.com/onsi/gomega"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kstatus "sigs.k8s.io/cli-utils/pkg/kstatus/status"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestOCIArtifactReconciler_Reconcile(t *testing.T) {
	g := NewWithT(t)

	// Login to the registry
	err := testRegistryserver.RegistryClient.Login(testRegistryserver.DockerRegistryHost,
		registry.LoginOptBasicAuth(testUsername, testPassword),
		registry.LoginOptInsecure(true))
	g.Expect(err).NotTo(HaveOccurred())

	// Push a test image to the registry
	// push a kustomization archive
	kustomizationData, err := ioutil.ReadFile("./testdata/podinfo-kustomize-1.0.0.tgz")
	g.Expect(err).NotTo(HaveOccurred())

	contents := registry.Contents{
		{
			Data:      nil,
			MediaType: ocispec.MediaTypeImageConfig,
		},
		{
			Data:      kustomizationData,
			MediaType: ocispec.MediaTypeImageLayerGzip,
		},
	}

	ref := fmt.Sprintf("%s/ocirepo/%s:%s", testRegistryserver.DockerRegistryHost, "podinfo-kustomize", "1.0.0")
	_, err = testRegistryserver.RegistryClient.Push(contents, ocispec.MediaTypeImageConfig, ref)
	g.Expect(err).NotTo(HaveOccurred())

	pullResult, err := testRegistryserver.RegistryClient.Pull(ref)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(*pullResult.Layers[0]).To(HaveField("MediaType", ocispec.MediaTypeImageLayerGzip))

	ns, err := testEnv.CreateNamespace(ctx, "oci-artifact-reconciler-test")
	g.Expect(err).ToNot(HaveOccurred())
	defer func() { g.Expect(testEnv.Delete(ctx, ns)).To(Succeed()) }()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "ociregistry-",
			Namespace:    ns.Name,
		},
		Data: map[string][]byte{
			"username": []byte(testUsername),
			"password": []byte(testPassword),
		},
	}

	g.Expect(testEnv.CreateAndWait(ctx, secret)).To(Succeed())

	registry := &sourcev1.OCIRegistry{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "ociregistry-",
			Namespace:    ns.Name,
		},
		Spec: sourcev1.OCIRegistrySpec{
			URL: fmt.Sprintf("%s/ocirepo/%s", testRegistryserver.DockerRegistryHost, "podinfo-kustomize"),
			Authentication: &sourcev1.OCIRegistryAuth{
				SecretRef: &meta.LocalObjectReference{
					Name: secret.Name,
				},
			},
		},
	}
	g.Expect(testEnv.CreateAndWait(ctx, registry)).To(Succeed())

	key := client.ObjectKey{Name: registry.Name, Namespace: registry.Namespace}

	// Wait for OCIRegistry to be Ready
	g.Eventually(func() bool {
		if err := testEnv.Get(ctx, key, registry); err != nil {
			return false
		}
		if !conditions.IsReady(registry) {
			return false
		}
		readyCondition := conditions.Get(registry, meta.ReadyCondition)
		return registry.Generation == readyCondition.ObservedGeneration &&
			registry.Generation == registry.Status.ObservedGeneration
	}, timeout).Should(BeTrue())

	obj := &sourcev1.OCIArtifact{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "ociartifact-reconcile-",
			Namespace:    ns.Name,
		},
		Spec: sourcev1.OCIArtifactSpec{
			OCIRegistryRef: meta.LocalObjectReference{
				Name: registry.Name,
			},
			Reference: &sourcev1.OCIArtifactRef{
				SemVer: "1.0.x",
			},
			Interval: metav1.Duration{Duration: interval},
		},
	}
	g.Expect(testEnv.Create(ctx, obj)).To(Succeed())

	key = client.ObjectKey{Name: obj.Name, Namespace: obj.Namespace}

	// Wait for finalizer to be set
	g.Eventually(func() bool {
		if err := testEnv.Get(ctx, key, obj); err != nil {
			return false
		}
		return len(obj.Finalizers) > 0
	}, timeout).Should(BeTrue())

	// Wait for OCIArtifact to be Ready
	g.Eventually(func() bool {
		if err := testEnv.Get(ctx, key, obj); err != nil {
			return false
		}
		if !conditions.IsReady(obj) || obj.Status.Artifact == nil {
			return false
		}
		readyCondition := conditions.Get(obj, meta.ReadyCondition)
		return obj.Generation == readyCondition.ObservedGeneration &&
			obj.Generation == obj.Status.ObservedGeneration
	}, timeout).Should(BeTrue())

	// Check if the object status is valid.
	condns := &status.Conditions{NegativePolarity: ociArtifactReadyConditions.NegativePolarity}
	checker := status.NewChecker(testEnv.Client, testEnv.GetScheme(), condns)
	checker.CheckErr(ctx, obj)

	// kstatus client conformance check.
	u, err := patch.ToUnstructured(obj)
	g.Expect(err).ToNot(HaveOccurred())
	res, err := kstatus.Compute(u)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res.Status).To(Equal(kstatus.CurrentStatus))

	// Patch the object with reconcile request annotation.
	patchHelper, err := patch.NewHelper(obj, testEnv.Client)
	g.Expect(err).ToNot(HaveOccurred())
	annotations := map[string]string{
		meta.ReconcileRequestAnnotation: "now",
	}
	obj.SetAnnotations(annotations)
	g.Expect(patchHelper.Patch(ctx, obj)).ToNot(HaveOccurred())
	g.Eventually(func() bool {
		if err := testEnv.Get(ctx, key, obj); err != nil {
			return false
		}
		return obj.Status.LastHandledReconcileAt == "now"
	}, timeout).Should(BeTrue())

	g.Expect(testEnv.Delete(ctx, obj)).To(Succeed())

	// Wait for OCIArtifact to be deleted
	g.Eventually(func() bool {
		if err := testEnv.Get(ctx, key, obj); err != nil {
			return apierrors.IsNotFound(err)
		}
		return false
	}, timeout).Should(BeTrue())
}
