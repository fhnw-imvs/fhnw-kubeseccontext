package runner

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	checksv1alpha1 "github.com/fhnw-imvs/fhnw-kubeseccontext/api/v1alpha1"
	"github.com/fhnw-imvs/fhnw-kubeseccontext/internal/recording"
	"github.com/fhnw-imvs/fhnw-kubeseccontext/internal/valkey"
	wh "github.com/fhnw-imvs/fhnw-kubeseccontext/internal/workload"
	"github.com/fhnw-imvs/fhnw-kubeseccontext/pkg/orakel"
	"github.com/go-logr/logr"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

type WorkloadCheckRunner struct {
	client.Client
	checkManager *wh.WorkloadCheckManager

	scheme *runtime.Scheme

	valKeyClient *valkey.ValkeyClient
	logger       logr.Logger
	recorder     record.EventRecorder

	workloadHardeningCheck *checksv1alpha1.WorkloadHardeningCheck
	checkType              string
	conditionType          string

	checkSuccessful bool
}

// Required to convert "user" to "User", strings.ToTitle converts each rune to title case not just the first one
var titleCase = cases.Title(language.English)

func NewWorkloadCheckRunner(ctx context.Context, valKeyClient *valkey.ValkeyClient, recorder record.EventRecorder, workloadHardeningCheck *checksv1alpha1.WorkloadHardeningCheck, checkType string) *WorkloadCheckRunner {

	log := logf.FromContext(ctx).WithName("CheckRunner").WithValues("checkType", checkType)

	conditionType := titleCase.String(checkType) + checksv1alpha1.ConditionTypeCheck
	if strings.Contains(strings.ToLower(checkType), "baseline") {
		conditionType = checksv1alpha1.ConditionTypeBaseline
	}
	if strings.Contains(strings.ToLower(checkType), "final") {
		conditionType = checksv1alpha1.ConditionTypeFinalCheck
	}

	scheme := runtime.NewScheme()

	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(checksv1alpha1.AddToScheme(scheme))

	cfg, err := ctrl.GetConfig()
	if err != nil {
		log.Error(err, "failed to get Kubernetes config")
		return nil
	}

	cl, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		log.Error(err, "failed to create Kubernetes client")
		return nil
	}

	checkRunner := &WorkloadCheckRunner{
		Client:                 cl,
		checkManager:           wh.NewWorkloadCheckManager(ctx, valKeyClient, workloadHardeningCheck),
		logger:                 log,
		valKeyClient:           valKeyClient,
		recorder:               recorder,
		workloadHardeningCheck: workloadHardeningCheck.DeepCopy(),
		checkType:              checkType,
		conditionType:          conditionType,
		scheme:                 scheme,
	}

	// Override checkRunner with one that also includes the targetNamespace
	checkRunner.logger = checkRunner.logger.WithValues("targetNamespace", checkRunner.generateTargetNamespaceName())

	return checkRunner

}

// Create the target namespace name. It consists of the base namespace, the suffix set on the workload hardening check, and the check type.
func (r *WorkloadCheckRunner) generateTargetNamespaceName() string {
	base := r.workloadHardeningCheck.Namespace
	if len(base) > 45 {
		base = base[:45] // Limit the base namespace to 45 characters to ensure the total length does not exceed 63 characters
	}

	// max length: 63
	// suffix length: 8
	// checkType: variable ~10-30 characters => find abbreviation... for checkType

	namespaceName := strings.ToLower(fmt.Sprintf("%s-%s-%s", base, r.workloadHardeningCheck.Spec.Suffix, r.checkType))

	if len(namespaceName) > 63 {
		return namespaceName[:63] // Ensure the namespace name does not exceed 63 characters
	}

	return namespaceName
}

func (r *WorkloadCheckRunner) namespaceExists(ctx context.Context, namespaceName string) bool {
	targetNs := &corev1.Namespace{}
	err := r.Get(ctx, client.ObjectKey{Name: namespaceName}, targetNs)

	return !apierrors.IsNotFound(err)
}

