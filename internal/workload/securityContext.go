package workload

import (
	"context"
	"fmt"

	checksv1alpha1 "github.com/fhnw-imvs/fhnw-kubeseccontext/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

func mergePodSecurityContexts(ctx context.Context, base, extends *corev1.PodSecurityContext) *corev1.PodSecurityContext {
	log := log.FromContext(ctx)

	if base == nil {
		log.Info("Base security context is nil, using override")
		return extends
	}

	if extends == nil {
		log.Info("Override security context is nil, using base")
		return base
	}

	merged := base.DeepCopy()
	if merged.RunAsUser == nil && extends.RunAsUser != nil {
		log.V(3).Info("Merging RunAsUser from override into base")
		merged.RunAsUser = extends.RunAsUser
	}
	if merged.RunAsGroup == nil && extends.RunAsGroup != nil {
		log.V(3).Info("Merging RunAsGroup from override into base")
		merged.RunAsGroup = extends.RunAsGroup
	}
	if merged.FSGroup == nil && extends.FSGroup != nil {
		log.V(3).Info("Merging FSGroup from override into base")
		merged.FSGroup = extends.FSGroup
	}
	if merged.RunAsNonRoot == nil && extends.RunAsNonRoot != nil {
		log.V(3).Info("Merging RunAsNonRoot from override into base")
		merged.RunAsNonRoot = extends.RunAsNonRoot
	}
	if merged.SeccompProfile == nil && extends.SeccompProfile != nil {
		log.V(3).Info("Merging SeccompProfile from override into base")
		merged.SeccompProfile = extends.SeccompProfile
	}
	if merged.SELinuxOptions == nil && extends.SELinuxOptions != nil {
		log.V(3).Info("Merging SELinuxOptions from override into base")
		merged.SELinuxOptions = extends.SELinuxOptions
	}
	if merged.SupplementalGroups == nil && extends.SupplementalGroups != nil {
		log.V(3).Info("Merging SupplementalGroups from override into base")
		merged.SupplementalGroups = extends.SupplementalGroups
	}
	if merged.SupplementalGroupsPolicy == nil && extends.SupplementalGroupsPolicy != nil {
		log.V(3).Info("Merging SupplementalGroupsPolicy from override into base")
		merged.SupplementalGroupsPolicy = extends.SupplementalGroupsPolicy
	}

	return merged
}

func mergeContainerSecurityContexts(ctx context.Context, base, extends *corev1.SecurityContext) *corev1.SecurityContext {
	log := log.FromContext(ctx)

	if base == nil {
		log.Info("Base security context is nil, using override")
		return extends
	}

	if extends == nil {
		log.Info("Override security context is nil, using base")
		return base
	}

	merged := base.DeepCopy()
	if merged.RunAsUser == nil && extends.RunAsUser != nil {
		log.V(3).Info("Merging RunAsUser from override into base")
		merged.RunAsUser = extends.RunAsUser
	}
	if merged.RunAsGroup == nil && extends.RunAsGroup != nil {
		log.V(3).Info("Merging RunAsGroup from override into base")
		merged.RunAsGroup = extends.RunAsGroup
	}
	if merged.Capabilities == nil && extends.Capabilities != nil {
		log.V(3).Info("Merging Capabilities from override into base")
		merged.Capabilities = extends.Capabilities
	}
	if merged.Privileged == nil && extends.Privileged != nil {
		log.V(3).Info("Merging Privileged from override into base")
		merged.Privileged = extends.Privileged
	}
	if merged.AllowPrivilegeEscalation == nil && extends.AllowPrivilegeEscalation != nil {
		log.V(3).Info("Merging AllowPrivilegeEscalation from override into base")
		merged.AllowPrivilegeEscalation = extends.AllowPrivilegeEscalation
	}
	if merged.ReadOnlyRootFilesystem == nil && extends.ReadOnlyRootFilesystem != nil {
		log.V(3).Info("Merging ReadOnlyRootFilesystem from override into base")
		merged.ReadOnlyRootFilesystem = extends.ReadOnlyRootFilesystem
	}
	if merged.SeccompProfile == nil && extends.SeccompProfile != nil {
		log.V(3).Info("Merging SeccompProfile from override into base")
		merged.SeccompProfile = extends.SeccompProfile
	}
	if merged.SELinuxOptions == nil && extends.SELinuxOptions != nil {
		log.V(3).Info("Merging SELinuxOptions from override into base")
		merged.SELinuxOptions = extends.SELinuxOptions
	}

	return merged
}

func ApplySecurityContext(ctx context.Context, workloadUnderTest *client.Object, containerSecurityContext *checksv1alpha1.ContainerSecurityContextDefaults, podSecurityContext *checksv1alpha1.PodSecurityContextDefaults) error {
	switch v := (*workloadUnderTest).(type) {
	case *appsv1.Deployment:
		return ApplyDeploymentSecurityContext(ctx, v, containerSecurityContext, podSecurityContext)
	case *appsv1.StatefulSet:
		return ApplyStatefulSetSecurityContext(ctx, v, containerSecurityContext, podSecurityContext)
	case *appsv1.DaemonSet:
		return ApplyDaemonSetSecurityContext(ctx, v, containerSecurityContext, podSecurityContext)
	}

	return fmt.Errorf("kind of workloadUnderTest not supported")
}

func ApplyDeploymentSecurityContext(ctx context.Context, deployment *appsv1.Deployment, containerSecurityContext *checksv1alpha1.ContainerSecurityContextDefaults, podSecurityContext *checksv1alpha1.PodSecurityContextDefaults) error {
	if deployment.Spec.Template.Spec.SecurityContext == nil {
		deployment.Spec.Template.Spec.SecurityContext = podSecurityContext.ToK8sSecurityContext()
	} else {
		deployment.Spec.Template.Spec.SecurityContext = mergePodSecurityContexts(ctx, deployment.Spec.Template.Spec.SecurityContext, podSecurityContext.ToK8sSecurityContext())
	}

	for i := range deployment.Spec.Template.Spec.Containers {
		if deployment.Spec.Template.Spec.Containers[i].SecurityContext == nil {
			deployment.Spec.Template.Spec.Containers[i].SecurityContext = containerSecurityContext.ToK8sSecurityContext()
		} else {
			deployment.Spec.Template.Spec.Containers[i].SecurityContext = mergeContainerSecurityContexts(ctx, deployment.Spec.Template.Spec.Containers[i].SecurityContext, containerSecurityContext.ToK8sSecurityContext())
		}
	}

	for i := range deployment.Spec.Template.Spec.InitContainers {
		if deployment.Spec.Template.Spec.InitContainers[i].SecurityContext == nil {
			deployment.Spec.Template.Spec.InitContainers[i].SecurityContext = containerSecurityContext.ToK8sSecurityContext()
		} else {
			deployment.Spec.Template.Spec.InitContainers[i].SecurityContext = mergeContainerSecurityContexts(ctx, deployment.Spec.Template.Spec.InitContainers[i].SecurityContext, containerSecurityContext.ToK8sSecurityContext())
		}
	}

	return nil
}

func ApplyStatefulSetSecurityContext(ctx context.Context, statefulSet *appsv1.StatefulSet, containerSecurityContext *checksv1alpha1.ContainerSecurityContextDefaults, podSecurityContext *checksv1alpha1.PodSecurityContextDefaults) error {
	if statefulSet.Spec.Template.Spec.SecurityContext == nil {
		statefulSet.Spec.Template.Spec.SecurityContext = podSecurityContext.ToK8sSecurityContext()
	} else {
		statefulSet.Spec.Template.Spec.SecurityContext = mergePodSecurityContexts(ctx, statefulSet.Spec.Template.Spec.SecurityContext, podSecurityContext.ToK8sSecurityContext())
	}

	for i := range statefulSet.Spec.Template.Spec.Containers {
		if statefulSet.Spec.Template.Spec.Containers[i].SecurityContext == nil {
			statefulSet.Spec.Template.Spec.Containers[i].SecurityContext = containerSecurityContext.ToK8sSecurityContext()
		} else {
			statefulSet.Spec.Template.Spec.Containers[i].SecurityContext = mergeContainerSecurityContexts(ctx, statefulSet.Spec.Template.Spec.Containers[i].SecurityContext, containerSecurityContext.ToK8sSecurityContext())
		}
	}

	for i := range statefulSet.Spec.Template.Spec.InitContainers {
		if statefulSet.Spec.Template.Spec.InitContainers[i].SecurityContext == nil {
			statefulSet.Spec.Template.Spec.InitContainers[i].SecurityContext = containerSecurityContext.ToK8sSecurityContext()
		} else {
			statefulSet.Spec.Template.Spec.InitContainers[i].SecurityContext = mergeContainerSecurityContexts(ctx, statefulSet.Spec.Template.Spec.InitContainers[i].SecurityContext, containerSecurityContext.ToK8sSecurityContext())
		}
	}

	return nil
}

func ApplyDaemonSetSecurityContext(ctx context.Context, daemonSet *appsv1.DaemonSet, containerSecurityContext *checksv1alpha1.ContainerSecurityContextDefaults, podSecurityContext *checksv1alpha1.PodSecurityContextDefaults) error {
	if daemonSet.Spec.Template.Spec.SecurityContext == nil {
		daemonSet.Spec.Template.Spec.SecurityContext = podSecurityContext.ToK8sSecurityContext()
	} else {
		daemonSet.Spec.Template.Spec.SecurityContext = mergePodSecurityContexts(ctx, daemonSet.Spec.Template.Spec.SecurityContext, podSecurityContext.ToK8sSecurityContext())
	}

	for i := range daemonSet.Spec.Template.Spec.Containers {
		if daemonSet.Spec.Template.Spec.Containers[i].SecurityContext == nil {
			daemonSet.Spec.Template.Spec.Containers[i].SecurityContext = containerSecurityContext.ToK8sSecurityContext()
		} else {
			daemonSet.Spec.Template.Spec.Containers[i].SecurityContext = mergeContainerSecurityContexts(ctx, daemonSet.Spec.Template.Spec.Containers[i].SecurityContext, containerSecurityContext.ToK8sSecurityContext())
		}
	}

	for i := range daemonSet.Spec.Template.Spec.InitContainers {
		if daemonSet.Spec.Template.Spec.InitContainers[i].SecurityContext == nil {
			daemonSet.Spec.Template.Spec.InitContainers[i].SecurityContext = containerSecurityContext.ToK8sSecurityContext()
		} else {
			daemonSet.Spec.Template.Spec.InitContainers[i].SecurityContext = mergeContainerSecurityContexts(ctx, daemonSet.Spec.Template.Spec.InitContainers[i].SecurityContext, containerSecurityContext.ToK8sSecurityContext())
		}
	}

	return nil
}
