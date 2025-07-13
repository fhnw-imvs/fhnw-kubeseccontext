package runner

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	checksv1alpha1 "github.com/fhnw-imvs/fhnw-kubeseccontext/api/v1alpha1"
	"github.com/fhnw-imvs/fhnw-kubeseccontext/internal/recording"
	"github.com/fhnw-imvs/fhnw-kubeseccontext/internal/valkey"
	wh "github.com/fhnw-imvs/fhnw-kubeseccontext/internal/workload"
	"github.com/go-logr/logr"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
	corev1 "k8s.io/api/core/v1"
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
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type CheckRunner struct {
	client.Client
	checkManager *wh.WorkloadCheckManager

	scheme *runtime.Scheme

	valKeyClient *valkey.ValkeyClient
	logger       logr.Logger
	recorder     record.EventRecorder

	workloadHardeningCheck *checksv1alpha1.WorkloadHardeningCheck
	checkType              string
	conditionType          string
}

// Required to convert "user" to "User", strings.ToTitle converts each rune to title case not just the first one
var titleCase = cases.Title(language.English)

func NewCheckRunner(ctx context.Context, valKeyClient *valkey.ValkeyClient, recorder record.EventRecorder, workloadHardeningCheck *checksv1alpha1.WorkloadHardeningCheck, checkType string) *CheckRunner {

	log := log.FromContext(ctx).WithName(checkType + "CheckRunner")

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

	return &CheckRunner{
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

}

// Create the target namespace name. It consists of the base namespace, the suffix set on the workload hardening check, and the check type.
func (r *CheckRunner) generateTargetNamespaceName() string {
	base := r.workloadHardeningCheck.Namespace
	if len(base) > 45 {
		base = base[:45] // Limit the base namespace to 45 characters to ensure the total length does not exceed 63 characters
	}

	// max length: 63
	// suffix length: 8
	// checkType: variable ~10-30 characters => find abreviation... for checkType

	namespaceName := strings.ToLower(fmt.Sprintf("%s-%s-%s", base, r.workloadHardeningCheck.Spec.Suffix, r.checkType))

	if len(namespaceName) > 63 {
		return namespaceName[:63] // Ensure the namespace name does not exceed 63 characters
	}

	return namespaceName
}

func (r *CheckRunner) namespaceExists(ctx context.Context, namespaceName string) bool {
	targetNs := &corev1.Namespace{}
	err := r.Get(ctx, client.ObjectKey{Name: namespaceName}, targetNs)

	return !apierrors.IsNotFound(err)
}

// createCheckNamespace clones the namespace of the workload hardening check target workload into a new namespace.
func (r *CheckRunner) createCheckNamespace(ctx context.Context) error {
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

	// Set the owner reference of the cloned namespace to the workload hardening check
	err = ctrl.SetControllerReference(
		r.workloadHardeningCheck,
		targetNs,
		r.scheme,
	)

	if err != nil {
		r.logger.Error(err, "failed to set controller reference for target namespace")
	}

	return nil

}

func (r *CheckRunner) deleteCheckNamespace(ctx context.Context) error {
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
	return nil
}

func (r *CheckRunner) setStatusRunning(ctx context.Context, message string) {
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

func (r *CheckRunner) setStatusFailed(ctx context.Context, message string) {
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

func (r *CheckRunner) setStatusFinished(ctx context.Context, message string, securityContext *checksv1alpha1.SecurityContextDefaults) {
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
		log.FromContext(ctx).Error(err, "Failed to update WorkloadHardeningCheck status after recording finished")
		r.recorder.Event(
			r.workloadHardeningCheck,
			corev1.EventTypeWarning,
			conditionReason,
			fmt.Sprintf("Failed to update WorkloadHardeningCheck status after recording finished: %s", err.Error()),
		)
		return
	}
}

func (r *CheckRunner) RunCheck(ctx context.Context, securityContext *checksv1alpha1.SecurityContextDefaults) {
	var conditionReason string

	targetNamespaceName := r.generateTargetNamespaceName()

	log := r.logger.WithValues(
		"targetNamespace", targetNamespaceName,
		"checkType", r.checkType,
		"workloadName", r.workloadHardeningCheck.Spec.TargetRef.Name,
	)

	// Check if the target namespace already exists, otherwise create it
	if r.namespaceExists(ctx, targetNamespaceName) {
		if meta.IsStatusConditionPresentAndEqual(
			r.workloadHardeningCheck.Status.Conditions,
			r.conditionType,
			metav1.ConditionUnknown,
		) {
			log.Info(
				"Target namespace already exists, and check is in unknown state indicating a previous run was not finished",
			)

			r.deleteCheckNamespace(ctx)
		}

		// ToDo: Should we just delete it, and wait for it to be gone, and then create it again?

	}

	if !r.namespaceExists(ctx, targetNamespaceName) {

		// clone into target namespace
		err := r.createCheckNamespace(ctx)
		if err != nil {
			log.Error(err, "failed to create target namespace for baseline recording")
			return
		}

		log.Info("created namespace")

		time.Sleep(1 * time.Second) // Give the namespace some time to be fully created and ready
	}

	// Set condition to false, as we are about to start the check
	r.setStatusRunning(ctx, "Start target workload with updated security context")

	// Fetch the workload we want to test, make sure we fetch it from the target namespace
	workloadUnderTest, err := r.checkManager.GetWorkloadUnderTest(ctx, targetNamespaceName)
	if err != nil {
		log.Error(err, "failed to get workload under test")
		r.setStatusFailed(ctx, "Failed to get workload under test")
		return
	}

	if securityContext != nil {
		log.V(1).Info("applying security context to workload under test")
		err = wh.ApplySecurityContext(ctx, workloadUnderTest, securityContext.Container, securityContext.Pod)
		if err != nil {
			log.Error(err, "failed to apply security context to workload under test")
			r.setStatusFailed(ctx, "Failed to apply security context to workload")

			return
		}
	}

	err = r.Update(ctx, *workloadUnderTest)
	if err != nil {
		log.Error(err, "failed to update workload under test with security context")
		r.setStatusFailed(ctx, "Failed to update security context on workload")

		return
	}
	log.V(2).Info("applied security context to workload under test")

	// ToDo: detect if pods are able to get into a running state, otherwise we cannot record metrics and it's clear this has failed
	running := false
	startTime := metav1.Now()
	for !running {
		r.Get(ctx, types.NamespacedName{Namespace: targetNamespaceName, Name: r.workloadHardeningCheck.Spec.TargetRef.Name}, *workloadUnderTest)
		running, _ = wh.VerifySuccessfullyRunning(*workloadUnderTest)

		if time.Now().After(startTime.Add(2 * time.Minute)) {
			log.Error(fmt.Errorf("timeout while waiting for workload to be running"), "timeout while waiting for workload to be running")
			r.setStatusFailed(ctx, "Timeout while waiting for workload to be running")

			r.recorder.Event(
				r.workloadHardeningCheck,
				corev1.EventTypeWarning,
				conditionReason,
				fmt.Sprintf(
					"%sCheck: Timeout while waiting for workloads to be running in namespace %s",
					r.checkType,
					targetNamespaceName,
				),
			)

			logs, err := r.recordLogs(ctx, true)
			if err != nil {
				log.Error(err, "failed to record logs")
				return
			}

			err = r.valKeyClient.StoreRecording(
				ctx,
				// prefix with original namespace to avoid conflict if suffix is reused
				r.workloadHardeningCheck.GetNamespace()+":"+r.workloadHardeningCheck.Spec.Suffix,
				&recording.WorkloadRecording{
					Type:                          r.checkType,
					Success:                       false,
					StartTime:                     startTime,
					EndTime:                       metav1.Now(),
					SecurityContextConfigurations: securityContext,
					Logs:                          logs,
				},
			)
			if err != nil {
				log.Error(err, "failed to store workload recording in Valkey")
			}

			err = r.deleteCheckNamespace(ctx)
			if err != nil {
				log.Error(err, "failed to delete target namespace with failed workload")
			}

			return
		}
		if !running {
			log.V(2).Info("workload is not running yet, waiting for it to be ready")
			time.Sleep(5 * time.Second) // Wait for 5 seconds before checking again
		}
	}

	log.Info("workload is running")

	r.setStatusRunning(ctx, "Recording metrics")

	// start recording metrics for target workload
	recordedMetrics, err := r.recordMetrics(ctx)

	if err != nil {
		log.Error(err, "failed to record metrics")

		r.setStatusFailed(ctx, "Failed to record metrics")

		return
	}

	workloadRecording := recording.WorkloadRecording{
		Type:      r.checkType,
		Success:   true,
		StartTime: startTime,
		EndTime:   metav1.Now(),

		RecordedMetrics: recordedMetrics,
	}

	workloadRecording.SecurityContextConfigurations = securityContext

	// Record logs for the workload
	logs, err := r.recordLogs(ctx, false) // false means we want the current logs, not the previous ones
	if err != nil {
		log.Error(err, "failed to record logs")
		r.setStatusFailed(ctx, "Failed to record logs")
		return
	}
	workloadRecording.Logs = logs

	err = r.valKeyClient.StoreRecording(
		ctx,
		// prefix with original namespace to avoid conflict if suffix is reused
		r.workloadHardeningCheck.GetNamespace()+":"+r.workloadHardeningCheck.Spec.Suffix,
		&workloadRecording,
	)

	if err != nil {
		log.Error(err, "failed to store workload recording in Valkey")
	}

	log.Info("recorded signals")

	r.setStatusFinished(ctx, "Signals recorded successfully", securityContext)

	r.recorder.Event(
		r.workloadHardeningCheck,
		corev1.EventTypeNormal,
		conditionReason,
		fmt.Sprintf(
			"Recorded %sCheck in namespace %s",
			r.checkType,
			targetNamespaceName,
		),
	)

	// Cleanup: delete the check namespace after recording
	err = r.deleteCheckNamespace(ctx)
	if err != nil {
		log.Error(err, "failed to delete target namespace after recording")
	} else {
		log.Info("deleted target namespace after recording")
	}
}

func (r *CheckRunner) recordMetrics(ctx context.Context) ([]recording.ResourceUsageRecord, error) {
	targetNamespace := r.generateTargetNamespaceName()
	log := r.logger.WithValues(
		"targetNamespace", targetNamespace,
		"checkType", r.checkType,
		"workloadName", r.workloadHardeningCheck.Spec.TargetRef.Name,
	)

	labelSelector, err := r.checkManager.GetLabelSelector(ctx)
	if err != nil {
		return nil, err
	}

	time.Sleep(1 * time.Second) // Give the workload some time to be ready with the updated security context

	// get pods under observation, we use the label selector from the workload under test
	pods := &corev1.PodList{}
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
			log.Error(err, "error fetching pods")
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
				break // All pods are assigned to a node, we can proceed
			}
			pods = &corev1.PodList{} // reset podList to retry fetching
			log.Info("Pods are not assigned to a node yet, retrying")
		}
	}

	log.Info(
		"fetched pods matching workload under",
		"numberOfPods", len(pods.Items),
	)

	var wg sync.WaitGroup
	wg.Add(len(pods.Items))

	// Initialize the channels with the expected capacity to avoid blocking
	metricsChannel := make(chan *recording.RecordedMetrics, len(pods.Items))

	checkDurationSeconds := int(r.checkManager.GetCheckDuration().Seconds())

	for _, pod := range pods.Items {
		go func() {
			// the metrics are collected per pod
			recordedMetrics, err := RecordMetrics(ctx, &pod, checkDurationSeconds, 15)
			if err != nil {
				log.Error(err, "failed recording metrics")
			} else {
				log.Info(
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
	log.V(1).Info("collected metrics")
	if len(resourceUsageRecords) == 0 {
		log.Info("no resource usage records found, nothing to store")
	}

	return resourceUsageRecords, nil
}

func (r *CheckRunner) recordLogs(ctx context.Context, previous bool) (map[string][]string, error) {
	targetNamespace := r.generateTargetNamespaceName()
	log := r.logger.WithValues(
		"targetNamespace", targetNamespace,
		"checkType", r.checkType,
		"workloadName", r.workloadHardeningCheck.Spec.TargetRef.Name,
	)

	labelSelector, err := r.checkManager.GetLabelSelector(ctx)
	if err != nil {
		return nil, err
	}

	// get pods under observation, we use the label selector from the workload under test
	// Shouldn't be necessary anymore, as this was mostly relevant to record metrics. We are only recording logs after the metrics have been recorded, so the pods should already be running.
	// ToDo: Cleanup, only fetch the pods and then fetch their logs
	pods := &corev1.PodList{}
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
			log.Error(err, "error fetching pods")
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
				break // All pods are assigned to a node, we can proceed
			}
			pods = &corev1.PodList{} // reset podList to retry fetching
			log.Info("Pods are not assigned to a node yet, retrying")
		}
	}

	log.Info(
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
					log.Error(
						err,
						"error fetching logs",
						"podName", pod.Name,
						"containerName", container.Name,
					)
					continue
				}

				log.V(1).Info(
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
					log.Error(
						err,
						"error fetching logs",
						"podName", pod.Name,
						"containerName", container.Name,
					)
					continue
				}
				log.V(1).Info(
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

	log.V(1).Info("collected logs")

	return logMap, nil
}
