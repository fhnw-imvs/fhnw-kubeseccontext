package checks_test

import (
	"testing"

	checksv1alpha1 "github.com/fhnw-imvs/fhnw-kubeseccontext/api/v1alpha1"
	"github.com/fhnw-imvs/fhnw-kubeseccontext/pkg/checks"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

func TestGroupCheck(t *testing.T) {

	t.Run("GetType", func(t *testing.T) {
		check := &checks.GroupCheck{}
		expectedType := "group"
		if check.GetType() != expectedType {
			t.Errorf("Expected %s, got %s", expectedType, check.GetType())
		}
	})

	t.Run("GetSecurityContextDefaults", func(t *testing.T) {
		check := &checks.GroupCheck{}

		baseSecurityContext := &checksv1alpha1.SecurityContextDefaults{
			Pod:       &checksv1alpha1.PodSecurityContextDefaults{},
			Container: &checksv1alpha1.ContainerSecurityContextDefaults{},
		}

		defaults := check.GetSecurityContextDefaults(baseSecurityContext)

		assert.Equal(t, int64(1000), *defaults.Container.RunAsGroup, "Expected RunAsUser to be set to 1000 by default")
		assert.Equal(t, int64(1000), *defaults.Pod.RunAsGroup, "Expected RunAsUser to be set to 1000 by default")

		assert.Equal(t, int64(1000), *defaults.Pod.FSGroup, "Expected RunAsUser to be set to 1000 by default")
	})

	t.Run("ShouldRunSingleContainer", func(t *testing.T) {
		check := &checks.GroupCheck{}
		podSpec := &corev1.PodSpec{
			SecurityContext: &corev1.PodSecurityContext{
				RunAsGroup: nil,
				FSGroup:    nil,
			},
		}

		assert.True(t, check.ShouldRun(podSpec), "Expected ShouldRun to return true if runAsUser or runAsNonRoot are not set")
	})

	t.Run("ShouldRunSingleContainerFsGroupSet", func(t *testing.T) {
		check := &checks.GroupCheck{}
		podSpec := &corev1.PodSpec{
			SecurityContext: &corev1.PodSecurityContext{
				FSGroup: ptr.To(int64(2000)),
			},
		}

		assert.True(t, check.ShouldRun(podSpec), "Expected ShouldRun to return true if runAsUser or runAsNonRoot are not set")
	})

	t.Run("ShouldRunSingleContainerRunAsGroupSet", func(t *testing.T) {
		check := &checks.GroupCheck{}
		podSpec := &corev1.PodSpec{
			SecurityContext: &corev1.PodSecurityContext{
				RunAsGroup: ptr.To(int64(2000)),
			},
		}

		assert.True(t, check.ShouldRun(podSpec), "Expected ShouldRun to return true if runAsUser or runAsNonRoot are not set")
	})

	t.Run("ShouldRunNotRunSingleContainer", func(t *testing.T) {
		check := &checks.GroupCheck{}
		podSpec := &corev1.PodSpec{
			SecurityContext: &corev1.PodSecurityContext{
				RunAsGroup: ptr.To(int64(2000)),
				FSGroup:    ptr.To(int64(2000)),
			},
		}

		assert.False(t, check.ShouldRun(podSpec), "Expected ShouldRun to return false if runAsUser and runAsNonRoot are set")
	})

}
