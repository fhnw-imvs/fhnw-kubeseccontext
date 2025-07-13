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
// The flow is basically implemented in reverse, to finish execution early
// The order they are implemented is as follows:
// 1. Check if the WorkloadHardeningCheck instance exists, if not, we clean up the resources (as the reconcile loop is also called on deletion)
// 2. If the WorkloadHardeningCheck instance is already finished, we skip the reconciliation
// 3. If the final check run is already finished, we set the Finished condition to true
// 4. If all checks are finished and the analysis is not yet done, we analyze the results
// 5. Create a final check run using the recommended security context
// -- Only now we start to check if we need to record a baseline or run checks --
// 6. Ensure the original workload is running, otherwise there is nothing to analyze
// 7. If the baseline is not recorded yet, we start the baseline recording
// 8. If the baselnie is recorded, we start recording the different checks
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

	// ConditionFinished is set to true when the reconciliation is finished
	if meta.IsStatusConditionTrue(workloadHardening.Status.Conditions, checksv1alpha1.ConditionTypeFinished) {
		log.Info("WorkloadHardeningCheck is already finished, skipping reconciliation")
		return ctrl.Result{}, nil
	}

	checkManager := workload.NewWorkloadCheckManager(ctx, r.ValKeyClient, workloadHardening)

	// Let's just set the status as Unknown when no status is available
	if len(workloadHardening.Status.Conditions) == 0 {

		// Set condition finished to false, so we can track the progress of the reconciliation
		err = checkManager.SetCondition(ctx, metav1.Condition{
			Type:    checksv1alpha1.ConditionTypeFinished,
			Status:  metav1.ConditionFalse,
			Reason:  checksv1alpha1.ReasonPreparationVerifying,
			Message: "Reconciliation started",
		})
		if err != nil {
			return ctrl.Result{RequeueAfter: 10 * time.Minute}, err
		}
	}

	// If the final check run is already finished, we set the Finished condition to true
	if checkManager.RecommendationExists() && checkManager.FinalCheckRecorded() {
		log.Info("Final check run finished, setting Finished condition")
		err = checkManager.SetCondition(ctx, metav1.Condition{
			Type:    checksv1alpha1.ConditionTypeFinished,
			Status:  metav1.ConditionTrue,
			Reason:  checksv1alpha1.ConditionTypeFinished,
			Message: "Finished, recommendation ready",
		})

		return ctrl.Result{}, err
	}

	// If all checks are finished and the analysis is not yet done, we need to analyze the results
	if checkManager.AllChecksFinished() && !meta.IsStatusConditionTrue(workloadHardening.Status.Conditions, checksv1alpha1.ConditionTypeAnalysis) {
		// !meta.IsStatusConditionTrue also returns true if the condition is not set at all !!

		checkManager.SetCondition(ctx, metav1.Condition{
			Type:    checksv1alpha1.ConditionTypeFinished,
			Status:  metav1.ConditionFalse,
			Reason:  checksv1alpha1.ReasonAnalysisRunning,
			Message: "Check runs are beeing analyzed",
		})

		log.Info("All checks are finished, analyzing results")
		err = checkManager.SetCondition(ctx, metav1.Condition{
			Type:    checksv1alpha1.ConditionTypeAnalysis,
			Status:  metav1.ConditionFalse,
			Reason:  checksv1alpha1.ReasonAnalysisRunning,
			Message: "Check runs are beeing analyzed",
		})

		err = checkManager.AnalyzeCheckRuns(ctx)
		if err != nil {
			log.Error(err, "Failed to analyze check runs")
			err = checkManager.SetCondition(ctx, metav1.Condition{
				Type:    checksv1alpha1.ConditionTypeAnalysis,
				Status:  metav1.ConditionFalse,
				Reason:  checksv1alpha1.ReasonAnalysisFailed,
				Message: "Error analyzing check runs",
			})
			return ctrl.Result{RequeueAfter: 10 * time.Minute}, nil
		}

		checkManager.SetRecommendation(ctx)

		err = checkManager.SetCondition(ctx, metav1.Condition{
			Type:    checksv1alpha1.ConditionTypeAnalysis,
			Status:  metav1.ConditionTrue,
			Reason:  checksv1alpha1.ReasonAnalysisFinished,
			Message: "Check runs are analyzed",
		})

		checkManager.SetCondition(ctx, metav1.Condition{
			Type:    checksv1alpha1.ConditionTypeFinished,
			Status:  metav1.ConditionFalse,
			Reason:  checksv1alpha1.ReasonAnalysisRunning,
			Message: "Check runs are analyzed, waiting for final check run",
		})

	}

	// The final check run has failed! We set the Finished condition to true and return
	// ToDo: Analyse the results for the final check run and report the errors
	if meta.IsStatusConditionPresentAndEqual(workloadHardening.Status.Conditions, checksv1alpha1.ConditionTypeFinished, metav1.ConditionUnknown) {
		log.Info("Final check run failed, setting Finished condition to true")
		err = checkManager.SetCondition(ctx, metav1.Condition{
			Type:    checksv1alpha1.ConditionTypeFinished,
			Status:  metav1.ConditionTrue,
			Reason:  "FinalCheckFailed",
			Message: "Final check run failed, cannot proceed with checks",
		})

		return ctrl.Result{RequeueAfter: 10 * time.Minute}, err
	}

	// If the final check run is already running, we need to wait for it to finish
	if checkManager.FinalCheckInProgress() {
		log.Info("Final check run is still running, waiting for it to finish")

		return ctrl.Result{RequeueAfter: checkManager.GetCheckDuration() / 2}, nil
	}

	// If all checks are finished and the results are analyzed, create a final check run using the recommended security context
	if checkManager.RecommendationExists() && !checkManager.FinalCheckRecorded() {
		// !meta.IsStatusConditionTrue also returns true if the condition is not set at all !!
		log.Info("Starting Final check run with recommended security context")

		securityContext := checkManager.GetRecommendedSecurityContext()
		finalCheckRunner := runner.NewCheckRunner(ctx, r.ValKeyClient, r.Recorder, workloadHardening, "Final")
		go finalCheckRunner.RunCheck(ctx, securityContext)

		checkManager.SetCondition(ctx, metav1.Condition{
			Type:    checksv1alpha1.ConditionTypeFinished,
			Status:  metav1.ConditionFalse,
			Reason:  "Final" + checksv1alpha1.ReasonCheckRecording,
			Message: "Final check run with recommended security context started",
		})

		return ctrl.Result{RequeueAfter: checkManager.GetCheckDuration() + 10*time.Second}, nil
	}

	// Verify if the workload is running, if it is not running, we will never get it running in a cloned namespace
	originalRunnig, err := checkManager.VerifyRunning(ctx, workloadHardening.GetNamespace())
	if err != nil {
		log.Error(err, "Failed to verify if the workload is running")
		return ctrl.Result{}, err
	}
	if !originalRunnig {

		// If the original workload is not running, and it's the first time we are checking this, we will set the condition to Unknown, and retry after 1 minute
		// If the condition is already set to Unknown, this is the second attempt, and we consider it a failure
		if meta.IsStatusConditionPresentAndEqual(workloadHardening.Status.Conditions, checksv1alpha1.ConditionTypeFinished, metav1.ConditionUnknown) {
			log.Info("Original workload is not running, marking as failed")
			err = checkManager.SetCondition(ctx, metav1.Condition{
				Type:    checksv1alpha1.ConditionTypeFinished,
				Status:  metav1.ConditionFalse,
				Reason:  "OriginalNotRunning",
				Message: "Original workload is not running, cannot proceed with checks",
			})
			if err != nil {
				log.Error(err, "Failed to set condition for not running workload")
			}
			return ctrl.Result{}, nil
		} else {
			log.Info("Original workload is not running, requeuing")
			err = checkManager.SetCondition(ctx, metav1.Condition{
				Type:    checksv1alpha1.ConditionTypeFinished,
				Status:  metav1.ConditionUnknown,
				Reason:  "OriginalNotRunning",
				Message: "Original workload is not running, will retry in 1 minute",
			})
			if err != nil {
				log.Error(err, "Failed to set condition for not running workload")
			}
			return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
		}

	}

	// We use the baseline duration to determine how long we should wait before requeuing the reconciliation
	duration := checkManager.GetCheckDuration()

	// Based on the Status, we need to decide what to do next
	// If there is no Baseline recorded yet, we need to start the baseline recording

	if checkManager.BaselineInProgress() {
		log.Info("Baseline not recorded yet, waiting for baseline recording to finish")
		// If the baseline is not recorded yet, we need to wait for the baseline recording to finish
		if checkManager.BaselineOverdue() {
			log.Info("Baseline recording is overdue, requeuing reconciliation")
			checkManager.SetCondition(ctx, metav1.Condition{
				Type:    checksv1alpha1.ConditionTypeBaseline,
				Status:  metav1.ConditionUnknown,
				Reason:  checksv1alpha1.ReasonRequeue,
				Message: "Baseline recording is still running, but last transition time is older than 2x duration, requeuing",
			})
		}

		return ctrl.Result{RequeueAfter: duration / 2}, nil
	}

	if !checkManager.BaselineRecorded() {
		log.Info("Baseline not recorded yet. Starting baseline recording")
		// Set the condition to indicate that we are starting the baseline recording

		baselineRunner := runner.NewCheckRunner(ctx, r.ValKeyClient, r.Recorder, workloadHardening, "baseline")
		go baselineRunner.RunCheck(ctx, workloadHardening.Spec.SecurityContext)

		checkManager.SetCondition(ctx, metav1.Condition{
			Type:    checksv1alpha1.ConditionTypeFinished,
			Status:  metav1.ConditionFalse,
			Reason:  checksv1alpha1.ReasonBaselineRecording,
			Message: "Baseline recording started",
		})

		// The baseline is recorded twice, to make the log matching better, as the logs are ingested using different timestamps

		time.Sleep(10 * time.Second) // Sleep for a short duration to allow the first baseline recording to start
		baselineRunner = runner.NewCheckRunner(ctx, r.ValKeyClient, r.Recorder, workloadHardening, "baseline-2")
		go baselineRunner.RunCheck(ctx, workloadHardening.Spec.SecurityContext)

		// Requeue the reconciliation after the baseline duration, to continue with the next steps
		return ctrl.Result{RequeueAfter: duration + 10*time.Second}, nil
	}

	// If we are here, it means that the baseline recording is done
	// We can now start recording the workload under test with different security context configurations

	requiredChecks := checkManager.GetRequiredCheckRuns(ctx)

	if checkManager.BaselineRecorded() && !checkManager.AllChecksFinished() {
		log.Info("Not all checks are finished, running checks")

		checkManager.SetCondition(ctx, metav1.Condition{
			Type:    checksv1alpha1.ConditionTypeFinished,
			Status:  metav1.ConditionFalse,
			Reason:  checksv1alpha1.ReasonCheckRecording,
			Message: "Check recording started",
		})

		if workloadHardening.Spec.RunMode == checksv1alpha1.RunModeParallel {
			log.Info("Running checks in parallel mode")
			// Run all checks in parallel
			for _, checkType := range requiredChecks {

				if checkManager.CheckRecorded(checkType) {
					log.V(2).Info("Check already finished, skipping", "checkType", checkType)
					continue // Skip if the check is already recorded
				}

				if checkManager.CheckInProgress(checkType) {

					if checkManager.CheckOverdue(checkType) {
						checkManager.SetCondition(ctx, metav1.Condition{
							Type:    titleCase.String(checkType) + checksv1alpha1.ConditionTypeCheck,
							Status:  metav1.ConditionUnknown,
							Reason:  checksv1alpha1.ReasonRequeue,
							Message: "Check is still running, but last transition time is older than 2x duration, requeuing",
						})

					} else {
						log.V(2).Info("Check still running, skipping", "checkType", checkType)
						continue // Skip if the check might still be running
					}

				}

				log.Info("Starting new check run", "checkType", checkType)

				securityContext := checkManager.GetSecurityContextForCheckType(checkType)

				checkRunner := runner.NewCheckRunner(ctx, r.ValKeyClient, r.Recorder, workloadHardening, checkType)

				go checkRunner.RunCheck(ctx, securityContext)
			}

			// Requeue the reconciliation after the baseline duration, to continue with the next steps
			return ctrl.Result{RequeueAfter: duration + 10*time.Second}, nil
		} else {
			log.Info("Running checks in sequential mode")

			for _, checkType := range requiredChecks {
				if checkManager.CheckRecorded(checkType) {
					log.V(2).Info("Check already finished, skipping", "checkType", checkType)
					continue // Skip if the check is already recorded
				}
				if checkManager.CheckInProgress(checkType) {

					if checkManager.CheckOverdue(checkType) {
						checkManager.SetCondition(ctx, metav1.Condition{
							Type:    titleCase.String(checkType) + checksv1alpha1.ConditionTypeCheck,
							Status:  metav1.ConditionUnknown,
							Reason:  checksv1alpha1.ReasonRequeue,
							Message: "Check is still running, but last transition time is older than duration + 1 minute, requeuing",
						})

					} else {
						log.V(2).Info("Check still running, skipping", "checkType", checkType)
						continue // Skip if the check is already recorded
					}
				}

				securityContext := checkManager.GetSecurityContextForCheckType(checkType)
				checkRunner := runner.NewCheckRunner(ctx, r.ValKeyClient, r.Recorder, workloadHardening, checkType)
				log.Info("Running check", "checkType", checkType)
				go checkRunner.RunCheck(ctx, securityContext)

				// Requeue the reconciliation after the  duration, to continue with the next check
				return ctrl.Result{RequeueAfter: duration + 10*time.Second}, nil
			}
		}
	}

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
