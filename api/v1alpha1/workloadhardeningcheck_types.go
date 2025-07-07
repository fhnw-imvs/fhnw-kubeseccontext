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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

const (
	RunModeParallel   = "parallel"
	RunModeSequential = "sequential"
)

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

	// Suffix used for all namespaces created during testing. If not specified, a random suffix will be generated.
	// +kubebuilder:validation:Pattern=`^[a-z0-9-]{0,15}$`
	// +kubebuilder:default=""
	Suffix string `json:"suffix,omitempty"`
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
	RunAsNonRoot       *bool   `json:"runAsNonRoot,omitempty"`
	SupplementalGroups []int64 `json:"supplementalGroups,omitempty"`

	// SeccompProfile type (e.g., RuntimeDefault, Localhost, Unconfined).
	SeccompProfile *SeccompProfile `json:"seccompProfile,omitempty"`
}

func (p *PodSecurityContextDefaults) ToK8sSecurityContext() *corev1.PodSecurityContext {
	if p == nil {
		return nil
	}

	sc := &corev1.PodSecurityContext{
		FSGroup:            p.FSGroup,
		RunAsUser:          p.RunAsUser,
		RunAsGroup:         p.RunAsGroup,
		RunAsNonRoot:       p.RunAsNonRoot,
		SupplementalGroups: p.SupplementalGroups,
	}

	if p.SeccompProfile != nil {
		sc.SeccompProfile = &corev1.SeccompProfile{
			Type: corev1.SeccompProfileType(p.SeccompProfile.Type),
		}
	}

	return sc
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

func (c *ContainerSecurityContextDefaults) ToK8sSecurityContext() *corev1.SecurityContext {
	if c == nil {
		return nil
	}

	sc := &corev1.SecurityContext{
		RunAsNonRoot:             c.RunAsNonRoot,
		ReadOnlyRootFilesystem:   c.ReadOnlyRootFilesystem,
		AllowPrivilegeEscalation: c.AllowPrivilegeEscalation,
		RunAsUser:                c.RunAsUser,
		RunAsGroup:               c.RunAsGroup,
	}

	if len(c.CapabilitiesDrop) > 0 {
		// Convert string slice to corev1.Capabilities
		dropCapabilities := make([]corev1.Capability, len(c.CapabilitiesDrop))
		for i, cap := range c.CapabilitiesDrop {
			dropCapabilities[i] = corev1.Capability(cap)
		}

		sc.Capabilities = &corev1.Capabilities{
			Drop: dropCapabilities,
		}
	}

	if c.SeccompProfile != nil {
		sc.SeccompProfile = &corev1.SeccompProfile{
			Type: corev1.SeccompProfileType(c.SeccompProfile.Type),
		}
	}

	return sc
}

// SeccompProfile specifies the seccomp profile type.
type SeccompProfile struct {
	// Type of seccomp profile. Allowed values: RuntimeDefault, Localhost, Unconfined.
	// +kubebuilder:validation:Enum=RuntimeDefault;Localhost;Unconfined
	Type string `json:"type"`
}

const (
	// Represents the initial state, before the baseline recording is started
	ConditionTypePreparation = "Preparation"
	// Represents a running baseline recording
	ConditionTypeBaseline = "Baseline"
	// Represents ongoing check jobs, this state will be used until all checks are finished
	ConditionTypeCheck = "Check"
	// All checks are finished, and the results are being analyzed, and the recommendations are being generated
	ConditionTypeAnalysis = "Analyzing"
	// All checks are finished, the comparison of the baseline and the checks is done, and the results are available
	ConditionTypeFinished = "Finished"

	// Baseline recording states
	ReasonBaselineRecording         = "BaselineRecording"
	ReasonBaselineRecordingFailed   = "BaselineRecordingFailed"
	ReasonBaselineRecordingFinished = "BaselineRecordingFinished"

	// single check, prefixed with the check name
	ReasonCheckRecording         = "CheckRecording"
	ReasonCheckRecordingFailed   = "CheckRecordingFailed"
	ReasonCheckRecordingFinished = "CheckRecordingFinished"
)

// WorkloadHardeningCheckStatus defines the observed state of WorkloadHardeningCheck
type WorkloadHardeningCheckStatus struct {

	// Represents the observations of a WorkloadHardeningCheck's current state.
	// WorkloadHardeningCheck.status.conditions.type are: "Preparation", "Baseline", "Check"
	// WorkloadHardeningCheck.status.conditions.status are one of True, False, Unknown.
	// WorkloadHardeningCheck.status.conditions.reason the value should be a CamelCase string and producers of specific
	// condition types may define expected values and meanings for this field, and whether the values
	// are considered a guaranteed API.
	// WorkloadHardeningCheck.status.conditions.Message is a human readable message indicating details about the transition.
	// For further information see: https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties

	// Conditions store the status conditions of the WorkloadHardeningCheck instances
	// +operator-sdk:csv:customresourcedefinitions:type=status
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,1,rep,name=conditions"`

	// A list of all check runs that were executed as part of this WorkloadHardeningCheck.
	// Each check run corresponds to a specific security context configuration applied to the workload.
	CheckRuns []CheckRun `json:"checkRuns,omitempty"`

	Recommendation *Recommendation `json:"recommendation,omitempty"`
}

// CheckRun represents the result of a single check run within a WorkloadHardeningCheck.
type CheckRun struct {
	// Name of the check run, e.g., "Baseline", "User", "ReadOnlyRootFilesystem", etc.
	Name string `json:"name,omitempty"`
	// Boolean flag indicating whether the check run was successful or not. A check is considered successful if the workload started and metrics and logs were recorded.
	RecordingSuccessfull *bool `json:"recordingSuccessfull,omitempty"`
	// Boolean flag indicating whether the check run was successful or not. A check is considered successful if the workload started and metrics and logs were recorded.
	CheckSuccessfull *bool `json:"checkSuccessfull,omitempty"`
	// SecurityContext which was applied for this check run.
	SecurityContext *SecurityContextDefaults `json:"securityContext,omitempty"`
	// FailureReason provides a reason for the failure of the check run, if applicable.
	FailureReason string              `json:"failureReason,omitempty"`
	Anomalies     map[string][]string `json:"anomalies,omitempty"`
}

// Recommendation provides the recommended security contexts for the workload under test.
type Recommendation struct {
	// ContainerSecurityContexts is a map of container names to their recommended security contexts.
	ContainerSecurityContexts *corev1.SecurityContext `json:"containerSecurityContexts,omitempty"`
	// PodSecurityContext is the recommended security context for the pod under test.
	PodSecurityContext *corev1.PodSecurityContext `json:"podSecurityContext,omitempty"`
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
