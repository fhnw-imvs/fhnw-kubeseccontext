package recorder

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

type PodLogRecorder struct {
	kubernetes.Clientset
	logger logr.Logger
}

func NewPodLogRecorder(ctx context.Context) *PodLogRecorder {

	clientset, _ := kubernetes.NewForConfig(config.GetConfigOrDie())

	return &PodLogRecorder{
		Clientset: *clientset,
		logger:    logf.FromContext(ctx).WithName("PodLogRecorder"),
	}
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