// createCheckNamespace clones the namespace of the workload hardening check target workload into a new namespace.
func (r *WorkloadCheckRunner) createCheckNamespace(ctx context.Context) error {
	targetNamespace := r.generateTargetNamespaceName()

	err := CloneNamespace(ctx, r.workloadHardeningCheck.Namespace, targetNamespace, r.workloadHardeningCheck.Spec.Suffix)

	if err != nil {
		r.logger.Error(err, fmt.Sprintf("failed to clone namespace %s", r.workloadHardeningCheck.Namespace))
		return err
	}

	targetNs := &corev1.Namespace{}
	err = r.Get(ctx, client.ObjectKey{Name: targetNamespace}, targetNs)
	if err != nil {
		if apierrors.IsNotFound(err) {
			r.logger.Error(err, "target namespace not found after cloning")
			return fmt.Errorf("target namespace %s not found after cloning", targetNamespace)
		}
		// Error reading the object - requeue the request.
		r.logger.Error(err, "failed to get target namespace after cloning")
		return fmt.Errorf("failed to get target namespace %s after cloning: %w", targetNamespace, err)
	}

	// While it would be useful to set the owner reference to the workload hardening check,
	// only cluster wide resources, can claim cluster wide resources as owner.

	return nil

}

func (r *WorkloadCheckRunner) deleteCheckNamespace(ctx context.Context) error {
	namespaceName := r.generateTargetNamespaceName()

	log := r.logger.WithValues("namespace", namespaceName)

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

	// Remove all ClusterRoleBindings that are associated with this namespace
	clusterRoleBindingList := &rbacv1.ClusterRoleBindingList{}
	r.List(ctx, clusterRoleBindingList)
	for _, clusterRoleBinding := range clusterRoleBindingList.Items {
		if clusterRoleBinding.Labels["orakel.fhnw.ch/target-namespace"] == namespaceName {
			log.Info("Deleting ClusterRoleBinding", "name", clusterRoleBinding.Name)
			err = r.Delete(ctx, &clusterRoleBinding)
			if err != nil {
				log.Error(err, "Failed to delete ClusterRoleBinding", "name", clusterRoleBinding.Name)
			}
		}
	}

	return nil
}

func (r *WorkloadCheckRunner) setStatusRunning(ctx context.Context, message string) {
	conditionReason := checksv1alpha1.ReasonCheckRecording
	if r.conditionType == checksv1alpha1.ConditionTypeBaseline {
		conditionReason = checksv1alpha1.ReasonBaselineRecording
	}

	r.checkManager.SetCondition(ctx, metav1.Condition{
		Type:    r.conditionType,
		Status:  metav1.ConditionFalse,
		Reason:  conditionReason,
		Message: message,
	})
}

// setStatusFailed sets the status of the check to failed, and sets the condition to unknown
// Those errors are considered transient, and the check can be retried later
func (r *WorkloadCheckRunner) setStatusFailed(ctx context.Context, message string) {
	conditionReason := checksv1alpha1.ReasonCheckRecordingFailed
	if strings.Contains(r.checkType, "baseline") {
		conditionReason = checksv1alpha1.ReasonBaselineRecordingFailed
	}

	r.checkManager.SetCondition(ctx, metav1.Condition{
		Type:    r.conditionType,
		Status:  metav1.ConditionUnknown,
		Reason:  conditionReason,
		Message: message,
	})

}

