package orakel

import (
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/faceair/drain"
)

var (
	drainTemplateRegex = regexp.MustCompile(`id=\{\d+\}\s+:\s+size=\{(?P<size>\d+)\}\s+:\s+(?P<template>.*)$`)
	sizeIndex          = drainTemplateRegex.SubexpIndex("size")
	templateIndex      = drainTemplateRegex.SubexpIndex("template")
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

// GetTemplates returns a map of templates extracted from the trained model
// The keys are the template strings and the values are the sizes of the clusters
// The map is sorted by size in descending order
func (dm *LogOrakel) GetTemplates() []string {
	// Returns the templates extracted from the trained model
	// These are the clusters that represent the learned patterns from the baseline logs
	clusters := dm.Drain.Clusters()
	templates := make(map[string]int, len(clusters))

	for _, cluster := range dm.Drain.Clusters() {
		template := strings.TrimSpace(cluster.String())

		matches := drainTemplateRegex.FindStringSubmatch(template)
		templates[matches[templateIndex]], _ = strconv.Atoi(matches[sizeIndex])
	}

	// Sort the anomalies by size (descending)
	keys := make([]string, 0, len(templates))
	for key := range templates {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return templates[keys[i]] > templates[keys[j]] })

	return keys
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
