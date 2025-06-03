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
	"fmt"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	checksv1alpha1 "github.com/fhnw-imvs/fhnw-kubeseccontext/api/v1alpha1"
	"github.com/fhnw-imvs/fhnw-kubeseccontext/internal/runner"
)

// Definitions to manage status conditions
const (
	// Represents the initial state, before the baseline recording is started
	typeWorkloadCheckStartup = "Preparation"
	// Represents a running baseline recording
	typeWorkloadCheckBaseline = "Baseline"
	// Represents ongoing check jobs, this state will be used until all checks are finished
	typeWorkloadCheckRunning = "Running"
	// Represents a finished check. Now the Status should contain the report
	typeWorkloadCheckFinished = "Finished"
)

// WorkloadHardeningCheckReconciler reconciles a WorkloadHardeningCheck object
type WorkloadHardeningCheckReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=checks.funk.fhnw.ch,resources=workloadhardeningchecks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=checks.funk.fhnw.ch,resources=workloadhardeningchecks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=checks.funk.fhnw.ch,resources=workloadhardeningchecks/finalizers,verbs=update
// +kubebuilder:rbac:groups=*,resources=*,verbs=*

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the WorkloadHardeningCheck object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
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
			log.Info("WorkloadHardeningCheck not found. Ignoring since object must be deleted")
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

	ready, err := r.verifyWorkloadTargetRef(ctx, workloadHardening)
	if err != nil {
		log.Info("failed to verify workload")
		return ctrl.Result{}, err
	}

	if !ready {
		log.Info("targetRef not in ready state. Rescheduling...")
		// Should the requeue interval be configurable?
		// Lower is better for development, but can cause high load in production
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	log.Info("targetRef ready. Starting baseline recording")

	baselineNamespaceName := generateTargetNamespaceName(*workloadHardening, "baseline")

	if !r.namespaceExists(ctx, baselineNamespaceName) {
		// clone into baseline namespace
		// Add check if we already created the baseline...
		baselineNamespaceName, err = r.createBaselineNamespace(ctx, workloadHardening)
		if err != nil {
			if strings.Contains(err.Error(), "target namespace already exists") {
				return ctrl.Result{}, nil
			}
			return ctrl.Result{}, err
		}

		log.Info("created baseline namespace", "baselineNamespace", baselineNamespaceName)
	}

	// start recording metrics for target workload
	err = r.recordSignals(ctx, workloadHardening, baselineNamespaceName)
	if err != nil {
		return ctrl.Result{}, err
	}

	log.Info("recorded baseline signals", "baselineNamespace", baselineNamespaceName)

	// Create hardening job for baseline recording

	return ctrl.Result{}, nil
}

func (r *WorkloadHardeningCheckReconciler) namespaceExists(ctx context.Context, namespaceName string) bool {
	targetNs := &corev1.Namespace{}
	err := r.Get(ctx, client.ObjectKey{Name: namespaceName}, targetNs)

	return !apierrors.IsNotFound(err)
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
	return fmt.Sprintf("%s-%s-%s", base, workloadHardening.Status.Suffix, checkName)
}

