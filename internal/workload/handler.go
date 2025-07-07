package workload

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/exp/maps"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	checksv1alpha1 "github.com/fhnw-imvs/fhnw-kubeseccontext/api/v1alpha1"
	"github.com/fhnw-imvs/fhnw-kubeseccontext/internal/valkey"
	"github.com/fhnw-imvs/fhnw-kubeseccontext/pkg/orakel"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
)

// Required to convert "user" to "User", strings.ToTitle converts each rune to title case not just the first one
var titleCase = cases.Title(language.English)

type WorkloadCheckHandler struct {
	client.Client

	valKeyClient *valkey.ValkeyClient

	logger logr.Logger

	workloadHardeningCheck *checksv1alpha1.WorkloadHardeningCheck
}

func NewWorkloadCheckHandler(ctx context.Context, valKeyClient *valkey.ValkeyClient, workloadHardeningCheck *checksv1alpha1.WorkloadHardeningCheck) *WorkloadCheckHandler {

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

	return &WorkloadCheckHandler{
		Client:                 cl,
		logger:                 log.FromContext(ctx).WithName("WorkloadHandler"),
		workloadHardeningCheck: workloadHardeningCheck,
		valKeyClient:           valKeyClient,
	}

}

func (h *WorkloadCheckHandler) GetWorkloadUnderTest(ctx context.Context, namespace string) (*client.Object, error) {

	name := h.workloadHardeningCheck.Spec.TargetRef.Name
	kind := h.workloadHardeningCheck.Spec.TargetRef.Kind

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
			h.logger.Info("workloadHardeningCheck.Spec.TargetRef not found. You must reference an existing workload to test it")
			return nil, fmt.Errorf("workloadHardeningCheck.Spec.TargetRef not found. You must reference an existing workload to test it")
		}
		// Error reading the object - requeue the request.
		h.logger.Error(err, "failed to get workloadHardeningCheck.Spec.TargetRef, requeing")
		return nil, fmt.Errorf("failed to get workloadHardeningCheck.Spec.TargetRef: %w", err)
	}
	h.logger.Info("TargetRef found")

	return &workloadUnderTest, nil
}

