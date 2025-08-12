package recorder

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

type PodLogRecorder struct {
	client.Client
	kubernetes.Clientset
	logger logr.Logger
}

func NewPodLogRecorder(ctx context.Context, ksClient client.Client) *PodLogRecorder {

	clientset, _ := kubernetes.NewForConfig(config.GetConfigOrDie())

	return &PodLogRecorder{
		Client:    ksClient,
		Clientset: *clientset,
		logger:    logf.FromContext(ctx).WithName("PodLogRecorder"),
	}
}

func (r *PodLogRecorder) RecordLogs(ctx context.Context, targetNamespace string, labelSelector labels.Selector, previous bool) (map[string][]string, error) {

	// get pods under observation, we use the label selector from the workload under test
	pods := &corev1.PodList{}
PodsAssigned:
	for len(pods.Items) == 0 {
		err := r.List(
			ctx,
			pods,
			&client.ListOptions{
				Namespace:     targetNamespace,
				LabelSelector: labelSelector,
			},
		)
		if err != nil {
			r.logger.Error(err, "error fetching pods")
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
				break PodsAssigned // All pods are assigned to a node, we can proceed
			}

			pods = &corev1.PodList{} // reset podList to retry fetching
			r.logger.Info("Pods are not assigned to a node yet, retrying")
			time.Sleep(1 * time.Second) // Wait for 1 second before retrying
		}
	}

	r.logger.Info(
		"fetched pods matching workload under",
		"numberOfPods", len(pods.Items),
	)

	var wg sync.WaitGroup
	wg.Add(len(pods.Items))

	logMap := make(map[string][]string, len(pods.Items)*(len(pods.Items[0].Spec.Containers)+len(pods.Items[0].Spec.InitContainers)))
	mapMutex := &sync.Mutex{}

	for _, pod := range pods.Items {
		go func() {

			// The logs are collected per container in the pod
			for _, container := range pod.Spec.Containers {

				containerLog, err := r.GetLogs(ctx, &pod, container.Name, previous)
				if err != nil {
					r.logger.Error(
						err,
						"error fetching logs",
						"podName", pod.Name,
						"containerName", container.Name,
					)
					continue
				}

				r.logger.V(1).Info(
					"fetched logs",
					"podName", pod.Name,
					"containerName", container.Name,
				)

				// Use a mutex to safely write to the map from multiple goroutines
				mapMutex.Lock()
				logMap[container.Name] = append(logMap[container.Name], strings.Split(containerLog, "\n")...)
				mapMutex.Unlock()

			}

			for _, container := range pod.Spec.InitContainers {

				containerLog, err := r.GetLogs(ctx, &pod, container.Name, previous)
				if err != nil {
					r.logger.Error(
						err,
						"error fetching logs",
						"podName", pod.Name,
						"containerName", container.Name,
					)
					continue
				}
				r.logger.V(1).Info(
					"fetched logs",
					"podName", pod.Name,
					"containerName", container.Name,
				)

				mapMutex.Lock()
				logMap["init:"+container.Name] = append(logMap["init:"+container.Name], strings.Split(containerLog, "\n")...)
				mapMutex.Unlock()

			}

			wg.Done()

		}()
	}

	wg.Wait()

	r.logger.V(1).Info("collected logs")

	return logMap, nil
}

// Fetches the logs of a specific pod and container
func (r *PodLogRecorder) GetLogs(ctx context.Context, pod *corev1.Pod, containerName string, previous bool) (string, error) {
	podLogOpts := corev1.PodLogOptions{
		Container: containerName,
		Previous:  previous,
	}

	// Set timeout for stream context, otherwise the stream won't close even if we no longer receive
	ctx, cancelTimeout := context.WithTimeout(ctx, time.Duration(5)*time.Second)
	defer cancelTimeout()

	req := r.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &podLogOpts)
	podLogs, err := req.Stream(ctx)
	if err != nil {
		return "", fmt.Errorf("error opening log stream")
	}
	defer podLogs.Close()

	buf := new(bytes.Buffer)

	_, err = io.Copy(buf, podLogs)
	if err != nil {
		return "", fmt.Errorf("couldn't copy logs from buffer")
	}
	str := buf.String()

	return str, nil
}
