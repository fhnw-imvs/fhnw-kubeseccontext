package v1alpha1

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	checksv1alpha1 "github.com/fhnw-imvs/fhnw-kubeseccontext/api/v1alpha1"
	"github.com/fhnw-imvs/fhnw-kubeseccontext/internal/workload"
)

// nolint:unused
// log is for logging in this package.
var workloadhardeningchecklog = logf.Log.WithName("workloadhardeningcheck-resource")

// SetupWorkloadHardeningCheckWebhookWithManager registers the webhook for WorkloadHardeningCheck in the manager.
func SetupWorkloadHardeningCheckWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).For(&checksv1alpha1.WorkloadHardeningCheck{}).
		WithDefaulter(&WorkloadHardeningCheckCustomDefaulter{}).
		WithValidator(&WorkloadHardeningCheckCustomValidator{}).
		Complete()
}

// +kubebuilder:webhook:path=/mutate-checks-funk-fhnw-ch-v1alpha1-workloadhardeningcheck,mutating=true,failurePolicy=fail,sideEffects=None,groups=checks.funk.fhnw.ch,resources=workloadhardeningchecks,verbs=create;update,versions=v1alpha1,name=mworkloadhardeningcheck-v1alpha1.kb.io,admissionReviewVersions=v1

// WorkloadHardeningCheckCustomDefaulter struct is responsible for setting default values on the custom resource of the
// Kind WorkloadHardeningCheck when those are created or updated.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as it is used only for temporary operations and does not need to be deeply copied.
type WorkloadHardeningCheckCustomDefaulter struct {
	// TODO(user): Add more fields as needed for defaulting
}

var _ webhook.CustomDefaulter = &WorkloadHardeningCheckCustomDefaulter{}

// Default implements webhook.CustomDefaulter so a webhook will be registered for the Kind WorkloadHardeningCheck.
func (d *WorkloadHardeningCheckCustomDefaulter) Default(ctx context.Context, obj runtime.Object) error {
	workloadhardeningcheck, ok := obj.(*checksv1alpha1.WorkloadHardeningCheck)

	if !ok {
		workloadhardeningchecklog.Info("object type mismatch", "expected", "WorkloadHardeningCheck", "got", fmt.Sprintf("%T", obj))
		return fmt.Errorf("expected an WorkloadHardeningCheck object but got %T", obj)
	}
	workloadhardeningchecklog.Info("Defaulting for WorkloadHardeningCheck", "name", workloadhardeningcheck.GetName())

	// Set suffix as early as possible
	if len(workloadhardeningcheck.Spec.Suffix) == 0 {
		workloadhardeningchecklog.Info("Setting suffix for WorkloadHardeningCheck", "name", workloadhardeningcheck.GetName())
		workloadhardeningcheck.Spec.Suffix = utilrand.String(8) // Generate a random suffix of 8 characters
	} else {
		workloadhardeningchecklog.V(2).Info("Suffix already set", "suffix", workloadhardeningcheck.Spec.Suffix)
	}

	return nil
}

// NOTE: The 'path' attribute must follow a specific pattern and should not be modified directly here.
// Modifying the path for an invalid path can cause API server errors; failing to locate the webhook.

// +kubebuilder:webhook:path=/validate-checks-funk-fhnw-ch-v1alpha1-workloadhardeningcheck,mutating=false,failurePolicy=fail,sideEffects=None,groups=checks.funk.fhnw.ch,resources=workloadhardeningchecks,verbs=create;update,versions=v1alpha1,name=vworkloadhardeningcheck-v1alpha1.kb.io,admissionReviewVersions=v1

// WorkloadHardeningCheckCustomValidator struct is responsible for validating the custom resource of the
// Kind WorkloadHardeningCheck when those are created or updated.

// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as it is used only for temporary operations and does not need to be deeply copied.
type WorkloadHardeningCheckCustomValidator struct {
	// TODO(user): Add more fields as needed for validation
}

var _ webhook.CustomValidator = &WorkloadHardeningCheckCustomValidator{}