func (r *WorkloadHardeningCheckReconciler) recordSignals(ctx context.Context, workloadHardening *checksv1alpha1.WorkloadHardeningCheck, targetNamespace string) error {
	log := log.FromContext(ctx)

	err := r.setCondition(ctx, workloadHardening, metav1.Condition{
		Type:    typeWorkloadCheckBaseline,
		Status:  metav1.ConditionTrue,
		Reason:  "BaselineRecording",
		Message: "Recording baseline signals",
	})
	if err != nil {
		return err
	}

	r.Recorder.Event(
		workloadHardening,
		corev1.EventTypeNormal,
		"BaselineRecording",
		fmt.Sprintf(
			"Recording baseline signals in %s for %s/%s",
			targetNamespace,
			workloadHardening.Spec.TargetRef.Kind,
			workloadHardening.Spec.TargetRef.Name,
		),
	)

	labelSelector, err := r.getLabelSelector(ctx, workloadHardening)
	if err != nil {
		return err
	}

	// get pods under observation. They need to be managed by the targetRef workload
	pods := &corev1.PodList{}
	err = r.List(ctx, pods, &client.ListOptions{Namespace: targetNamespace, LabelSelector: labelSelector})
	if err != nil {
		log.Error(err, "error fetching pods")
		return err
	}

	log.Info(
		fmt.Sprintf("fetched pods matching workload under test in target namespace %s", targetNamespace),
		"numberOfPods", len(pods.Items),
	)

	startTime := time.Now()

	var wg sync.WaitGroup
	wg.Add(len(pods.Items))
	duration, _ := time.ParseDuration(workloadHardening.Spec.BaselineDuration)

	metricsChannel := make(chan *runner.RecordedMetrics, len(pods.Items))
	logsChannel := make(chan string, len(pods.Items))

	for _, pod := range pods.Items {
		go func() {
			recordedMetrics, err := runner.RecordMetrics(ctx, &pod, int(duration.Seconds()), 15)
			if err != nil {
				log.Error(err, "failed recording metrics")
			} else {
				log.Info(
					"recorded metrics",
					"pod", pod.Name,
					"values", recordedMetrics.Usage,
				)
			}

			logs, err := runner.GetLogs(ctx, &pod)
			if err != nil {
				log.Error(
					err,
					"error fetching logs",
					"pod.name", pod.Name,
				)
			} else {
				log.V(1).Info("fetched logs")
			}

			metricsChannel <- recordedMetrics
			logsChannel <- logs

			wg.Done()

		}()
	}

	wg.Wait()

	// close channels so that the range loops will stop
	close(metricsChannel)
	close(logsChannel)

	endTime := time.Now()

	resourceUsageRecords := []checksv1alpha1.ResourceUsageRecord{}
	for result := range metricsChannel {
		for _, usage := range result.Usage {
			resourceUsageRecords = append(resourceUsageRecords, checksv1alpha1.ResourceUsageRecord{
				Time:   metav1.NewTime(usage.Time),
				CPU:    int64(usage.CPU),
				Memory: int64(usage.Memory),
			})
		}
	}
	log.V(2).Info("collected metrics")

	logs := []string{}
	for podLogs := range logsChannel {
		logs = append(logs, strings.Split(podLogs, "\n")...)
	}
	log.V(2).Info("collected logs")

	// ToDo: Why?!? setCondition already updates it, and it acting on a pointer, shouldn't this also update the ref we have in here?
	if err := r.Get(ctx, types.NamespacedName{Name: workloadHardening.Name, Namespace: workloadHardening.Namespace}, workloadHardening); err != nil {
		log.Error(err, "Failed to re-fetch WorkloadHardeningCheck")
		return err
	}

	meta.SetStatusCondition(
		&workloadHardening.Status.Conditions,
		metav1.Condition{
			Type:    typeWorkloadCheckBaseline,
			Status:  metav1.ConditionTrue,
			Reason:  "BaselineRecorded",
			Message: "Basline signals recorded",
		},
	)

	workloadHardening.Status.BaselineRecording = &checksv1alpha1.WorkloadRecording{
		Type:            "Baseline",
		Success:         true,
		StartTime:       metav1.NewTime(startTime),
		EndTime:         metav1.NewTime(endTime),
		RecordedMetrics: resourceUsageRecords,
		Logs:            logs,
	}

	if err := r.Status().Update(ctx, workloadHardening); err != nil {
		log.Error(err, "Failed to update WorkloadHardeningCheck status")
		return err
	}

	return nil
}

func (r *WorkloadHardeningCheckReconciler) getLabelSelector(ctx context.Context, workloadHardening *checksv1alpha1.WorkloadHardeningCheck) (labels.Selector, error) {
	workloadUnderTest, err := r.getWorkloadUnderTest(ctx, workloadHardening)
	if err != nil {
		return nil, err
	}

	var labelSelector *metav1.LabelSelector

	switch v := (*workloadUnderTest).(type) {
	case *appsv1.Deployment:
		labelSelector = v.Spec.Selector
	case *appsv1.StatefulSet:
		labelSelector = v.Spec.Selector
	case *appsv1.DaemonSet:
		labelSelector = v.Spec.Selector
	}

	return metav1.LabelSelectorAsSelector(labelSelector)
}

func (r *WorkloadHardeningCheckReconciler) createBaselineNamespace(ctx context.Context, workloadHardening *checksv1alpha1.WorkloadHardeningCheck) (string, error) {
	log := log.FromContext(ctx)

	err := r.setCondition(ctx, workloadHardening, metav1.Condition{
		Type:    typeWorkloadCheckStartup,
		Status:  metav1.ConditionTrue,
		Reason:  "CloningNamespace",
		Message: "Cloning into baseline namespace",
	})
	if err != nil {
		return "", err
	}

	targetNamespace := generateTargetNamespaceName(*workloadHardening, "baseline")

	r.Recorder.Event(
		workloadHardening,
		corev1.EventTypeNormal,
		"BaselineCreate",
		fmt.Sprintf("Cloning source namespace %s into baseline namespace %s", workloadHardening.Namespace, targetNamespace),
	)

	err = runner.CloneNamespace(ctx, workloadHardening.Namespace, targetNamespace)

	if err != nil {
		log.Error(err, fmt.Sprintf("failed to clone namespace %s", workloadHardening.Namespace))
		return "", err
	}

	return targetNamespace, r.setCondition(ctx, workloadHardening, metav1.Condition{
		Type:    typeWorkloadCheckStartup,
		Status:  metav1.ConditionTrue,
		Reason:  "NamespaceCloned",
		Message: "Cloned baseline namespace",
	})

}

