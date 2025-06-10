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
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	checksv1alpha1 "github.com/fhnw-imvs/fhnw-kubeseccontext/api/v1alpha1"
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