// ValidateCreate implements webhook.CustomValidator so a webhook will be registered for the Kind WorkloadHardeningCheck.
func (v *WorkloadHardeningCheckCustomValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	workloadhardeningcheck, ok := obj.(*checksv1alpha1.WorkloadHardeningCheck)

	if !ok {
		workloadhardeningchecklog.Info("object type mismatch", "expected", "WorkloadHardeningCheck", "got", fmt.Sprintf("%T", obj))
		return nil, fmt.Errorf("expected an WorkloadHardeningCheck object but got %T", obj)
	}
	workloadhardeningchecklog.Info("Validating creation of WorkloadHardeningCheck", "name", workloadhardeningcheck.GetName())

	if workloadhardeningcheck.Spec.Suffix == "" {
		workloadhardeningchecklog.Info("Suffix is empty during creation, this is not allowed", "name", workloadhardeningcheck.GetName())
		return nil, fmt.Errorf("suffix must be set during creation of WorkloadHardeningCheck")
	}

	// ValKeyClient is not used in this validation, so we pass nil
	workloadManager := workload.NewWorkloadCheckManager(ctx, nil, workloadhardeningcheck)
	if workloadManager == nil {
		workloadhardeningchecklog.Error(fmt.Errorf("failed to create workload manager"), "WorkloadHandler creation failed")
		return nil, fmt.Errorf("failed to create workload manager for WorkloadHardeningCheck")
	}

	// Verify that the target workload exists
	if running, err := workloadManager.VerifyRunning(ctx, workloadhardeningcheck.GetNamespace()); err != nil {
		workloadhardeningchecklog.Error(err, "Failed to verify if target workload is running", "name", workloadhardeningcheck.GetName())
		return nil, fmt.Errorf("failed to verify if target workload is running: %w", err)
	} else if !running {
		workloadhardeningchecklog.Info("Target workload is not running", "name", workloadhardeningcheck.GetName())
		return nil, fmt.Errorf("target workload %s/%s is not running", workloadhardeningcheck.GetNamespace(), workloadhardeningcheck.Spec.TargetRef.Name)
	}

	workloadhardeningchecklog.Info("WorkloadHardeningCheck creation validation passed", "name", workloadhardeningcheck.GetName())

	return nil, nil

}

// ValidateUpdate implements webhook.CustomValidator so a webhook will be registered for the Kind WorkloadHardeningCheck.
func (v *WorkloadHardeningCheckCustomValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	oldWorkloadhardeningcheck, ok := oldObj.(*checksv1alpha1.WorkloadHardeningCheck)
	newWorkloadhardeningcheck, ok2 := newObj.(*checksv1alpha1.WorkloadHardeningCheck)

	if !ok || !ok2 {
		return nil, fmt.Errorf("expected an WorkloadHardeningCheck object but got %T and %T", oldObj, newObj)
	}
	workloadhardeningchecklog.Info("Validating update of WorkloadHardeningCheck", "name", newWorkloadhardeningcheck.GetName())

	// Suffix cannot be changed during update
	if oldWorkloadhardeningcheck.Spec.Suffix != newWorkloadhardeningcheck.Spec.Suffix {
		workloadhardeningchecklog.Info("Suffix cannot be changed during update", "oldSuffix", oldWorkloadhardeningcheck.Spec.Suffix, "newSuffix", newWorkloadhardeningcheck.Spec.Suffix)
		return nil, fmt.Errorf("suffix cannot be changed during update of WorkloadHardeningCheck")
	}

	// TargetRef cannot be changed during update
	if oldWorkloadhardeningcheck.Spec.TargetRef.Name != newWorkloadhardeningcheck.Spec.TargetRef.Name || oldWorkloadhardeningcheck.Spec.TargetRef.Kind != newWorkloadhardeningcheck.Spec.TargetRef.Kind {
		workloadhardeningchecklog.Info("TargetRef cannot be changed during update", "oldTargetRef", oldWorkloadhardeningcheck.Spec.TargetRef, "newTargetRef", newWorkloadhardeningcheck.Spec.TargetRef)
		return nil, fmt.Errorf("targetRef cannot be changed during update of WorkloadHardeningCheck")
	}

	workloadhardeningchecklog.Info("WorkloadHardeningCheck update validation passed", "name", newWorkloadhardeningcheck.GetName())

	return nil, nil
}

// ValidateDelete implements webhook.CustomValidator so a webhook will be registered for the Kind WorkloadHardeningCheck.
// This is just a placeholder to implement the webhook interface.
func (v *WorkloadHardeningCheckCustomValidator) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	workloadhardeningcheck, ok := obj.(*checksv1alpha1.WorkloadHardeningCheck)

	if !ok {
		return nil, fmt.Errorf("expected an WorkloadHardeningCheck object but got %T", obj)
	}

	workloadhardeningchecklog.Info("WorkloadHardeningCheck deletion validation passed", "name", workloadhardeningcheck.GetName())

	return nil, nil
}
