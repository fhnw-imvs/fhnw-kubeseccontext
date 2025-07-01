package workload

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	checksv1alpha1 "github.com/fhnw-imvs/fhnw-kubeseccontext/api/v1alpha1"
)

type WorkloadHandler struct {
	client.Client

	l logr.Logger

	w *checksv1alpha1.WorkloadHardeningCheck
}

func NewWorkloadHandler(ctx context.Context, workloadHardeningCheck *checksv1alpha1.WorkloadHardeningCheck) *WorkloadHandler {
	l := log.FromContext(ctx).WithName("WorkloadHandler")
	config, err := ctrl.GetConfig()
	if err != nil {
		l.Error(err, "Failed to get runtime client config")
	}
	c, err := client.New(config, client.Options{})
	if err != nil {
		l.Error(err, "Failed to create client")
	}

	return &WorkloadHandler{
		Client: c,
		l:      l,
		w:      workloadHardeningCheck,
	}

}

func (h *WorkloadHandler) GetWorkloadUnderTest(ctx context.Context) (*client.Object, error) {

	namespace := h.w.GetNamespace()
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

func (h *WorkloadHandler) VerifyRunning(ctx context.Context) (bool, error) {
	workloadUnderTestPtr, err := h.GetWorkloadUnderTest(ctx)
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
