package workload

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	checksv1alpha1 "github.com/fhnw-imvs/fhnw-kubeseccontext/api/v1alpha1"
	"github.com/fhnw-imvs/fhnw-kubeseccontext/internal/valkey"
	"github.com/fhnw-imvs/fhnw-kubeseccontext/pkg/orakel"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type WorkloadHandler struct {
	client.Client

	vk *valkey.ValkeyClient

	l logr.Logger

	w *checksv1alpha1.WorkloadHardeningCheck
}

func NewWorkloadHandler(ctx context.Context, valKeyClient *valkey.ValkeyClient, client client.Client, workloadHardeningCheck *checksv1alpha1.WorkloadHardeningCheck) *WorkloadHandler {

	return &WorkloadHandler{
		Client: client,
		l:      log.FromContext(ctx).WithName("WorkloadHandler"),
		w:      workloadHardeningCheck,
		vk:     valKeyClient,
	}

}

func (h *WorkloadHandler) GetWorkloadUnderTest(ctx context.Context, namespace string) (*client.Object, error) {

	name := h.w.Spec.TargetRef.Name
	kind := h.w.Spec.TargetRef.Kind

	// Verify namespace contains target workload
	var workloadUnderTest client.Object
	switch strings.ToLower(kind) {
	case "deployment":
		workloadUnderTest = &appsv1.Deployment{}
	case "statefulset":
		workloadUnderTest = &appsv1.StatefulSet{}
	case "daemonset":
		workloadUnderTest = &appsv1.DaemonSet{}
	}

	err := h.Get(
		ctx,
		types.NamespacedName{
			Namespace: namespace,
			Name:      name,
		},
		workloadUnderTest,
	)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// If the custom resource is not found then it usually means that it was deleted or not created
			h.l.Info("workloadHardeningCheck.Spec.TargetRef not found. You must reference an existing workload to test it")
			return nil, fmt.Errorf("workloadHardeningCheck.Spec.TargetRef not found. You must reference an existing workload to test it")
		}
		// Error reading the object - requeue the request.
		h.l.Error(err, "failed to get workloadHardeningCheck.Spec.TargetRef, requeing")
		return nil, fmt.Errorf("failed to get workloadHardeningCheck.Spec.TargetRef: %w", err)
	}
	h.l.Info("TargetRef found")

	return &workloadUnderTest, nil
}

func (h *WorkloadHandler) VerifyRunning(ctx context.Context, namespace string) (bool, error) {
	workloadUnderTestPtr, err := h.GetWorkloadUnderTest(ctx, namespace)
	if err != nil {
		h.l.Error(err, "failed to get workload under test")
		return false, fmt.Errorf("failed to get workload under test: %w", err)
	}

	return VerifySuccessfullyRunning(*workloadUnderTestPtr)
}

func VerifySuccessfullyRunning(workloadUnderTest client.Object) (bool, error) {
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

func (r *WorkloadHandler) GetLabelSelector(ctx context.Context) (labels.Selector, error) {
	workloadUnderTest, err := r.GetWorkloadUnderTest(ctx, r.w.GetNamespace())
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

func (r *WorkloadHandler) SetCondition(ctx context.Context, condition metav1.Condition) error {

	log := log.FromContext(ctx)

	var err error
	retryCount := 0

	// retry 3 times to update the status of the WorkloadHardeningCheck, to avoid concurrent updates failing
	for retryCount < 3 {
		// Let's re-fetch the workload hardening check Custom Resource after updating the status so that we have the latest state
		if err = r.Get(ctx, types.NamespacedName{Name: r.w.Name, Namespace: r.w.Namespace}, r.w); err != nil {
			log.Error(err, "Failed to re-fetch WorkloadHardeningCheck")
			return err
		}

		// Set/Update condition
		meta.SetStatusCondition(
			&r.w.Status.Conditions,
			condition,
		)

		if err := r.Status().Update(ctx, r.w); err != nil {
			log.V(3).Info("Failed to update WorkloadHardeningCheck status, retrying")
			retryCount++
			continue // Retry updating the status
		} else {
			break
		}

	}

	return err
}

func (r *WorkloadHandler) AnalyzeCheckRuns(ctx context.Context) error {
	log := log.FromContext(ctx)

	// Get results from the workload hardening check from ValKey
	baselineRecording, err := r.vk.GetRecording(ctx, fmt.Sprintf("%s:%s:%s", r.w.Namespace, r.w.Spec.Suffix, "baseline"))
	if err != nil {
		log.Error(err, "Failed to get baseline recording from ValKey")
		return fmt.Errorf("failed to get baseline recording from ValKey: %w", err)
	}
	if baselineRecording == nil {
		log.Info("No baseline recording found, skipping analysis")
		return nil
	}

	// Contains a drainMiner for each container in the baseline recording
	drainMinerPerContainer := make(map[string]*orakel.LogOrakel, len(baselineRecording.Logs))

	for podName, logs := range baselineRecording.Logs {
		// Initialize a DrainMiner for each pod
		drainMiner := orakel.NewDrainMiner(logs)
		drainMiner.LoadBaseline(logs)
		drainMinerPerContainer[podName] = drainMiner

	}

	// Iterate over all check runs and analyze the logs
	for _, checkRun := range r.w.Status.CheckRuns {
		log.V(2).Info("Analyzing check run", "checkRun", checkRun.Name)

		// Get the recording for this check run
		checkRecording, err := r.vk.GetRecording(ctx, fmt.Sprintf("%s:%s:%s", r.w.Namespace, r.w.Spec.Suffix, checkRun.Name))
		if err != nil {
			return fmt.Errorf("failed to get recording for check run from ValKey: %w", err)
		}
		if checkRecording == nil {
			return fmt.Errorf("no recording found for check run %s", checkRun.Name)
		}

		for podName, logs := range checkRecording.Logs {
			drainMiner, exists := drainMinerPerContainer[podName]
			if !exists {
				log.V(2).Info("No baseline found for pod", "podName", podName)
				continue
			}

			anomalies, _ := drainMiner.AnalyzeTarget(logs)
			if len(anomalies) > 0 {
				log.V(2).Info("Anomalies found in check run", "checkRun", checkRun.Name, "podName", podName, "anomalyCount", len(anomalies))

			} else {
				log.V(2).Info("No anomalies found in check run", "checkRun", checkRun.Name, "podName", podName)
			}
		}
	}

	return nil

}
