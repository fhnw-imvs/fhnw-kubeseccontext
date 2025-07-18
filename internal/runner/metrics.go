package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/fhnw-imvs/fhnw-kubeseccontext/internal/recording"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	statsapi "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

var podMetrics = map[types.UID]statsapi.PodStats{}
var knownPods = []types.UID{}
var podMetricsLastUpdate = time.Time{}

var refreshMutex sync.Mutex

type PodError struct {
	message string
	Pod     corev1.Pod
}

func (e *PodError) Error() string {
	return fmt.Sprintf("pod %s: %v", e.Pod.Name, e.message)
}

func GetNodes() (*corev1.NodeList, error) {
	client, err := kubernetes.NewForConfig(config.GetConfigOrDie())
	if err != nil {
		return nil, fmt.Errorf("couldn't get kubernetes client: %v", err)
	}

	nodes, err := client.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("couldn't list nodes: %v", err)
	}

	return nodes, nil
}

func GetPodResourceUsage(pod corev1.Pod) (*statsapi.PodStats, error) {

	if podMetricsLastUpdate.IsZero() {
		return nil, &PodError{Pod: pod, message: "pod metrics not initialized"}
	}

	if podStat, ok := podMetrics[pod.ObjectMeta.UID]; ok {
		return &podStat, nil
	} else {
		return nil, &PodError{Pod: pod, message: "pod not found in metrics"}
	}
}

func RefreshPodMetrics(ctx context.Context) error {
	refreshMutex.Lock()
	defer refreshMutex.Unlock()

	if time.Since(podMetricsLastUpdate) < 30*time.Second {
		return nil
	}

	nodes, err := GetNodes()
	if err != nil {
		return err
	}

	knownPods = []types.UID{}

	var wg sync.WaitGroup
	wg.Add(len(nodes.Items))

	resultsChannel := make(chan map[types.UID]statsapi.PodStats, len(nodes.Items))

	for _, node := range nodes.Items {
		// update nodes in parallel
		go func() {
			RefreshPodMetricsPerNode(ctx, node.Name, resultsChannel)
			wg.Done()
		}()
	}

	wg.Wait()

	close(resultsChannel)

	newPodMetrics := map[types.UID]statsapi.PodStats{}
	newKnownPods := []types.UID{}

	for result := range resultsChannel {
		for uid, podStat := range result {
			newPodMetrics[uid] = podStat
			newKnownPods = append(newKnownPods, uid)
		}
	}

	podMetricsLastUpdate = time.Now()
	podMetrics = newPodMetrics
	knownPods = newKnownPods

	return nil

}

func RefreshPodMetricsPerNode(ctx context.Context, nodeName string, resultsChannel chan map[types.UID]statsapi.PodStats) error {
	if time.Since(podMetricsLastUpdate) < 10*time.Second {
		fmt.Println("Pod metrics already refreshed within the last 10 seconds")
		return nil
	}

	// Reset the pod metrics map and known pods
	newMetrics := map[types.UID]statsapi.PodStats{}

	nodeMetrics, err := getNodeMetrics(ctx, nodeName)
	if err != nil {
		return err
	}

	for _, pod := range nodeMetrics.Pods {
		newMetrics[types.UID(pod.PodRef.UID)] = pod
	}

	resultsChannel <- newMetrics

	return nil

}

func getNodeMetrics(ctx context.Context, nodeName string) (*statsapi.Summary, error) {
	log := log.FromContext(ctx).WithName("metrics")
	config := config.GetConfigOrDie()

	client, _ := rest.HTTPClientFor(config)
	apiHost := config.Host

	req, err := client.Get(apiHost + "/api/v1/nodes/" + nodeName + "/proxy/stats/summary")
	if err != nil {
		log.Error(
			err,
			"couldn't fetch metrics",
			"nodeName", nodeName,
		)
		return nil, err
	}
	defer req.Body.Close()

	if req.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to get node metrics: %s", req.Status)
	}

	var metrics statsapi.Summary
	err = json.NewDecoder(req.Body).Decode(&metrics)
	if err != nil {
		return nil, err
	}

	return &metrics, nil
}

// Fetches the logs of a specific pod and container
func GetLogs(ctx context.Context, pod *corev1.Pod, containerName string, previous bool) (string, error) {
	podLogOpts := corev1.PodLogOptions{
		Container: containerName,
		Previous:  previous,
	}

	clientset, _ := kubernetes.NewForConfig(config.GetConfigOrDie())

	// Set timeout for stream context, otherwise the stream won't close even if we no longer receive
	ctx, cancelTimeout := context.WithTimeout(ctx, time.Duration(5)*time.Second)
	defer cancelTimeout()

	req := clientset.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &podLogOpts)
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

// Records cpu and memory usage metrics for a pod
// The metrics are collected at pod level, and not at container level
func RecordMetrics(ctx context.Context, pod *corev1.Pod, duration, interval int) (*recording.RecordedMetrics, error) {
	log := log.FromContext(ctx).WithName("metrics").WithValues(
		"podName", pod.Name,
		"podUID", pod.UID,
		"targetNamespace", pod.Namespace,
	)

	recordedMetrics := map[metav1.Time]recording.ResourceUsageRecord{}

	start := time.Now()
	log.Info("Recording resource usage for pod")

	lastLoop := false
	for {
		// break loop if the duration is reached
		if time.Since(start) >= time.Duration(duration)*time.Second {
			lastLoop = true
		}

		if pod.Spec.NodeName == "" {
			return nil, fmt.Errorf("pod %s has no node assigned", pod.Name)
		}
		err := RefreshPodMetrics(ctx)
		if err != nil {
			return nil, err
		}
		podResources, err := GetPodResourceUsage(*pod)

		if err != nil {
			if strings.Contains(err.Error(), "pod not found in metrics") {
				log.V(2).Info("pod not yet found in metrics, retry in 5 seconds")
			} else {
				log.Error(err, "error getting pod resource usage")
			}
			if lastLoop {
				break
			}
			// wait for 5 seconds and try again
			time.Sleep(time.Duration(5) * time.Second)
			continue
		}

		metric := recording.ResourceUsageRecord{
			Time:   metav1.Now(),
			CPU:    int64(*podResources.CPU.UsageNanoCores),
			Memory: int64(*podResources.Memory.WorkingSetBytes),
		}
		recordedMetrics[metric.Time] = metric

		log.V(3).Info(
			"Recorded metrics",
			"nanoCpu", metric.CPU,
			"memoryBytes", metric.Memory,
		)
		if lastLoop {
			break
		}

		time.Sleep(time.Duration(interval) * time.Second)
	}

	log.Info("Recording resource usage finished")

	recordedMetricsData := recording.RecordedMetrics{
		Pod:      pod.Name,
		Start:    start,
		End:      time.Now(),
		Interval: interval,
		Duration: duration,
		Usage:    recordedMetrics,
	}

	return &recordedMetricsData, nil
}
