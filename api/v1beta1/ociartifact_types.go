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

package v1beta1

import (
	"time"

	"github.com/fluxcd/pkg/apis/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// OCIArtifactKind is the string representation of a OCIArtifact.
	OCIArtifactKind = "OCIArtifact"
)

// OCIArtifactSpec defines the desired state of OCIArtifact
type OCIArtifactSpec struct {
	// Pull artifacts using this registry.
	// +required
	OCIRegistryRef meta.LocalObjectReference `json:"ociRegistryRef"`

	// The OCI reference to pull and monitor for changes, defaults to
	// latest tag.
	// +optional
	Reference *OCIArtifactRef `json:"ref,omitempty"`

	// The interval at which to check for image updates.
	// +required
	Interval metav1.Duration `json:"interval"`

	// The timeout for remote OCI Repository operations like pulling, defaults to 20s.
	// +kubebuilder:default="20s"
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`

	// Ignore overrides the set of excluded patterns in the .sourceignore format
	// (which is the same as .gitignore). If not provided, a default will be used,
	// consult the documentation for your version to find out what those are.
	// +optional
	Ignore *string `json:"ignore,omitempty"`

	// This flag tells the controller to suspend the reconciliation of this source.
	// +optional
	Suspend bool `json:"suspend,omitempty"`
}

// OCIArtifactRef defines the image reference for the OCIArtifact's URL
type OCIArtifactRef struct {
	// Digest is the image digest to pull, takes precedence over SemVer.
	// Value should be in the form sha256:cbbf2f9a99b47fc460d422812b6a5adff7dfee951d8fa2e4a98caa0382cfbdbf
	// +optional
	Digest string `json:"digest,omitempty"`

	// SemVer is the range of tags to pull selecting the latest within
	// the range, takes precedence over Tag.
	// +optional
	SemVer string `json:"semver,omitempty"`

	// Tag is the image tag to pull, defaults to latest.
	// +kubebuilder:default:=latest
	// +optional
	Tag string `json:"tag,omitempty"`
}

// OCIArtifactStatus defines the observed state of OCIArtifact
type OCIArtifactStatus struct {
	// ObservedGeneration is the last observed generation.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions holds the conditions for the OCIArtifact.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// URL is the download link for the artifact output of the last
	//image  sync.
	// +optional
	URL string `json:"url,omitempty"`

	// Artifact represents the output of the last successful image sync.
	// +optional
	Artifact *Artifact `json:"artifact,omitempty"`

	meta.ReconcileRequestStatus `json:",inline"`
}

const (
	// OCIArtifactOperationSucceedReason represents the fact that the
	// image pull operation succeeded.
	OCIArtifactOperationSucceedReason string = "OCIArtifactOperationSucceed"

	// OCIArtifactOperationFailedReason represents the fact that the
	// image pull operation failed.
	OCIArtifactOperationFailedReason string = "OCIArtifactOperationFailed"
)

// GetConditions returns the status conditions of the object.
func (in OCIArtifact) GetConditions() []metav1.Condition {
	return in.Status.Conditions
}

// SetConditions sets the status conditions on the object.
func (in *OCIArtifact) SetConditions(conditions []metav1.Condition) {
	in.Status.Conditions = conditions
}

// GetRequeueAfter returns the duration after which the source must be reconciled again.
func (in OCIArtifact) GetRequeueAfter() time.Duration {
	return in.Spec.Interval.Duration
}

// GetArtifact returns the latest artifact from the source if present in the
// status sub-resource.
func (in *OCIArtifact) GetArtifact() *Artifact {
	return in.Status.Artifact
}

// +genclient
// +genclient:Namespaced
//+kubebuilder:object:root=true
// +kubebuilder:resource:shortName=ociartifact
//+kubebuilder:subresource:status
//+kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.spec.url`
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type==\"Ready\")].status",description=""
// +kubebuilder:printcolumn:name="Status",type="string",JSONPath=".status.conditions[?(@.type==\"Ready\")].message",description=""
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description=""

// OCIArtifact is the Schema for the ociartifacts API
type OCIArtifact struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   OCIArtifactSpec   `json:"spec,omitempty"`
	Status OCIArtifactStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// OCIArtifactList contains a list of OCIArtifact
type OCIArtifactList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OCIArtifact `json:"items"`
}

func init() {
	SchemeBuilder.Register(&OCIArtifact{}, &OCIArtifactList{})
}
