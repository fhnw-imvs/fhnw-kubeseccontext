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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type CheckRunner struct {
	client.Client
	*wh.WorkloadHandler

	valKeyClient *valkey.ValkeyClient
	l            logr.Logger
	recorder     record.EventRecorder

	workloadHardeningCheck *checksv1alpha1.WorkloadHardeningCheck
	checkType              string
	conditionType          string
}

// Required to convert "user" to "User", strings.ToTitle converts each rune to title case not just the first one
var titleCase = cases.Title(language.English)

func NewCheckRunner(ctx context.Context, client client.Client, valKeyClient *valkey.ValkeyClient, recorder record.EventRecorder, workloadHardeningCheck *checksv1alpha1.WorkloadHardeningCheck, checkType string) *CheckRunner {

	conditionTYpe := titleCase.String(checkType) + checksv1alpha1.ConditionTypeCheck
	if checkType == "baseline" {
		conditionTYpe = checksv1alpha1.ConditionTypeBaseline
	}

	return &CheckRunner{
		Client:                 client,
		l:                      log.FromContext(ctx).WithName("WorkloadHandler"),
		valKeyClient:           valKeyClient,
		recorder:               recorder,
		WorkloadHandler:        wh.NewWorkloadHandler(ctx, client, workloadHardeningCheck),
		workloadHardeningCheck: workloadHardeningCheck,
		checkType:              checkType,
		conditionType:          conditionTYpe,
	}

}

// Create the target namespace name. It consists of the base namespace, the suffix set on the workload hardening check, and the check type.
func (r *CheckRunner) generateTargetNamespaceName() string {
	base := r.workloadHardeningCheck.Namespace
	if len(base) > 200 {
		base = base[:200]
	}
	return fmt.Sprintf("%s-%s-%s", base, r.workloadHardeningCheck.Spec.Suffix, r.checkType)
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
	// err = ctrl.SetControllerReference(
	// 	workloadHardening,
	// 	targetNs,
	// 	r.Scheme,
	// )

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

func (r *CheckRunner) setConditionFailed(ctx context.Context, message string) {
	conditionReason := titleCase.String(r.checkType) + checksv1alpha1.ReasonCheckRecordingFailed
	if r.checkType == "baseline" {
		conditionReason = checksv1alpha1.ReasonBaselineRecordingFailed
	}

	r.SetCondition(ctx, metav1.Condition{
		Type:    r.conditionType,
		Status:  metav1.ConditionUnknown,
		Reason:  conditionReason,
		Message: message,
	})

}

func (r *CheckRunner) setConditionFinished(ctx context.Context, message string) {
	conditionReason := titleCase.String(r.checkType) + checksv1alpha1.ReasonCheckRecordingFinished
	if r.checkType == "baseline" {
		conditionReason = checksv1alpha1.ReasonBaselineRecordingFinished
	}

	r.SetCondition(ctx, metav1.Condition{
		Type:    r.conditionType,
		Status:  metav1.ConditionTrue,
		Reason:  conditionReason,
		Message: message,
	})
}

func (r *CheckRunner) setConditionRunning(ctx context.Context, message string) {
	conditionReason := titleCase.String(r.checkType) + checksv1alpha1.ReasonCheckRecording
	if r.checkType == "baseline" {
		conditionReason = checksv1alpha1.ReasonBaselineRecording
	}

	r.SetCondition(ctx, metav1.Condition{
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
	r.setConditionRunning(ctx, "Starting recording signals")

	// Fetch the workload we want to test, make sure we fetch it from the target namespace
	workloadUnderTest, err := r.GetWorkloadUnderTest(ctx, targetNamespaceName)
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
			r.setConditionFailed(ctx, "Failed to apply security context to workload")

			return
		}
	}

	err = r.Update(ctx, *workloadUnderTest)
	if err != nil {
		log.Error(err, "failed to update workload under test with security context",
			"workloadName", r.workloadHardeningCheck.Spec.TargetRef.Name,
		)
		r.setConditionFailed(ctx, "Failed to update security context on workload")

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
			r.setConditionFailed(ctx, "Timeout while waiting for workload to be running")

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

	r.setConditionRunning(ctx, "Recording signals")

	// start recording metrics for target workload
	workloadRecording, err := r.RecordSignals(ctx)

	if err != nil {
		log.Error(err, "failed to record signals")

		r.setConditionFailed(ctx, "Failed to record signals")

		return
	}

	workloadRecording.SecurityContextConfigurations = securityContext

	err = r.valKeyClient.StoreRecording(
		ctx,
		// prefix with original namespace to avoid conflict if suffix is reused
		r.workloadHardeningCheck.GetNamespace()+":"+r.workloadHardeningCheck.Spec.Suffix,
		workloadRecording,
	)

	if err != nil {
		log.Error(err, "failed to store workload recording in Valkey")
	}

	log.Info("recorded signals")

	r.setConditionFinished(ctx, "Signals recorded successfully")

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

func (r *CheckRunner) RecordSignals(ctx context.Context) (*recording.WorkloadRecording, error) {
	targetNamespace := r.generateTargetNamespaceName()
	log := log.FromContext(ctx).WithValues(
		"targetNamespace", targetNamespace,
		"checkType", r.checkType,
		"workloadName", r.workloadHardeningCheck.Spec.TargetRef.Name,
	)

	labelSelector, err := r.GetLabelSelector(ctx)
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
	// As we collect logs per container, we need to multiply the number of pods with the number of containers in each pod
	logsChannel := make(chan string, len(pods.Items)*len(pods.Items[0].Spec.Containers))

	startTime := metav1.Now()

	checkDurationSeconds := int(r.workloadHardeningCheck.GetCheckDuration().Seconds())

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

			// The logs are collected per container in the pod
			for _, container := range pod.Spec.Containers {

				logs, err := GetLogs(ctx, &pod, container.Name)
				if err != nil {
					log.Error(
						err,
						"error fetching logs",
						"podName", pod.Name,
						"containerName", container.Name,
					)
				} else {
					log.V(1).Info(
						"fetched logs",
						"podName", pod.Name,
						"containerName", container.Name,
					)
				}
				logsChannel <- logs
			}

			wg.Done()

		}()
	}

	wg.Wait()

	// close channels so that the range loops will stop
	close(metricsChannel)
	close(logsChannel)

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

	logs := []string{}
	for podLogs := range logsChannel {
		logs = append(logs, strings.Split(podLogs, "\n")...)
	}
	log.V(1).Info("collected logs")

	workloadRecording := recording.WorkloadRecording{
		Type:      r.checkType,
		Success:   true,
		StartTime: startTime,
		EndTime:   metav1.Now(),

		RecordedMetrics: resourceUsageRecords,
		Logs:            logs,
	}

	return &workloadRecording, nil
}
