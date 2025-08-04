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

package namespace

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	checksv1alpha1 "github.com/fhnw-imvs/fhnw-kubeseccontext/api/v1alpha1"
	"github.com/fhnw-imvs/fhnw-kubeseccontext/internal/runner"
)

// NamespaceHardeningCheckReconciler reconciles a NamespaceHardeningCheck object
type NamespaceHardeningCheckReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=checks.funk.fhnw.ch,resources=namespacehardeningchecks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=checks.funk.fhnw.ch,resources=namespacehardeningchecks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=checks.funk.fhnw.ch,resources=namespacehardeningchecks/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the NamespaceHardeningCheck object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.21.0/pkg/reconcile
func (r *NamespaceHardeningCheckReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logf.FromContext(ctx).WithName("NamespaceHardeningCheckReconciler")

	// Get Resource
	// Fetch the NamespaceHardeningCheck instance
	namespaceHardening := &checksv1alpha1.NamespaceHardeningCheck{}
	err := r.Get(ctx, req.NamespacedName, namespaceHardening)
	if err != nil {
		// If the resource is not found it's usually because it was deleted, we need to cleanup remaining resources
		if apierrors.IsNotFound(err) {
			return r.cleanupReconcileLoop(ctx, req.Namespace)
		}
		// Error reading the object - requeue the request.
		logger.Error(err, "Failed to get NamespaceHardeningCheck, requeing")
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, err
	}

	// ConditionFinished is set to true when the reconciliation is finished
	if meta.IsStatusConditionTrue(namespaceHardening.Status.Conditions, checksv1alpha1.ConditionTypeFinished) {
		logger.Info("NamespaceHardeningCheck is already finished, skipping reconciliation")
		return ctrl.Result{}, nil
	}

	// Validate the namespace referenced exists => Move to validation webhook
	if namespaceHardening.Spec.TargetNamespace == "" {
		logger.Error(nil, "NamespaceHardeningCheck has no namespace specified, skipping reconciliation")
		return ctrl.Result{}, nil
	}

	targetNamespace := corev1.Namespace{}
	err = r.Get(ctx, client.ObjectKey{Name: namespaceHardening.Spec.TargetNamespace}, &targetNamespace)
	if err != nil {
		if apierrors.IsNotFound(err) {
			logger.Error(err, "Target namespace for NamespaceHardeningCheck not found, aborting reconciliation")

			r.SetCondition(ctx, namespaceHardening, metav1.Condition{
				Type:    checksv1alpha1.ConditionTypeFinished,
				Status:  metav1.ConditionFalse,
				Reason:  checksv1alpha1.ReasonTargetNamespaceNotFound,
				Message: "The target namespace for the NamespaceHardeningCheck does not exist",
			})

			return ctrl.Result{}, nil
		}

		// Error reading the object - requeue the request.
		logger.Error(err, "Failed to get target namespace for NamespaceHardeningCheck, requeuing")

		return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
	}

	// Check if the NamespaceHardeningCheck is already in progress
	if meta.FindStatusCondition(namespaceHardening.Status.Conditions, checksv1alpha1.ConditionTypeFinished) == nil {
		// Initial run...
		logger.Info("Starting NamespaceHardeningCheck reconciliation", "namespace", namespaceHardening.Spec.TargetNamespace)
		r.SetCondition(ctx, namespaceHardening, metav1.Condition{
			Type:    checksv1alpha1.ConditionTypeFinished,
			Status:  metav1.ConditionFalse,
			Reason:  checksv1alpha1.ConditionTypePreparation,
			Message: "Starting NamespaceHardeningCheck reconciliation, preparing to create WorkloadHardeningChecks",
		})
	}

	topLevelResources, err := r.getTopLevelResourcesToCheck(ctx, namespaceHardening)
	if err != nil {
		logger.Error(err, "Failed to get top-level resources in target namespace", "namespace", namespaceHardening.Spec.TargetNamespace)

		// ToDo: Need to handle the "no top-level resources found" case differently
		r.SetCondition(ctx, namespaceHardening, metav1.Condition{
			Type:    checksv1alpha1.ConditionTypeFinished,
			Status:  metav1.ConditionTrue,
			Reason:  checksv1alpha1.ReasonAnalysisFailed,
			Message: "Failed to get top-level resources in target namespace",
		})
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, err
	}

	// Filter topLevelResoruces for those compatible with WorkloadHardeningCheck
	if len(topLevelResources) == 0 {
		logger.Info("No top-level resources found in target namespace, skipping WorkloadHardeningCheck creation",
			"namespace", namespaceHardening.Spec.TargetNamespace)
		r.SetCondition(ctx, namespaceHardening, metav1.Condition{
			Type:    checksv1alpha1.ConditionTypeFinished,
			Status:  metav1.ConditionTrue,
			Reason:  checksv1alpha1.ReasonAnalysisFinished,
			Message: "No top-level resources found in target namespace, skipping WorkloadHardeningCheck creation",
		})
		return ctrl.Result{}, nil
	}

	workloadChecks := []*checksv1alpha1.WorkloadHardeningCheck{}
	for _, resource := range topLevelResources {
		logger.Info("Found top-level resource to check", "kind", resource.GetKind(), "name", resource.GetName(), "namespace", namespaceHardening.Spec.TargetNamespace)
		// Create a WorkloadHardeningCheck for each top-level resource
		workloadCheck, err := r.createWorkloadHardeningCheck(ctx, namespaceHardening, resource)
		if err != nil {
			logger.Error(err, "Failed to create WorkloadHardeningCheck for top-level resource",
				"kind", resource.GetKind(), "name", resource.GetName(), "namespace", namespaceHardening.Spec.TargetNamespace)
			r.Recorder.Eventf(namespaceHardening, corev1.EventTypeWarning, "Failed",
				"Failed to create WorkloadHardeningCheck for %s/%s in namespace %s: %v",
				resource.GetKind(), resource.GetName(), namespaceHardening.Spec.TargetNamespace, err)

			continue // Skip this resource and continue with the next one
		}
		workloadChecks = append(workloadChecks, workloadCheck)

	}

	if len(workloadChecks) == 0 {
		logger.Info("No WorkloadHardeningChecks created as they already exist",
			"namespace", namespaceHardening.Spec.TargetNamespace)
	} else {
		r.Recorder.Eventf(namespaceHardening, corev1.EventTypeNormal, "WorkloadChecksCreated",
			"Created %d WorkloadHardeningChecks for top-level resources in namespace %s",
			len(workloadChecks), namespaceHardening.Spec.TargetNamespace)

		r.SetCondition(ctx, namespaceHardening, metav1.Condition{
			Type:   checksv1alpha1.ConditionTypeFinished,
			Status: metav1.ConditionFalse,
			Reason: checksv1alpha1.ReasonWorkloadChecksCreated,
			Message: fmt.Sprintf("Created %d WorkloadHardeningChecks for top-level resources in namespace %s",
				len(workloadChecks), namespaceHardening.Spec.TargetNamespace),
		})
	}

	// check if all WorkloadHardeningChecks in the namespace are finished
	// If not, we can return and wait for the next reconciliation loop
	workloadCheckList := &checksv1alpha1.WorkloadHardeningCheckList{}
	err = r.List(ctx, workloadCheckList, client.InNamespace(namespaceHardening.Spec.TargetNamespace))
	if err != nil {
		logger.Error(err, "Failed to list WorkloadHardeningChecks in target namespace", "namespace", namespaceHardening.Spec.TargetNamespace)
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, err
	}

	// Check if all WorkloadHardeningChecks are finished
	allFinished := true
	finishedCount := 0
	for _, check := range workloadCheckList.Items {
		if !meta.IsStatusConditionTrue(check.Status.Conditions, checksv1alpha1.ConditionTypeFinished) {
			allFinished = false
			logger.Info("WorkloadHardeningCheck is not finished", "check", check.Name, "namespace", check.Namespace)
		} else {
			finishedCount++
		}
	}
	if !allFinished {
		logger.Info("Not all WorkloadHardeningChecks are finished, waiting for next reconciliation loop",
			"namespace", namespaceHardening.Spec.TargetNamespace)

		r.SetCondition(ctx, namespaceHardening, metav1.Condition{
			Type:    checksv1alpha1.ConditionTypeFinished,
			Status:  metav1.ConditionFalse,
			Reason:  checksv1alpha1.ReasonNamespaceInProgress,
			Message: fmt.Sprintf("NamespaceHardeningCheck is in progress, %d/%d finished", finishedCount, len(topLevelResources)),
		})
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
	}

	// All WorkloadHardeningChecks are finished, we can finalize the NamespaceHardeningCheck
	logger.Info("All WorkloadHardeningChecks are finished, finalizing NamespaceHardeningCheck",
		"namespace", namespaceHardening.Spec.TargetNamespace)

	recommendations := make(map[string]*checksv1alpha1.Recommendation)
	for _, check := range workloadCheckList.Items {
		if check.Status.Recommendation != nil {
			recommendations[check.Spec.TargetRef.Kind+"/"+check.Spec.TargetRef.Name] = check.Status.Recommendation
		}
	}

	retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Re-fetch the NamespaceHardeningCheck instance to ensure we have the latest state
		if err := r.Get(ctx, req.NamespacedName, namespaceHardening); err != nil {
			if apierrors.IsNotFound(err) {
				logger.Info("NamespaceHardeningCheck not found, skipping finalization")
				return nil // If the resource is not found, we can skip the update
			}
			logger.Error(err, "Failed to re-fetch NamespaceHardeningCheck for finalization")
			return err
		}

		namespaceHardening.Status.Recommendations = recommendations

		meta.SetStatusCondition(&namespaceHardening.Status.Conditions, metav1.Condition{
			Type:    checksv1alpha1.ConditionTypeFinished,
			Status:  metav1.ConditionTrue,
			Reason:  checksv1alpha1.ReasonAnalysisFinished,
			Message: "Finished, recommendations ready",
		})
		return r.Status().Update(ctx, namespaceHardening)
	})

	// TODO(user): your logic here

	return ctrl.Result{}, nil
}

