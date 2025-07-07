package orakel

import (
	"strings"

	"github.com/faceair/drain"
)

type DrainMiner struct {
	*drain.Drain

	BaselineLogsCount int
	TargetLogCount    int

	Anomalies []string
}

func NewDrainMiner(delimiters []string) *DrainMiner {
	drainConfig := drain.DefaultConfig()
	if delimiters != nil {
		drainConfig.ExtraDelimiters = delimiters
	} else {
		drainConfig.ExtraDelimiters = []string{"\t", " ", ",", ";", ":", "=", "(", ")", "{", "}", "[", "]", "\"", "'", "|", "\\", "/", "!", "?"}
	}

	return &DrainMiner{
		Drain: drain.New(drainConfig),
	}

}

func (dm *DrainMiner) LoadBaseline(input []string) int {

	for _, line := range input {
		if strings.TrimSpace(line) == "" {
			continue
		}
		dm.BaselineLogsCount++
		dm.Drain.Train(strings.TrimSpace(line))
	}

	return dm.BaselineLogsCount
}

func (dm *DrainMiner) AnalyzeTarget(input []string) ([]string, int) {
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
