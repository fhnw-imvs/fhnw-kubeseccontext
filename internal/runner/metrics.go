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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	statsapi "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

var podMetrics = map[string]map[types.UID]statsapi.PodStats{}
var knownPods = []types.UID{}
var podMetricsLastUpdate = time.Time{}
var nodePodMetricsLastUpdate = map[string]time.Time{}

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

	if _, ok := nodePodMetricsLastUpdate[pod.Spec.NodeName]; !ok {
		return nil, &PodError{Pod: pod, message: "pod metrics not initialized"}
	}

	if podStat, ok := podMetrics[pod.Spec.NodeName][pod.ObjectMeta.UID]; ok {
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

	for _, node := range nodes.Items {
		// update nodes in parallel
		go func() {
			RefreshPodMetricsPerNode(ctx, node.Name)
			wg.Done()
		}()
	}

	wg.Wait()

	podMetricsLastUpdate = time.Now()

	return nil

}

func RefreshPodMetricsPerNode(ctx context.Context, nodeName string) error {
	if time.Since(podMetricsLastUpdate) < 10*time.Second {
		fmt.Println("Pod metrics already refreshed within the last 10 seconds")
		return nil
	}

	// Reset the pod metrics map and known pods
	nodePodMetrics := map[types.UID]statsapi.PodStats{}

	nodeMetrics, err := getNodeMetrics(ctx, nodeName)
	if err != nil {
		return err
	}

	for _, pod := range nodeMetrics.Pods {
		nodePodMetrics[types.UID(pod.PodRef.UID)] = pod
		knownPods = append(knownPods, types.UID(pod.PodRef.UID))
	}

	podMetrics[nodeName] = nodePodMetrics

	nodePodMetricsLastUpdate[nodeName] = time.Now()

	return nil

}

func getNodeMetrics(ctx context.Context, nodeName string) (*statsapi.Summary, error) {
	log := log.FromContext(ctx).WithName("runner")
	config := config.GetConfigOrDie()

	client, _ := rest.HTTPClientFor(config)
	apiHost := config.Host

	req, err := client.Get(apiHost + "/api/v1/nodes/" + nodeName + "/proxy/stats/summary")
	if err != nil {
		log.Error(
			err,
			"couldn't fetch metrics",
			"node.name", nodeName,
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

type ResourceUsageRecord struct {
	Time   time.Time `json:"ts"`
	CPU    uint64    `json:"cpu"`
	Memory uint64    `json:"memory"`
}

type RecordedMetrics struct {
	Pod      string                            `json:"pod"`
	Start    time.Time                         `json:"start_time"`
	End      time.Time                         `json:"end_time"`
	Interval int                               `json:"interval"`
	Duration int                               `json:"duration"`
	Usage    map[time.Time]ResourceUsageRecord `json:"usage"`
}

// Fetches the logs of the default container
// To support multiple containers, this needs to be adjusted, and also init-container should be handled
func GetLogs(ctx context.Context, pod *corev1.Pod, containerName string) (string, error) {
	podLogOpts := corev1.PodLogOptions{
		// Always choose the first pod
		Container: containerName,
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
func RecordMetrics(ctx context.Context, pod *corev1.Pod, duration, interval int) (*RecordedMetrics, error) {
	log := log.FromContext(ctx).WithName("runner")

	recordedMetrics := map[time.Time]ResourceUsageRecord{}

	start := time.Now()
	end := start.Add(time.Duration(duration) * time.Second)
	log.Info("Recording resource usage for pod", "podName", pod.Name)

	for {
		if time.Now().After(end) {
			log.Info("Recording resource usage finished", "podName", pod.Name)
			break
		}
		if pod.Spec.NodeName == "" {
			return nil, fmt.Errorf("pod %s has no node assigned", pod.Name)
		}
		err := RefreshPodMetricsPerNode(ctx, pod.Spec.NodeName)
		if err != nil {
			return nil, err
		}
		podResources, err := GetPodResourceUsage(*pod)

		if err != nil {
			if strings.Contains(err.Error(), "pod not found in metrics") {
				log.V(2).Info("pod not yet found in metrics, retry in 5 seconds")
			} else {
				log.Error(err, "error getting pod resource usage", "pod.name", pod.Name, "pod.uid", pod.UID)
			}
			// wait for 5 seconds and try again
			time.Sleep(time.Duration(5) * time.Second)
			continue
		}

		metric := ResourceUsageRecord{
			Time:   time.Now(),
			CPU:    *podResources.CPU.UsageNanoCores,
			Memory: *podResources.Memory.WorkingSetBytes,
		}
		recordedMetrics[metric.Time] = metric

		log.V(3).Info(
			"Recorded metrics",
			"pod.name", pod.Name,
			"pod.uid", pod.UID,
			"nanoCpu", metric.CPU,
			"memoryBytes", metric.Memory,
		)

		// break loop if the next interval is after the end time
		if time.Now().Add(time.Duration(interval) * time.Second).After(end) {
			break
		}

		time.Sleep(time.Duration(interval) * time.Second)
	}

	recordedMetricsData := RecordedMetrics{
		Pod:      pod.Name,
		Start:    start,
		End:      end,
		Interval: interval,
		Duration: duration,
		Usage:    recordedMetrics,
	}

	return &recordedMetricsData, nil
}
