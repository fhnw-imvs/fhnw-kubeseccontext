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

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	checksv1alpha1 "github.com/fhnw-imvs/fhnw-kubeseccontext/api/v1alpha1"
	"github.com/fhnw-imvs/fhnw-kubeseccontext/internal/runner"
	"github.com/fhnw-imvs/fhnw-kubeseccontext/internal/valkey"
	wh "github.com/fhnw-imvs/fhnw-kubeseccontext/internal/workload"
)

// Definitions to manage status conditions
const (
	// Represents the initial state, before the baseline recording is started
	typeWorkloadCheckStartup = "Preparation"
	// Represents a running baseline recording
	typeWorkloadCheckBaseline = "Baseline"
	// Represents ongoing check jobs, this state will be used until all checks are finished
	typeWorkloadCheck = "Check"

	// Baseline is currently being recorded
	reasonBaselineRecording = "BaselineRecording"
	reasonBaselineFailed    = "BaselineRecordingFailed"
	// Baseline has been recorded successfully
	reasonBaselineRecorded = "BaselineRecorded"

	// Single check
	reasonCheckRecording = "CheckRecording"
	reasonCheckFailed    = "CheckRecordingFailed"
	reasonCheckRecorded  = "CheckRecorded"
)

// WorkloadHardeningCheckReconciler reconciles a WorkloadHardeningCheck object
type WorkloadHardeningCheckReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	Recorder     record.EventRecorder
	ValKeyClient *valkey.ValkeyClient
}

// Required to convert "user" to "User", strings.ToTitle converts each rune to title case not just the first one
var titleCase = cases.Title(language.English)

