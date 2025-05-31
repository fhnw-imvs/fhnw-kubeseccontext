/*
Copyright 2025.

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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// WorkloadHardeningCheckSpec defines the desired state of WorkloadHardeningCheck
type WorkloadHardeningCheckSpec struct {
	// TargetRef specifies the workload to be hardened.
	// +kubebuilder:validation:Required
	TargetRef TargetReference `json:"targetRef"`

	// BaselineDuration specifies how long to observe the baseline workload before applying hardening tests.
	// +kubebuilder:validation:Pattern=`^\d+[smh]$`
	// +kubebuilder:validation:Required
	BaselineDuration string `json:"baselineDuration"`

	// RunMode specifies whether the checks should be run in parallel or sequentially.
	// +kubebuilder:validation:Enum=parallel;sequential
	// +kubebuilder:default=parallel
	RunMode string `json:"runMode,omitempty"`

	// SecurityContext allows the user to define default values for Pod and Container SecurityContext fields.
	SecurityContext *SecurityContextDefaults `json:"securityContext,omitempty"`
}

// TargetReference defines a reference to a Kubernetes workload.
type TargetReference struct {
	// API version of the workload.
	// +kubebuilder:validation:Required
	APIVersion string `json:"apiVersion"`

	// Kind of the workload (e.g., Deployment, StatefulSet).
	// +kubebuilder:validation:Required
	Kind string `json:"kind"`

	// Name of the workload.
	// +kubebuilder:validation:Required
	Name string `json:"name"`
}

// SecurityContextDefaults defines the defaults for podSecurityContext and containerSecurityContext fields.
type SecurityContextDefaults struct {
	// PodSecurityContext defaults.
	Pod *PodSecurityContextDefaults `json:"pod,omitempty"`

	// ContainerSecurityContext defaults.
	Container *ContainerSecurityContextDefaults `json:"container,omitempty"`
}

// PodSecurityContextDefaults specifies the fields for the Pod-level security context.
type PodSecurityContextDefaults struct {
	FSGroup            *int64  `json:"fsGroup,omitempty"`
	RunAsUser          *int64  `json:"runAsUser,omitempty"`
	RunAsGroup         *int64  `json:"runAsGroup,omitempty"`
	SupplementalGroups []int64 `json:"supplementalGroups,omitempty"`

	// SeccompProfile type (e.g., RuntimeDefault, Localhost, Unconfined).
	SeccompProfile *SeccompProfile `json:"seccompProfile,omitempty"`
}

// ContainerSecurityContextDefaults specifies the fields for the Container-level security context.
type ContainerSecurityContextDefaults struct {
	RunAsNonRoot             *bool           `json:"runAsNonRoot,omitempty"`
	ReadOnlyRootFilesystem   *bool           `json:"readOnlyRootFilesystem,omitempty"`
	AllowPrivilegeEscalation *bool           `json:"allowPrivilegeEscalation,omitempty"`
	CapabilitiesDrop         []string        `json:"capabilitiesDrop,omitempty"`
	SeccompProfile           *SeccompProfile `json:"seccompProfile,omitempty"`
	RunAsUser                *int64          `json:"runAsUser,omitempty"`
	RunAsGroup               *int64          `json:"runAsGroup,omitempty"`
}

// SeccompProfile specifies the seccomp profile type.
type SeccompProfile struct {
	// Type of seccomp profile. Allowed values: RuntimeDefault, Localhost, Unconfined.
	// +kubebuilder:validation:Enum=RuntimeDefault;Localhost;Unconfined
	Type string `json:"type"`
}

// WorkloadHardeningCheckStatus defines the observed state of WorkloadHardeningCheck
type WorkloadHardeningCheckStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// WorkloadHardeningCheck is the Schema for the workloadhardeningchecks API
type WorkloadHardeningCheck struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   WorkloadHardeningCheckSpec   `json:"spec,omitempty"`
	Status WorkloadHardeningCheckStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// WorkloadHardeningCheckList contains a list of WorkloadHardeningCheck
type WorkloadHardeningCheckList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []WorkloadHardeningCheck `json:"items"`
}

func init() {
	SchemeBuilder.Register(&WorkloadHardeningCheck{}, &WorkloadHardeningCheckList{})
}