func (r *WorkloadCheckRunner) setStatusFinishedFailure(ctx context.Context, message string, failureReason string, workloadRecording recording.WorkloadRecording) {
	conditionReason := checksv1alpha1.ReasonCheckRecordingFailed
	if r.conditionType == checksv1alpha1.ConditionTypeBaseline {
		conditionReason = checksv1alpha1.ReasonBaselineRecordingFailed
	}

	err := r.checkManager.SetCondition(ctx, metav1.Condition{
		Type:    r.conditionType,
		Status:  metav1.ConditionTrue,
		Reason:  conditionReason,
		Message: message,
	})

	if err != nil {
		r.logger.Error(err, "Failed to set condition for check")
	}

	// Load the logs into the log orakel for analysis
	anomalies := make(map[string][]string)
	for container, logs := range workloadRecording.Logs {
		logOrakel := orakel.NewLogOrakel()
		logOrakel.LoadBaseline(logs)
		anomalies[container] = logOrakel.GetTemplates()
	}

	checkRun := checksv1alpha1.CheckRun{
		Name:                 r.checkType,
		RecordingSuccessfull: ptr.To(false),
		CheckSuccessfull:     ptr.To(false),
		SecurityContext:      workloadRecording.SecurityContextConfigurations,
		FailureReason:        failureReason,
		LogAnomalies:         anomalies,
	}

	retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := r.Get(ctx, types.NamespacedName{Name: r.workloadHardeningCheck.Name, Namespace: r.workloadHardeningCheck.Namespace}, r.workloadHardeningCheck); err != nil {
			if apierrors.IsNotFound(err) {
				r.logger.Info("WorkloadHardeningCheck not found, skipping status update")
				return nil // If the resource is not found, we can skip the update
			}
			r.logger.Error(err, "Failed to re-fetch WorkloadHardeningCheck")
			return err
		}

		switch r.conditionType {
		case checksv1alpha1.ConditionTypeBaseline:
			if r.workloadHardeningCheck.Status.BaselineRuns == nil {
				r.workloadHardeningCheck.Status.BaselineRuns = []*checksv1alpha1.CheckRun{}
			}
			r.workloadHardeningCheck.Status.BaselineRuns = append(r.workloadHardeningCheck.Status.BaselineRuns, &checkRun)

		case checksv1alpha1.ConditionTypeFinalCheck:
			r.workloadHardeningCheck.Status.FinalRun = &checkRun

		default:
			if r.workloadHardeningCheck.Status.CheckRuns == nil {
				r.workloadHardeningCheck.Status.CheckRuns = make(map[string]*checksv1alpha1.CheckRun)
			}
			r.workloadHardeningCheck.Status.CheckRuns[checkRun.Name] = &checkRun
		}

		return r.Status().Update(ctx, r.workloadHardeningCheck)
	})

}

