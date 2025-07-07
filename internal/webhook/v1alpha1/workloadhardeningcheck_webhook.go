/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
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
		Complete()
}

// TODO(user): EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!

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
		return fmt.Errorf("expected an WorkloadHardeningCheck object but got %T", obj)
	}
	workloadhardeningchecklog.Info("Defaulting for WorkloadHardeningCheck", "name", workloadhardeningcheck.GetName())

	// Set suffix as early as possible
	if workloadhardeningcheck.Spec.Suffix == "" {
		workloadhardeningchecklog.Info("Setting suffix for WorkloadHardeningCheck", "name", workloadhardeningcheck.GetName())
		workloadhardeningcheck.Spec.Suffix = utilrand.String(10)
	} else {
		workloadhardeningchecklog.Info("Suffix already set", "suffix", workloadhardeningcheck.Spec.Suffix)
	}

	return nil
}

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
	log := log.FromContext(ctx)
	workloadhardeningcheck, ok := obj.(*checksv1alpha1.WorkloadHardeningCheck)

	if !ok {
		return nil, fmt.Errorf("expected an WorkloadHardeningCheck object but got %T", obj)
	}
	log.Info("Validating creation of WorkloadHardeningCheck", "name", workloadhardeningcheck.GetName())

	if workloadhardeningcheck.Spec.Suffix == "" {
		log.Info("Suffix is empty during creation, this is not allowed", "name", workloadhardeningcheck.GetName())
		return nil, fmt.Errorf("suffix must be set during creation of WorkloadHardeningCheck")
	}

	config, err := ctrl.GetConfig()
	if err != nil {
		log.Error(err, "Failed to get runtime client config")
		return nil, fmt.Errorf("failed to get runtime client config: %w", err)
	}
	scheme := runtime.NewScheme()
	if err := checksv1alpha1.AddToScheme(scheme); err != nil {
		log.Error(err, "Failed to add scheme for WorkloadHardeningCheck")
		return nil, fmt.Errorf("failed to add scheme for WorkloadHardeningCheck: %w", err)
	}

	c, err := client.New(config, client.Options{Scheme: scheme})

	// ValKeyClient is not used in this validation, so we pass nil
	workloadHandler := workload.NewWorkloadHandler(ctx, nil, c, workloadhardeningcheck)
	if workloadHandler == nil {
		log.Error(fmt.Errorf("failed to create workload handler"), "WorkloadHandler creation failed")
		return nil, fmt.Errorf("failed to create workload handler for WorkloadHardeningCheck")
	}

	// Verify that the target workload exists
	if running, err := workloadHandler.VerifyRunning(ctx, workloadhardeningcheck.GetNamespace()); err != nil {
		log.Error(err, "Failed to verify if target workload is running", "name", workloadhardeningcheck.GetName())
		return nil, fmt.Errorf("failed to verify if target workload is running: %w", err)
	} else if !running {
		log.Info("Target workload is not running", "name", workloadhardeningcheck.GetName())
		return nil, fmt.Errorf("target workload %s/%s is not running", workloadhardeningcheck.GetNamespace(), workloadhardeningcheck.Spec.TargetRef.Name)
	}

	log.Info("WorkloadHardeningCheck creation validation passed", "name", workloadhardeningcheck.GetName())

	return nil, nil

}

// ValidateUpdate implements webhook.CustomValidator so a webhook will be registered for the Kind WorkloadHardeningCheck.
func (v *WorkloadHardeningCheckCustomValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	log := log.FromContext(ctx)
	oldWorkloadhardeningcheck, ok := oldObj.(*checksv1alpha1.WorkloadHardeningCheck)
	newWorkloadhardeningcheck, ok2 := newObj.(*checksv1alpha1.WorkloadHardeningCheck)

	if !ok || !ok2 {
		return nil, fmt.Errorf("expected an WorkloadHardeningCheck object but got %T and %T", oldObj, newObj)
	}
	log.Info("Validating update of WorkloadHardeningCheck", "name", newWorkloadhardeningcheck.GetName())

	// Suffix cannot be changed during update
	if oldWorkloadhardeningcheck.Spec.Suffix != newWorkloadhardeningcheck.Spec.Suffix {
		log.Info("Suffix cannot be changed during update", "oldSuffix", oldWorkloadhardeningcheck.Spec.Suffix, "newSuffix", newWorkloadhardeningcheck.Spec.Suffix)
		return nil, fmt.Errorf("suffix cannot be changed during update of WorkloadHardeningCheck")
	}

	// TargetRef cannot be changed during update
	if oldWorkloadhardeningcheck.Spec.TargetRef.Name != newWorkloadhardeningcheck.Spec.TargetRef.Name || oldWorkloadhardeningcheck.Spec.TargetRef.Kind != newWorkloadhardeningcheck.Spec.TargetRef.Kind {
		log.Info("TargetRef cannot be changed during update", "oldTargetRef", oldWorkloadhardeningcheck.Spec.TargetRef, "newTargetRef", newWorkloadhardeningcheck.Spec.TargetRef)
		return nil, fmt.Errorf("targetRef cannot be changed during update of WorkloadHardeningCheck")
	}

	log.Info("WorkloadHardeningCheck update validation passed", "name", newWorkloadhardeningcheck.GetName())

	return nil, nil
}

// ValidateDelete implements webhook.CustomValidator so a webhook will be registered for the Kind WorkloadHardeningCheck.
func (v *WorkloadHardeningCheckCustomValidator) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	log := log.FromContext(ctx)
	workloadhardeningcheck, ok := obj.(*checksv1alpha1.WorkloadHardeningCheck)

	if !ok {
		return nil, fmt.Errorf("expected an WorkloadHardeningCheck object but got %T", obj)
	}

	log.Info("WorkloadHardeningCheck deletion validation passed", "name", workloadhardeningcheck.GetName())

	return nil, nil
}