// +kubebuilder:rbac:groups=checks.funk.fhnw.ch,resources=workloadhardeningchecks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=checks.funk.fhnw.ch,resources=workloadhardeningchecks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=checks.funk.fhnw.ch,resources=workloadhardeningchecks/finalizers,verbs=update
// +kubebuilder:rbac:groups=*,resources=*,verbs=*

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
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
			log.Info("WorkloadHardeningCheck deleted, cleaning up resources")

			checkNamespaces := corev1.NamespaceList{}
			err = r.List(
				ctx,
				&checkNamespaces,
				&client.ListOptions{
					LabelSelector: labels.SelectorFromSet(map[string]string{
						"orakel.fhnw.ch/source-namespace": workloadHardening.GetNamespace(),
						"orakel.fhnw.ch/suffix":           workloadHardening.Spec.Suffix,
					}),
				},
			)
			if err != nil {
				log.Error(err, "Failed to list namespaces for cleanup")
				return ctrl.Result{}, err
			}
			if len(checkNamespaces.Items) == 0 {
				log.Info("No namespaces found for cleanup")
				return ctrl.Result{}, nil
			}

			log.Info("Found namespaces for cleanup", "count", len(checkNamespaces.Items))

			for _, ns := range checkNamespaces.Items {
				log.Info("Deleting namespace", "namespace", ns.Name)
				err = r.deleteNamespace(ctx, ns.Name)
				if err != nil {
					log.Error(err, "Failed to delete namespace", "namespace", ns.Name)
				} else {
					log.Info("Deleted namespace", "namespace", ns.Name)
				}
			}

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

	// We use the baseline duration to determine how long we should wait before requeuing the reconciliation
	duration := workloadHardening.GetCheckDuration()

	// Based on the Status, we need to decide what to do next
	// If there is no Baseline recorded yet, we need to start the baseline recording
	if meta.IsStatusConditionTrue(workloadHardening.Status.Conditions, typeWorkloadCheckBaseline) {
		log.Info("Baseline recorded. Start recording different security context configurations")
	} else {
		log.Info("Baseline not recorded yet. Starting baseline recording")
		// Set the condition to indicate that we are starting the baseline recording

		// Refactor to be reusable for other checks, than the baseline
		go r.runCheck(ctx, workloadHardening, "baseline", workloadHardening.Spec.SecurityContext)

		// Requeue the reconciliation after the baseline duration, to continue with the next steps
		return ctrl.Result{RequeueAfter: duration + 10*time.Second}, nil
	}

	// If we are here, it means that the baseline recording is done
	// We can now start recording the workload under test with different security context configurations

	// ToDo: Define the neccessary checks to run, eg. user(runAsUser, runAsNonRoot), group(runAsGroup, fsGroup), roFs(readOnlyFilesystem), seccomp, etc.
	// Does it make sense to first run a check with a fully hardened security context and only if that fails, run the more granular checks?
	// ToDo: Refactor & Move into runner package
	podChecks := []string{"group", "user"}

	if workloadHardening.Spec.RunMode == checksv1alpha1.RunModeParallel {
		log.Info("Running checks in parallel mode")
		// Run all checks in parallel
		for _, checkType := range podChecks {

			if meta.IsStatusConditionTrue(workloadHardening.Status.Conditions, titleCase.String(checkType)+typeWorkloadCheck) {
				log.Info("Check already finished, skipping", "checkType", checkType)
				continue // Skip if the check is already recorded
			}

			if meta.IsStatusConditionFalse(workloadHardening.Status.Conditions, titleCase.String(checkType)+typeWorkloadCheck) {
				log.Info("Check still running, skipping", "checkType", checkType)
				continue // Skip if the check is already recorded
			}

			securityContext := workloadHardening.Spec.SecurityContext.DeepCopy()
			if securityContext == nil {
				securityContext = &checksv1alpha1.SecurityContextDefaults{
					Pod:       &checksv1alpha1.PodSecurityContextDefaults{},
					Container: &checksv1alpha1.ContainerSecurityContextDefaults{},
				}
			}

			if checkType == "user" {
				if securityContext.Pod.RunAsUser == nil {
					securityContext.Pod.RunAsUser = ptr.To(int64(10000))
				}
				if securityContext.Pod.RunAsNonRoot == nil {
					securityContext.Pod.RunAsNonRoot = ptr.To(true)
				}

				if securityContext.Container.RunAsUser == nil {
					securityContext.Container.RunAsUser = ptr.To(int64(10000))
				}
				if securityContext.Container.RunAsNonRoot == nil {
					securityContext.Container.RunAsNonRoot = ptr.To(true)
				}
			}

			if checkType == "group" {
				if securityContext.Pod.RunAsGroup == nil {
					securityContext.Pod.RunAsGroup = ptr.To(int64(10000))
				}
				if securityContext.Pod.FSGroup == nil {
					securityContext.Pod.FSGroup = ptr.To(int64(10000))
				}

				if securityContext.Container.RunAsGroup == nil {
					securityContext.Container.RunAsGroup = ptr.To(int64(10000))
				}
			}
			// If the user is set, we need to set the runAsUser field in the security context
			go r.runCheck(ctx, workloadHardening, checkType, securityContext)
		}

		// Requeue the reconciliation after the baseline duration, to continue with the next steps
		return ctrl.Result{RequeueAfter: duration + 10*time.Second}, nil
	} else {
		log.Info("Running checks in sequential mode")
		// ToDo: How to determnine the order of checks? Which ones are already done?
	}

	// If all checks are done, we can now analyze the recorded signals
	// ToDo: Implement log comparison between checks and baseline
	// ToDo: Use metrics if feasible to support the findings from the logs
	// ToDo: Write the results to the status of the WorkloadHardeningCheck

	// Refactor into a go routine, and check for status updates
	return ctrl.Result{}, nil
}

func (r *WorkloadHardeningCheckReconciler) mergePodSecurityContexts(ctx context.Context, base, extends *corev1.PodSecurityContext) *corev1.PodSecurityContext {
	log := log.FromContext(ctx)

	if base == nil {
		log.Info("Base security context is nil, using override")
		return extends
	}

	if extends == nil {
		log.Info("Override security context is nil, using base")
		return base
	}

	merged := base.DeepCopy()
	if merged.RunAsUser == nil && extends.RunAsUser != nil {
		log.V(3).Info("Merging RunAsUser from override into base")
		merged.RunAsUser = extends.RunAsUser
	}
	if merged.RunAsGroup == nil && extends.RunAsGroup != nil {
		log.V(3).Info("Merging RunAsGroup from override into base")
		merged.RunAsGroup = extends.RunAsGroup
	}
	if merged.FSGroup == nil && extends.FSGroup != nil {
		log.V(3).Info("Merging FSGroup from override into base")
		merged.FSGroup = extends.FSGroup
	}
	if merged.RunAsNonRoot == nil && extends.RunAsNonRoot != nil {
		log.V(3).Info("Merging RunAsNonRoot from override into base")
		merged.RunAsNonRoot = extends.RunAsNonRoot
	}
	if merged.SeccompProfile == nil && extends.SeccompProfile != nil {
		log.V(3).Info("Merging SeccompProfile from override into base")
		merged.SeccompProfile = extends.SeccompProfile
	}
	if merged.SELinuxOptions == nil && extends.SELinuxOptions != nil {
		log.V(3).Info("Merging SELinuxOptions from override into base")
		merged.SELinuxOptions = extends.SELinuxOptions
	}
	if merged.SupplementalGroups == nil && extends.SupplementalGroups != nil {
		log.V(3).Info("Merging SupplementalGroups from override into base")
		merged.SupplementalGroups = extends.SupplementalGroups
	}
	if merged.SupplementalGroupsPolicy == nil && extends.SupplementalGroupsPolicy != nil {
		log.V(3).Info("Merging SupplementalGroupsPolicy from override into base")
		merged.SupplementalGroupsPolicy = extends.SupplementalGroupsPolicy
	}

	return merged
}