func (r *WorkloadHardeningCheckReconciler) getWorkloadUnderTest(ctx context.Context,
	workloadHardening *checksv1alpha1.WorkloadHardeningCheck,
) (*client.Object, error) {
	log := log.FromContext(ctx)

	// Verify namespace contains target workload
	var workloadUnderTest client.Object
	switch kind := strings.ToLower(workloadHardening.Spec.TargetRef.Kind); kind {
	case "deployment":
		workloadUnderTest = &appsv1.Deployment{}
	case "statefulset":
		workloadUnderTest = &appsv1.StatefulSet{}
	case "daemonset":
		workloadUnderTest = &appsv1.DaemonSet{}
	}

	err := r.Get(
		ctx,
		types.NamespacedName{
			Namespace: workloadHardening.Namespace,
			Name:      workloadHardening.Spec.TargetRef.Name,
		},
		workloadUnderTest,
	)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// If the custom resource is not found then it usually means that it was deleted or not created
			log.Info("workloadHardeningCheck.Spec.TargetRef not found. You must reference an existing workload to test it")
			return nil, fmt.Errorf("workloadHardeningCheck.Spec.TargetRef not found. You must reference an existing workload to test it")
		}
		// Error reading the object - requeue the request.
		log.Error(err, "failed to get workloadHardeningCheck.Spec.TargetRef, requeing")
		return nil, fmt.Errorf("failed to get workloadHardeningCheck.Spec.TargetRef: %w", err)
	}
	log.Info("TargetRef found")

	return &workloadUnderTest, nil
}

// Verify if the targetRef workload is up & running
// The validation that the workload exists at all (and matches the currently supported kinds)
// should be moved to a webhook
func (r *WorkloadHardeningCheckReconciler) verifyWorkloadTargetRef(
	ctx context.Context,
	workloadHardening *checksv1alpha1.WorkloadHardeningCheck,
) (bool, error) {
	//log := log.FromContext(ctx)

	workloadUnderTest, err := r.getWorkloadUnderTest(ctx, workloadHardening)
	if err != nil {
		return false, err
	}

	// Verify targetRef is running/valid
	ready, err := verifySuccessfullyRunning(*workloadUnderTest)
	if err != nil {
		return false, fmt.Errorf("kind of workloadUnderTest not supported: %w", err)
	}

	return ready, nil

}

func (r *WorkloadHardeningCheckReconciler) setCondition(ctx context.Context, workloadHardening *checksv1alpha1.WorkloadHardeningCheck, condition metav1.Condition) error {

	log := log.FromContext(ctx)

	// Let's re-fetch the workload hardening check Custom Resource after updating the status so that we have the latest state
	if err := r.Get(ctx, types.NamespacedName{Name: workloadHardening.Name, Namespace: workloadHardening.Namespace}, workloadHardening); err != nil {
		log.Error(err, "Failed to re-fetch WorkloadHardeningCheck")
		return err
	}

	// Set all current conditions to ConditionFalse
	for _, cond := range workloadHardening.Status.Conditions {
		meta.SetStatusCondition(
			&workloadHardening.Status.Conditions,
			metav1.Condition{
				Type:    cond.Type,
				Status:  metav1.ConditionFalse,
				Reason:  cond.Reason,
				Message: cond.Message,
			},
		)
	}

	// Set/Update condition
	meta.SetStatusCondition(
		&workloadHardening.Status.Conditions,
		condition,
	)

	if err := r.Status().Update(ctx, workloadHardening); err != nil {
		log.Error(err, "Failed to update WorkloadHardeningCheck status")
		return err
	}

	// Let's re-fetch the workload hardening check Custom Resource after updating the status so that we have the latest state
	if err := r.Get(ctx, types.NamespacedName{Name: workloadHardening.Name, Namespace: workloadHardening.Namespace}, workloadHardening); err != nil {
		log.Error(err, "Failed to re-fetch WorkloadHardeningCheck")
		return err
	}

	return nil
}

func verifySuccessfullyRunning(workloadUnderTest client.Object) (bool, error) {
	switch v := workloadUnderTest.(type) {
	case *appsv1.Deployment:
		return *v.Spec.Replicas == v.Status.ReadyReplicas, nil
	case *appsv1.StatefulSet:
		return *v.Spec.Replicas == v.Status.ReadyReplicas, nil
	case *appsv1.DaemonSet:
		return v.Status.DesiredNumberScheduled == v.Status.NumberReady, nil
	}

	return false, fmt.Errorf("kind of workloadUnderTest not supported")
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
