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

package workload

import (
	"context"
	"time"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	checksv1alpha1 "github.com/fhnw-imvs/fhnw-kubeseccontext/api/v1alpha1"
	"github.com/fhnw-imvs/fhnw-kubeseccontext/internal/runner"
	"github.com/fhnw-imvs/fhnw-kubeseccontext/internal/valkey"
)

// Definitions to manage status conditions
const (
	// Represents the initial state, before the baseline recording is started
	typeWorkloadCheckStartup = "Preparation"
	// Represents a running baseline recording
	typeWorkloadCheckBaseline = "Baseline"
	// Represents ongoing check jobs, this state will be used until all checks are finished
	typeWorkloadCheck = "Check"

	// Baseline is currently being recorded
	reasonBaselineRecording = "BaselineRecording"
	reasonBaselineFailed    = "BaselineRecordingFailed"
	// Baseline has been recorded successfully
	reasonBaselineRecorded = "BaselineRecorded"

	// Single check
	reasonCheckRecording = "CheckRecording"
	reasonCheckFailed    = "CheckRecordingFailed"
	reasonCheckRecorded  = "CheckRecorded"
)

// WorkloadHardeningCheckReconciler reconciles a WorkloadHardeningCheck object
type WorkloadHardeningCheckReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	Recorder     record.EventRecorder
	ValKeyClient *valkey.ValkeyClient
}

// Required to convert "user" to "User", strings.ToTitle converts each rune to title case not just the first one
var titleCase = cases.Title(language.English)

// +kubebuilder:rbac:groups=checks.funk.fhnw.ch,resources=workloadhardeningchecks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=checks.funk.fhnw.ch,resources=workloadhardeningchecks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=checks.funk.fhnw.ch,resources=workloadhardeningchecks/finalizers,verbs=update
// +kubebuilder:rbac:groups=*,resources=*,verbs=*

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.19.0/pkg/reconcile
func (r *WorkloadHardeningCheckReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Get Resource
	// Fetch the WorkloadHardeningCheck instance
	// If the resource is not found, we stop the reconciliation
	workloadHardening := &checksv1alpha1.WorkloadHardeningCheck{}
	err := r.Get(ctx, req.NamespacedName, workloadHardening)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// If the custom resource is not found then it usually means that it was deleted or not created
			log.Info("WorkloadHardeningCheck deleted, cleaning up resources")

			checkNamespaces := corev1.NamespaceList{}
			err = r.List(
				ctx,
				&checkNamespaces,
				&client.ListOptions{
					LabelSelector: labels.SelectorFromSet(map[string]string{
						"orakel.fhnw.ch/source-namespace": workloadHardening.GetNamespace(),
						"orakel.fhnw.ch/suffix":           workloadHardening.Spec.Suffix,
					}),
				},
			)
			if err != nil {
				log.Error(err, "Failed to list namespaces for cleanup")
				return ctrl.Result{}, err
			}
			if len(checkNamespaces.Items) == 0 {
				log.Info("No namespaces found for cleanup")
				return ctrl.Result{}, nil
			}

			log.Info("Found namespaces for cleanup", "count", len(checkNamespaces.Items))

			for _, ns := range checkNamespaces.Items {
				log.Info("Deleting namespace", "namespace", ns.Name)
				err = r.deleteNamespace(ctx, ns.Name)
				if err != nil {
					log.Error(err, "Failed to delete namespace", "namespace", ns.Name)
				} else {
					log.Info("Deleted namespace", "namespace", ns.Name)
				}
			}

			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		log.Error(err, "Failed to get WorkloadHardeningCheck, requeing")
		return ctrl.Result{}, err
	}

	// Let's just set the status as Unknown when no status is available
	if len(workloadHardening.Status.Conditions) == 0 {
		err = r.setCondition(ctx, workloadHardening, metav1.Condition{
			Type:    typeWorkloadCheckStartup,
			Status:  metav1.ConditionUnknown,
			Reason:  "Verifying",
			Message: "Starting reconciliation",
		})
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	// We use the baseline duration to determine how long we should wait before requeuing the reconciliation
	duration := workloadHardening.GetCheckDuration()

	// Based on the Status, we need to decide what to do next
	// If there is no Baseline recorded yet, we need to start the baseline recording
	if meta.IsStatusConditionTrue(workloadHardening.Status.Conditions, typeWorkloadCheckBaseline) {
		log.Info("Baseline recorded. Start recording different security context configurations")
	} else {
		log.Info("Baseline not recorded yet. Starting baseline recording")
		// Set the condition to indicate that we are starting the baseline recording

		// Refactor to be reusable for other checks, than the baseline
		runner := runner.NewCheckRunner(ctx, workloadHardening, "baseline")

		go runner.RunCheck(ctx, workloadHardening.Spec.SecurityContext)

		// Requeue the reconciliation after the baseline duration, to continue with the next steps
		return ctrl.Result{RequeueAfter: duration + 10*time.Second}, nil
	}

	// If we are here, it means that the baseline recording is done
	// We can now start recording the workload under test with different security context configurations

	// ToDo: Define the neccessary checks to run, eg. user(runAsUser, runAsNonRoot), group(runAsGroup, fsGroup), roFs(readOnlyFilesystem), seccomp, etc.
	// Does it make sense to first run a check with a fully hardened security context and only if that fails, run the more granular checks?
	// ToDo: Refactor & Move into runner package
	podChecks := []string{"group", "user"}

	if workloadHardening.Spec.RunMode == checksv1alpha1.RunModeParallel {
		log.Info("Running checks in parallel mode")
		// Run all checks in parallel
		for _, checkType := range podChecks {

			if meta.IsStatusConditionTrue(workloadHardening.Status.Conditions, titleCase.String(checkType)+typeWorkloadCheck) {
				log.Info("Check already finished, skipping", "checkType", checkType)
				continue // Skip if the check is already recorded
			}

			if meta.IsStatusConditionFalse(workloadHardening.Status.Conditions, titleCase.String(checkType)+typeWorkloadCheck) {
				log.Info("Check still running, skipping", "checkType", checkType)
				continue // Skip if the check is already recorded
			}

			securityContext := workloadHardening.Spec.SecurityContext.DeepCopy()
			if securityContext == nil {
				securityContext = &checksv1alpha1.SecurityContextDefaults{
					Pod:       &checksv1alpha1.PodSecurityContextDefaults{},
					Container: &checksv1alpha1.ContainerSecurityContextDefaults{},
				}
			}

			if checkType == "user" {
				if securityContext.Pod.RunAsUser == nil {
					securityContext.Pod.RunAsUser = ptr.To(int64(10000))
				}
				if securityContext.Pod.RunAsNonRoot == nil {
					securityContext.Pod.RunAsNonRoot = ptr.To(true)
				}

				if securityContext.Container.RunAsUser == nil {
					securityContext.Container.RunAsUser = ptr.To(int64(10000))
				}
				if securityContext.Container.RunAsNonRoot == nil {
					securityContext.Container.RunAsNonRoot = ptr.To(true)
				}
			}

			if checkType == "group" {
				if securityContext.Pod.RunAsGroup == nil {
					securityContext.Pod.RunAsGroup = ptr.To(int64(10000))
				}
				if securityContext.Pod.FSGroup == nil {
					securityContext.Pod.FSGroup = ptr.To(int64(10000))
				}

				if securityContext.Container.RunAsGroup == nil {
					securityContext.Container.RunAsGroup = ptr.To(int64(10000))
				}
			}

			runner := runner.NewCheckRunner(ctx, workloadHardening, checkType)

			go runner.RunCheck(ctx, securityContext)
		}

		// Requeue the reconciliation after the baseline duration, to continue with the next steps
		return ctrl.Result{RequeueAfter: duration + 10*time.Second}, nil
	} else {
		log.Info("Running checks in sequential mode")
		// ToDo: How to determnine the order of checks? Which ones are already done?
	}

	// If all checks are done, we can now analyze the recorded signals
	// ToDo: Implement log comparison between checks and baseline
	// ToDo: Use metrics if feasible to support the findings from the logs
	// ToDo: Write the results to the status of the WorkloadHardeningCheck

	// Refactor into a go routine, and check for status updates
	return ctrl.Result{}, nil
}

