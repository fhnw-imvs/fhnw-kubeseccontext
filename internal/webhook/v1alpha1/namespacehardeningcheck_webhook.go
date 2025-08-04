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
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	checksv1alpha1 "github.com/fhnw-imvs/fhnw-kubeseccontext/api/v1alpha1"
)

// nolint:unused
// log is for logging in this package.
var namespacehardeningchecklog = logf.Log.WithName("namespacehardeningcheck-resource")

// SetupNamespaceHardeningCheckWebhookWithManager registers the webhook for NamespaceHardeningCheck in the manager.
func SetupNamespaceHardeningCheckWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).For(&checksv1alpha1.NamespaceHardeningCheck{}).
		WithValidator(&NamespaceHardeningCheckCustomValidator{}).
		Complete()
}

// TODO(user): EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!

// TODO(user): change verbs to "verbs=create;update;delete" if you want to enable deletion validation.
// NOTE: The 'path' attribute must follow a specific pattern and should not be modified directly here.
// Modifying the path for an invalid path can cause API server errors; failing to locate the webhook.
// +kubebuilder:webhook:path=/validate-checks-funk-fhnw-ch-v1alpha1-namespacehardeningcheck,mutating=false,failurePolicy=fail,sideEffects=None,groups=checks.funk.fhnw.ch,resources=namespacehardeningchecks,verbs=create;update,versions=v1alpha1,name=vnamespacehardeningcheck-v1alpha1.kb.io,admissionReviewVersions=v1

// NamespaceHardeningCheckCustomValidator struct is responsible for validating the NamespaceHardeningCheck resource
// when it is created, updated, or deleted.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as this struct is used only for temporary operations and does not need to be deeply copied.
type NamespaceHardeningCheckCustomValidator struct {
	// TODO(user): Add more fields as needed for validation
}

var _ webhook.CustomValidator = &NamespaceHardeningCheckCustomValidator{}

// ValidateCreate implements webhook.CustomValidator so a webhook will be registered for the type NamespaceHardeningCheck.
func (v *NamespaceHardeningCheckCustomValidator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	namespacehardeningcheck, ok := obj.(*checksv1alpha1.NamespaceHardeningCheck)
	if !ok {
		return nil, fmt.Errorf("expected a NamespaceHardeningCheck object but got %T", obj)
	}
	namespacehardeningchecklog.Info("Validation for NamespaceHardeningCheck upon creation", "name", namespacehardeningcheck.GetName())

	// TODO(user): fill in your validation logic upon object creation.

	return nil, nil
}

// ValidateUpdate implements webhook.CustomValidator so a webhook will be registered for the type NamespaceHardeningCheck.
func (v *NamespaceHardeningCheckCustomValidator) ValidateUpdate(_ context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	namespacehardeningcheck, ok := newObj.(*checksv1alpha1.NamespaceHardeningCheck)
	if !ok {
		return nil, fmt.Errorf("expected a NamespaceHardeningCheck object for the newObj but got %T", newObj)
	}
	namespacehardeningchecklog.Info("Validation for NamespaceHardeningCheck upon update", "name", namespacehardeningcheck.GetName())

	// TODO(user): fill in your validation logic upon object update.

	return nil, nil
}

// ValidateDelete implements webhook.CustomValidator so a webhook will be registered for the type NamespaceHardeningCheck.
func (v *NamespaceHardeningCheckCustomValidator) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	namespacehardeningcheck, ok := obj.(*checksv1alpha1.NamespaceHardeningCheck)
	if !ok {
		return nil, fmt.Errorf("expected a NamespaceHardeningCheck object but got %T", obj)
	}
	namespacehardeningchecklog.Info("Validation for NamespaceHardeningCheck upon deletion", "name", namespacehardeningcheck.GetName())

	// TODO(user): fill in your validation logic upon object deletion.

	return nil, nil
}
