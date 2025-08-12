package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// NamespaceHardeningCheckSpec defines the desired state of NamespaceHardeningCheck
type NamespaceHardeningCheckSpec struct {

	// TargetNamespace references the namespace where all workloads should be hardened.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	TargetNamespace string `json:"targetNamespace,omitempty"`

	// RecordingDuration specifies how long to observe the baseline workload before applying hardening tests.
	// +kubebuilder:validation:Pattern=`^\d+[smh]$`
	// +kubebuilder:default="5m"
	RecordingDuration string `json:"recordingDuration,omitempty"`

	// RunMode specifies whether the checks should be run in parallel or sequentially.
	// +kubebuilder:validation:Enum=parallel;sequential
	// +kubebuilder:default=parallel
	RunMode string `json:"runMode,omitempty"`

	// SecurityContext allows the user to define default values for Pod and Container SecurityContext fields.
	SecurityContext *SecurityContextDefaults `json:"securityContext,omitempty"`

	// Suffix used for all namespaces created during testing. If not specified, a random suffix will be generated.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]{0,5}$`
	// +kubebuilder:default=""
	Suffix string `json:"suffix,omitempty"`
}

// NamespaceHardeningCheckStatus defines the observed state of NamespaceHardeningCheck.
type NamespaceHardeningCheckStatus struct {

	// Conditions store the status conditions of the WorkloadHardeningCheck instances
	// +operator-sdk:csv:customresourcedefinitions:type=status
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,1,rep,name=conditions"`

	// Recommendation contains the recommended security contexts for all workloads in the target namespace.
	Recommendations map[string]*Recommendation `json:"recommendations,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:printcolumn:name="Finished",type="string",JSONPath=".status.conditions[?(@.type==\"Finished\")].status"
// +kubebuilder:printcolumn:name="Suffix",type="string",JSONPath=".spec.suffix"
// +kubebuilder:printcolumn:name="Status",type="string",JSONPath=".status.conditions[?(@.type==\"Finished\")].message"

// NamespaceHardeningCheck is the Schema for the namespacehardeningchecks API
type NamespaceHardeningCheck struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of NamespaceHardeningCheck
	// +required
	Spec NamespaceHardeningCheckSpec `json:"spec"`

	// status defines the observed state of NamespaceHardeningCheck
	// +optional
	Status NamespaceHardeningCheckStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// NamespaceHardeningCheckList contains a list of NamespaceHardeningCheck
type NamespaceHardeningCheckList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NamespaceHardeningCheck `json:"items"`
}

func init() {
	SchemeBuilder.Register(&NamespaceHardeningCheck{}, &NamespaceHardeningCheckList{})
}
