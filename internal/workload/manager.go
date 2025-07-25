package workload

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	"k8s.io/client-go/util/retry"
)

var (
	// Required to convert "user" to "User", strings.ToTitle converts each rune to title case not just the first one
	titleCase          = cases.Title(language.English)
	drainTemplateRegex = regexp.MustCompile(`id=\{\d+\}\s+:\s+size=\{(?P<size>\d+)\}\s+:\s+(?P<template>.*)$`)
	sizeIndex          = drainTemplateRegex.SubexpIndex("size")
	templateIndex      = drainTemplateRegex.SubexpIndex("template")
)

type WorkloadCheckManager struct {
	client.Client

	valKeyClient *valkey.ValkeyClient

	logger logr.Logger

	workloadHardeningCheck *checksv1alpha1.WorkloadHardeningCheck
}

func NewWorkloadCheckManager(ctx context.Context, valKeyClient *valkey.ValkeyClient, workloadHardeningCheck *checksv1alpha1.WorkloadHardeningCheck) *WorkloadCheckManager {

	log := log.FromContext(ctx).WithName("WorkloadManager")
	scheme := runtime.NewScheme()

	clientgoscheme.AddToScheme(scheme)
	checksv1alpha1.AddToScheme(scheme)

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

	checkManager := &WorkloadCheckManager{
		Client:                 cl,
		logger:                 log,
		workloadHardeningCheck: workloadHardeningCheck.DeepCopy(),
		valKeyClient:           valKeyClient,
	}

	// Let's just set the status as Unknown when no status is available
	if len(workloadHardeningCheck.Status.Conditions) == 0 {

		// Set condition finished to false, so we can track the progress of the reconciliation
		err = checkManager.SetCondition(ctx, metav1.Condition{
			Type:    checksv1alpha1.ConditionTypeFinished,
			Status:  metav1.ConditionFalse,
			Reason:  checksv1alpha1.ReasonPreparationVerifying,
			Message: "Reconciliation started",
		})
	}

	return checkManager

}

func (m *WorkloadCheckManager) refreshWorkloadHardeningCheck() error {
	if err := m.Get(context.Background(), types.NamespacedName{Name: m.workloadHardeningCheck.Name, Namespace: m.workloadHardeningCheck.Namespace}, m.workloadHardeningCheck); err != nil {
		m.logger.Error(err, "Failed to re-fetch WorkloadHardeningCheck")
		return fmt.Errorf("failed to re-fetch WorkloadHardeningCheck: %w", err)
	}
	return nil
}

func (m *WorkloadCheckManager) GetReplicaCount(ctx context.Context, namespace string) (int32, error) {

	workloadUnderTestPtr, err := m.GetWorkloadUnderTest(ctx, namespace)
	if err != nil {
		m.logger.Error(err, "failed to get workload under test")
		return 0, fmt.Errorf("failed to get workload under test: %w", err)
	}

	switch v := (*workloadUnderTestPtr).(type) {
	case *appsv1.Deployment:
		if v.Spec.Replicas != nil {
			return *v.Spec.Replicas, nil
		}
		return 1, nil // Default to 1 if not set
	case *appsv1.StatefulSet:
		if v.Spec.Replicas != nil {
			return *v.Spec.Replicas, nil
		}
		return 1, nil // Default to 1 if not set
	case *appsv1.DaemonSet:
		return 0, fmt.Errorf("cannot scale DaemonSet")
	default:
		return 0, fmt.Errorf("unsupported workload kind: %T", v)
	}
}

