package checks

import (
	"fmt"

	v1 "k8s.io/api/core/v1"

	checksv1alpha1 "github.com/fhnw-imvs/fhnw-kubeseccontext/api/v1alpha1"
)

type CheckInterface interface {
	// GetType returns the type of the check, e.g., "group", "user
	GetType() string
	// GetSecurityContext returns the security context defaults for the check
	GetSecurityContextDefaults(*checksv1alpha1.SecurityContextDefaults) *checksv1alpha1.SecurityContextDefaults
	// ShouldRun checks if the check should run for the given pod spec
	ShouldRun(*v1.PodSpec) bool
}

var checks = map[string]CheckInterface{}

func RegisterCheck(check CheckInterface) error {
	if _, exists := checks[check.GetType()]; exists {
		return fmt.Errorf("check of type %s already registered", check.GetType())
	}
	checks[check.GetType()] = check
	return nil
}

func GetAllChecks() map[string]CheckInterface {
	return checks
}