func (r *NamespaceHardeningCheckReconciler) createWorkloadHardeningCheck(ctx context.Context, namespaceHardeningCheck *checksv1alpha1.NamespaceHardeningCheck, resource *unstructured.Unstructured) (*checksv1alpha1.WorkloadHardeningCheck, error) {
	logger := log.FromContext(ctx)

	workloadCheck := &checksv1alpha1.WorkloadHardeningCheck{}
	if err := r.Get(ctx, client.ObjectKey{
		Name:      strings.ToLower(resource.GetKind() + "-" + resource.GetName() + "-" + namespaceHardeningCheck.Spec.Suffix),
		Namespace: namespaceHardeningCheck.Spec.TargetNamespace,
	}, workloadCheck); err == nil {
		// WorkloadHardeningCheck already exists, we can skip creating it
		logger.Info("WorkloadHardeningCheck already exists, skipping creation", "workload", resource.GetName(), "namespace", namespaceHardeningCheck.Spec.TargetNamespace)
		return workloadCheck, nil
	} else {
		if !apierrors.IsNotFound(err) {
			logger.Error(err, "Failed to get existing WorkloadHardeningCheck", "workload", resource.GetName(), "namespace", namespaceHardeningCheck.Spec.TargetNamespace)
			return nil, fmt.Errorf("failed to get existing WorkloadHardeningCheck for %s/%s in namespace %s: %w",
				resource.GetKind(), resource.GetName(), namespaceHardeningCheck.Spec.TargetNamespace, err)
		}

		// If the WorkloadHardeningCheck does not exist, we will create it
		logger.Info("WorkloadHardeningCheck not found, creating new one", "workload", resource.GetName(), "namespace", namespaceHardeningCheck.Spec.TargetNamespace)
	}

	// Create a new WorkloadHardeningCheck for each top-level resource
	workloadCheck = &checksv1alpha1.WorkloadHardeningCheck{
		ObjectMeta: metav1.ObjectMeta{
			Name:      strings.ToLower(resource.GetKind() + "-" + resource.GetName() + "-" + namespaceHardeningCheck.Spec.Suffix),
			Namespace: namespaceHardeningCheck.Spec.TargetNamespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       strings.ToLower(resource.GetKind() + "-" + resource.GetName() + "-" + namespaceHardeningCheck.Spec.Suffix),
				"app.kubernetes.io/managed-by": "oracle-of-funk",
				"appkubernetes.io/part-of":     namespaceHardeningCheck.Name,
			},
		},
		Spec: checksv1alpha1.WorkloadHardeningCheckSpec{
			Suffix: namespaceHardeningCheck.Spec.Suffix + "-" + utilrand.String(8), // Generate a random suffix of 8 characters
			TargetRef: checksv1alpha1.TargetReference{
				Kind: resource.GetKind(),
				Name: resource.GetName(),
			},
			RecordingDuration: namespaceHardeningCheck.Spec.RecordingDuration,
			RunMode:           namespaceHardeningCheck.Spec.RunMode,
			SecurityContext:   namespaceHardeningCheck.Spec.SecurityContext.DeepCopy(),
		},
	}

	// set owner reference to the NamespaceHardeningCheck
	if err := ctrl.SetControllerReference(namespaceHardeningCheck, workloadCheck, r.Scheme); err != nil {
		logger.Error(err, "Failed to set controller reference for WorkloadHardeningCheck", "workload", workloadCheck.Spec.TargetRef.Name, "namespace", namespaceHardeningCheck.Spec.TargetNamespace)
	}

	// Create the WorkloadHardeningCheck
	if err := r.Create(ctx, workloadCheck); err != nil {
		logger.Error(err, "Failed to create WorkloadHardeningCheck", "workload", workloadCheck.Spec.TargetRef.Name, "namespace", namespaceHardeningCheck.Spec.TargetNamespace)
		return nil, fmt.Errorf("failed to create WorkloadHardeningCheck for %s/%s in namespace %s: %w",
			workloadCheck.Spec.TargetRef.Kind, workloadCheck.Spec.TargetRef.Name, namespaceHardeningCheck.Spec.TargetNamespace, err)
	}

	logger.Info("Created WorkloadHardeningCheck", "workload", workloadCheck.Spec.TargetRef.Name, "namespace", namespaceHardeningCheck.Spec.TargetNamespace)

	return workloadCheck, nil
}

