package workload

import (
	"context"
	"fmt"

	checksv1alpha1 "github.com/fhnw-imvs/fhnw-kubeseccontext/api/v1alpha1"
	"golang.org/x/exp/maps"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

const (
	// Sets runAsGroup and fsGroup to non-zero values
	CheckTypeGroup = "group"
	// Sets runAsUser to non-zero values, and runAsNonRoot to true
	CheckTypeUser = "user"
	// Sets readOnlyRootFilesystem to true
	CheckTypeReadOnlyRootFilesystem = "readOnlyRootFilesystem"
	// Sets allowPrivilegeEscalation to false
	CheckTypeAllowPrivilegeEscalation = "allowPrivilegeEscalation"
	// Sets capabilities to drop all capabilities
	CheckTypeDropCapabilitiesAll = "dropCapabilities"
)

func (m *WorkloadCheckManager) GetRequiredCheckRuns(ctx context.Context) []string {
	checks := map[string]bool{}

	workloadUnderTest, err := m.GetWorkloadUnderTest(ctx, m.workloadHardeningCheck.Namespace)
	if err != nil {
		m.logger.Error(err, "Failed to get workload under test")
		return []string{}
	}

	// Determine the type of workload under test and extract the pod spec template
	var podSpecTemplate *v1.PodSpec
	switch v := (*workloadUnderTest).(type) {
	case *appsv1.Deployment:
		podSpecTemplate = &v.Spec.Template.Spec
	case *appsv1.StatefulSet:
		podSpecTemplate = &v.Spec.Template.Spec
	case *appsv1.DaemonSet:
		podSpecTemplate = &v.Spec.Template.Spec
	}

	if podSpecTemplate.SecurityContext != nil {
		if podSpecTemplate.SecurityContext.RunAsGroup == nil || podSpecTemplate.SecurityContext.FSGroup == nil {
			checks[CheckTypeGroup] = true
		}
		if podSpecTemplate.SecurityContext.RunAsUser == nil || podSpecTemplate.SecurityContext.RunAsNonRoot == nil {
			checks[CheckTypeUser] = true
		}
	} else {
		checks[CheckTypeGroup] = true
		checks[CheckTypeUser] = true
	}

	for _, container := range podSpecTemplate.Containers {
		if container.SecurityContext != nil {
			if container.SecurityContext.ReadOnlyRootFilesystem == nil || !*container.SecurityContext.ReadOnlyRootFilesystem {
				checks[CheckTypeReadOnlyRootFilesystem] = true
			}
			if container.SecurityContext.AllowPrivilegeEscalation == nil || *container.SecurityContext.AllowPrivilegeEscalation {
				checks[CheckTypeAllowPrivilegeEscalation] = true
			}
			if container.SecurityContext.Capabilities == nil || len(container.SecurityContext.Capabilities.Drop) == 0 {
				checks[CheckTypeDropCapabilitiesAll] = true
			}
		} else {
			// If the container does not have a security context, we assume it needs all checks
			checks[CheckTypeReadOnlyRootFilesystem] = true
			checks[CheckTypeAllowPrivilegeEscalation] = true
			checks[CheckTypeDropCapabilitiesAll] = true
		}
	}

	for key := range checks {
		if meta.FindStatusCondition(m.workloadHardeningCheck.Status.Conditions, titleCase.String(key)+checksv1alpha1.ConditionTypeCheck) == nil {
			// if the conditions is not found, we add it in unknown state
			m.SetCondition(ctx, metav1.Condition{
				Type:    titleCase.String(key) + checksv1alpha1.ConditionTypeCheck,
				Status:  metav1.ConditionUnknown,
				Reason:  checksv1alpha1.ReasonCheckNotStarted,
				Message: fmt.Sprintf("Check %s has not been started yet", key),
			})

		} else if m.CheckRecorded(key) {
			// If the check is already recorded, we don't need to run it again
			delete(checks, key)
		}

	}

	return maps.Keys(checks)

}

func (m *WorkloadCheckManager) GetSecurityContextForCheckType(checkType string) *checksv1alpha1.SecurityContextDefaults {

	baseSecurityContext := m.workloadHardeningCheck.Spec.SecurityContext
	if baseSecurityContext == nil {
		baseSecurityContext = &checksv1alpha1.SecurityContextDefaults{
			Pod:       &checksv1alpha1.PodSecurityContextDefaults{},
			Container: &checksv1alpha1.ContainerSecurityContextDefaults{},
		}
	}

	switch checkType {
	case CheckTypeGroup:
		return m.securityContextForCheckTypeGroup(baseSecurityContext)
	case CheckTypeUser:
		return m.securityContextForCheckTypeUser(baseSecurityContext)
	case CheckTypeReadOnlyRootFilesystem:
		return m.securityContextForCheckTypeReadOnlyRootFilesystem(baseSecurityContext)
	case CheckTypeAllowPrivilegeEscalation:
		return m.securityContextForCheckTypeAllowPrivilegeEscalation(baseSecurityContext)
	case CheckTypeDropCapabilitiesAll: // This could be made more granular in the future
		return m.securityContextForCheckTypeCapabilitiesDropAll(baseSecurityContext)
	}

	return baseSecurityContext

}

func (m *WorkloadCheckManager) securityContextForCheckTypeGroup(baseSecurityContext *checksv1alpha1.SecurityContextDefaults) *checksv1alpha1.SecurityContextDefaults {

	if baseSecurityContext.Pod.RunAsGroup == nil {
		baseSecurityContext.Pod.RunAsGroup = ptr.To(int64(1000)) // Default group ID
	}

	if baseSecurityContext.Container.RunAsGroup == nil {
		baseSecurityContext.Container.RunAsGroup = ptr.To(int64(1000)) // Default group ID
	}

	return baseSecurityContext
}

func (m *WorkloadCheckManager) securityContextForCheckTypeUser(baseSecurityContext *checksv1alpha1.SecurityContextDefaults) *checksv1alpha1.SecurityContextDefaults {
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

func (m *WorkloadCheckManager) securityContextForCheckTypeReadOnlyRootFilesystem(baseSecurityContext *checksv1alpha1.SecurityContextDefaults) *checksv1alpha1.SecurityContextDefaults {
	if baseSecurityContext.Container.ReadOnlyRootFilesystem == nil {
		baseSecurityContext.Container.ReadOnlyRootFilesystem = ptr.To(true) // Default to read-only root filesystem
	}

	return baseSecurityContext
}

func (m *WorkloadCheckManager) securityContextForCheckTypeAllowPrivilegeEscalation(baseSecurityContext *checksv1alpha1.SecurityContextDefaults) *checksv1alpha1.SecurityContextDefaults {
	if baseSecurityContext.Container.AllowPrivilegeEscalation == nil {
		baseSecurityContext.Container.AllowPrivilegeEscalation = ptr.To(false) // Default to no privilege escalation
	}

	return baseSecurityContext
}

func (m *WorkloadCheckManager) securityContextForCheckTypeCapabilitiesDropAll(baseSecurityContext *checksv1alpha1.SecurityContextDefaults) *checksv1alpha1.SecurityContextDefaults {
	if baseSecurityContext.Container.CapabilitiesDrop == nil {
		baseSecurityContext.Container.CapabilitiesDrop = []corev1.Capability{"ALL"}
	}

	return baseSecurityContext
}
