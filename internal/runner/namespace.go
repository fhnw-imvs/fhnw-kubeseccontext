package runner

import (
	"context"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// ToDo: Decide if it's better to use an deny- or allow- list to clone the namespace
var resourcesToSkip = []string{
	"bindings",
	"localsubjectaccessreviews",
	"endpointslices",
	"endpoints",
	"events",
	"podmetrics",
	"controllerrevisions",
	// exclude our own resource to avoid infinite loops
	"workloadhardeningchecks",
}

func CloneNamespace(ctx context.Context, sourceNamespace, targetNamespace, suffix string) error {
	log := log.FromContext(ctx).WithName("runner")

	// check if targetNamespace already exists
	cl, err := client.New(config.GetConfigOrDie(), client.Options{})
	if err != nil {
		return fmt.Errorf("failed to create client")
	}
	targetNs := &corev1.Namespace{}
	err = cl.Get(ctx, client.ObjectKey{Name: targetNamespace}, targetNs)

	// we expect at least a NotFound error here, otherwise the namespace already exists, and we don't want to override it
	if err == nil {
		return fmt.Errorf("target namespace already exists %s", targetNamespace)
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("error fetching namespace: %w", err)
	}

	// only get resources without ownership
	resources, err := getTopLevelResources(ctx, sourceNamespace)
	if err != nil {
		return fmt.Errorf("unable to fetch all resources from %s", sourceNamespace)
	}

	// create targetNamespace
	targetNs.Name = targetNamespace
	targetNs.Labels = map[string]string{
		"app.kubernetes.io/name":          targetNamespace,
		"app.kubernetes.io/managed-by":    "oracle-of-funk",
		"orakel.fhnw.ch/source-namespace": sourceNamespace,
		"orakel.fhnw.ch/suffix":           suffix,
	}

	err = cl.Create(ctx, targetNs)
	if err != nil {
		return fmt.Errorf("error creating target namespace: %w", err)
	}

	for _, resource := range resources {

		// Skip resources which automatically created in each namespace
		if (resource.GetKind() == "ServiceAccount" && resource.GetName() == "default") ||
			(resource.GetKind() == "ConfigMap" && resource.GetName() == "kube-root-ca.crt") {
			continue
		}

		log.V(1).Info(fmt.Sprintf("cloning %s/%s to %s", resource.GetKind(), resource.GetName(), targetNamespace))

		// cleanup resource before cloning
		clonedResource := resource.DeepCopy()
		clonedResource.SetResourceVersion("")
		clonedResource.SetSelfLink("")
		clonedResource.SetGeneration(0)
		clonedResource.SetUID("")
		// override namespace!
		clonedResource.SetNamespace(targetNamespace)

		// ToDo: Clone PVC using the original PV as datasource
		if clonedResource.GetKind() == "PersistentVolumeClaim" {
			// remove all annotations, as the storage controller adds its own which will conflict
			clonedResource.SetAnnotations(map[string]string{})
			// we need to remove the PersistentVolumeClaim's reference to the PersistentVolume
			clonedResource.Object["spec"].(map[string]any)["volumeName"] = ""
		}

		// Remove all ips from services, otherwise they will conflict with the original namespace
		if clonedResource.GetKind() == "Service" {
			var svc *corev1.Service
			// Convert to service object
			runtime.DefaultUnstructuredConverter.FromUnstructured(clonedResource.Object, &svc)

			svc.Spec.ClusterIP = ""
			svc.Spec.ClusterIPs = []string{}
			svc.Spec.ExternalIPs = []string{}
			svc.Spec.LoadBalancerIP = ""

			// Convert back to unstructured object
			clonedResource.Object, _ = runtime.DefaultUnstructuredConverter.ToUnstructured(svc)
		}

		err = cl.Create(ctx, clonedResource)
		if err != nil {
			log.Error(err, fmt.Sprintf("error creating %s/%s in %s", resource.GetKind(), resource.GetName(), targetNamespace))
		} else {
			log.V(1).Info(fmt.Sprintf("created %s/%s in %s", resource.GetKind(), resource.GetName(), targetNamespace))
		}
	}

	log.Info("namespace cloned", "sourceNamespace", sourceNamespace, "targetNamespace", targetNamespace)

	return nil
}

func getTopLevelResources(ctx context.Context, namespace string) ([]*unstructured.Unstructured, error) {

	allResources, err := getAllResources(ctx, namespace)
	if err != nil {
		return nil, err
	}

	unownedResources := []*unstructured.Unstructured{}
	for _, resourceList := range allResources {
		for _, resource := range resourceList.Items {
			if len(resource.GetOwnerReferences()) == 0 {
				unownedResources = append(unownedResources, &resource)
			}
		}
	}

	return unownedResources, nil

}

// Get all resources in a namespace, this doesn't filter owned resources
// First we need to use a discovery client to get all namespaced resource types
// Afterwards we can use a dynamic client, to load resources from any type into Unstructred objects
func getAllResources(ctx context.Context, namespace string) (map[string]*unstructured.UnstructuredList, error) {
	log := log.FromContext(ctx).WithName("runner")

	dynamicClient, err := dynamic.NewForConfig(config.GetConfigOrDie())
	if err != nil {
		return nil, err
	}

	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config.GetConfigOrDie())
	if err != nil {
		return nil, err
	}

	namespacedResources, err := discoveryClient.ServerPreferredNamespacedResources()
	if err != nil {
		// ignore groupDiscoveryFailed error, most likely due to orphaned CRDs
		if !discovery.IsGroupDiscoveryFailedError(err) {
			return nil, err
		}

	}

	resources := make(map[string]*unstructured.UnstructuredList)
	for _, resourceList := range namespacedResources {
		if len(resourceList.APIResources) == 0 {
			log.V(3).Info("no resources found for group version", "groupVersion", resourceList.GroupVersion)
			continue
		}

		for _, resourceType := range resourceList.APIResources {
			if !resourceType.Namespaced {
				log.V(2).Info("skipping non-namespaced resource", "resourceType", resourceType.Name)
				continue
			}

			// Skip all resources in the metrics group
			if strings.Contains(resourceType.Group, "metrics") {
				continue
			}

			if slices.Contains(resourcesToSkip, strings.ToLower(resourceType.Name)) || strings.Contains(resourceType.Name, "/") {
				log.V(2).Info("skipping resource", "resourceName", resourceType.Name)
				continue
			}
			groupVersion, err := schema.ParseGroupVersion(resourceList.GroupVersion)
			if err != nil {
				log.Error(err, "error parsing group version", "groupVersion", resourceList.GroupVersion)
				continue
			}

			gvr := schema.GroupVersionResource{
				Group:    groupVersion.Group,
				Version:  groupVersion.Version,
				Resource: resourceType.Name,
			}

			resourceObjects, err := dynamicClient.Resource(gvr).Namespace(namespace).List(ctx, v1.ListOptions{})

			if err != nil {
				log.Error(err, "error listing resources", "kind", gvr.String())
				continue
			}
			if len(resourceObjects.Items) == 0 {
				log.V(1).Info("no resources found for", "kind", gvr.String())
				continue
			}
			log.V(1).Info(fmt.Sprintf("found %d resources for: %s\n", len(resourceObjects.Items), gvr.String()))

			if _, ok := resources[resourceType.Name]; !ok {
				resources[resourceType.Name] = &unstructured.UnstructuredList{}
				resources[resourceType.Name].SetGroupVersionKind(gvr.GroupVersion().WithKind(resourceType.Kind))
			}
			// Append the items to the existing list
			resources[resourceType.Name].Items = append(resources[resourceType.Name].Items, resourceObjects.Items...)
		}

	}

	return resources, nil
}
