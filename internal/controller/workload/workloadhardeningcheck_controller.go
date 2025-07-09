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
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	checksv1alpha1 "github.com/fhnw-imvs/fhnw-kubeseccontext/api/v1alpha1"
	"github.com/fhnw-imvs/fhnw-kubeseccontext/internal/runner"
	"github.com/fhnw-imvs/fhnw-kubeseccontext/internal/valkey"
	"github.com/fhnw-imvs/fhnw-kubeseccontext/internal/workload"
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

	handler := workload.NewWorkloadCheckHandler(ctx, r.ValKeyClient, workloadHardening)

	// Let's just set the status as Unknown when no status is available
	if len(workloadHardening.Status.Conditions) == 0 {

		err = handler.SetCondition(ctx, metav1.Condition{
			Type:    checksv1alpha1.ConditionTypePreparation,
			Status:  metav1.ConditionUnknown,
			Reason:  "Verifying",
			Message: "Starting reconciliation",
		})
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	// We use the baseline duration to determine how long we should wait before requeuing the reconciliation
	duration := handler.GetCheckDuration()

	// Based on the Status, we need to decide what to do next
	// If there is no Baseline recorded yet, we need to start the baseline recording

	if !meta.IsStatusConditionTrue(workloadHardening.Status.Conditions, checksv1alpha1.ConditionTypeBaseline) {
		log.Info("Baseline not recorded yet. Starting baseline recording")
		// Set the condition to indicate that we are starting the baseline recording

		baselineRunner := runner.NewCheckRunner(ctx, r.ValKeyClient, r.Recorder, workloadHardening, "baseline")
		go baselineRunner.RunCheck(ctx, workloadHardening.Spec.SecurityContext)

		// The baseline is recorded twice, to make the log matching better, as the logs are ingested using different timestamps

		time.Sleep(10 * time.Second) // Sleep for a short duration to allow the first baseline recording to start
		baselineRunner = runner.NewCheckRunner(ctx, r.ValKeyClient, r.Recorder, workloadHardening, "baseline-2")
		go baselineRunner.RunCheck(ctx, workloadHardening.Spec.SecurityContext)

		// Requeue the reconciliation after the baseline duration, to continue with the next steps
		return ctrl.Result{RequeueAfter: duration + 10*time.Second}, nil
	}

	// If we are here, it means that the baseline recording is done
	// We can now start recording the workload under test with different security context configurations

	// ToDo: Define the neccessary checks to run, eg. user(runAsUser, runAsNonRoot), group(runAsGroup, fsGroup), roFs(readOnlyFilesystem), seccomp, etc.
	// Does it make sense to first run a check with a fully hardened security context and only if that fails, run the more granular checks?
	// ToDo: Refactor & Move into runner package

	requiredChecks := handler.GetRequiredCheckRuns(ctx)

	if len(requiredChecks) > 0 || !handler.AllChecksFinished() {
		log.Info("Not all checks are finished, running checks")

		if workloadHardening.Spec.RunMode == checksv1alpha1.RunModeParallel {
			log.Info("Running checks in parallel mode")
			// Run all checks in parallel
			for _, checkType := range requiredChecks {

				if meta.IsStatusConditionTrue(workloadHardening.Status.Conditions, titleCase.String(checkType)+checksv1alpha1.ConditionTypeCheck) {
					log.V(2).Info("Check already finished, skipping", "checkType", checkType)
					continue // Skip if the check is already recorded
				}

				if meta.IsStatusConditionFalse(workloadHardening.Status.Conditions, titleCase.String(checkType)+checksv1alpha1.ConditionTypeCheck) {
					log.V(2).Info("Check still running, skipping", "checkType", checkType)

					condition := meta.FindStatusCondition(workloadHardening.Status.Conditions, titleCase.String(checkType)+checksv1alpha1.ConditionTypeCheck)
					expiryTime := metav1.NewTime(time.Now().Add(-2 * duration))
					if condition.LastTransitionTime.Before(&expiryTime) {
						log.V(2).Info("Check still running, but last transition time is older than 2x duration, requeuing", "checkType", checkType)
						handler.SetCondition(ctx, metav1.Condition{
							Type:    titleCase.String(checkType) + checksv1alpha1.ConditionTypeCheck,
							Status:  metav1.ConditionUnknown,
							Reason:  "Requeuing",
							Message: "Check is still running, but last transition time is older than 2x duration, requeuing",
						})

					} else {
						continue // Skip if the check might still be running
					}

				}

				securityContext := handler.GetSecurityContextForCheckType(checkType)

				checkRunner := runner.NewCheckRunner(ctx, r.ValKeyClient, r.Recorder, workloadHardening, checkType)

				go checkRunner.RunCheck(ctx, securityContext)
			}

			// Requeue the reconciliation after the baseline duration, to continue with the next steps
			return ctrl.Result{RequeueAfter: duration + 10*time.Second}, nil
		} else {
			log.Info("Running checks in sequential mode")

			for _, checkType := range requiredChecks {
				if meta.IsStatusConditionTrue(workloadHardening.Status.Conditions, titleCase.String(checkType)+checksv1alpha1.ConditionTypeCheck) {
					log.V(2).Info("Check already finished, skipping", "checkType", checkType)
					continue // Skip if the check is already recorded
				}
				if meta.IsStatusConditionFalse(workloadHardening.Status.Conditions, titleCase.String(checkType)+checksv1alpha1.ConditionTypeCheck) {
					log.V(2).Info("Check still running, skipping", "checkType", checkType)

					condition := meta.FindStatusCondition(workloadHardening.Status.Conditions, titleCase.String(checkType)+checksv1alpha1.ConditionTypeCheck)
					expiryTime := metav1.NewTime(time.Now().Add(-2 * duration))
					if condition.LastTransitionTime.Before(&expiryTime) {
						log.V(2).Info("Check still running, but last transition time is older than 2x duration, requeuing", "checkType", checkType)
						handler.SetCondition(ctx, metav1.Condition{
							Type:    titleCase.String(checkType) + checksv1alpha1.ConditionTypeCheck,
							Status:  metav1.ConditionUnknown,
							Reason:  "Requeuing",
							Message: "Check is still running, but last transition time is older than 2x duration, requeuing",
						})

					} else {
						continue // Skip if the check is already recorded
					}
				}

				securityContext := handler.GetSecurityContextForCheckType(checkType)
				checkRunner := runner.NewCheckRunner(ctx, r.ValKeyClient, r.Recorder, workloadHardening, checkType)
				log.Info("Running check", "checkType", checkType)
				go checkRunner.RunCheck(ctx, securityContext)

				// Requeue the reconciliation after the  duration, to continue with the next check
				return ctrl.Result{RequeueAfter: duration + 10*time.Second}, nil
			}
		}
	}

	if !meta.IsStatusConditionTrue(workloadHardening.Status.Conditions, checksv1alpha1.ConditionTypeAnalysis) && handler.AllChecksFinished() {

		log.Info("All checks are finished, analyzing results")
		err = handler.SetCondition(ctx, metav1.Condition{
			Type:    checksv1alpha1.ConditionTypeAnalysis,
			Status:  metav1.ConditionFalse,
			Reason:  "Analyzing",
			Message: "Check runs are beeing analyzed",
		})

		err = handler.AnalyzeCheckRuns(ctx)
		if err != nil {
			log.Error(err, "Failed to analyze check runs")
			err = handler.SetCondition(ctx, metav1.Condition{
				Type:    checksv1alpha1.ConditionTypeAnalysis,
				Status:  metav1.ConditionFalse,
				Reason:  "AnalyzeFailed",
				Message: "Error analyzing check runs",
			})
			return ctrl.Result{}, nil
		}

		handler.SetRecommendation(ctx)

		err = handler.SetCondition(ctx, metav1.Condition{
			Type:    checksv1alpha1.ConditionTypeAnalysis,
			Status:  metav1.ConditionTrue,
			Reason:  "Analyzed",
			Message: "Check runs are analyzed",
		})

	}

	// If all checks are done, we can now analyze the recorded signals
	// ToDo: Write the results to the status of the WorkloadHardeningCheck / Generate recommendations

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