func (r *WorkloadCheckRunner) setStatusFinishedSuccessfully(ctx context.Context, message string, securityContext *checksv1alpha1.SecurityContextDefaults) {
	conditionReason := checksv1alpha1.ReasonCheckRecordingFinished
	if r.conditionType == checksv1alpha1.ConditionTypeBaseline {
		conditionReason = checksv1alpha1.ReasonBaselineRecordingFinished
	}

	err := r.checkManager.SetCondition(ctx, metav1.Condition{
		Type:    r.conditionType,
		Status:  metav1.ConditionTrue,
		Reason:  conditionReason,
		Message: message,
	})

	if err != nil {
		r.logger.Error(err, "Failed to set condition for check")
	}

	checkRun := checksv1alpha1.CheckRun{
		Name:                 r.checkType,
		RecordingSuccessfull: ptr.To(true),
		CheckSuccessfull:     ptr.To(r.checkSuccessful),
		SecurityContext:      securityContext,
	}

	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Let's re-fetch the workload hardening check Custom Resource after updating the status so that we have the latest state
		if err := r.Get(ctx, types.NamespacedName{Name: r.workloadHardeningCheck.Name, Namespace: r.workloadHardeningCheck.Namespace}, r.workloadHardeningCheck); err != nil {
			if apierrors.IsNotFound(err) {
				// workloadHardeningCheck resource was deleted, while a check was running
				r.logger.Info("WorkloadHardeningCheck not found, skipping status update")
				return nil // If the resource is not found, we can skip the update
			}
			r.logger.Error(err, "Failed to re-fetch WorkloadHardeningCheck")
		}

		// Baseline and final checks are handled differently
		switch r.conditionType {
		case checksv1alpha1.ConditionTypeBaseline:
			// Set/Update condition for baseline check
			if r.workloadHardeningCheck.Status.BaselineRuns == nil {
				r.workloadHardeningCheck.Status.BaselineRuns = []*checksv1alpha1.CheckRun{}
			}

			r.workloadHardeningCheck.Status.BaselineRuns = append(r.workloadHardeningCheck.Status.BaselineRuns, &checkRun)

		case checksv1alpha1.ConditionTypeFinalCheck:
			r.workloadHardeningCheck.Status.FinalRun = &checkRun
		default:

			// Set/Update condition
			if r.workloadHardeningCheck.Status.CheckRuns == nil {
				r.workloadHardeningCheck.Status.CheckRuns = make(map[string]*checksv1alpha1.CheckRun)
			}
			r.workloadHardeningCheck.Status.CheckRuns[checkRun.Name] = &checkRun
		}
		return r.Status().Update(ctx, r.workloadHardeningCheck)

	})

	if err != nil {
		logf.FromContext(ctx).Error(err, "Failed to update WorkloadHardeningCheck status after recording finished")
		r.recorder.Event(
			r.workloadHardeningCheck,
			corev1.EventTypeWarning,
			conditionReason,
			fmt.Sprintf("Failed to update WorkloadHardeningCheck status after recording finished: %s", err.Error()),
		)
		return
	}

	r.recorder.Event(
		r.workloadHardeningCheck,
		corev1.EventTypeNormal,
		conditionReason,
		fmt.Sprintf(
			"Recorded %sCheck in namespace %s",
			r.checkType,
			r.generateTargetNamespaceName(),
		),
	)
}

