package runner

import (
	"context"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

var resourcesToSkip = []string{
	"bindings",
	"localsubjectaccessreviews",
	"endpointslices",
	"endpoints",
	"events",
	"ingresses",
	"httproutes",
	"gateways",
	"podmetrics",
	"controllerrevisions",
	// exclude our own resource to avoid infinite loops
	"workloadhardeningchecks",
	"namespacehardeningchecks",
}

func CloneNamespace(ctx context.Context, sourceNamespace, targetNamespace, suffix string) error {
	log := logf.FromContext(ctx).WithName("runner").WithValues("targetNamespace", targetNamespace, "suffix", suffix)

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

	// only get topLevelResources without ownership
	topLevelResources, err := GetTopLevelResources(ctx, sourceNamespace)
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

	// First let's clone the clusterRoleBindings, as they are not namespaced
	// and pods might fail if their serviceAccounts are not bound to the correct roles
	clusterRoleBindingList := &rbacv1.ClusterRoleBindingList{}
	cl.List(ctx, clusterRoleBindingList)

	if len(clusterRoleBindingList.Items) > 0 {
		for _, clusterRoleBinding := range clusterRoleBindingList.Items {
			needsToBeCloned := false
			if clusterRoleBinding.Subjects != nil {
			Subjects:
				for _, subject := range clusterRoleBinding.Subjects {
					if subject.Kind == "ServiceAccount" && subject.Namespace == sourceNamespace {
						log.V(1).Info("found cluster role binding for service account in source namespace", "name", clusterRoleBinding.Name, "serviceAccount", subject.Name)
						needsToBeCloned = true
						break Subjects
					}
				}
			}

			if needsToBeCloned {
				// we need to clone the cluster role binding to the target namespace
				clonedClusterRoleBinding := clusterRoleBinding.DeepCopy()
				clonedClusterRoleBinding.SetResourceVersion("")
				clonedClusterRoleBinding.SetSelfLink("")
				clonedClusterRoleBinding.SetUID("")
				clonedClusterRoleBinding.SetGeneration(0)

				roleBindingName := clusterRoleBinding.Name + "-" + targetNamespace
				// If the composite name is too long, we need to shorten it, and to avoid conflicts, we add a random suffix
				if len(roleBindingName) > 250 {
					suffixLength := 10
					if len(clusterRoleBinding.Name) > 245 {
						suffixLength = 253 - len(clusterRoleBinding.Name)
					}
					roleBindingName = clusterRoleBinding.Name + "-" + utilrand.String(suffixLength)
					if len(roleBindingName) > 253 {
						roleBindingName = roleBindingName[:253]
					}
				}
				clonedClusterRoleBinding.SetName(roleBindingName)

				// Add labels to the cloned ClusterRoleBinding
				if clonedClusterRoleBinding.Labels == nil {
					clonedClusterRoleBinding.Labels = make(map[string]string)
				}
				clonedClusterRoleBinding.Labels["app.kubernetes.io/managed-by"] = "oracle-of-funk"
				clonedClusterRoleBinding.Labels["orakel.fhnw.ch/source-namespace"] = sourceNamespace
				clonedClusterRoleBinding.Labels["orakel.fhnw.ch/target-namespace"] = targetNamespace
				clonedClusterRoleBinding.Labels["orakel.fhnw.ch/suffix"] = suffix

				// override namespace!
				for i, subject := range clonedClusterRoleBinding.Subjects {
					if subject.Kind == "ServiceAccount" && subject.Namespace == sourceNamespace {
						// change the namespace to the target namespace
						clonedClusterRoleBinding.Subjects[i].Namespace = targetNamespace
					}
				}

				err = cl.Create(ctx, clonedClusterRoleBinding)
				if err != nil {
					return fmt.Errorf("error creating cluster role binding %s: %w", clusterRoleBinding.Name, err)
				}
			}

		}
	}

	// Now we can clone the resources, and be sure that the serviceAccounts have their rolebindings in place
	for _, resource := range topLevelResources {

		// Skip resources which automatically created in each namespace
		if (resource.GetKind() == "ServiceAccount" && resource.GetName() == "default") ||
			(resource.GetKind() == "ConfigMap" && resource.GetName() == "kube-root-ca.crt") {
			continue
		}

		log.V(1).Info(fmt.Sprintf("cloning %s/%s", resource.GetKind(), resource.GetName()))

		// cleanup resource before cloning
		clonedResource := resource.DeepCopy()
		clonedResource.SetResourceVersion("")
		clonedResource.SetSelfLink("")
		clonedResource.SetGeneration(0)
		clonedResource.SetUID("")
		// override namespace!
		clonedResource.SetNamespace(targetNamespace)

		if clonedResource.GetKind() == "PersistentVolumeClaim" {
			// remove all annotations, as the storage controller adds its own which will conflict
			clonedResource.SetAnnotations(map[string]string{})
			// we need to remove the PersistentVolumeClaim's reference to the PersistentVolume
			clonedResource.Object["spec"].(map[string]any)["volumeName"] = ""
		}

		// Remove all IPs from services, otherwise they will conflict with the original namespace
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

		if clonedResource.GetKind() == "RoleBinding" {
			// RoleBindings are namespaced, so we need to change the namespace of the subjects
			for i, subject := range clonedResource.Object["subjects"].([]interface{}) {
				subjectMap := subject.(map[string]interface{})
				if subjectMap["kind"] == "ServiceAccount" && subjectMap["namespace"] == sourceNamespace {
					subjectMap["namespace"] = targetNamespace
					clonedResource.Object["subjects"].([]interface{})[i] = subjectMap
				}
			}
		}

		err = cl.Create(ctx, clonedResource)
		if err != nil {
			log.Error(err, fmt.Sprintf("error creating %s/%s", resource.GetKind(), resource.GetName()))
		} else {
			log.V(3).Info(fmt.Sprintf("created %s/%s", resource.GetKind(), resource.GetName()))
		}
	}

	log.Info("namespace cloned")

	return nil
}

func GetTopLevelResources(ctx context.Context, namespace string) ([]*unstructured.Unstructured, error) {

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
	log := logf.FromContext(ctx).WithName("runner")

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
				log.V(2).Info("no resources found for", "kind", gvr.String())
				continue
			}
			log.V(3).Info(fmt.Sprintf("found %d resources for: %s\n", len(resourceObjects.Items), gvr.String()))

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
