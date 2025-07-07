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
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type CheckRunner struct {
	client.Client
	checkHandler *wh.WorkloadCheckHandler

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

	conditionTYpe := titleCase.String(checkType) + checksv1alpha1.ConditionTypeCheck
	if strings.Contains(checkType, "baseline") {
		conditionTYpe = checksv1alpha1.ConditionTypeBaseline
	}

	scheme := runtime.NewScheme()

	clientgoscheme.AddToScheme(scheme)
	checksv1alpha1.AddToScheme(scheme)

	cfg, err := ctrl.GetConfig()
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to get Kubernetes config")
		return nil
	}

	cl, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to create Kubernetes client")
		return nil
	}

	return &CheckRunner{
		Client:                 cl,
		checkHandler:           wh.NewWorkloadCheckHandler(ctx, valKeyClient, workloadHardeningCheck),
		logger:                 log.FromContext(ctx).WithName("WorkloadHandler"),
		valKeyClient:           valKeyClient,
		recorder:               recorder,
		workloadHardeningCheck: workloadHardeningCheck,
		checkType:              checkType,
		conditionType:          conditionTYpe,
		scheme:                 scheme,
	}

}

// Create the target namespace name. It consists of the base namespace, the suffix set on the workload hardening check, and the check type.
func (r *CheckRunner) generateTargetNamespaceName() string {
	base := r.workloadHardeningCheck.Namespace
	if len(base) > 200 {
		base = base[:200]
	}

	return strings.ToLower(fmt.Sprintf("%s-%s-%s", base, r.workloadHardeningCheck.Spec.Suffix, r.checkType))
}

func (r *CheckRunner) namespaceExists(ctx context.Context, namespaceName string) bool {
	targetNs := &corev1.Namespace{}
	err := r.Get(ctx, client.ObjectKey{Name: namespaceName}, targetNs)

	return !apierrors.IsNotFound(err)
}

// createCheckNamespace clones the namespace of the workload hardening check target workload into a new namespace.
func (r *CheckRunner) createCheckNamespace(ctx context.Context) error {
	log := log.FromContext(ctx)

	targetNamespace := r.generateTargetNamespaceName()

	err := CloneNamespace(ctx, r.workloadHardeningCheck.Namespace, targetNamespace, r.workloadHardeningCheck.Spec.Suffix)

	if err != nil {
		log.Error(err, fmt.Sprintf("failed to clone namespace %s", r.workloadHardeningCheck.Namespace))
		return err
	}

	targetNs := &corev1.Namespace{}
	err = r.Get(ctx, client.ObjectKey{Name: targetNamespace}, targetNs)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Error(err, "target namespace not found after cloning")
			return fmt.Errorf("target namespace %s not found after cloning", targetNamespace)
		}
		// Error reading the object - requeue the request.
		log.Error(err, "failed to get target namespace after cloning, requeuing")
		return fmt.Errorf("failed to get target namespace %s after cloning: %w", targetNamespace, err)
	}

	// ToDo: Reactivate!
	// Set the owner reference of the cloned namespace to the workload hardening check
	err = ctrl.SetControllerReference(
		r.workloadHardeningCheck,
		targetNs,
		r.scheme,
	)

	if err != nil {
		log.Error(err, "failed to set controller reference for target namespace")
	}

	return nil

}