func (m *WorkloadCheckManager) ScaleWorkloadUnderTest(ctx context.Context, namespace string, replicas int32) error {

	workloadUnderTestPtr, err := m.GetWorkloadUnderTest(ctx, namespace)
	if err != nil {
		m.logger.Error(err, "failed to get workload under test")
		return fmt.Errorf("failed to get workload under test: %w", err)
	}

	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {

		// Let's re-fetch the workload hardening check Custom Resource after updating the status so that we have the latest state
		if err := m.Get(ctx, types.NamespacedName{Name: (*workloadUnderTestPtr).GetName(), Namespace: namespace}, *workloadUnderTestPtr); err != nil {
			if apierrors.IsNotFound(err) {
				// workloadHardeningCheck resource was deleted, while a check was running
				m.logger.Info("WorkloadHardeningCheck not found, skipping check run update")
				return nil // If the resource is not found, we can skip the update
			}
			m.logger.Error(err, "Failed to re-fetch WorkloadHardeningCheck")
			return fmt.Errorf("failed to re-fetch WorkloadHardeningCheck: %w", err)
		}

		switch v := (*workloadUnderTestPtr).(type) {
		case *appsv1.Deployment:
			v.Spec.Replicas = &replicas
		case *appsv1.StatefulSet:
			v.Spec.Replicas = &replicas
		case *appsv1.DaemonSet:
			return fmt.Errorf("cannot scale DaemonSet")
		default:
			return fmt.Errorf("unsupported workload kind: %T", v)
		}

		return m.Update(ctx, *workloadUnderTestPtr)
	})

	if err == nil {
		m.logger.V(2).Info("scaled workload under test", "replicas", replicas, "workload", (*workloadUnderTestPtr).GetName())
		return nil
	}

	return err
}

func (m *WorkloadCheckManager) GetPodSpecTemplate(ctx context.Context, namespace string) (*v1.PodSpec, error) {

	workloadUnderTestPtr, err := m.GetWorkloadUnderTest(ctx, namespace)
	if err != nil {
		m.logger.Error(err, "failed to get workload under test")
		return nil, fmt.Errorf("failed to get workload under test: %w", err)
	}

	var podSpecTemplate *v1.PodSpec
	switch v := (*workloadUnderTestPtr).(type) {
	case *appsv1.Deployment:
		podSpecTemplate = &v.Spec.Template.Spec
	case *appsv1.StatefulSet:
		podSpecTemplate = &v.Spec.Template.Spec
	case *appsv1.DaemonSet:
		podSpecTemplate = &v.Spec.Template.Spec
	default:
		return nil, fmt.Errorf("unsupported workload kind: %T", v)
	}

	return podSpecTemplate, nil
}

func (m *WorkloadCheckManager) GetWorkloadUnderTest(ctx context.Context, namespace string) (*client.Object, error) {

	name := m.workloadHardeningCheck.Spec.TargetRef.Name
	kind := m.workloadHardeningCheck.Spec.TargetRef.Kind

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

	err := m.Get(
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
			m.logger.Info("workloadHardeningCheck.Spec.TargetRef not found. You must reference an existing workload to test it")
			return nil, fmt.Errorf("workloadHardeningCheck.Spec.TargetRef not found. You must reference an existing workload to test it")
		}
		// Error reading the object - requeue the request.
		m.logger.Error(err, "failed to get workloadHardeningCheck.Spec.TargetRef, requeing")
		return nil, fmt.Errorf("failed to get workloadHardeningCheck.Spec.TargetRef: %w", err)
	}
	m.logger.Info("TargetRef found")

	return &workloadUnderTest, nil
}

func (m *WorkloadCheckManager) VerifyRunning(ctx context.Context, namespace string) (bool, error) {
	workloadUnderTestPtr, err := m.GetWorkloadUnderTest(ctx, namespace)
	if err != nil {
		m.logger.Error(err, "failed to get workload under test")
		return false, fmt.Errorf("failed to get workload under test: %w", err)
	}

	return VerifySuccessfullyRunning(*workloadUnderTestPtr)
}

