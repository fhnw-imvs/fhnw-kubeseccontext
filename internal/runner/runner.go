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
	ctrl "sigs.k8s.io/controller-runtime"
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
}

// Required to convert "user" to "User", strings.ToTitle converts each rune to title case not just the first one
var titleCase = cases.Title(language.English)

func NewCheckRunner(ctx context.Context, workloadHardeningCheck *checksv1alpha1.WorkloadHardeningCheck, checkType string) *CheckRunner {

	l := log.FromContext(ctx).WithName("WorkloadHandler")
	config, err := ctrl.GetConfig()
	if err != nil {
		l.Error(err, "Failed to get runtime client config")
	}
	c, err := client.New(config, client.Options{})
	if err != nil {
		l.Error(err, "Failed to create client")
	}

	return &CheckRunner{
		Client:                 c,
		WorkloadHandler:        wh.NewWorkloadHandler(ctx, workloadHardeningCheck),
		workloadHardeningCheck: workloadHardeningCheck,
		checkType:              checkType,
	}
}

func generateTargetNamespaceName(
	workloadHardening checksv1alpha1.WorkloadHardeningCheck,
	checkName string,
) string {
	// create a random namespace name. They are limited to 253 chars in kubernetes, but we make it a bit shorter by default
	// We might add an additional identifier (eg. baseline, runAsNonRoot, etc) later on to make differentiation eaiser for the user
	base := workloadHardening.Namespace
	if len(base) > 200 {
		base = base[:200]
	}
	return fmt.Sprintf("%s-%s-%s", base, workloadHardening.Spec.Suffix, checkName)
}

func (r *CheckRunner) namespaceExists(ctx context.Context, namespaceName string) bool {
	targetNs := &corev1.Namespace{}
	err := r.Get(ctx, client.ObjectKey{Name: namespaceName}, targetNs)

	return !apierrors.IsNotFound(err)
}