func (r *WorkloadCheckRunner) RunCheck(ctx context.Context, securityContext *checksv1alpha1.SecurityContextDefaults) {

	targetNamespaceName := r.generateTargetNamespaceName()

	// Check if the target namespace already exists, otherwise create it
	if r.namespaceExists(ctx, targetNamespaceName) {
		if meta.IsStatusConditionPresentAndEqual(
			r.workloadHardeningCheck.Status.Conditions,
			r.conditionType,
			metav1.ConditionUnknown,
		) {
			r.logger.Info(
				"Target namespace already exists, and check is in unknown state indicating a previous run was not finished",
			)

			r.deleteCheckNamespace(ctx)
		}

	}

	if !r.namespaceExists(ctx, targetNamespaceName) {

		// clone into target namespace
		err := r.createCheckNamespace(ctx)
		if err != nil {
			r.logger.Error(err, "failed to create target namespace for baseline recording")
			return
		}

		r.logger.Info("created namespace")

		time.Sleep(1 * time.Second) // Give the namespace some time to be fully created and ready
	}

	// Set condition to false, as we are about to start the check
	r.setStatusRunning(ctx, "Start target workload with updated security context")

	// Fetch the workload we want to test, make sure we fetch it from the target namespace
	workloadUnderTest, err := r.checkManager.GetWorkloadUnderTest(ctx, targetNamespaceName)
	if err != nil {
		r.logger.Error(err, "failed to get workload under test")
		r.setStatusFailed(ctx, "Failed to get workload under test")
		return
	}

	// Eg. Baseline checks, or final checks if nothing need to be applied
	if securityContext.IsEmpty() {
		r.logger.V(2).Info("Security context is empty, nothing to apply")
	} else {
		err = r.applySecurityContext(ctx, workloadUnderTest, securityContext)
		if err != nil {
			r.logger.Error(err, "failed to apply security context to workload under test")
			r.setStatusFailed(ctx, "Failed to apply security context to workload under test")
			return
		}
	}

	// We need to ensure that the pods are updated and scheduled to nodes before we can record metrics
	// they don't need to be started/running, as they could be in a crash loop
	updated, err := r.waitForUpdatedPods(ctx, workloadUnderTest)
	if err != nil || !updated {
		r.logger.Error(err, "failed to wait for updated pods")
		r.setStatusFailed(ctx, "Failed to wait for updated pods")
		conditionReason := checksv1alpha1.ReasonCheckRecordingFailed
		if r.conditionType == checksv1alpha1.ConditionTypeBaseline {
			conditionReason = checksv1alpha1.ReasonBaselineRecordingFailed
		}
		r.recorder.Event(
			r.workloadHardeningCheck,
			corev1.EventTypeWarning,
			conditionReason,
			fmt.Sprintf(
				"%sCheck: Failed to wait for updated pods in namespace %s",
				r.checkType,
				targetNamespaceName,
			),
		)
	}

	r.logger.Info("workload is updated and ready for recording")
	r.setStatusRunning(ctx, "Recording metrics")

	startTime := metav1.Now()

	// start recording metrics for target workload
	recordedMetrics, err := r.recordMetrics(ctx)

	if err != nil {
		r.logger.Error(err, "failed to record metrics")
		r.setStatusFailed(ctx, "Failed to record metrics")
		return
	}

	// Record logs for the workload, since recordMetrics only returns after the duration is reached, we can asusme that we get the full logs here
	logs, err := r.recordLogs(ctx, false) // false means we want the current logs, not the previous ones
	if err != nil {
		r.logger.Error(err, "failed to record logs")
		r.setStatusFailed(ctx, "Failed to record logs")
		return
	}

	// If pods are crashLooping, we still want to record the metrics and logs, but we will mark the check as unsuccessful
	r.checkSuccessful, _ = wh.VerifyReadiness(workloadUnderTest, r.Client)

	workloadRecording := recording.WorkloadRecording{
		Type:      r.checkType,
		Success:   r.checkSuccessful,
		StartTime: startTime,
		EndTime:   metav1.Now(),

		RecordedMetrics:               recordedMetrics,
		SecurityContextConfigurations: securityContext,
		Logs:                          logs,
	}

	err = r.valKeyClient.StoreRecording(
		ctx,
		// prefix with original namespace to avoid conflict if suffix is reused
		r.workloadHardeningCheck.GetNamespace()+":"+r.workloadHardeningCheck.Spec.Suffix,
		&workloadRecording,
	)

	if err != nil {
		r.logger.Error(err, "failed to store workload recording in Valkey")
		r.setStatusFailed(ctx, "Failed to store workload recording in Valkey")
		conditionReason := checksv1alpha1.ReasonCheckRecordingFailed
		if r.conditionType == checksv1alpha1.ConditionTypeBaseline {
			conditionReason = checksv1alpha1.ReasonBaselineRecordingFailed
		}
		r.recorder.Event(
			r.workloadHardeningCheck,
			corev1.EventTypeWarning,
			conditionReason,
			fmt.Sprintf(
				"Failed to store %sCheck recording in Valkey",
				r.checkType,
			),
		)
		return
	}

	r.logger.Info("recorded signals")

	if r.checkSuccessful {
		r.setStatusFinishedSuccessfully(ctx, "Check finished successfully", securityContext)
	} else {
		r.setStatusFinishedFailure(ctx, "Signal recording failed", "Workload never became ready", workloadRecording)
	}

	// Cleanup: delete the check namespace after recording
	err = r.deleteCheckNamespace(ctx)
	if err != nil {
		r.logger.Error(err, "failed to delete target namespace after recording")
	} else {
		r.logger.Info("deleted target namespace after recording")
	}
}

