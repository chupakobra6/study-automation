package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chupakobra6/study-automation/internal/config"
	"github.com/chupakobra6/study-automation/internal/monitor"
	"github.com/chupakobra6/study-automation/internal/queue"
)

func manualTestConfig(t *testing.T) config.Config {
	t.Helper()
	var cfg config.Config
	cfg.Paths.StateDir = filepath.Join(t.TempDir(), ".state")
	cfg.Updates.StateDir = filepath.Join(cfg.Paths.StateDir, "updates")
	cfg.Manual.InboxDir = filepath.Join(cfg.Paths.StateDir, "manual", "inbox")
	cfg.Monitoring.QueueDir = filepath.Join(cfg.Paths.StateDir, "queue")
	cfg.Monitoring.LatestReport = filepath.Join(cfg.Updates.StateDir, "monitoring-latest.md")
	return cfg
}

func readManualCheckpoint(t *testing.T, cfg config.Config) manualCheckpoint {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(cfg.Paths.StateDir, "manual", "checkpoint.json"))
	if err != nil {
		t.Fatal(err)
	}
	var checkpoint manualCheckpoint
	if err := json.Unmarshal(data, &checkpoint); err != nil {
		t.Fatal(err)
	}
	return checkpoint
}

func TestManualChangesKeepSnapshotsAndPendingUntilAcknowledged(t *testing.T) {
	cfg := manualTestConfig(t)
	path := filepath.Join(cfg.Manual.InboxDir, "receipts", "Подтверждение.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	original, replacement := "private original evidence", "private modified evidence"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	source := manualSource(cfg)
	if source.ReportPath != filepath.Join(cfg.Updates.StateDir, "manual-latest.md") {
		t.Fatalf("missing durable report path: %+v", source)
	}
	now := time.Unix(100, 0)
	run := func(wantChanges, wantPending int) monitor.Report {
		t.Helper()
		now = now.Add(time.Minute)
		report, err := monitor.Run(context.Background(), []monitor.Source{source}, cfg.Monitoring.LatestReport, cfg.Monitoring.QueueDir, now)
		if err != nil || report.Pending.Pending != wantPending || report.Results[0].Added != wantChanges || report.Results[0].NeedsAnalysis != (wantChanges > 0) {
			t.Fatalf("report=%+v err=%v", report, err)
		}
		return report
	}
	first := run(1, 1)
	snapshot := readManualCheckpoint(t, cfg).Files[filepath.Join("receipts", "Подтверждение.md")].Snapshot
	if data, err := os.ReadFile(snapshot); err != nil || string(data) != original {
		t.Fatalf("snapshot=%q err=%v", data, err)
	}
	firstReport, err := os.ReadFile(first.Pending.Items[0].ReportPath)
	if err != nil || !strings.Contains(string(firstReport), snapshot) || strings.Contains(string(firstReport), original) {
		t.Fatalf("report must contain snapshot paths without raw evidence: %q err=%v", firstReport, err)
	}
	if err := os.Chtimes(path, now, now); err != nil {
		t.Fatal(err)
	}
	unchanged := run(0, 1)
	if unchanged.Pending.Items[0].ID != first.Pending.Items[0].ID {
		t.Fatal("mtime created another pending event")
	}
	if err := os.WriteFile(path, []byte(replacement), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, now, now); err != nil {
		t.Fatal(err)
	}
	run(1, 2)
	newSnapshot := readManualCheckpoint(t, cfg).Files[filepath.Join("receipts", "Подтверждение.md")].Snapshot
	if newSnapshot == snapshot {
		t.Fatal("content replacement reused the old snapshot path")
	}
	for object, want := range map[string]string{snapshot: original, newSnapshot: replacement} {
		if data, err := os.ReadFile(object); err != nil || string(data) != want {
			t.Fatalf("immutable evidence changed: %s = %q err=%v", object, data, err)
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	deleted := run(1, 3)
	body, err := os.ReadFile(source.ReportPath)
	if err != nil || !strings.Contains(string(body), "deleted") || !strings.Contains(string(body), newSnapshot) {
		t.Fatalf("deletion lost prior evidence: %q err=%v", body, err)
	}
	ids := make([]string, 0, len(deleted.Pending.Items))
	reviewed := make([]string, 0)
	for _, item := range deleted.Pending.Items {
		ids = append(ids, item.ID)
		reviewed = append(reviewed, item.RequiredEvidence...)
	}
	reviewed = uniquePaths(reviewed)
	receipt := filepath.Join(cfg.Paths.StateDir, "analysis-review.json")
	decisions := []reviewDecision{}
	for _, id := range ids {
		decisions = append(decisions, reviewDecision{Kind: "event", Key: id, Outcome: "updated", Finding: "Reviewed all versions and the deletion."})
	}
	receiptBody, err := json.Marshal(map[string]any{"decisions": decisions, "evidence": []map[string]any{{"paths": reviewed}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(receipt, receiptBody, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := queue.RecordReview(cfg.Monitoring.QueueDir, ids, nil, receipt, reviewed, now); err != nil {
		t.Fatal(err)
	}
	quiet := run(0, 0)
	if quiet.NeedsAnalysis {
		t.Fatal("acknowledged unchanged inbox still requested analysis")
	}
}

func TestManualBadFileDoesNotAdvanceCheckpoint(t *testing.T) {
	for _, failure := range []string{"symlink", "oversize"} {
		t.Run(failure, func(t *testing.T) {
			cfg := manualTestConfig(t)
			source := manualSource(cfg)
			if _, err := source.Sync(context.Background()); err != nil {
				t.Fatal(err)
			}
			checkpointPath := filepath.Join(cfg.Paths.StateDir, "manual", "checkpoint.json")
			before, err := os.ReadFile(checkpointPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cfg.Manual.InboxDir, "a-good.md"), []byte("new receipt"), 0o600); err != nil {
				t.Fatal(err)
			}
			badPath := filepath.Join(cfg.Manual.InboxDir, "z-bad")
			if failure == "symlink" {
				if err := os.Symlink(checkpointPath, badPath); err != nil {
					t.Fatal(err)
				}
			} else {
				file, err := os.Create(badPath)
				if err != nil {
					t.Fatal(err)
				}
				if err := file.Truncate(manualMaxFileBytes + 1); err != nil {
					t.Fatal(err)
				}
				file.Close()
			}
			report, err := monitor.Run(context.Background(), []monitor.Source{source}, cfg.Monitoring.LatestReport, cfg.Monitoring.QueueDir, time.Now())
			if !monitor.IsRunError(err) || report.Results[0].Status != "failed" || report.Results[0].Added != 0 || !strings.Contains(report.Results[0].Detail, badPath) {
				t.Fatalf("file failure was not explicit: report=%+v err=%v", report, err)
			}
			after, err := os.ReadFile(checkpointPath)
			if err != nil || string(after) != string(before) {
				t.Fatal("checkpoint advanced despite an unreadable file")
			}
			if err := os.Remove(badPath); err != nil {
				t.Fatal(err)
			}
			recovered, err := source.Sync(context.Background())
			if err != nil || recovered.Added != 1 || !recovered.NeedsAnalysis {
				t.Fatalf("retry lost the good receipt: result=%+v err=%v", recovered, err)
			}
		})
	}
}

func TestManualCrashReportRecoversBeforeNextSync(t *testing.T) {
	cfg := manualTestConfig(t)
	if err := os.MkdirAll(cfg.Manual.InboxDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg.Manual.InboxDir, "receipt.md")
	if err := os.WriteFile(path, []byte("first version"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := manualSource(cfg)
	crashing := source
	crashing.Sync = func(ctx context.Context) (monitor.Result, error) {
		if _, err := source.Sync(ctx); err != nil {
			t.Fatal(err)
		}
		panic("crash after source checkpoint")
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected simulated crash")
			}
		}()
		_, _ = monitor.Run(context.Background(), []monitor.Source{crashing}, cfg.Monitoring.LatestReport, cfg.Monitoring.QueueDir, time.Unix(100, 0))
	}()
	oldSnapshot := readManualCheckpoint(t, cfg).Files["receipt.md"].Snapshot
	if err := os.WriteFile(path, []byte("second version"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := monitor.Run(context.Background(), []monitor.Source{source}, cfg.Monitoring.LatestReport, cfg.Monitoring.QueueDir, time.Unix(200, 0))
	if err != nil || report.Pending.Pending != 2 {
		t.Fatalf("lost interrupted source event: report=%+v err=%v", report, err)
	}
	oldReport, err := os.ReadFile(report.Pending.Items[0].ReportPath)
	if err != nil || !strings.Contains(string(oldReport), oldSnapshot) {
		t.Fatalf("original snapshot reference lost after restart: %q err=%v", oldReport, err)
	}
}
