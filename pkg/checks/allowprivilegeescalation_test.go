package checks_test

import (
	"testing"

	checksv1alpha1 "github.com/fhnw-imvs/fhnw-kubeseccontext/api/v1alpha1"
	"github.com/fhnw-imvs/fhnw-kubeseccontext/pkg/checks"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

func TestAllowPrivilegeEscalationCheck(t *testing.T) {

	t.Run("GetType", func(t *testing.T) {
		check := &checks.AllowPrivilegeEscalationCheck{}
		expectedType := "allowPrivilegeEscalation"
		if check.GetType() != expectedType {
			t.Errorf("Expected %s, got %s", expectedType, check.GetType())
		}
	})

	t.Run("GetSecurityContextDefaults", func(t *testing.T) {
		check := &checks.AllowPrivilegeEscalationCheck{}

		baseSecurityContext := &checksv1alpha1.SecurityContextDefaults{
			Pod:       &checksv1alpha1.PodSecurityContextDefaults{},
			Container: &checksv1alpha1.ContainerSecurityContextDefaults{},
		}

		defaults := check.GetSecurityContextDefaults(baseSecurityContext)

		assert.False(t, *defaults.Container.AllowPrivilegeEscalation, "Expected AllowPrivilegeEscalation to be set")
	})

	t.Run("ShouldRunSingleContainer", func(t *testing.T) {
		check := &checks.AllowPrivilegeEscalationCheck{}
		podSpec := &corev1.PodSpec{
			Containers: []corev1.Container{
				{
					SecurityContext: &corev1.SecurityContext{
						AllowPrivilegeEscalation: ptr.To(true),
					},
				},
			},
		}

		assert.True(t, check.ShouldRun(podSpec), "Expected ShouldRun to return true for AllowPrivilegeEscalation set to true")
	})

	t.Run("ShouldRunMultipleContainers", func(t *testing.T) {
		check := &checks.AllowPrivilegeEscalationCheck{}
		podSpec := &corev1.PodSpec{
			Containers: []corev1.Container{
				{
					SecurityContext: &corev1.SecurityContext{
						AllowPrivilegeEscalation: ptr.To(false),
					},
				},
				{
					SecurityContext: &corev1.SecurityContext{
						AllowPrivilegeEscalation: ptr.To(true),
					},
				},
			},
		}

		assert.True(t, check.ShouldRun(podSpec), "Expected ShouldRun to return true when at least one container has AllowPrivilegeEscalation set to true")
	})

	t.Run("ShouldRunNotRunSingleContainer", func(t *testing.T) {
		check := &checks.AllowPrivilegeEscalationCheck{}
		podSpec := &corev1.PodSpec{
			Containers: []corev1.Container{
				{
					SecurityContext: &corev1.SecurityContext{
						AllowPrivilegeEscalation: ptr.To(false),
					},
				},
			},
		}

		assert.False(t, check.ShouldRun(podSpec), "Expected ShouldRun to return false for AllowPrivilegeEscalation set to false")
	})

	t.Run("ShouldRunNotRunMultipleContainers", func(t *testing.T) {
		check := &checks.AllowPrivilegeEscalationCheck{}
		podSpec := &corev1.PodSpec{
			Containers: []corev1.Container{
				{
					SecurityContext: &corev1.SecurityContext{
						AllowPrivilegeEscalation: ptr.To(false),
					},
				},
				{
					SecurityContext: &corev1.SecurityContext{
						AllowPrivilegeEscalation: ptr.To(false),
					},
				},
			},
		}

		assert.False(t, check.ShouldRun(podSpec), "Expected ShouldRun to return false for AllowPrivilegeEscalation set to false")
	})
}