// waits for pods to be updated and scheduled to nodes, they don't need to be started/running, as they could be in a crash loop if the security context is too strict
func (r *WorkloadCheckRunner) waitForUpdatedPods(ctx context.Context, workloadUnderTest *client.Object) (bool, error) {
	// We need to ensure that the pods are updated and scheduled to nodes before we can record metrics, they don't need to be started/running, as they could be in a crash loop
	updated := false
	startTime := metav1.Now()
	for !updated {
		r.Get(ctx, types.NamespacedName{Namespace: (*workloadUnderTest).GetNamespace(), Name: (*workloadUnderTest).GetName()}, *workloadUnderTest)
		updated, _ = wh.VerifyUpdated(*workloadUnderTest)

		// Timeout after 2 minutes if the workload is not updated
		if time.Since(startTime.Time) > 2*time.Minute {
			r.logger.Error(fmt.Errorf("timeout while waiting for workload to be updated"), "timeout while waiting for workload to be updated")
			r.setStatusFailed(ctx, "Timeout while waiting for workload to be updated")

			conditionReason := checksv1alpha1.ReasonCheckRecordingFailed
			if r.conditionType == checksv1alpha1.ConditionTypeBaseline {
				conditionReason = checksv1alpha1.ReasonBaselineRecordingFailed
			}

			r.recorder.Event(
				r.workloadHardeningCheck,
				corev1.EventTypeWarning,
				conditionReason,
				fmt.Sprintf(
					"%sCheck: Timeout while waiting for workloads to be updated in namespace %s",
					r.checkType,
					(*workloadUnderTest).GetNamespace(),
				),
			)

			logs, err := r.recordLogs(ctx, true)
			if err != nil {
				r.logger.Error(err, "failed to record logs")
				return false, err
			}

			err = r.valKeyClient.StoreRecording(
				ctx,
				// prefix with original namespace to avoid conflict if suffix is reused
				r.workloadHardeningCheck.GetNamespace()+":"+r.workloadHardeningCheck.Spec.Suffix,
				&recording.WorkloadRecording{
					Type:      r.checkType,
					Success:   false,
					StartTime: startTime,
					EndTime:   metav1.Now(),
					Logs:      logs,
				},
			)
			if err != nil {
				r.logger.Error(err, "failed to store workload recording in Valkey")
			}

			err = r.deleteCheckNamespace(ctx)
			if err != nil {
				r.logger.Error(err, "failed to delete target namespace with failed workload")
			}

			return false, err
		}
		if !updated {
			r.logger.V(2).Info("workload is not updated yet, waiting for it to be ready")
			time.Sleep(5 * time.Second) // Wait for 5 seconds before checking again
		}
	}

	return true, nil
}

func (r *WorkloadCheckRunner) applySecurityContext(ctx context.Context, workloadUnderTest *client.Object, securityContext *checksv1alpha1.SecurityContextDefaults) error {

	originalReplicaCount := int32(0)
	var err error
	// Daemonsets can't be scaled down, so we just apply the security context to them
	if strings.ToLower(r.workloadHardeningCheck.Spec.TargetRef.Kind) != "daemonset" {

		originalReplicaCount, err = r.checkManager.GetReplicaCount(ctx, (*workloadUnderTest).GetNamespace())
		// If there's an error getting the replica count, we log it but continue without scaling down
		if err != nil {
			r.logger.Error(err, "failed to get replica count for workload under test")
		}
		if originalReplicaCount > 0 && err == nil {
			r.logger.V(1).Info("Scaling target workload to 0", "originalReplicaCount", originalReplicaCount)
			r.checkManager.ScaleWorkloadUnderTest(ctx, (*workloadUnderTest).GetNamespace(), 0)
		}

	}

	if securityContext != nil {
		r.logger.V(1).Info("applying security context to workload under test")

		err = retry.RetryOnConflict(retry.DefaultRetry, func() error {

			// Re-fetch the workload under test to ensure we have the latest state
			if err := r.Get(ctx, types.NamespacedName{Namespace: (*workloadUnderTest).GetNamespace(), Name: (*workloadUnderTest).GetName()}, *workloadUnderTest); err != nil {
				if apierrors.IsNotFound(err) {
					// workloadHardeningCheck resource was deleted, while a check was running
					r.logger.Info("WorkloadHardeningCheck not found, skipping check run update")
					return err // If the resource is not found, we can skip the update
				}
				r.logger.Error(err, "Failed to re-fetch WorkloadHardeningCheck")
				return fmt.Errorf("failed to re-fetch WorkloadHardeningCheck: %w", err)

			}

			err := wh.ApplyCheckSecurityContext(ctx, workloadUnderTest, securityContext.Container, securityContext.Pod)
			if err != nil {
				r.logger.Error(err, "failed to apply security context to workload under test")

				return fmt.Errorf("failed to apply security context to workload under test: %w", err)
			}

			return r.Update(ctx, *workloadUnderTest)

		})

		if err != nil {
			r.logger.Error(err, "failed to update workload under test with security context")
			return err
		}

		r.logger.V(2).Info("applied security context to workload under test")
	}

	// Scale the workload under test to the original replica count
	if strings.ToLower(r.workloadHardeningCheck.Spec.TargetRef.Kind) != "daemonset" && originalReplicaCount > 0 {
		r.logger.V(1).Info("scaling workload to original replica count", "replicaCount", originalReplicaCount)
		err = r.checkManager.ScaleWorkloadUnderTest(ctx, (*workloadUnderTest).GetNamespace(), originalReplicaCount)
		if err != nil {
			r.logger.Error(err, "failed to scale workload under test to original replica count")
			return err
		}
	}

	return nil
}