func (r *NamespaceHardeningCheckReconciler) getTopLevelResourcesToCheck(ctx context.Context, namespaceHardeningCheck *checksv1alpha1.NamespaceHardeningCheck) ([]*unstructured.Unstructured, error) {
	logger := logf.FromContext(ctx).WithName("getTopLevelResourcesToCheck")

	// Fetch all top-level resources in the target namespace
	allTopLevelResources, err := runner.GetTopLevelResources(ctx, namespaceHardeningCheck.Spec.TargetNamespace)
	if err != nil {
		logger.Error(err, "Failed to get top-level resources in target namespace", "namespace", namespaceHardeningCheck.Spec.TargetNamespace)
		return nil, err
	}

	if len(allTopLevelResources) == 0 {
		logger.Info("No top-level resources found in target namespace", "namespace", namespaceHardeningCheck.Spec.TargetNamespace)
		return nil, fmt.Errorf("no top-level resources found in target namespace %s", namespaceHardeningCheck.Spec.TargetNamespace)
	}

	// Filter for resources that are compatible with WorkloadHardeningCheck,
	// Currently supported are:
	// - Deployments
	// - StatefulSets
	// - DaemonSets
	usableResources := []*unstructured.Unstructured{}
	for _, resource := range allTopLevelResources {
		// Check if the resource is a supported workload type
		switch resource.GetKind() {
		case "Deployment", "StatefulSet", "DaemonSet":
			// These are supported workload types
			usableResources = append(usableResources, resource)
		default:
			// Unsupported workload type, we can skip it
			logger.Info("Skipping unsupported workload type", "kind", resource.GetKind(), "name", resource.GetName(), "namespace", namespaceHardeningCheck.Spec.TargetNamespace)
		}
	}

	return usableResources, nil
}

