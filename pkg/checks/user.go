package checks

import (
	checksv1alpha1 "github.com/fhnw-imvs/fhnw-kubeseccontext/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

type UserCheck struct{}

func (c *UserCheck) GetType() string {
	return "user"
}

func (c *UserCheck) GetSecurityContextDefaults(baseSecurityContext *checksv1alpha1.SecurityContextDefaults) *checksv1alpha1.SecurityContextDefaults {
	if baseSecurityContext.Pod.RunAsUser == nil {
		baseSecurityContext.Pod.RunAsUser = ptr.To(int64(1000)) // Default user ID
	}
	if baseSecurityContext.Pod.RunAsNonRoot == nil {
		baseSecurityContext.Pod.RunAsNonRoot = ptr.To(true) // Default to non-root
	}
	if baseSecurityContext.Container.RunAsUser == nil {
		baseSecurityContext.Container.RunAsUser = ptr.To(int64(1000)) // Default user ID
	}

	if baseSecurityContext.Container.RunAsNonRoot == nil {
		baseSecurityContext.Container.RunAsNonRoot = ptr.To(true) // Default to non-root
	}

	return baseSecurityContext
}

// This check should run if the pod spec does not have a RunAsUser set or RunAsNonRoot set
func (c *UserCheck) ShouldRun(podSpec *corev1.PodSpec) bool {
	if podSpec.SecurityContext == nil {
		return true
	}
	return podSpec.SecurityContext.RunAsUser == nil || podSpec.SecurityContext.RunAsNonRoot == nil
}

func init() {
	RegisterCheck(&UserCheck{})
}
