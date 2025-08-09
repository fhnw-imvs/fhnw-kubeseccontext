package orakel

import (
	"slices"

	"github.com/fhnw-imvs/fhnw-kubeseccontext/internal/recording"
)

type CheckMetricsSummary struct {
	CpuSummary    MetricsSummary
	MemorySummary MetricsSummary
}

func (cms *CheckMetricsSummary) updateValues(cpu, memory int64) {
	cms.CpuSummary.originalValues = append(cms.CpuSummary.originalValues, cpu)
	cms.MemorySummary.originalValues = append(cms.MemorySummary.originalValues, memory)
}

func NewCheckMetricsSummary(metrics []recording.ResourceUsageRecord) *CheckMetricsSummary {
	checkSummary := &CheckMetricsSummary{
		CpuSummary: MetricsSummary{
			originalValues: []int64{},
		},
		MemorySummary: MetricsSummary{
			originalValues: []int64{},
		},
	}

	for _, containerMetrics := range metrics {
		checkSummary.updateValues(containerMetrics.CPU, containerMetrics.Memory)
	}

	// Calculate the summary statistics for CPU and Memory
	checkSummary.CpuSummary.updateSummary()
	checkSummary.MemorySummary.updateSummary()

	return checkSummary
}

type MetricsSummary struct {
	originalValues   []int64
	normalizedValues []int64

	// Absolute values
	Min int64
	Max int64
	Avg int64

	// Normalized values
	Median int64
	StdDev int64
}

// updateSummary calculates the summary statistics based on the original values
// and updates the Min, Max, Avg, Median, and StdDev fields.
func (ms *MetricsSummary) updateSummary() {
	if len(ms.originalValues) == 0 {
		return
	}

	slices.Sort(ms.originalValues)

	ms.Min = ms.originalValues[0]
	ms.Max = ms.originalValues[len(ms.originalValues)-1]

	total := int64(0)
	for _, value := range ms.originalValues {
		total += value
	}
	ms.Avg = total / int64(len(ms.originalValues))

	// Reset normalized values
	ms.normalizedValues = make([]int64, len(ms.originalValues))
	for i, value := range ms.originalValues {
		if ms.Max == ms.Min {
			ms.normalizedValues[i] = 0 // Avoid division by zero
		} else {
			ms.normalizedValues[i] = (value - ms.Min) * 100 / (ms.Max - ms.Min)
		}
	}

	// Calculate median
	if len(ms.normalizedValues)%2 == 0 {
		ms.Median = (ms.normalizedValues[len(ms.normalizedValues)/2-1] + ms.normalizedValues[len(ms.normalizedValues)/2]) / 2
	} else {
		// Odd number of elements
		ms.Median = ms.normalizedValues[len(ms.normalizedValues)/2]
	}

	// Calculate standard deviation
	var variance int64
	for _, value := range ms.normalizedValues {
		diff := value - ms.Median
		variance += diff * diff
	}
	ms.StdDev = variance / int64(len(ms.normalizedValues))

}

type MetricsOrakel struct {
	// Baseline metrics count
	BaselineMetricsSummary *CheckMetricsSummary
}

func NewMetricsOrakel() *MetricsOrakel {
	return &MetricsOrakel{
		BaselineMetricsSummary: &CheckMetricsSummary{
			CpuSummary:    MetricsSummary{},
			MemorySummary: MetricsSummary{},
		},
	}
}

func (mo *MetricsOrakel) LoadBaseline(recording *recording.WorkloadRecording) {
	// Load baseline metrics from the recording
	if mo.BaselineMetricsSummary == nil {
		mo.BaselineMetricsSummary = NewCheckMetricsSummary(recording.RecordedMetrics)
		return
	}

	for _, containerMetrics := range recording.RecordedMetrics {
		mo.BaselineMetricsSummary.updateValues(containerMetrics.CPU, containerMetrics.Memory)
	}

	// Calculate the summary statistics for CPU and Memory
	mo.BaselineMetricsSummary.CpuSummary.updateSummary()
	mo.BaselineMetricsSummary.MemorySummary.updateSummary()

}

func (mo *MetricsOrakel) AnalyzeTarget(recording *recording.WorkloadRecording) (cpuDeviation bool, memoryDeviation bool) {
	// This is called after all baseline metrics have been loaded, so we calculate the summary statistics
	targetMetricsSummary := NewCheckMetricsSummary(recording.RecordedMetrics)

	cpuUpperThreshold := mo.BaselineMetricsSummary.CpuSummary.Avg * (1 + mo.BaselineMetricsSummary.CpuSummary.StdDev)
	cpuLowerThreshold := mo.BaselineMetricsSummary.CpuSummary.Avg * (1 - mo.BaselineMetricsSummary.CpuSummary.StdDev)

	memoryUpperThreshold := mo.BaselineMetricsSummary.MemorySummary.Avg * (1 + mo.BaselineMetricsSummary.MemorySummary.StdDev)
	memoryLowerThreshold := mo.BaselineMetricsSummary.MemorySummary.Avg * (1 - mo.BaselineMetricsSummary.MemorySummary.StdDev)

	// Check if deviation from baseline is significant
	// This can be done by comparing the target metrics with the baseline metrics
	cpuDeviation = targetMetricsSummary.CpuSummary.Avg > cpuUpperThreshold || targetMetricsSummary.CpuSummary.Avg < cpuLowerThreshold
	memoryDeviation = targetMetricsSummary.MemorySummary.Avg > memoryUpperThreshold || targetMetricsSummary.MemorySummary.Avg < memoryLowerThreshold

	// Here you can implement further analysis logic, e.g., comparing with baseline
	// For now, we just return the target metrics summary
	return
}