func (r *NamespaceHardeningCheckReconciler) createWorkloadHardeningChecks(ctx context.Context, namespaceHardeningCheck *checksv1alpha1.NamespaceHardeningCheck) ([]*checksv1alpha1.WorkloadHardeningCheck, error) {
	logger := logf.FromContext(ctx).WithName("CreateWorkloadHardeningChecks")

	// Fetch all workloads in the target namespace, to do this we fetch all pods, and from there get their parent resources until we get the unowned resource (e.g. Deployment, StatefulSet, DaemonSet, etc.)
	topLevelResources, err := r.getTopLevelResourcesToCheck(ctx, namespaceHardeningCheck)
	if err != nil {
		logger.Error(err, "Failed to get top-level resources in target namespace", "namespace", namespaceHardeningCheck.Spec.TargetNamespace)
		return nil, err
	}

	// Filter topLevelResoruces for those compatible with WorkloadHardeningCheck
	if len(topLevelResources) == 0 {
		logger.Info("No top-level resources found in target namespace, skipping WorkloadHardeningCheck creation",
			"namespace", namespaceHardeningCheck.Spec.TargetNamespace)
		r.SetCondition(ctx, namespaceHardeningCheck, metav1.Condition{
			Type:    checksv1alpha1.ConditionTypeFinished,
			Status:  metav1.ConditionTrue,
			Reason:  checksv1alpha1.ReasonAnalysisFinished,
			Message: "No top-level resources found in target namespace, skipping WorkloadHardeningCheck creation",
		})
		return nil, fmt.Errorf("no top-level resources found in target namespace %s", namespaceHardeningCheck.Spec.TargetNamespace)
	}

	workloadChecks := []*checksv1alpha1.WorkloadHardeningCheck{}
	for _, resource := range topLevelResources {
		workloadCheck, _ := r.createWorkloadHardeningCheck(ctx, namespaceHardeningCheck, resource)
		workloadChecks = append(workloadChecks, workloadCheck)
	}

	if len(workloadChecks) == 0 {
		logger.Info("Checks for all top-level resources in target namespace already exist, skipping creation",
			"namespace", namespaceHardeningCheck.Spec.TargetNamespace)
		return nil, nil
	}

	logger.Info("Created WorkloadHardeningChecks for all top-level resources in target namespace",
		"namespace", namespaceHardeningCheck.Spec.TargetNamespace, "count", len(workloadChecks))

	return workloadChecks, nil
}