func (m *WorkloadCheckManager) GetLabelSelector(ctx context.Context) (labels.Selector, error) {
	workloadUnderTest, err := m.GetWorkloadUnderTest(ctx, m.workloadHardeningCheck.GetNamespace())
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

func (m *WorkloadCheckManager) AnalyzeCheckRuns(ctx context.Context) error {

	// Contains a drainMiner for each container in the baseline recording
	drainMinerPerContainer := make(map[string]*orakel.LogOrakel)

	// Record baseline for both baseline recordings
	for _, baseline := range []string{"baseline", "baseline-2"} {
		// Get results from the workload hardening check from ValKey
		baselineRecording, err := m.valKeyClient.GetRecording(ctx, fmt.Sprintf("%s:%s:%s", m.workloadHardeningCheck.Namespace, m.workloadHardeningCheck.Spec.Suffix, baseline))
		if err != nil {
			m.logger.Error(err, "Failed to get baseline recording from ValKey")
			return fmt.Errorf("failed to get baseline recording from ValKey: %w", err)
		}
		if baselineRecording == nil {
			m.logger.Info("No baseline recording found, skipping analysis")
			return nil
		}

		for containerName, logs := range baselineRecording.Logs {

			drainMiner, exists := drainMinerPerContainer[containerName]
			if !exists {
				// Initialize a new DrainMiner
				drainMiner = orakel.NewDrainMiner()
			}

			drainMiner.LoadBaseline(logs)
			drainMinerPerContainer[containerName] = drainMiner
		}
	}

	checkRuns := m.workloadHardeningCheck.Status.CheckRuns
	// Baseline is also listed as check run
	if len(checkRuns) <= 0 {
		m.logger.V(2).Info("No check runs found in workload hardening check status, skipping analysis")
		return nil
	}
	updatedCheckRuns := make(map[string]*checksv1alpha1.CheckRun, len(checkRuns))
	// Iterate over all check runs and analyze the logs
	for _, checkRun := range checkRuns {
		checkRun := checkRun.DeepCopy() // Create a copy to avoid modifying the original

		if strings.Contains(checkRun.Name, "baseline") {
			continue // Skip baseline check run
		}

		m.logger.V(2).Info("Analyzing check run", "checkRun", checkRun.Name)

		// Get the recording for this check run
		checkRecording, err := m.valKeyClient.GetRecording(ctx, fmt.Sprintf("%s:%s:%s", m.workloadHardeningCheck.Namespace, m.workloadHardeningCheck.Spec.Suffix, checkRun.Name))
		if err != nil {
			return fmt.Errorf("failed to get recording for check run from ValKey: %w", err)
		}
		if checkRecording == nil {
			return fmt.Errorf("no recording found for check run %s", checkRun.Name)
		}

		checkSuccessful := true
		if checkRun.CheckSuccessfull != nil {
			checkSuccessful = *checkRun.CheckSuccessfull // Use the existing value if it exists
		}
		for containerName, logs := range checkRecording.Logs {
			drainMiner, exists := drainMinerPerContainer[containerName]
			if !exists {
				m.logger.Info("No baseline found for pod", "podName", containerName)
				continue
			}

			anomalies, _ := drainMiner.AnalyzeTarget(logs)
			if len(anomalies) > 0 && len(anomalies) <= 5 {
				m.logger.Info("Anomalies found in check run", "checkRun", checkRun.Name, "containerName", containerName, "anomalyCount", len(anomalies))
				checkSuccessful = false
				if checkRun.Anomalies == nil {
					checkRun.Anomalies = make(map[string][]string)
				}

				checkRun.Anomalies[containerName] = anomalies

			} else if len(anomalies) > 5 {
				m.logger.Info("Anomalies found in check run", "checkRun", checkRun.Name, "containerName", containerName, "anomalyCount", len(anomalies))
				checkSuccessful = false
				if checkRun.Anomalies == nil {
					checkRun.Anomalies = make(map[string][]string)
				}

				anomalyMiner := orakel.NewDrainMiner()
				anomalyMiner.LoadBaseline(logs)

				anomalyClusters := anomalyMiner.Clusters()
				anomalyTemplates := make(map[string]int)
				for _, cluster := range anomalyClusters {
					// Convert the template to a string and trim it
					template := strings.TrimSpace(cluster.String())

					matches := drainTemplateRegex.FindStringSubmatch(template)
					anomalyTemplates[matches[templateIndex]], _ = strconv.Atoi(matches[sizeIndex])
				}

				// Sort the anomalies by size (descending)
				keys := make([]string, 0, len(anomalyTemplates))
				for key := range anomalyTemplates {
					keys = append(keys, key)
				}
				sort.Slice(keys, func(i, j int) bool { return anomalyTemplates[keys[i]] > anomalyTemplates[keys[j]] })

				if len(anomalyTemplates) > 5 {
					m.logger.V(2).Info("Trimming anomaly templates to last 5", "checkRun", checkRun.Name, "containerName", containerName)
					// First 5 anomalies are the most significant ones
					checkRun.Anomalies[containerName] = keys[:5]
				} else {
					checkRun.Anomalies[containerName] = keys
				}

			} else {
				m.logger.Info("No anomalies found in check run", "checkRun", checkRun.Name, "containerName", containerName)
			}
		}

		// Update the check run with the analysis results
		checkRun.CheckSuccessfull = ptr.To(checkSuccessful)
		updatedCheckRuns[checkRun.Name] = checkRun
	}

	// Update the check run status
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Let's re-fetch the workload hardening check Custom Resource after updating the status so that we have the latest state
		if err := m.Get(ctx, types.NamespacedName{Name: m.workloadHardeningCheck.Name, Namespace: m.workloadHardeningCheck.Namespace}, m.workloadHardeningCheck); err != nil {
			if apierrors.IsNotFound(err) {
				// workloadHardeningCheck resource was deleted, while a check was running
				m.logger.Info("WorkloadHardeningCheck not found, skipping check run update")
				return nil // If the resource is not found, we can skip the update
			}
			m.logger.Error(err, "Failed to re-fetch WorkloadHardeningCheck")
			return fmt.Errorf("failed to re-fetch WorkloadHardeningCheck: %w", err)
		}

		// Set/Update condition
		m.workloadHardeningCheck.Status.CheckRuns = updatedCheckRuns

		return m.Status().Update(ctx, m.workloadHardeningCheck)
	})

	return err

}

