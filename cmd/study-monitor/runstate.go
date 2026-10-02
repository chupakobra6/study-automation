package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/chupakobra6/study-automation/internal/monitor"
)

type collectedSource struct {
	Name           string `json:"name"`
	Status         string `json:"status"`
	DurationMillis int64  `json:"duration_ms"`
	Detail         string `json:"detail,omitempty"`
}

type collectionRun struct {
	Version    int               `json:"version"`
	ID         string            `json:"id"`
	StartedAt  time.Time         `json:"started_at"`
	FinishedAt *time.Time        `json:"finished_at,omitempty"`
	FullScope  bool              `json:"full_scope"`
	Status     string            `json:"status"`
	Error      string            `json:"error,omitempty"`
	Sources    []collectedSource `json:"sources"`
}

func collectionDir(stateDir string) string {
	return filepath.Join(stateDir, "monitoring-runs")
}

func newCollectionRun(now time.Time, sources []monitor.Source, fullScope bool) collectionRun {
	run := collectionRun{Version: 1, ID: now.UTC().Format("20060102T150405.000000000Z"), StartedAt: now.UTC(), FullScope: fullScope, Status: "running", Sources: make([]collectedSource, 0, len(sources))}
	for _, source := range sources {
		run.Sources = append(run.Sources, collectedSource{Name: source.Name, Status: "pending"})
	}
	return run
}

func applyCollectionProgress(run collectionRun, progress monitor.Progress) collectionRun {
	for i := range run.Sources {
		if run.Sources[i].Name != progress.Source {
			continue
		}
		if progress.Phase == "started" {
			run.Sources[i].Status = "running"
		} else if progress.Phase == "finished" {
			run.Sources[i].Status = progress.Status
			run.Sources[i].DurationMillis = progress.Duration.Milliseconds()
			if progress.Status == "failed" || progress.Status == "blocked" || progress.Status == "incomplete" {
				run.Sources[i].Detail = progress.Detail
			}
		}
		break
	}
	return run
}

func finishCollectionRun(run collectionRun, runErr error, now time.Time) collectionRun {
	finished := now.UTC()
	run.FinishedAt = &finished
	run.Status = "complete"
	if runErr != nil {
		run.Status = "partial"
		run.Error = runErr.Error()
	}
	if !allSourcesFinished(run.Sources) {
		run.Status = "partial"
	}
	return run
}

func allSourcesFinished(sources []collectedSource) bool {
	if len(sources) == 0 {
		return false
	}
	for _, source := range sources {
		if source.Status == "" || source.Status == "pending" || source.Status == "running" {
			return false
		}
	}
	return true
}

func saveCollectionRun(dir string, run collectionRun, final bool) error {
	data, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if final {
		if err := writeCollectionFile(filepath.Join(dir, run.ID+".json"), data); err != nil {
			return err
		}
	}
	if err := writeCollectionFile(filepath.Join(dir, "latest.json"), data); err != nil {
		return err
	}
	if run.FullScope {
		return writeCollectionFile(filepath.Join(dir, "latest-full.json"), data)
	}
	return nil
}

func writeCollectionFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".collection-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), path)
}

func loadCollectionRun(dir string) (collectionRun, error) {
	var run collectionRun
	data, err := os.ReadFile(filepath.Join(dir, "latest-full.json"))
	if errors.Is(err, os.ErrNotExist) {
		data, err = os.ReadFile(filepath.Join(dir, "latest.json"))
	}
	if err != nil {
		return run, err
	}
	if err := json.Unmarshal(data, &run); err != nil {
		return run, fmt.Errorf("invalid collection state: %w", err)
	}
	if run.Version != 1 || run.ID == "" || run.StartedAt.IsZero() {
		return run, errors.New("invalid collection state: missing version, ID or start time")
	}
	return run, nil
}

func collectionBrief(run collectionRun, err error, now time.Time) map[string]any {
	if err != nil {
		return map[string]any{"ready": false, "reason": "collection state unavailable: " + err.Error()}
	}
	moscow, _ := time.LoadLocation("Europe/Moscow")
	today := now.In(moscow).Format("2006-01-02")
	ready := run.FullScope && run.FinishedAt != nil && allSourcesFinished(run.Sources) && run.StartedAt.In(moscow).Format("2006-01-02") == today
	status := map[string]any{"ready": ready, "date": run.StartedAt.In(moscow).Format("2006-01-02"), "status": run.Status, "run_id": run.ID, "full_scope": run.FullScope, "sources": run.Sources}
	if run.FinishedAt != nil {
		status["duration_ms"] = run.FinishedAt.Sub(run.StartedAt).Milliseconds()
	}
	if run.Error != "" {
		status["error"] = run.Error
	}
	if !ready {
		switch {
		case !run.FullScope:
			status["reason"] = "latest collection covers only selected sources"
		case run.FinishedAt == nil:
			status["reason"] = "full collection has not completed"
		case !allSourcesFinished(run.Sources):
			status["reason"] = "full collection stopped before every source finished"
		default:
			status["reason"] = "no completed full collection started today"
		}
	}
	return status
}