func (r *WorkloadCheckRunner) recordMetrics(ctx context.Context) ([]recording.ResourceUsageRecord, error) {

	targetNamespace := r.generateTargetNamespaceName()

	labelSelector, err := r.checkManager.GetLabelSelector(ctx)
	if err != nil {
		return nil, err
	}

	time.Sleep(2 * time.Second) // Give the workload some time to be ready with the updated security context

	// get pods under observation, we use the label selector from the workload under test
	pods := &corev1.PodList{}
PodsAssigned:
	for len(pods.Items) == 0 {
		err = r.List(
			ctx,
			pods,
			&client.ListOptions{
				Namespace:     targetNamespace,
				LabelSelector: labelSelector,
			},
		)
		if err != nil {
			r.logger.Error(err, "error fetching pods")
			return nil, err
		}

		// If the pods aren't assigned to a node, we cannot record metrics
		if len(pods.Items) > 0 {
			allAssigned := true
			for _, pod := range pods.Items {
				if pod.Spec.NodeName == "" {
					allAssigned = false
					break
				}
			}
			if allAssigned {
				break PodsAssigned // All pods are assigned to a node, we can proceed
			}

			pods = &corev1.PodList{} // reset podList to retry fetching
			r.logger.Info("Pods are not assigned to a node yet, retrying")
			time.Sleep(1 * time.Second) // Wait for 1 second before retrying
		}
	}

	// Filter pods not belonging to the latest generation
	podsToRecord := []corev1.Pod{}
	podNames := make(map[string]bool)
	for _, pod := range pods.Items {
		r.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}, &pod)

		if pod.Status.Phase != corev1.PodRunning && pod.Status.Phase != corev1.PodPending {
			continue
		}
		if pod.DeletionTimestamp != nil {
			// Pod is being deleted, skip it
			continue
		}

		podsToRecord = append(podsToRecord, pod)
		podNames[pod.Name] = true
	}

	r.logger.Info(
		"fetched pods matching workload",
		"numberOfPods", len(podsToRecord),
		"podsToRecord", slices.Collect(maps.Keys(podNames)),
	)

	var wg sync.WaitGroup
	wg.Add(len(podsToRecord))

	// Initialize the channels with the expected capacity to avoid blocking
	metricsChannel := make(chan *recording.RecordedMetrics, len(podsToRecord))

	checkDurationSeconds := int(r.checkManager.GetCheckDuration().Seconds())

	for _, pod := range podsToRecord {
		go func() {
			// the metrics are collected per pod
			recordedMetrics, err := RecordMetrics(ctx, &pod, checkDurationSeconds, 15)
			if err != nil {
				r.logger.Error(err, "failed recording metrics")
			} else {
				r.logger.Info(
					"recorded metrics",
					"podName", pod.Name,
				)
			}

			metricsChannel <- recordedMetrics

			wg.Done()

		}()
	}

	wg.Wait()

	// close channels so that the range loops will stop
	close(metricsChannel)

	resourceUsageRecords := []recording.ResourceUsageRecord{}
	for result := range metricsChannel {
		for _, usage := range result.Usage {
			resourceUsageRecords = append(resourceUsageRecords, usage)
		}
	}
	r.logger.V(1).Info("collected metrics")
	if len(resourceUsageRecords) == 0 {
		r.logger.Info("no resource usage metrics recoded, this could be due to the workload not being ready or not having any resource usage metrics available")
	}

	return resourceUsageRecords, nil
}