func (r *CheckRunner) deleteCheckNamespace(ctx context.Context) error {
	namespaceName := r.generateTargetNamespaceName()
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

func (r *CheckRunner) setStatusFailed(ctx context.Context, message string) {
	conditionReason := titleCase.String(r.checkType) + checksv1alpha1.ReasonCheckRecordingFailed
	if strings.Contains(r.checkType, "baseline") {
		conditionReason = checksv1alpha1.ReasonBaselineRecordingFailed
	}

	r.checkHandler.SetCondition(ctx, metav1.Condition{
		Type:    r.conditionType,
		Status:  metav1.ConditionUnknown,
		Reason:  conditionReason,
		Message: message,
	})

}

func (r *CheckRunner) setStatusFinished(ctx context.Context, message string, securityContext *checksv1alpha1.SecurityContextDefaults) {
	conditionReason := titleCase.String(r.checkType) + checksv1alpha1.ReasonCheckRecordingFinished
	if strings.Contains(r.checkType, "baseline") {
		conditionReason = checksv1alpha1.ReasonBaselineRecordingFinished
	}

	r.checkHandler.SetCondition(ctx, metav1.Condition{
		Type:    r.conditionType,
		Status:  metav1.ConditionTrue,
		Reason:  conditionReason,
		Message: message,
	})

	retryCount := 0
	// retry 3 times to update the status of the WorkloadHardeningCheck, to avoid concurrent updates failing
	for retryCount < 3 {
		// Let's re-fetch the workload hardening check Custom Resource after updating the status so that we have the latest state
		if err := r.Get(ctx, types.NamespacedName{Name: r.workloadHardeningCheck.Name, Namespace: r.workloadHardeningCheck.Namespace}, r.workloadHardeningCheck); err != nil {
			log.FromContext(ctx).Error(err, "Failed to re-fetch WorkloadHardeningCheck")
		}

		// Set/Update condition
		r.workloadHardeningCheck.Status.CheckRuns = append(r.workloadHardeningCheck.Status.CheckRuns, checksv1alpha1.CheckRun{
			Name:                 r.checkType,
			RecordingSuccessfull: ptr.To(true),
			SecurityContext:      securityContext,
		})

		if err := r.Status().Update(ctx, r.workloadHardeningCheck); err != nil {
			log.FromContext(ctx).Error(err, "failed to add check run to workload hardening check status",
				"checkType", r.checkType,
				"namespace", r.workloadHardeningCheck.Namespace,
				"name", r.workloadHardeningCheck.Name,
			)
			retryCount++
			continue // Retry updating the status
		} else {
			break
		}

	}
}

func (r *CheckRunner) setStatusRunning(ctx context.Context, message string) {
	conditionReason := titleCase.String(r.checkType) + checksv1alpha1.ReasonCheckRecording
	if strings.Contains(r.checkType, "baseline") {
		conditionReason = checksv1alpha1.ReasonBaselineRecording
	}

	r.checkHandler.SetCondition(ctx, metav1.Condition{
		Type:    r.conditionType,
		Status:  metav1.ConditionFalse,
		Reason:  conditionReason,
		Message: message,
	})
}

func (r *CheckRunner) RunCheck(ctx context.Context, securityContext *checksv1alpha1.SecurityContextDefaults) {
	var conditionReason string
	log := log.FromContext(ctx).WithValues("checkType", r.checkType)

	targetNamespaceName := r.generateTargetNamespaceName()

	log = log.WithValues("targetNamespace", targetNamespaceName)

	// Check if the target namespace already exists, otherwise create it
	if r.namespaceExists(ctx, targetNamespaceName) {
		if meta.IsStatusConditionPresentAndEqual(
			r.workloadHardeningCheck.Status.Conditions,
			r.conditionType,
			metav1.ConditionUnknown,
		) {
			log.V(1).Info(
				"Target namespace already exists, and check is in unknown state. Most likely a previous run failed",
			)
		}

	} else {

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
	r.setStatusRunning(ctx, "Starting recording signals")

	// Fetch the workload we want to test, make sure we fetch it from the target namespace
	workloadUnderTest, err := r.checkHandler.GetWorkloadUnderTest(ctx, targetNamespaceName)
	if err != nil {
		log.Error(err, "failed to get workload under test",
			"workloadName", r.workloadHardeningCheck.Spec.TargetRef.Name,
		)
		return
	}

	log.Info("applying security context to workload under test",
		"workloadName", r.workloadHardeningCheck.Spec.TargetRef.Name,
	)

	if securityContext != nil {
		err = wh.ApplySecurityContext(ctx, workloadUnderTest, securityContext.Container, securityContext.Pod)
		if err != nil {
			log.Error(err, "failed to apply security context to workload under test",
				"workloadName", r.workloadHardeningCheck.Spec.TargetRef.Name,
			)
			r.setStatusFailed(ctx, "Failed to apply security context to workload")

			return
		}
	}

	err = r.Update(ctx, *workloadUnderTest)
	if err != nil {
		log.Error(err, "failed to update workload under test with security context",
			"workloadName", r.workloadHardeningCheck.Spec.TargetRef.Name,
		)
		r.setStatusFailed(ctx, "Failed to update security context on workload")

		return
	}
	log.Info("applied security context to workload under test",
		"workloadName", r.workloadHardeningCheck.Spec.TargetRef.Name,
	)

	// ToDo: detect if pods are able to get into a running state, otherwise we cannot record metrics and it's clear this has failed
	running := false
	startTime := metav1.Now()
	for !running {
		r.Get(ctx, types.NamespacedName{Namespace: targetNamespaceName, Name: r.workloadHardeningCheck.Spec.TargetRef.Name}, *workloadUnderTest)
		running, _ = wh.VerifySuccessfullyRunning(*workloadUnderTest)

		if time.Now().After(startTime.Add(2 * time.Minute)) {
			log.Error(fmt.Errorf("timeout while waiting for workload to be running"), "timeout while waiting for workload to be running",
				"targetNamespace", targetNamespaceName,
				"checkType", r.checkType,
				"workloadName", r.workloadHardeningCheck.Spec.TargetRef.Name,
			)
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

			//ToDo: Fetch pod events & pod logs, to add context to the failure

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
			log.V(1).Info("workload is not running yet, waiting for it to be ready",
				"workloadName", r.workloadHardeningCheck.Spec.TargetRef.Name,
			)
			time.Sleep(5 * time.Second) // Wait for 5 seconds before checking again
		}
	}

	log.Info("workload is running",
		"workloadName", r.workloadHardeningCheck.Spec.TargetRef.Name,
	)

	r.setStatusRunning(ctx, "Recording signals")

	// start recording metrics for target workload
	recordedMetrics, err := r.RecordMetrics(ctx)

	if err != nil {
		log.Error(err, "failed to record signals")

		r.setStatusFailed(ctx, "Failed to record signals")

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
	logs, err := r.RecordLogs(ctx)
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
			"Recorded %sCheck signals in namespace %s",
			r.checkType,
			targetNamespaceName,
		),
	)

	if err != nil {
		log.Error(err, "failed to set condition for check")
	}

	// Cleanup: delete the check namespace after recording
	err = r.deleteCheckNamespace(ctx)
	if err != nil {
		log.Error(err, "failed to delete target namespace after recording")
	} else {
		log.Info("deleted target namespace after recording")
	}
}

func (r *CheckRunner) RecordMetrics(ctx context.Context) ([]recording.ResourceUsageRecord, error) {
	targetNamespace := r.generateTargetNamespaceName()
	log := log.FromContext(ctx).WithValues(
		"targetNamespace", targetNamespace,
		"checkType", r.checkType,
		"workloadName", r.workloadHardeningCheck.Spec.TargetRef.Name,
	)

	labelSelector, err := r.checkHandler.GetLabelSelector(ctx)
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

	checkDurationSeconds := int(r.checkHandler.GetCheckDuration().Seconds())

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

func (r *CheckRunner) RecordLogs(ctx context.Context) (map[string][]string, error) {
	targetNamespace := r.generateTargetNamespaceName()
	log := log.FromContext(ctx).WithValues(
		"targetNamespace", targetNamespace,
		"checkType", r.checkType,
		"workloadName", r.workloadHardeningCheck.Spec.TargetRef.Name,
	)

	labelSelector, err := r.checkHandler.GetLabelSelector(ctx)
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

				containerLog, err := GetLogs(ctx, &pod, container.Name)
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

				containerLog, err := GetLogs(ctx, &pod, container.Name)
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