func (m *WorkloadCheckManager) SetRecommendation(ctx context.Context) error {

	securityContexts := map[string]*checksv1alpha1.SecurityContextDefaults{}

	// Get the security context for each check type
	for _, checkRun := range m.workloadHardeningCheck.Status.CheckRuns {
		if checkRun.Name == "baseline" {
			continue // Skip baseline check
		}
		if checkRun.CheckSuccessfull != nil && !*checkRun.CheckSuccessfull {
			continue // Skip check runs that were not successful
		}

		securityContexts[checkRun.Name] = checkRun.SecurityContext
	}

	podSpecTemplate, err := m.GetPodSpecTemplate(ctx, m.workloadHardeningCheck.Namespace)
	if err != nil {
		m.logger.Error(err, "Failed to get workload under test")
		return fmt.Errorf("failed to get workload under test: %w", err)
	}

	podSecurityContext := &v1.PodSecurityContext{}
	containerSecurityContext := &v1.SecurityContext{}

	// Get security context already set in the original manifest
	if podSpecTemplate != nil && podSpecTemplate.SecurityContext != nil {
		podSecurityContext = podSpecTemplate.SecurityContext
	}
	if len(podSpecTemplate.Containers) > 0 && podSpecTemplate.Containers[0].SecurityContext != nil {
		// We assume that the first container in the pod spec template is the main container
		containerSecurityContext = podSpecTemplate.Containers[0].SecurityContext
	}

	for _, securityContext := range securityContexts {
		podSecurityContext = mergePodSecurityContexts(ctx, podSecurityContext, securityContext.Pod.ToK8sSecurityContext())
		containerSecurityContext = mergeContainerSecurityContexts(ctx, containerSecurityContext, securityContext.Container.ToK8sSecurityContext())
	}

	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Awlays re-fetch the workload hardening check Custom before updating the status
		if err := m.Get(ctx, types.NamespacedName{Name: m.workloadHardeningCheck.Name, Namespace: m.workloadHardeningCheck.Namespace}, m.workloadHardeningCheck); err != nil {
			m.logger.Error(err, "Failed to re-fetch WorkloadHardeningCheck")
			return fmt.Errorf("failed to re-fetch WorkloadHardeningCheck: %w", err)
		}
		// Set/Update the recommendation

		m.workloadHardeningCheck.Status.Recommendation = &checksv1alpha1.Recommendation{
			ContainerSecurityContexts: containerSecurityContext,
			PodSecurityContext:        podSecurityContext,
		}

		return m.Status().Update(ctx, m.workloadHardeningCheck)
	})

	if err != nil {
		m.logger.Error(err, "Failed to update recommendation in WorkloadHardeningCheck status")
		return fmt.Errorf("failed to update recommendation in WorkloadHardeningCheck status: %w", err)
	}

	return nil

}

