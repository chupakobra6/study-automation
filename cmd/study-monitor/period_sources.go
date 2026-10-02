package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/chupakobra6/study-automation/internal/config"
)

// Each source adapter owns its date semantics. The coordinator only filters,
// paginates and reports how much saved history the adapter can substantiate.
func readDatedSourceAdapter(cfg config.Config, configPath, source string, start, end time.Time) ([]periodRecord, periodCoverage, error) {
	row := periodCoverage{Source: source, Mode: "unindexed", Detail: "Dated source adapter is unavailable."}
	if configPath == "" || cfg.Paths.StateDir == "" {
		return nil, row, nil
	}
	binary := filepath.Join(filepath.Dir(cfg.Paths.StateDir), "tools", "study-data")
	if info, err := os.Stat(binary); err != nil || !info.Mode().IsRegular() {
		return nil, row, nil
	}
	absoluteConfig, err := filepath.Abs(configPath)
	if err != nil {
		return nil, row, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	last := end.AddDate(0, 0, -1)
	cmd := exec.CommandContext(ctx, binary, "--config", absoluteConfig, "period-records", source,
		"--from", start.Format("2006-01-02"), "--to", last.Format("2006-01-02"))
	output, err := cmd.Output()
	if err != nil {
		row.Mode = "adapter-error"
		row.Detail = "Dated source adapter failed: " + err.Error()
		return nil, row, nil
	}
	var result struct {
		Mode         string         `json:"mode"`
		Detail       string         `json:"detail"`
		SnapshotPath string         `json:"snapshot_path"`
		Records      []periodRecord `json:"records"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		row.Mode, row.Detail = "adapter-error", "Dated source adapter returned invalid JSON."
		return nil, row, nil
	}
	if result.Mode == "" {
		row.Mode, row.Detail = "adapter-error", "Dated source adapter omitted coverage mode."
		return nil, row, nil
	}
	row.Mode, row.Detail, row.SnapshotPath = result.Mode, result.Detail, result.SnapshotPath
	for i := range result.Records {
		if result.Records[i].Source != source || strings.TrimSpace(result.Records[i].ID) == "" {
			row.Mode, row.Detail = "adapter-error", fmt.Sprintf("Dated source adapter returned invalid %s record.", source)
			return nil, row, nil
		}
	}
	row.Count = len(result.Records)
	return result.Records, row, nil
}
