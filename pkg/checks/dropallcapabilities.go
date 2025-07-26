package checks

import (
	checksv1alpha1 "github.com/fhnw-imvs/fhnw-kubeseccontext/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

type DropAllCapabilitiesCheck struct{}

func (c *DropAllCapabilitiesCheck) GetType() string {
	return "dropAllCapabilities"
}

func (c *DropAllCapabilitiesCheck) GetSecurityContextDefaults(baseSecurityContext *checksv1alpha1.SecurityContextDefaults) *checksv1alpha1.SecurityContextDefaults {
	if baseSecurityContext.Container.CapabilitiesDrop == nil {
		baseSecurityContext.Container.CapabilitiesDrop = []corev1.Capability{"ALL"}
	}

	return baseSecurityContext
}

// This check should run if the pod spec does not have ReadOnlyRootFilesystem set
func (c *DropAllCapabilitiesCheck) ShouldRun(podSpec *corev1.PodSpec) bool {
	// if any container does not have ReadOnlyRootFilesystem set to true, we should run this check
	for _, container := range podSpec.Containers {
		if container.SecurityContext != nil {
			if container.SecurityContext.Capabilities == nil || len(container.SecurityContext.Capabilities.Drop) == 0 {
				return true
			}
		} else {
			return true
		}
	}

	return false
}

func init() {
	RegisterCheck(&DropAllCapabilitiesCheck{})
}