func (r *NamespaceHardeningCheckReconciler) SetCondition(ctx context.Context, namespaceHardeningCheck *checksv1alpha1.NamespaceHardeningCheck, condition metav1.Condition) error {
	logger := logf.FromContext(ctx).WithName("NamespaceHardeningCheckReconciler")

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {

		// Let's re-fetch the workload hardening check Custom Resource after updating the status so that we have the latest state
		if err := r.Get(ctx, client.ObjectKey{Name: namespaceHardeningCheck.Name}, namespaceHardeningCheck); err != nil {
			if apierrors.IsNotFound(err) {
				// workloadHardeningCheck resource was deleted, while a check was running
				logger.Info("WorkloadHardeningCheck not found, skipping condition update")
				return nil // If the resource is not found, we can skip the update
			}
			logger.Error(err, "Failed to re-fetch WorkloadHardeningCheck")
			return err
		}

		// Set/Update condition
		meta.SetStatusCondition(
			&namespaceHardeningCheck.Status.Conditions,
			condition,
		)

		return r.Status().Update(ctx, namespaceHardeningCheck)

	})

	return err
}

// Called if the NamespaceHardeningCheck instance is removed or deleted and we need to clean up the resources
func (r *NamespaceHardeningCheckReconciler) cleanupReconcileLoop(ctx context.Context, sourceNamespace string) (ctrl.Result, error) {
	_ = log.FromContext(ctx).WithName("cleanupReconcileLoop")

	// Delete all WorkloadHardeningCheck instances in the namespace

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *NamespaceHardeningCheckReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&checksv1alpha1.NamespaceHardeningCheck{}).
		Owns(&checksv1alpha1.WorkloadHardeningCheck{}).
		// ToDo: Decide if configurable
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		WithEventFilter(ignoreStatusChanges()).
		Named("namespacehardeningcheck").
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
