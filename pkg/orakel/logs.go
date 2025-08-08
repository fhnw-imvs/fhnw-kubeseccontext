package orakel

import (
	"strings"

	"github.com/faceair/drain"
)

type LogOrakel struct {
	*drain.Drain

	BaselineLogsCount int
	TargetLogCount    int

	Anomalies []string
}

// Returns a new LogOrakel instance with a default drain configuration
// The drain is used to train the model with baseline logs and analyze target logs
func NewLogOrakel() *LogOrakel {
	drainConfig := drain.DefaultConfig()

	return &LogOrakel{
		Drain: drain.New(drainConfig),
	}

}

// LoadBaseline takes a slice of log lines, trains the drain model with them,
// and returns the numebr of logs processed and the number of templates extracted.
func (dm *LogOrakel) LoadBaseline(input []string) (int, int) {

	for _, line := range input {
		if strings.TrimSpace(line) == "" {
			continue
		}
		dm.BaselineLogsCount++
		dm.Drain.Train(strings.TrimSpace(line))
	}

	return dm.BaselineLogsCount, len(dm.Drain.Clusters())
}

// AnalyzeTarget takes a slice of log lines, analyzes them against the trained model,
// and returns a slice of anomalies and the number of logs processed.
// Anomalies are lines that do not match any of the trained templates.
func (dm *LogOrakel) AnalyzeTarget(input []string) ([]string, int) {
	dm.TargetLogCount = 0
	dm.Anomalies = []string{}

	for _, line := range input {
		if strings.TrimSpace(line) == "" {
			continue
		}
		dm.TargetLogCount++
		line = strings.TrimSpace(line)
		matchedCluster := dm.Drain.Match(line)
		if matchedCluster == nil {
			dm.Anomalies = append(dm.Anomalies, line)
			continue
		}
	}

	return dm.Anomalies, dm.TargetLogCount
}