func (r *WorkloadCheckRunner) recordLogs(ctx context.Context, previous bool) (map[string][]string, error) {
	targetNamespace := r.generateTargetNamespaceName()

	labelSelector, err := r.checkManager.GetLabelSelector(ctx)
	if err != nil {
		return nil, err
	}

	// get pods under observation, we use the label selector from the workload under test
	pods := &corev1.PodList{}
PodsAssigned:
	for len(pods.Items) == 0 {
		err = r.List(
			ctx,
			pods,
			&client.ListOptions{
				Namespace:     targetNamespace,
				LabelSelector: labelSelector,
			},
		)
		if err != nil {
			r.logger.Error(err, "error fetching pods")
			return nil, err
		}

		// If the pods aren't assigned to a node, we cannot record metrics
		if len(pods.Items) > 0 {
			allAssigned := true
			for _, pod := range pods.Items {
				if pod.Spec.NodeName == "" {
					allAssigned = false
					break
				}
			}
			if allAssigned {
				break PodsAssigned // All pods are assigned to a node, we can proceed
			}

			pods = &corev1.PodList{} // reset podList to retry fetching
			r.logger.Info("Pods are not assigned to a node yet, retrying")
			time.Sleep(1 * time.Second) // Wait for 1 second before retrying
		}
	}

	r.logger.Info(
		"fetched pods matching workload under",
		"numberOfPods", len(pods.Items),
	)

	var wg sync.WaitGroup
	wg.Add(len(pods.Items))

	logMap := make(map[string][]string, len(pods.Items)*(len(pods.Items[0].Spec.Containers)+len(pods.Items[0].Spec.InitContainers)))
	mapMutex := &sync.Mutex{}

	for _, pod := range pods.Items {
		go func() {

			// The logs are collected per container in the pod
			for _, container := range pod.Spec.Containers {

				containerLog, err := GetLogs(ctx, &pod, container.Name, previous)
				if err != nil {
					r.logger.Error(
						err,
						"error fetching logs",
						"podName", pod.Name,
						"containerName", container.Name,
					)
					continue
				}

				r.logger.V(1).Info(
					"fetched logs",
					"podName", pod.Name,
					"containerName", container.Name,
				)

				// Use a mutex to safely write to the map from multiple goroutines
				mapMutex.Lock()
				logMap[container.Name] = append(logMap[container.Name], strings.Split(containerLog, "\n")...)
				mapMutex.Unlock()

			}

			for _, container := range pod.Spec.InitContainers {

				containerLog, err := GetLogs(ctx, &pod, container.Name, previous)
				if err != nil {
					r.logger.Error(
						err,
						"error fetching logs",
						"podName", pod.Name,
						"containerName", container.Name,
					)
					continue
				}
				r.logger.V(1).Info(
					"fetched logs",
					"podName", pod.Name,
					"containerName", container.Name,
				)

				mapMutex.Lock()
				logMap["init:"+container.Name] = append(logMap["init:"+container.Name], strings.Split(containerLog, "\n")...)
				mapMutex.Unlock()

			}

			wg.Done()

		}()
	}

	wg.Wait()

	r.logger.V(1).Info("collected logs")

	return logMap, nil
}
