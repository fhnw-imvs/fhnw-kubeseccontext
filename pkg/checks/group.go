package checks

import (
	checksv1alpha1 "github.com/fhnw-imvs/fhnw-kubeseccontext/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

type GroupCheck struct{}

func (c *GroupCheck) GetType() string {
	return "group"
}

func (c *GroupCheck) GetSecurityContextDefaults(baseSecurityContext *checksv1alpha1.SecurityContextDefaults) *checksv1alpha1.SecurityContextDefaults {
	if baseSecurityContext.Pod.RunAsGroup == nil {
		baseSecurityContext.Pod.RunAsGroup = ptr.To(int64(1000)) // Default group ID
	}

	if baseSecurityContext.Pod.FSGroup == nil {
		baseSecurityContext.Pod.FSGroup = ptr.To(int64(1000)) // Default group ID
	}

	if baseSecurityContext.Container.RunAsGroup == nil {
		baseSecurityContext.Container.RunAsGroup = ptr.To(int64(1000)) // Default group ID
	}

	return baseSecurityContext
}

// This check should run if the pod spec does not have a RunAsGroup set
func (c *GroupCheck) ShouldRun(podSpec *corev1.PodSpec) bool {
	if podSpec.SecurityContext == nil {
		return true
	}

	return podSpec.SecurityContext.RunAsGroup == nil || podSpec.SecurityContext.FSGroup == nil
}

func init() {
	RegisterCheck(&GroupCheck{})
}