func (r *WorkloadHardeningCheckReconciler) deleteNamespace(ctx context.Context, namespaceName string) error {
	log := log.FromContext(ctx).WithValues("namespace", namespaceName)
	targetNs := &corev1.Namespace{}
	err := r.Get(ctx, client.ObjectKey{Name: namespaceName}, targetNs)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Namespace already deleted")
			return nil
		}
		log.Error(err, "Failed to get Namespace for deletion")
		return err
	}
	log.Info("Deleting Namespace")
	err = r.Delete(ctx, targetNs)
	if err != nil {
		log.Error(err, "Failed to delete Namespace")
		return err
	}
	return nil
}

func (r *WorkloadHardeningCheckReconciler) setCondition(ctx context.Context, workloadHardening *checksv1alpha1.WorkloadHardeningCheck, condition metav1.Condition) error {

	log := log.FromContext(ctx)

	var err error
	retryCount := 0

	// retry 3 times to update the status of the WorkloadHardeningCheck, to avoid concurrent updates failing
	for retryCount < 3 {
		// Let's re-fetch the workload hardening check Custom Resource after updating the status so that we have the latest state
		if err = r.Get(ctx, types.NamespacedName{Name: workloadHardening.Name, Namespace: workloadHardening.Namespace}, workloadHardening); err != nil {
			log.Error(err, "Failed to re-fetch WorkloadHardeningCheck")
			return err
		}

		// Set/Update condition
		meta.SetStatusCondition(
			&workloadHardening.Status.Conditions,
			condition,
		)

		if err := r.Status().Update(ctx, workloadHardening); err != nil {
			log.V(3).Info("Failed to update WorkloadHardeningCheck status, retrying")
			retryCount++
			continue // Retry updating the status
		} else {
			break
		}

	}

	return err
}

// SetupWithManager sets up the controller with the Manager.
func (r *WorkloadHardeningCheckReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&checksv1alpha1.WorkloadHardeningCheck{}).
		Owns(&corev1.Namespace{}).
		// ToDo: Decide if configurable
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		WithEventFilter(ignoreStatusChanges()).
		Complete(r)
}

func ignoreStatusChanges() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			// Ignore updates to CR status in which case metadata.Generation does not change
			return e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration()
		},
	}
}
