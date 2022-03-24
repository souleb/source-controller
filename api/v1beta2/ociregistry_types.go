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

package v1beta2

import (
	"time"

	"github.com/fluxcd/pkg/apis/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// OCIRegistyKind is the string representation of a OCIRegistry.
	OCIRegistryKind = "OCIRegistry"

	// OCIRegistryURLIndexKey is the key to use for indexing OCIRegistry
	// resources by their OCIRegistrySpec.URL.
	OCIRegistryURLIndexKey = ".metadata.ociRegistryURL"
)

type OCIRegistrySpec struct {
	// URL is a reference to an image in a remote registry
	// +required
	URL string `json:"url"`

	// The credentials to use to pull and monitor for changes, defaults
	// to anonymous access.
	// +optional
	Authentication *OCIRegistryAuth `json:"auth,omitempty"`

	// CertSecretRef can be given the name of a secret containing
	// a PEM-encoded CA certificate (`caFile`)
	// +optional
	CertSecretRef *meta.LocalObjectReference `json:"certSecretRef,omitempty"`

	// This flag tells the controller to suspend subsequent events handling.
	// Defaults to false.
	// +optional
	Suspend bool `json:"suspend,omitempty"`
}

// OCIRegistryAuth defines the desired authentication mechanism of OCIRegistry
type OCIRegistryAuth struct {
	// SecretRef contains the secret name containing the registry login
	// credentials to resolve image metadata.
	// The secret must be of type kubernetes.io/dockerconfigjson.
	// +optional
	SecretRef *meta.LocalObjectReference `json:"secretRef,omitempty"`

	// ServiceAccountName is the name of the Kubernetes ServiceAccount used to authenticate
	// the image pull if the service account has attached pull secrets. For more information:
	// https://kubernetes.io/docs/tasks/configure-pod-container/configure-service-account/#add-imagepullsecrets-to-a-service-account
	// +optional
	ServiceAccountName string `json:"serviceAccountName,omitempty"`
}

// OCIRegistryStatus defines the observed state of OCIRegistry
type OCIRegistryStatus struct {
	// ObservedGeneration is the last observed generation.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions holds the conditions for the OCIRegistry.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// GetStatusConditions returns a pointer to the Status.Conditions slice
func (in *OCIRegistry) GetStatusConditions() *[]metav1.Condition {
	return &in.Status.Conditions
}

// GetConditions returns the status conditions of the object.
func (in *OCIRegistry) GetConditions() []metav1.Condition {
	return in.Status.Conditions
}

// SetConditions sets the status conditions on the object.
func (in *OCIRegistry) SetConditions(conditions []metav1.Condition) {
	in.Status.Conditions = conditions
}

// GetRequeueAfter returns the duration after which the source must be
// reconciled again.
func (in OCIRegistry) GetRequeueAfter() time.Duration {
	// We need to implement this method to satisfy the ArtifactSource interface
	return 0
}

// GetArtifact returns the latest artifact from the source if present in the
// status sub-resource.
func (in *OCIRegistry) GetArtifact() *Artifact {
	// We need to implement this method to satisfy the ArtifactSource interface
	return nil
}

// +genclient
// +genclient:Namespaced
//+kubebuilder:object:root=true
// +kubebuilder:resource:shortName=ocireg
//+kubebuilder:subresource:status
//+kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.spec.url`
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type==\"Ready\")].status",description=""
// +kubebuilder:printcolumn:name="Status",type="string",JSONPath=".status.conditions[?(@.type==\"Ready\")].message",description=""
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description=""

// OCIRegistry is the Schema for the ociregistries API
type OCIRegistry struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   OCIRegistrySpec   `json:"spec,omitempty"`
	Status OCIRegistryStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// OCIRegistryList contains a list of OCIRegistry
type OCIRegistryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OCIRegistry `json:"items"`
}

func init() {
	SchemeBuilder.Register(&OCIRegistry{}, &OCIRegistryList{})
}