func (r *WorkloadHardeningCheckReconciler) mergeContainerSecurityContexts(ctx context.Context, base, extends *corev1.SecurityContext) *corev1.SecurityContext {
	log := log.FromContext(ctx)

	if base == nil {
		log.Info("Base security context is nil, using override")
		return extends
	}

	if extends == nil {
		log.Info("Override security context is nil, using base")
		return base
	}

	merged := base.DeepCopy()
	if merged.RunAsUser == nil && extends.RunAsUser != nil {
		log.V(3).Info("Merging RunAsUser from override into base")
		merged.RunAsUser = extends.RunAsUser
	}
	if merged.RunAsGroup == nil && extends.RunAsGroup != nil {
		log.V(3).Info("Merging RunAsGroup from override into base")
		merged.RunAsGroup = extends.RunAsGroup
	}
	if merged.Capabilities == nil && extends.Capabilities != nil {
		log.V(3).Info("Merging Capabilities from override into base")
		merged.Capabilities = extends.Capabilities
	}
	if merged.Privileged == nil && extends.Privileged != nil {
		log.V(3).Info("Merging Privileged from override into base")
		merged.Privileged = extends.Privileged
	}
	if merged.AllowPrivilegeEscalation == nil && extends.AllowPrivilegeEscalation != nil {
		log.V(3).Info("Merging AllowPrivilegeEscalation from override into base")
		merged.AllowPrivilegeEscalation = extends.AllowPrivilegeEscalation
	}
	if merged.ReadOnlyRootFilesystem == nil && extends.ReadOnlyRootFilesystem != nil {
		log.V(3).Info("Merging ReadOnlyRootFilesystem from override into base")
		merged.ReadOnlyRootFilesystem = extends.ReadOnlyRootFilesystem
	}
	if merged.SeccompProfile == nil && extends.SeccompProfile != nil {
		log.V(3).Info("Merging SeccompProfile from override into base")
		merged.SeccompProfile = extends.SeccompProfile
	}
	if merged.SELinuxOptions == nil && extends.SELinuxOptions != nil {
		log.V(3).Info("Merging SELinuxOptions from override into base")
		merged.SELinuxOptions = extends.SELinuxOptions
	}

	return merged
}