func (m *WorkloadCheckManager) GetRecommendedSecurityContext() *checksv1alpha1.SecurityContextDefaults {
	// If the recommendation is not set, return nil
	if !m.RecommendationExists() {
		return nil
	}

	recommendation := checksv1alpha1.SecurityContextDefaults{
		Pod:       &checksv1alpha1.PodSecurityContextDefaults{},
		Container: &checksv1alpha1.ContainerSecurityContextDefaults{},
	}

	// If the pod security context is set, use it
	if m.workloadHardeningCheck.Status.Recommendation.PodSecurityContext != nil {
		recommendation.Pod = &checksv1alpha1.PodSecurityContextDefaults{
			RunAsGroup:   m.workloadHardeningCheck.Status.Recommendation.PodSecurityContext.RunAsGroup,
			RunAsUser:    m.workloadHardeningCheck.Status.Recommendation.PodSecurityContext.RunAsUser,
			RunAsNonRoot: m.workloadHardeningCheck.Status.Recommendation.PodSecurityContext.RunAsNonRoot,
			FSGroup:      m.workloadHardeningCheck.Status.Recommendation.PodSecurityContext.FSGroup,
		}
	}

	// If the container security context is set, use it
	if m.workloadHardeningCheck.Status.Recommendation.ContainerSecurityContexts != nil {
		recommendation.Container = &checksv1alpha1.ContainerSecurityContextDefaults{
			RunAsGroup:               m.workloadHardeningCheck.Status.Recommendation.ContainerSecurityContexts.RunAsGroup,
			RunAsUser:                m.workloadHardeningCheck.Status.Recommendation.ContainerSecurityContexts.RunAsUser,
			RunAsNonRoot:             m.workloadHardeningCheck.Status.Recommendation.ContainerSecurityContexts.RunAsNonRoot,
			ReadOnlyRootFilesystem:   m.workloadHardeningCheck.Status.Recommendation.ContainerSecurityContexts.ReadOnlyRootFilesystem,
			AllowPrivilegeEscalation: m.workloadHardeningCheck.Status.Recommendation.ContainerSecurityContexts.AllowPrivilegeEscalation,
		}
		if m.workloadHardeningCheck.Status.Recommendation.ContainerSecurityContexts.Capabilities != nil &&
			m.workloadHardeningCheck.Status.Recommendation.ContainerSecurityContexts.Capabilities.Drop != nil {
			recommendation.Container.CapabilitiesDrop = m.workloadHardeningCheck.Status.Recommendation.ContainerSecurityContexts.Capabilities.Drop
		} else {
			recommendation.Container.CapabilitiesDrop = []corev1.Capability{} // Default to dropping all capabilities if not set
		}
	}

	// Return the recommendation from the status
	return &recommendation

}

func (m *WorkloadCheckManager) GetCheckDuration() time.Duration {
	// Default to 5 minutes if not specified
	if m.workloadHardeningCheck.Spec.BaselineDuration == "" {
		return 5 * time.Minute
	}

	// Parse the duration string
	duration, err := time.ParseDuration(m.workloadHardeningCheck.Spec.BaselineDuration)
	if err != nil {
		return 5 * time.Minute // Fallback to default if parsing fails
	}

	return duration
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

func VerifyUpdated(workloadUnderTest client.Object) (bool, error) {
	switch v := workloadUnderTest.(type) {
	case *appsv1.Deployment:
		return *v.Spec.Replicas == v.Status.UpdatedReplicas, nil
	case *appsv1.StatefulSet:
		return *v.Spec.Replicas == v.Status.UpdatedReplicas, nil
	case *appsv1.DaemonSet:
		return v.Status.DesiredNumberScheduled == v.Status.UpdatedNumberScheduled, nil
	}

	return false, fmt.Errorf("kind of workloadUnderTest not supported")
}
