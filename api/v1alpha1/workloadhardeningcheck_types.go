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
	// +kubebuilder:default="5m"
	BaselineDuration string `json:"baselineDuration,omitempty"`

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
	// Set the values for pod securityContext fields. These values will be used in all checks, and only fields not specified here will be checked dynamically.
	Pod *PodSecurityContextDefaults `json:"pod,omitempty"`

	// Set the values for container securityContext fields. These values will be used in all checks, and only fields not specified here will be checked dynamically.
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

	// Suffix used for all namespaces created during testing
	// Could/Should probably be set from a webhook
	Suffix string `json:"suffix,omitempty"`

	BaselineRecording *WorkloadRecording `json:"baselineRecording,omitempty"`

	CheckRecordings []*WorkloadRecording `json:"checkRecordings,omitempty"`

	// Recordings of all signals
	// List of custom structs, the struct must contain a list of signals
	// (Again a struct, containing resource recordings as well as logs) for each check/job

	// Represents the observations of a WorkloadHardeningCheck's current state.
	// WorkloadHardeningCheck.status.conditions.type are: "Preparation", "Baseline", "Running", "Finished" and "Failed"
	// WorkloadHardeningCheck.status.conditions.status are one of True, False, Unknown.
	// WorkloadHardeningCheck.status.conditions.reason the value should be a CamelCase string and producers of specific
	// condition types may define expected values and meanings for this field, and whether the values
	// are considered a guaranteed API.
	// WorkloadHardeningCheck.status.conditions.Message is a human readable message indicating details about the transition.
	// For further information see: https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties

	// Conditions store the status conditions of the WorkloadHardeningCheck instances
	// +operator-sdk:csv:customresourcedefinitions:type=status
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,1,rep,name=conditions"`
}

// WorkloadRecording contains all signals recorded during a single check/job
type WorkloadRecording struct {
	// Type of workload recording. Allowed values: Baseline,HardeningJob
	// +kubebuilder:validation:Enum=Baseline;HardeningJob
	Type string `json:"type,omitempty"`

	// Indicates wether this configuration ran successfully or not
	Success bool `json:"success,omitempty"`

	// Time the recording started at
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Format=date-time
	StartTime metav1.Time `json:"startTime,omitempty" protobuf:"bytes,8,opt,name=timestamp"`

	// Time the recording ended at
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Format=date-time
	EndTime metav1.Time `json:"endTime,omitempty" protobuf:"bytes,8,opt,name=timestamp"`

	// The securityContext configurations applied for this job
	SecurityContextConfigurations *SecurityContextDefaults `json:"securityContextConfigurations,omitempty"`

	// The recorded cpu and memory usage during this check run
	RecordedMetrics []ResourceUsageRecord `json:"recordedMetrics,omitempty"`
	// The logs generated by the pod under test
	Logs []string `json:"logs,omitempty"`
}

type ResourceUsageRecord struct {
	// Time when this recording was taken. Used to sort the recordings
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Format=date-time
	Time metav1.Time `json:"timestamp,omitempty" protobuf:"bytes,8,opt,name=timestamp"`
	// CPU usage in nanocpu
	CPU int64 `json:"cpu,omitempty"`
	// Memory usage in bytes
	Memory int64 `json:"memory,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// WorkloadHardeningCheck is the Schema for the workloadhardeningchecks API
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:printcolumn:name="Status",type="string",JSONPath=".status.conditions[?(@.status==\"True\")].message"
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