func (h *WorkloadCheckHandler) VerifyRunning(ctx context.Context, namespace string) (bool, error) {
	workloadUnderTestPtr, err := h.GetWorkloadUnderTest(ctx, namespace)
	if err != nil {
		h.logger.Error(err, "failed to get workload under test")
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
	workloadUnderTest, err := r.GetWorkloadUnderTest(ctx, r.workloadHardeningCheck.GetNamespace())
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
		if err = r.Get(ctx, types.NamespacedName{Name: r.workloadHardeningCheck.Name, Namespace: r.workloadHardeningCheck.Namespace}, r.workloadHardeningCheck); err != nil {
			log.Error(err, "Failed to re-fetch WorkloadHardeningCheck")
			return err
		}

		// Set/Update condition
		meta.SetStatusCondition(
			&r.workloadHardeningCheck.Status.Conditions,
			condition,
		)

		if err := r.Status().Update(ctx, r.workloadHardeningCheck); err != nil {
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
	baselineRecording, err := r.valKeyClient.GetRecording(ctx, fmt.Sprintf("%s:%s:%s", r.workloadHardeningCheck.Namespace, r.workloadHardeningCheck.Spec.Suffix, "baseline"))
	if err != nil {
		log.Error(err, "Failed to get baseline recording from ValKey")
		return fmt.Errorf("failed to get baseline recording from ValKey: %w", err)
	}
	if baselineRecording == nil {
		log.Info("No baseline recording found, skipping analysis")
		return nil
	}

	// Get results from the workload hardening check from ValKey
	baselineRecording2, err := r.valKeyClient.GetRecording(ctx, fmt.Sprintf("%s:%s:%s", r.workloadHardeningCheck.Namespace, r.workloadHardeningCheck.Spec.Suffix, "baseline-2"))
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
		drainMiner := orakel.NewDrainMiner()
		drainMiner.LoadBaseline(logs)
		drainMinerPerContainer[containerName] = drainMiner
	}

	// If the second baseline recording is available, load it into the drain miners
	if baselineRecording2 != nil {
		for containerName, logs := range baselineRecording2.Logs {
			// Check if the container already has a drainMiner, if not, create one
			drainMiner, exists := drainMinerPerContainer[containerName]
			if !exists {
				log.V(2).Info("No baseline found for pod", "podName", containerName)
				drainMiner = orakel.NewDrainMiner()
				drainMinerPerContainer[containerName] = drainMiner
			}

			drainMiner.LoadBaseline(logs)
		}
	}

	checkRuns := r.workloadHardeningCheck.Status.CheckRuns
	// Baseline is also listed as check run
	if len(checkRuns) <= 0 {
		log.V(2).Info("No check runs found in workload hardening check status, skipping analysis")
		return nil
	}
	// Iterate over all check runs and analyze the logs
	for i, checkRun := range checkRuns {
		if strings.Contains(checkRun.Name, "baseline") {
			continue // Skip baseline check run
		}

		log.V(2).Info("Analyzing check run", "checkRun", checkRun.Name)

		// Get the recording for this check run
		checkRecording, err := r.valKeyClient.GetRecording(ctx, fmt.Sprintf("%s:%s:%s", r.workloadHardeningCheck.Namespace, r.workloadHardeningCheck.Spec.Suffix, checkRun.Name))
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
	r.workloadHardeningCheck.Status.CheckRuns = checkRuns
	r.Status().Update(ctx, r.workloadHardeningCheck)

	return nil

}

func (r *WorkloadCheckHandler) GetCheckDuration() time.Duration {
	// Default to 5 minutes if not specified
	if r.workloadHardeningCheck.Spec.BaselineDuration == "" {
		return 5 * time.Minute
	}

	// Parse the duration string
	duration, err := time.ParseDuration(r.workloadHardeningCheck.Spec.BaselineDuration)
	if err != nil {
		return 5 * time.Minute // Fallback to default if parsing fails
	}

	return duration
}

func (r *WorkloadCheckHandler) BaselineRecorded() bool {

	if meta.IsStatusConditionTrue(r.workloadHardeningCheck.Status.Conditions, checksv1alpha1.ConditionTypeBaseline) {
		condition := meta.FindStatusCondition(r.workloadHardeningCheck.Status.Conditions, checksv1alpha1.ConditionTypeBaseline)
		return condition.Reason == checksv1alpha1.ReasonBaselineRecordingFinished
	}
	return false
}

func (r *WorkloadCheckHandler) AllChecksFinished() bool {

	if !r.BaselineRecorded() {
		return false // Baseline must be recorded before checks can be considered finished
	}

	if len(r.workloadHardeningCheck.Status.CheckRuns) == 1 {
		// If only the baseline check is present, we consider it not finished
		return false
	}

	requiredChecks := r.GetRequiredCheckRuns(context.Background())

	for _, checkType := range requiredChecks {
		// Check if the condition for the check is true
		if !meta.IsStatusConditionTrue(r.workloadHardeningCheck.Status.Conditions, titleCase.String(checkType)+checksv1alpha1.ConditionTypeCheck) {
			r.logger.V(2).Info("Check not finished", "checkType", checkType)
			return false // If any required check is not finished, return false
		}
	}

	// If we reach here, it means no checks are running
	return true
}

const (
	// Sets runAsGroup and fsGroup to non-zero values
	CheckTypeGroup = "group"
	// Sets runAsUser to non-zero values, and runAsNonRoot to true
	CheckTypeUser = "user"
	// Sets readOnlyRootFilesystem to true
	CheckTypeReadOnlyRootFile = "readOnlyRootFilesystem"
	// Sets allowPrivilegeEscalation to false
	CheckTypeAllowPrivilegeEscalation = "allowPrivilegeEscalation"
	// Sets capabilities to drop all capabilities
	CheckTypeDropCapabilities = "dropCapabilities"
)

func (r *WorkloadCheckHandler) GetRequiredCheckRuns(ctx context.Context) []string {
	checks := map[string]bool{}

	workloadUnderTest, err := r.GetWorkloadUnderTest(ctx, r.workloadHardeningCheck.Namespace)
	if err != nil {
		r.logger.Error(err, "Failed to get workload under test")
		return []string{}
	}

	// Determine the type of workload under test and extract the pod spec template
	var podSpecTemplate *v1.PodSpec
	switch v := (*workloadUnderTest).(type) {
	case *appsv1.Deployment:
		podSpecTemplate = &v.Spec.Template.Spec
	case *appsv1.StatefulSet:
		podSpecTemplate = &v.Spec.Template.Spec
	case *appsv1.DaemonSet:
		podSpecTemplate = &v.Spec.Template.Spec
	}

	if podSpecTemplate.SecurityContext != nil {
		if podSpecTemplate.SecurityContext.RunAsGroup == nil || podSpecTemplate.SecurityContext.FSGroup == nil {
			checks[CheckTypeGroup] = true
		}
		if podSpecTemplate.SecurityContext.RunAsUser == nil || podSpecTemplate.SecurityContext.RunAsNonRoot == nil {
			checks[CheckTypeUser] = true
		}
	} else {
		checks[CheckTypeGroup] = true
		checks[CheckTypeUser] = true
	}

	for _, container := range podSpecTemplate.Containers {
		if container.SecurityContext != nil {
			if container.SecurityContext.ReadOnlyRootFilesystem == nil || !*container.SecurityContext.ReadOnlyRootFilesystem {
				checks[CheckTypeReadOnlyRootFile] = true
			}
			if container.SecurityContext.AllowPrivilegeEscalation == nil || *container.SecurityContext.AllowPrivilegeEscalation {
				checks[CheckTypeAllowPrivilegeEscalation] = true
			}
			if container.SecurityContext.Capabilities == nil || len(container.SecurityContext.Capabilities.Drop) == 0 {
				checks[CheckTypeDropCapabilities] = true
			}
		} else {
			// If the container does not have a security context, we assume it needs all checks
			checks[CheckTypeReadOnlyRootFile] = true
			checks[CheckTypeAllowPrivilegeEscalation] = true
			checks[CheckTypeDropCapabilities] = true
		}
	}

	// If the check is already recorded, we don't need to run it again
	for key := range checks {
		if meta.IsStatusConditionTrue(r.workloadHardeningCheck.Status.Conditions, titleCase.String(key)+checksv1alpha1.ConditionTypeCheck) {
			delete(checks, key)
		}
	}

	return maps.Keys(checks)

}

func (r *WorkloadCheckHandler) GetSecurityContextForCheckType(checkType string) *checksv1alpha1.SecurityContextDefaults {

	baseSecurityContext := r.workloadHardeningCheck.Spec.SecurityContext
	if baseSecurityContext == nil {
		baseSecurityContext = &checksv1alpha1.SecurityContextDefaults{
			Pod:       &checksv1alpha1.PodSecurityContextDefaults{},
			Container: &checksv1alpha1.ContainerSecurityContextDefaults{},
		}
	}

	switch checkType {
	case CheckTypeGroup:
		if baseSecurityContext.Pod.RunAsGroup == nil {
			baseSecurityContext.Pod.RunAsGroup = ptr.To(int64(1000)) // Default group ID
		}
		baseSecurityContext.Container.RunAsGroup = baseSecurityContext.Pod.RunAsGroup
		if baseSecurityContext.Container.RunAsGroup == nil {
			baseSecurityContext.Container.RunAsGroup = ptr.To(int64(1000)) // Default group ID
		}
	case CheckTypeUser:
		if baseSecurityContext.Pod.RunAsUser == nil {
			baseSecurityContext.Pod.RunAsUser = ptr.To(int64(1000)) // Default user ID
		}
		if baseSecurityContext.Pod.RunAsNonRoot == nil {
			baseSecurityContext.Pod.RunAsNonRoot = ptr.To(true) // Default to non-root
		}
		baseSecurityContext.Container.RunAsUser = baseSecurityContext.Pod.RunAsUser
		baseSecurityContext.Container.RunAsNonRoot = baseSecurityContext.Pod.RunAsNonRoot
	case CheckTypeReadOnlyRootFile:
		if baseSecurityContext.Container.ReadOnlyRootFilesystem == nil {
			baseSecurityContext.Container.ReadOnlyRootFilesystem = ptr.To(true) // Default to read-only root filesystem
		}
	case CheckTypeAllowPrivilegeEscalation:
		if baseSecurityContext.Container.AllowPrivilegeEscalation == nil {
			baseSecurityContext.Container.AllowPrivilegeEscalation = ptr.To(false) // Default to no privilege escalation
		}
	case CheckTypeDropCapabilities:
		if baseSecurityContext.Container.CapabilitiesDrop == nil {
			baseSecurityContext.Container.CapabilitiesDrop = []string{"ALL"}
		}
	}

	return baseSecurityContext

}
