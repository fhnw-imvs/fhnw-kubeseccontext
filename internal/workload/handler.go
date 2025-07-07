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
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	checksv1alpha1 "github.com/fhnw-imvs/fhnw-kubeseccontext/api/v1alpha1"
	"github.com/fhnw-imvs/fhnw-kubeseccontext/internal/valkey"
	"github.com/fhnw-imvs/fhnw-kubeseccontext/pkg/orakel"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type WorkloadCheckHandler struct {
	client.Client

	vk *valkey.ValkeyClient

	l logr.Logger

	w *checksv1alpha1.WorkloadHardeningCheck
}

func NewWorkloadCheckHandler(ctx context.Context, valKeyClient *valkey.ValkeyClient, client client.Client, workloadHardeningCheck *checksv1alpha1.WorkloadHardeningCheck) *WorkloadCheckHandler {

	return &WorkloadCheckHandler{
		Client: client,
		l:      log.FromContext(ctx).WithName("WorkloadHandler"),
		w:      workloadHardeningCheck,
		vk:     valKeyClient,
	}

}

func (h *WorkloadCheckHandler) GetWorkloadUnderTest(ctx context.Context, namespace string) (*client.Object, error) {

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

func (h *WorkloadCheckHandler) VerifyRunning(ctx context.Context, namespace string) (bool, error) {
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

func (r *WorkloadCheckHandler) GetLabelSelector(ctx context.Context) (labels.Selector, error) {
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

func (r *WorkloadCheckHandler) SetCondition(ctx context.Context, condition metav1.Condition) error {

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

func (r *WorkloadCheckHandler) AnalyzeCheckRuns(ctx context.Context) error {
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

	for containerName, logs := range baselineRecording.Logs {
		// Initialize a DrainMiner for each pod
		drainMiner := orakel.NewDrainMiner(logs)
		drainMiner.LoadBaseline(logs)
		drainMinerPerContainer[containerName] = drainMiner

	}

	checkRuns := r.w.Status.CheckRuns
	// Baseline is also listed as check run
	if len(checkRuns) <= 0 {
		log.V(2).Info("No check runs found in workload hardening check status, skipping analysis")
		return nil
	}
	// Iterate over all check runs and analyze the logs
	for i, checkRun := range checkRuns {
		if checkRun.Name == "baseline" {
			continue // Skip baseline check run
		}

		log.V(2).Info("Analyzing check run", "checkRun", checkRun.Name)

		// Get the recording for this check run
		checkRecording, err := r.vk.GetRecording(ctx, fmt.Sprintf("%s:%s:%s", r.w.Namespace, r.w.Spec.Suffix, checkRun.Name))
		if err != nil {
			return fmt.Errorf("failed to get recording for check run from ValKey: %w", err)
		}
		if checkRecording == nil {
			return fmt.Errorf("no recording found for check run %s", checkRun.Name)
		}

		checkSuccessful := true
		for containerName, logs := range checkRecording.Logs {
			drainMiner, exists := drainMinerPerContainer[containerName]
			if !exists {
				log.Info("No baseline found for pod", "podName", containerName)
				continue
			}

			anomalies, _ := drainMiner.AnalyzeTarget(logs)
			if len(anomalies) > 0 {
				log.Info("Anomalies found in check run", "checkRun", checkRun.Name, "containerName", containerName, "anomalyCount", len(anomalies))
				checkSuccessful = false
				if checkRun.Anomalies == nil {
					checkRuns[i].Anomalies = make(map[string][]string)
				}
				checkRuns[i].Anomalies[containerName] = anomalies[len(anomalies)-5:] // Store only the last 5 anomalies for brevity
			} else {
				log.Info("No anomalies found in check run", "checkRun", checkRun.Name, "containerName", containerName)
			}
		}

		// Update the check run with the analysis results
		checkRuns[i].CheckSuccessfull = ptr.To(checkSuccessful)
	}

	// Update the check run status
	r.w.Status.CheckRuns = checkRuns
	r.Status().Update(ctx, r.w)

	return nil

}
