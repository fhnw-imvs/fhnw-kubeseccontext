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

func NewDrainMiner() *LogOrakel {
	drainConfig := drain.DefaultConfig()

	return &LogOrakel{
		Drain: drain.New(drainConfig),
	}

}

func (dm *LogOrakel) LoadBaseline(input []string) int {

	for _, line := range input {
		if strings.TrimSpace(line) == "" {
			continue
		}
		dm.BaselineLogsCount++
		cluster := dm.Drain.Train(strings.TrimSpace(line))
		if cluster == nil {
			continue
		}
	}

	for _, line := range input {
		if strings.TrimSpace(line) == "" {
			continue
		}
		dm.BaselineLogsCount++
		cluster := dm.Drain.Train(strings.TrimSpace(line))
		if cluster == nil {
			continue
		}
	}

	return dm.BaselineLogsCount
}

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
