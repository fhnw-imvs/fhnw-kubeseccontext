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
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

var resourcesToSkip = []string{
	"bindings",
	"localsubjectaccessreviews",
	// exclude our own resource to avoid infinite loops
	"workloadhardeningchecks.checks.funk.fhnw.ch",
	"endpointslices",
	"endpoints",
}

var resourceToClone = []string{
	"deployments",
	"statefulsets",
	"daemonsets",
}

func CloneNamespace(ctx context.Context, sourceNamespace, targetNamespace string) error {
	log := log.FromContext(ctx)

	// check if targetNamespace already exists
	cl, err := client.New(config.GetConfigOrDie(), client.Options{})
	if err != nil {
		return fmt.Errorf("failed to create client")
	}
	targetNs := &corev1.Namespace{}
	err = cl.Get(ctx, client.ObjectKey{Name: targetNamespace}, targetNs)

	// we expect at least a NotFound error here, otherwise the namespace already exists, and we don't want to override it
	if err == nil {
		return fmt.Errorf("target namespace %s already exists. aborting", targetNamespace)
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("error fetching namespace: %w", err)
	}

	resources, err := getAllResources(ctx, sourceNamespace)
	if err != nil {
		return fmt.Errorf("unable to fetch all resources from %s", sourceNamespace)
	}

	// create targetNamespace
	targetNs.Name = targetNamespace
	targetNs.Labels = map[string]string{
		"app.kubernetes.io/name":       targetNamespace,
		"app.kubernetes.io/managed-by": "oracle-of-funk",
	}
	err = cl.Create(ctx, targetNs)
	if err != nil {
		return fmt.Errorf("error creating target namespace: %w", err)
	}

	// Only  create "top level" resources. Avoid cloning pods if they are owned by a deployment end so forth
	for resourcesType, resourcesList := range resources {
		if !slices.Contains(resourceToClone, resourcesType) {
			continue
		}

		log.Info(fmt.Sprintf("cloning %s to %s", resourcesType, targetNamespace))

		for _, resource := range resourcesList.Items {
			// cleanup resource before cloning
			clonedResource := resource.DeepCopy()
			clonedResource.SetResourceVersion("")
			clonedResource.SetSelfLink("")
			clonedResource.SetGeneration(0)
			clonedResource.SetNamespace(targetNamespace)

			err = cl.Create(ctx, clonedResource)
			if err != nil {
				log.Error(err, fmt.Sprintf("error creating %s/%s in %s", resourcesType, resource.GetName(), targetNamespace))
			} else {
				log.Info(fmt.Sprintf("created %s/%s in %s", resourcesType, resource.GetName(), targetNamespace))
			}

		}
	}

	log.Info("namespace cloned", "sourceNamespace", sourceNamespace, "targetNamespace", targetNamespace)

	return nil
}

// Get all resources in a namespace, this doesn't filter owned resources
// First we need to use a discovery client to get all namespaced resource types
// Afterwards we can use a dynamic client, to load resources from any type into Unstructred objects
func getAllResources(ctx context.Context, namespace string) (map[string]*unstructured.UnstructuredList, error) {
	log := log.FromContext(ctx)

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
			log.Info("no resources found for group version", "groupVersion", resourceList.GroupVersion)
			continue
		}

		for _, resourceType := range resourceList.APIResources {
			if !resourceType.Namespaced {
				log.Info("skipping non-namespaced resource", "resourceType", resourceType.Name)
				continue
			}

			if slices.Contains(resourcesToSkip, resourceType.Name) || strings.Contains(resourceType.Name, "/") {
				log.Info("skipping resource", "resourceName", resourceType.Name)
				continue
			}
			groupVersion, err := schema.ParseGroupVersion(resourceList.GroupVersion)
			if err != nil {
				log.Info("error parsing group version", "groupVersion", resourceList.GroupVersion)
				continue
			}

			gvr := schema.GroupVersionResource{
				Group:    groupVersion.Group,
				Version:  groupVersion.Version,
				Resource: resourceType.Name,
			}

			resourceObjects, err := dynamicClient.Resource(gvr).Namespace(namespace).List(ctx, v1.ListOptions{})

			if err != nil {
				log.Info("error listing resources", "kind", gvr.String())
				continue
			}
			if len(resourceObjects.Items) == 0 {
				log.Info("no resources found for", "kind", gvr.String())
				continue
			}
			log.Info(fmt.Sprintf("found %d resources for: %s\n", len(resourceObjects.Items), gvr.String()))

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