func (r *CheckRunner) createCheckNamespace(ctx context.Context, workloadHardening *checksv1alpha1.WorkloadHardeningCheck, targetNamespace string) error {
	log := log.FromContext(ctx)

	err := CloneNamespace(ctx, workloadHardening.Namespace, targetNamespace, workloadHardening.Spec.Suffix)

	if err != nil {
		log.Error(err, fmt.Sprintf("failed to clone namespace %s", workloadHardening.Namespace))
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

func (r *CheckRunner) deleteNamespace(ctx context.Context, namespaceName string) error {
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

func (r *CheckRunner) RunCheck(ctx context.Context, securityContext *checksv1alpha1.SecurityContextDefaults) {
	var conditionType string
	var conditionReason string
	log := log.FromContext(ctx).WithValues("checkType", r.checkType)

	if r.checkType == "baseline" {
		conditionType = checksv1alpha1.ConditionTypeBaseline

	} else {
		conditionType = titleCase.String(r.checkType) + checksv1alpha1.ConditionTypeCheck
	}

	targetNamespaceName := generateTargetNamespaceName(*r.workloadHardeningCheck, r.checkType)

	log = log.WithValues("targetNamespace", targetNamespaceName)

	if r.namespaceExists(ctx, targetNamespaceName) {
		if meta.IsStatusConditionPresentAndEqual(
			r.workloadHardeningCheck.Status.Conditions,
			conditionType,
			metav1.ConditionUnknown,
		) {
			log.V(1).Info(
				"Target namespace already exists, and check is in unknown state. Most likely a previous run failed",
			)
		}

	} else {

		// clone into target namespace
		err := r.createCheckNamespace(ctx, r.workloadHardeningCheck, targetNamespaceName)
		if err != nil {
			log.Error(err, "failed to create target namespace for baseline recording")
			return
		}

		log.Info("created namespace")

		time.Sleep(1 * time.Second) // Give the namespace some time to be fully created and ready
	}

	// Fetch the workload we want to test, make sure we fetch it from the target namespace
	workloadUnderTest, err := r.GetWorkloadUnderTest(ctx)
	if err != nil {
		log.Error(err, "failed to get workload under test",
			"workloadName", r.workloadHardeningCheck.Spec.TargetRef.Name,
		)
		return
	}

	log.Info("applying security context to workload under test",
		"workloadName", r.workloadHardeningCheck.Spec.TargetRef.Name,
	)

	err = wh.ApplySecurityContext(ctx, workloadUnderTest, securityContext.Container, securityContext.Pod)

	err = r.Update(ctx, *workloadUnderTest)
	if err != nil {
		log.Error(err, "failed to update workload under test with security context",
			"workloadName", r.workloadHardeningCheck.Spec.TargetRef.Name,
		)
		if r.checkType == "baseline" {
			conditionReason = checksv1alpha1.ReasonBaselineRecording

		} else {
			conditionReason = titleCase.String(r.checkType) + checksv1alpha1.ReasonCheckRecording
		}

		r.SetCondition(ctx, metav1.Condition{
			Type:    conditionType,
			Status:  metav1.ConditionUnknown,
			Reason:  conditionReason,
			Message: "Failed to apply security context to workload",
		})
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
			if r.checkType == "baseline" {
				conditionReason = checksv1alpha1.ReasonBaselineFailed
			} else {
				conditionReason = titleCase.String(r.checkType) + checksv1alpha1.ReasonCheckRecordingFailed
			}
			r.SetCondition(ctx, metav1.Condition{
				Type:    conditionType,
				Status:  metav1.ConditionTrue,
				Reason:  conditionReason,
				Message: "Timeout while waiting for workload to be running",
			})
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

			//ToDo: Fetch pod events & maybe pod logs, to add context to the failure

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

			err = r.deleteNamespace(ctx, targetNamespaceName)
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

	if r.checkType == "baseline" {
		conditionReason = checksv1alpha1.ReasonBaselineRecording
	} else {
		conditionReason = titleCase.String(r.checkType) + checksv1alpha1.ReasonCheckRecording
	}

	r.SetCondition(ctx, metav1.Condition{
		Type:    conditionType,
		Status:  metav1.ConditionTrue,
		Reason:  conditionReason,
		Message: "Recording signals",
	})

	// start recording metrics for target workload
	workloadRecording, err := r.RecordSignals(ctx, targetNamespaceName, r.checkType, securityContext)

	if err != nil {
		log.Error(err, "failed to record signals")

		if r.checkType == "baseline" {
			conditionReason = checksv1alpha1.ReasonBaselineFailed
		} else {
			conditionReason = titleCase.String(r.checkType) + checksv1alpha1.ReasonCheckRecordingFailed
		}

		r.SetCondition(ctx, metav1.Condition{
			Type:    conditionType,
			Status:  metav1.ConditionTrue,
			Reason:  conditionReason,
			Message: "Failed to record signals",
		})

		return
	}

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

	if r.checkType == "baseline" {
		conditionReason = checksv1alpha1.ReasonBaselineRecorded
	} else {
		conditionReason = titleCase.String(r.checkType) + checksv1alpha1.ReasonCheckRecordingFinished
	}

	err = r.SetCondition(ctx, metav1.Condition{
		Type:    conditionType,
		Status:  metav1.ConditionTrue,
		Reason:  conditionReason,
		Message: "Signals recorded successfully",
	})

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
	err = r.deleteNamespace(ctx, targetNamespaceName)
	if err != nil {
		log.Error(err, "failed to delete target namespace after recording")
	} else {
		log.Info("deleted target namespace after recording")
	}
}

func (r *CheckRunner) RecordSignals(ctx context.Context, targetNamespace, recordingType string, securityContext *checksv1alpha1.SecurityContextDefaults) (*recording.WorkloadRecording, error) {
	log := log.FromContext(ctx).WithValues(
		"targetNamespace", targetNamespace,
		"recordingType", recordingType,
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
		Type:      recordingType,
		Success:   true,
		StartTime: startTime,
		EndTime:   metav1.Now(),

		SecurityContextConfigurations: securityContext,
		RecordedMetrics:               resourceUsageRecords,
		Logs:                          logs,
	}

	return &workloadRecording, nil
}
