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
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// log is for logging in this package.
var workloadhardeningchecklog = logf.Log.WithName("workloadhardeningcheck-resource")

// SetupWebhookWithManager will setup the manager to manage the webhooks
func (r *WorkloadHardeningCheck) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(r).
		Complete()
}

// TODO(user): EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!

// +kubebuilder:webhook:path=/mutate-checks-funk-fhnw-ch-v1alpha1-workloadhardeningcheck,mutating=true,failurePolicy=fail,sideEffects=None,groups=checks.funk.fhnw.ch,resources=workloadhardeningchecks,verbs=create;update,versions=v1alpha1,name=mworkloadhardeningcheck.kb.io,admissionReviewVersions=v1

var _ webhook.CustomDefaulter = &WorkloadHardeningCheck{}

// Default implements webhook.Defaulter so a webhook will be registered for the type
func (r *WorkloadHardeningCheck) Default(ctx context.Context, obj runtime.Object) error {

	workloadHardening, ok := obj.(*WorkloadHardeningCheck)

	if !ok {
		return fmt.Errorf("expected an WorkloadHardeningCheck object but got %T", obj)
	}
	workloadhardeningchecklog.Info("Defaulting for WorkloadHardeningCheck", "name", workloadHardening.GetName())

	// Set suffix as early as possible
	if workloadHardening.Status.Suffix == "" {
		workloadHardening.Status.Suffix = utilrand.String(10)
	}

	return nil
}

// TODO(user): change verbs to "verbs=create;update;delete" if you want to enable deletion validation.
// NOTE: The 'path' attribute must follow a specific pattern and should not be modified directly here.
// Modifying the path for an invalid path can cause API server errors; failing to locate the webhook.
// +kubebuilder:webhook:path=/validate-checks-funk-fhnw-ch-v1alpha1-workloadhardeningcheck,mutating=false,failurePolicy=fail,sideEffects=None,groups=checks.funk.fhnw.ch,resources=workloadhardeningchecks,verbs=create;update,versions=v1alpha1,name=vworkloadhardeningcheck.kb.io,admissionReviewVersions=v1

var _ webhook.CustomValidator = &WorkloadHardeningCheck{}

func (r *WorkloadHardeningCheck) getWorkloadUnderTest(ctx context.Context,
	workloadHardening *WorkloadHardeningCheck,
) (*client.Object, error) {
	log := log.FromContext(ctx)
	cl, _ := client.New(ctrl.GetConfigOrDie(), client.Options{})

	// Verify namespace contains target workload
	var workloadUnderTest client.Object
	switch kind := strings.ToLower(workloadHardening.Spec.TargetRef.Kind); kind {
	case "deployment":
		workloadUnderTest = &appsv1.Deployment{}
	case "statefulset":
		workloadUnderTest = &appsv1.StatefulSet{}
	case "daemonset":
		workloadUnderTest = &appsv1.DaemonSet{}
	}

	err := cl.Get(
		ctx,
		types.NamespacedName{
			Namespace: workloadHardening.Namespace,
			Name:      workloadHardening.Spec.TargetRef.Name,
		},
		workloadUnderTest,
	)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// If the custom resource is not found then it usually means that it was deleted or not created
			log.Info("workloadHardeningCheck.Spec.TargetRef not found. You must reference an existing workload to test it")
			return nil, fmt.Errorf("workloadHardeningCheck.Spec.TargetRef not found. You must reference an existing workload to test it")
		}
		// Error reading the object - requeue the request.
		log.Error(err, "failed to get workloadHardeningCheck.Spec.TargetRef, requeing")
		return nil, fmt.Errorf("failed to get workloadHardeningCheck.Spec.TargetRef: %w", err)
	}
	log.Info("TargetRef found")

	return &workloadUnderTest, nil
}

// ValidateCreate implements webhook.Validator so a webhook will be registered for the type
func (r *WorkloadHardeningCheck) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	workloadHardening, ok := obj.(*WorkloadHardeningCheck)

	if !ok {
		return nil, fmt.Errorf("expected an WorkloadHardeningCheck object but got %T", obj)
	}

	_, err := r.getWorkloadUnderTest(ctx, workloadHardening)

	return nil, err
}

// ValidateUpdate implements webhook.Validator so a webhook will be registered for the type
func (r *WorkloadHardeningCheck) ValidateUpdate(ctx context.Context, old runtime.Object, new runtime.Object) (admission.Warnings, error) {
	workloadHardening, ok := new.(*WorkloadHardeningCheck)

	if !ok {
		return nil, fmt.Errorf("expected an WorkloadHardeningCheck object but got %T", new)
	}

	_, err := r.getWorkloadUnderTest(ctx, workloadHardening)

	return nil, err
}

// ValidateDelete implements webhook.Validator so a webhook will be registered for the type
func (r *WorkloadHardeningCheck) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	workloadhardeningchecklog.Info("validate delete", "name", r.Name)

	// TODO(user): fill in your validation logic upon object deletion.
	return nil, nil
}