func (r *WorkloadHardeningCheckReconciler) runCheck(ctx context.Context, workloadHardening *checksv1alpha1.WorkloadHardeningCheck, checkType string, securityContext *checksv1alpha1.SecurityContextDefaults) {
	var conditionType string
	var conditionReason string
	log := log.FromContext(ctx).WithValues("checkType", checkType)

	if checkType == "baseline" {
		conditionType = typeWorkloadCheckBaseline
	} else {
		conditionType = titleCase.String(checkType) + typeWorkloadCheck
	}

	targetNamespaceName := generateTargetNamespaceName(*workloadHardening, checkType)

	log = log.WithValues("targetNamespace", targetNamespaceName)

	if r.namespaceExists(ctx, targetNamespaceName) {
		if meta.IsStatusConditionPresentAndEqual(
			workloadHardening.Status.Conditions,
			titleCase.String(checkType)+typeWorkloadCheck,
			metav1.ConditionUnknown,
		) {
			log.V(1).Info(
				"Target namespace already exists, and check is in unknown state. Most likely a previous run failed",
			)
		}

	} else {

		// clone into target namespace
		err := r.createCheckNamespace(ctx, workloadHardening, targetNamespaceName)
		if err != nil {
			log.Error(err, "failed to create target namespace for baseline recording")
			return
		}

		log.Info("created namespace")

		time.Sleep(1 * time.Second) // Give the namespace some time to be fully created and ready
	}

	// Fetch the workload we want to test, make sure we fetch it from the target namespace
	workload, err := r.getWorkloadUnderTest(ctx, targetNamespaceName, workloadHardening.Spec.TargetRef.Name, workloadHardening.Spec.TargetRef.Kind)
	if err != nil {
		log.Error(err, "failed to get workload under test",
			"workloadName", workloadHardening.Spec.TargetRef.Name,
		)
		return
	}

	log.Info("applying security context to workload under test",
		"workloadName", workloadHardening.Spec.TargetRef.Name,
	)

	if securityContext != nil && securityContext.Pod != nil {
		// convert workloadUnderTest to the correct type
		switch v := (*workload).(type) {
		case *appsv1.Deployment:
			currentSecurityContext := v.Spec.Template.Spec.SecurityContext.DeepCopy()
			v.Spec.Template.Spec.SecurityContext = r.mergePodSecurityContexts(ctx, currentSecurityContext, securityContext.Pod.ToK8sSecurityContext())
		case *appsv1.StatefulSet:
			currentSecurityContext := v.Spec.Template.Spec.SecurityContext.DeepCopy()
			v.Spec.Template.Spec.SecurityContext = r.mergePodSecurityContexts(ctx, currentSecurityContext, securityContext.Pod.ToK8sSecurityContext())
		case *appsv1.DaemonSet:
			currentSecurityContext := v.Spec.Template.Spec.SecurityContext.DeepCopy()
			v.Spec.Template.Spec.SecurityContext = r.mergePodSecurityContexts(ctx, currentSecurityContext, securityContext.Pod.ToK8sSecurityContext())
		}

	}

	if securityContext != nil && securityContext.Container != nil {
		// convert workloadUnderTest to the correct type
		switch v := (*workload).(type) {
		case *appsv1.Deployment:
			for i := range v.Spec.Template.Spec.Containers {
				currentSecurityContext := v.Spec.Template.Spec.Containers[i].SecurityContext.DeepCopy()
				v.Spec.Template.Spec.Containers[i].SecurityContext = r.mergeContainerSecurityContexts(ctx, currentSecurityContext, securityContext.Container.ToK8sSecurityContext())
			}
			for i := range v.Spec.Template.Spec.InitContainers {
				currentSecurityContext := v.Spec.Template.Spec.InitContainers[i].SecurityContext.DeepCopy()
				v.Spec.Template.Spec.InitContainers[i].SecurityContext = r.mergeContainerSecurityContexts(ctx, currentSecurityContext, securityContext.Container.ToK8sSecurityContext())
			}
		case *appsv1.StatefulSet:
			for i := range v.Spec.Template.Spec.Containers {
				currentSecurityContext := v.Spec.Template.Spec.Containers[i].SecurityContext.DeepCopy()
				v.Spec.Template.Spec.Containers[i].SecurityContext = r.mergeContainerSecurityContexts(ctx, currentSecurityContext, securityContext.Container.ToK8sSecurityContext())
			}
			for i := range v.Spec.Template.Spec.InitContainers {
				currentSecurityContext := v.Spec.Template.Spec.InitContainers[i].SecurityContext.DeepCopy()
				v.Spec.Template.Spec.InitContainers[i].SecurityContext = r.mergeContainerSecurityContexts(ctx, currentSecurityContext, securityContext.Container.ToK8sSecurityContext())
			}
		case *appsv1.DaemonSet:
			for i := range v.Spec.Template.Spec.Containers {
				currentSecurityContext := v.Spec.Template.Spec.Containers[i].SecurityContext.DeepCopy()
				v.Spec.Template.Spec.Containers[i].SecurityContext = r.mergeContainerSecurityContexts(ctx, currentSecurityContext, securityContext.Container.ToK8sSecurityContext())
			}
			for i := range v.Spec.Template.Spec.InitContainers {
				currentSecurityContext := v.Spec.Template.Spec.InitContainers[i].SecurityContext.DeepCopy()
				v.Spec.Template.Spec.InitContainers[i].SecurityContext = r.mergeContainerSecurityContexts(ctx, currentSecurityContext, securityContext.Container.ToK8sSecurityContext())
			}
		}
	}

	err = r.Update(ctx, *workload)
	if err != nil {
		log.Error(err, "failed to update workload under test with security context",
			"workloadName", workloadHardening.Spec.TargetRef.Name,
		)
		if checkType == "baseline" {
			conditionReason = reasonBaselineRecording

		} else {
			conditionReason = titleCase.String(checkType) + reasonCheckRecording
		}

		r.setCondition(ctx, workloadHardening, metav1.Condition{
			Type:    conditionType,
			Status:  metav1.ConditionUnknown,
			Reason:  conditionReason,
			Message: "Failed to apply security context to workload",
		})
		return
	}
	log.Info("applied security context to workload under test",
		"workloadName", workloadHardening.Spec.TargetRef.Name,
	)

	// ToDo: detect if pods are able to get into a running state, otherwise we cannot record metrics and it's clear this has failed
	running := false
	startTime := metav1.Now()
	for !running {
		r.Get(ctx, types.NamespacedName{Namespace: targetNamespaceName, Name: workloadHardening.Spec.TargetRef.Name}, *workload)
		running, _ = wh.VerifySuccessfullyRunning(*workload)

		if time.Now().After(startTime.Add(2 * time.Minute)) {
			log.Error(fmt.Errorf("timeout while waiting for workload to be running"), "timeout while waiting for workload to be running",
				"targetNamespace", targetNamespaceName,
				"checkType", checkType,
				"workloadName", workloadHardening.Spec.TargetRef.Name,
			)
			if checkType == "baseline" {
				conditionReason = reasonBaselineFailed
			} else {
				conditionReason = titleCase.String(checkType) + reasonCheckFailed
			}
			r.setCondition(ctx, workloadHardening, metav1.Condition{
				Type:    conditionType,
				Status:  metav1.ConditionTrue,
				Reason:  conditionReason,
				Message: "Timeout while waiting for workload to be running",
			})
			r.Recorder.Event(
				workloadHardening,
				corev1.EventTypeWarning,
				conditionReason,
				fmt.Sprintf(
					"%sCheck: Timeout while waiting for workloads to be running in namespace %s",
					checkType,
					targetNamespaceName,
				),
			)

			//ToDo: Fetch pod events & maybe pod logs, to add context to the failure

			err = r.ValKeyClient.StoreRecording(
				ctx,
				// prefix with original namespace to avoid conflict if suffix is reused
				workloadHardening.GetNamespace()+":"+workloadHardening.Spec.Suffix,
				&runner.WorkloadRecording{
					Type:                          checkType,
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
				"workloadName", workloadHardening.Spec.TargetRef.Name,
			)
			time.Sleep(5 * time.Second) // Wait for 5 seconds before checking again
		}
	}

	log.Info("workload is running",
		"workloadName", workloadHardening.Spec.TargetRef.Name,
	)

	if checkType == "baseline" {
		conditionReason = reasonBaselineRecording
	} else {
		conditionReason = titleCase.String(checkType) + reasonCheckRecording
	}

	err = r.setCondition(ctx, workloadHardening, metav1.Condition{
		Type:    conditionType,
		Status:  metav1.ConditionTrue,
		Reason:  conditionReason,
		Message: "Recording signals",
	})

	if err != nil {
		log.Error(err, "failed to set condition for check")
		return
	}

	// start recording metrics for target workload
	workloadRecording, err := r.recordSignals(ctx, workloadHardening, targetNamespaceName, checkType, securityContext)
	if err != nil {
		log.Error(err, "failed to record signals")

		if checkType == "baseline" {
			conditionReason = reasonBaselineFailed
		} else {
			conditionReason = titleCase.String(checkType) + reasonCheckFailed
		}

		r.setCondition(ctx, workloadHardening, metav1.Condition{
			Type:    conditionType,
			Status:  metav1.ConditionTrue,
			Reason:  conditionReason,
			Message: "Failed to record signals",
		})

		return
	}

	err = r.ValKeyClient.StoreRecording(
		ctx,
		// prefix with original namespace to avoid conflict if suffix is reused
		workloadHardening.GetNamespace()+":"+workloadHardening.Spec.Suffix,
		workloadRecording,
	)

	if err != nil {
		log.Error(err, "failed to store workload recording in Valkey")
	}

	log.Info("recorded signals")

	if checkType == "baseline" {
		conditionReason = reasonBaselineRecorded
	} else {
		conditionReason = titleCase.String(checkType) + reasonCheckRecorded
	}

	err = r.setCondition(ctx, workloadHardening, metav1.Condition{
		Type:    conditionType,
		Status:  metav1.ConditionTrue,
		Reason:  conditionReason,
		Message: "Signals recorded successfully",
	})

	r.Recorder.Event(
		workloadHardening,
		corev1.EventTypeNormal,
		conditionReason,
		fmt.Sprintf(
			"Recorded %sCheck signals in namespace %s",
			checkType,
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

func (r *WorkloadHardeningCheckReconciler) namespaceExists(ctx context.Context, namespaceName string) bool {
	targetNs := &corev1.Namespace{}
	err := r.Get(ctx, client.ObjectKey{Name: namespaceName}, targetNs)

	return !apierrors.IsNotFound(err)
}

func (r *WorkloadHardeningCheckReconciler) deleteNamespace(ctx context.Context, namespaceName string) error {
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

func (r *WorkloadHardeningCheckReconciler) recordSignals(ctx context.Context, workloadHardening *checksv1alpha1.WorkloadHardeningCheck, targetNamespace, recordingType string, securityContext *checksv1alpha1.SecurityContextDefaults) (*runner.WorkloadRecording, error) {
	log := log.FromContext(ctx).WithValues(
		"targetNamespace", targetNamespace,
		"recordingType", recordingType,
		"workloadName", workloadHardening.Spec.TargetRef.Name,
	)

	labelSelector, err := r.getLabelSelector(ctx, workloadHardening)
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
	duration, _ := time.ParseDuration(workloadHardening.Spec.BaselineDuration)

	// Initialize the channels with the expected capacity to avoid blocking
	metricsChannel := make(chan *runner.RecordedMetrics, len(pods.Items))
	// As we collect logs per container, we need to multiply the number of pods with the number of containers in each pod
	logsChannel := make(chan string, len(pods.Items)*len(pods.Items[0].Spec.Containers))

	startTime := metav1.Now()

	for _, pod := range pods.Items {
		go func() {
			// the metrics are collected per pod
			recordedMetrics, err := runner.RecordMetrics(ctx, &pod, int(duration.Seconds()), 15)
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

				logs, err := runner.GetLogs(ctx, &pod, container.Name)
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

	resourceUsageRecords := []runner.ResourceUsageRecord{}
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

	workloadRecording := runner.WorkloadRecording{
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

func (r *WorkloadHardeningCheckReconciler) getLabelSelector(ctx context.Context, workloadHardening *checksv1alpha1.WorkloadHardeningCheck) (labels.Selector, error) {
	workloadUnderTest, err := r.getWorkloadUnderTest(ctx, workloadHardening.Namespace, workloadHardening.Spec.TargetRef.Name, workloadHardening.Spec.TargetRef.Kind)
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

func (r *WorkloadHardeningCheckReconciler) createCheckNamespace(ctx context.Context, workloadHardening *checksv1alpha1.WorkloadHardeningCheck, targetNamespace string) error {
	log := log.FromContext(ctx)

	err := runner.CloneNamespace(ctx, workloadHardening.Namespace, targetNamespace, workloadHardening.Spec.Suffix)

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

	// Set the owner reference of the cloned namespace to the workload hardening check
	err = ctrl.SetControllerReference(
		workloadHardening,
		targetNs,
		r.Scheme,
	)

	if err != nil {
		log.Error(err, "failed to set controller reference for target namespace")
	}

	return nil

}

func (r *WorkloadHardeningCheckReconciler) getWorkloadUnderTest(ctx context.Context,
	namespace, name, kind string,
) (*client.Object, error) {
	log := log.FromContext(ctx)

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

	err := r.Get(
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

	workloadUnderTest, err := r.getWorkloadUnderTest(ctx, workloadHardening.Namespace, workloadHardening.Spec.TargetRef.Name, workloadHardening.Spec.TargetRef.Kind)
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

	var err error
	retryCount := 0

	// retry 3 times to update the status of the WorkloadHardeningCheck, to avoid concurrent updates failing
	for retryCount < 3 {
		// Let's re-fetch the workload hardening check Custom Resource after updating the status so that we have the latest state
		if err = r.Get(ctx, types.NamespacedName{Name: workloadHardening.Name, Namespace: workloadHardening.Namespace}, workloadHardening); err != nil {
			log.Error(err, "Failed to re-fetch WorkloadHardeningCheck")
			return err
		}

		// Set/Update condition
		meta.SetStatusCondition(
			&workloadHardening.Status.Conditions,
			condition,
		)

		if err := r.Status().Update(ctx, workloadHardening); err != nil {
			log.V(3).Info("Failed to update WorkloadHardeningCheck status, retrying")
			retryCount++
			continue // Retry updating the status
		} else {
			break
		}

	}

	return err
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
