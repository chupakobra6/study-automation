package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chupakobra6/study-automation/internal/queue"
)

func TestRunIsolatesFailuresAndWritesAllStatuses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "monitoring-latest.md")
	called := false
	report, err := Run(context.Background(), []Source{
		{Name: "broken", ReportPath: filepath.Join(t.TempDir(), "broken.md"), Sync: func(context.Context) (Result, error) {
			return Result{Status: "failed", Detail: "offline"}, errors.New("offline")
		}},
		{Name: "healthy", ReportPath: filepath.Join(t.TempDir(), "healthy.md"), Sync: func(context.Context) (Result, error) {
			called = true
			return Result{Status: "delta", Detail: "one update", Added: 1}, nil
		}},
	}, path, filepath.Join(t.TempDir(), "queue"), time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC))
	if !IsRunError(err) || !called {
		t.Fatalf("err=%v called=%t", err, called)
	}
	if !report.NeedsAnalysis || len(report.Results) != 2 {
		t.Fatalf("report=%+v", report)
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	text := string(data)
	if !strings.Contains(text, "## broken") || !strings.Contains(text, "## healthy") {
		t.Fatalf("report missing source statuses:\n%s", text)
	}
}

func TestRunDoesNotRequestAnalysisForNoChangesOrPaused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "monitoring-latest.md")
	report, err := Run(context.Background(), []Source{
		{Name: "mail", ReportPath: filepath.Join(t.TempDir(), "mail.md"), Sync: func(context.Context) (Result, error) {
			return Result{Status: "no-changes", Detail: "0 new messages"}, nil
		}},
		{Name: "smart_lms", ReportPath: filepath.Join(t.TempDir(), "lms.md"), Sync: func(context.Context) (Result, error) {
			return Result{Status: "paused", Detail: "0 configured courses"}, nil
		}},
	}, path, filepath.Join(t.TempDir(), "queue"), time.Now())
	if err != nil || report.NeedsAnalysis {
		t.Fatalf("report=%+v err=%v", report, err)
	}
}

func TestRunReportsStartedAndDurablyFinishedSourceProgress(t *testing.T) {
	dir := t.TempDir()
	events := []Progress{}
	report, err := Run(context.Background(), []Source{{
		Name: "mail", ReportPath: filepath.Join(dir, "mail.md"),
		Progress: func(progress Progress) { events = append(events, progress) },
		Sync: func(context.Context) (Result, error) {
			if err := os.WriteFile(filepath.Join(dir, "mail.md"), []byte("One update\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			return Result{Status: "delta", Detail: "one update", Added: 1}, nil
		},
	}}, filepath.Join(dir, "monitoring.md"), filepath.Join(dir, "queue"), time.Now())
	if err != nil || report.Pending.Pending != 1 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	if len(events) != 2 || events[0].Phase != "started" || events[0].Timeout != 5*time.Minute {
		t.Fatalf("unexpected start progress: %+v", events)
	}
	if events[1].Phase != "finished" || events[1].Status != "delta" || events[1].Detail != "one update" {
		t.Fatalf("unexpected finish progress: %+v", events)
	}
	if _, err := queue.Inspect(filepath.Join(dir, "queue"), report.Pending.Items[0].ID); err != nil {
		t.Fatalf("finished was reported without a durable event: %v", err)
	}
}

func TestSourceSuccessThenFailureAndRetryKeepPendingDelta(t *testing.T) {
	dir := t.TempDir()
	queueDir, reportPath, latest := filepath.Join(dir, "queue"), filepath.Join(dir, "monitoring.md"), filepath.Join(dir, "mail.md")
	first := true
	sources := []Source{
		{Name: "mail", ReportPath: latest, Sync: func(context.Context) (Result, error) {
			body, added := "No new messages.\n", 0
			if first {
				body, added = "Important new assignment #42\n", 1
				first = false
			}
			if err := os.WriteFile(latest, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			return Result{Status: "ok", Added: added, Report: latest}, nil
		}},
		{Name: "lms", ReportPath: filepath.Join(dir, "lms.md"), Sync: func(context.Context) (Result, error) {
			return Result{Status: "failed", Detail: "access expired"}, errors.New("access expired")
		}},
	}
	initial, err := Run(context.Background(), sources, reportPath, queueDir, time.Unix(10, 0))
	if !IsRunError(err) || initial.Pending.Pending != 1 || len(initial.Pending.Failures) != 1 {
		t.Fatalf("initial=%+v err=%v", initial, err)
	}
	retry, err := Run(context.Background(), sources, reportPath, queueDir, time.Unix(20, 0))
	if !IsRunError(err) || retry.Pending.Pending != 1 || !retry.NeedsAnalysis {
		t.Fatalf("retry=%+v err=%v", retry, err)
	}
	for _, item := range retry.Pending.Items {
		if item.Source == "mail" {
			body, err := os.ReadFile(item.ReportPath)
			if err != nil || string(body) != "Important new assignment #42\n" {
				t.Fatalf("lost delta=%q err=%v", body, err)
			}
		}
	}
	ids := []string{retry.Pending.Items[0].ID}
	receipt := filepath.Join(dir, "review.json")
	decisions := []map[string]string{}
	for _, id := range ids {
		decisions = append(decisions, map[string]string{"kind": "event", "key": id, "outcome": "updated"})
	}
	receiptBody, err := json.Marshal(map[string]any{"decisions": decisions})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(receipt, receiptBody, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := queue.RecordReview(queueDir, ids, nil, receipt, nil, time.Unix(30, 0)); err != nil {
		t.Fatal(err)
	}
	quiet, err := Run(context.Background(), sources, reportPath, queueDir, time.Unix(40, 0))
	if !IsRunError(err) || quiet.NeedsAnalysis || len(quiet.Pending.Failures) != 1 {
		t.Fatalf("unchanged failure caused repeated alert=%+v err=%v", quiet, err)
	}
}

func TestCrashAfterSourceCheckpointRecoversReportBeforeAnotherSync(t *testing.T) {
	dir := t.TempDir()
	queueDir, latest := filepath.Join(dir, "queue"), filepath.Join(dir, "source.md")
	reportPath := filepath.Join(dir, "monitoring.md")
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected simulated crash")
			}
		}()
		_, _ = Run(context.Background(), []Source{{Name: "mail", ReportPath: latest, Sync: func(context.Context) (Result, error) {
			if err := os.WriteFile(latest, []byte("Assignment received before crash.\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "source-checkpoint.json"), []byte(`{"seen":[42]}`), 0o600); err != nil {
				t.Fatal(err)
			}
			panic("simulated process crash after source checkpoint")
		}}}, reportPath, queueDir, time.Unix(10, 0))
	}()
	report, err := Run(context.Background(), []Source{{Name: "mail", ReportPath: latest, Sync: func(context.Context) (Result, error) {
		paths, err := filepath.Glob(filepath.Join(queueDir, "events", "*", "report.md"))
		if err != nil || len(paths) != 1 {
			t.Fatalf("source called before recovering report: %v err=%v", paths, err)
		}
		body, err := os.ReadFile(paths[0])
		if err != nil || string(body) != "Assignment received before crash.\n" {
			t.Fatalf("recovered=%q err=%v", body, err)
		}
		if err := os.WriteFile(latest, []byte("No changes.\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return Result{Status: "no-changes", Report: latest}, nil
	}}}, reportPath, queueDir, time.Unix(20, 0))
	if err != nil || report.Pending.Pending != 1 || !report.NeedsAnalysis {
		t.Fatalf("restart=%+v err=%v", report, err)
	}
}

func TestCombinedReportFailureDoesNotLoseQueuedSourceDelta(t *testing.T) {
	dir := t.TempDir()
	queueDir, latest := filepath.Join(dir, "queue"), filepath.Join(dir, "source.md")
	blockedReport := filepath.Join(dir, "report-is-directory")
	if err := os.Mkdir(blockedReport, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := Run(context.Background(), []Source{{Name: "mail", ReportPath: latest, Sync: func(context.Context) (Result, error) {
		if err := os.WriteFile(latest, []byte("New assignment\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return Result{Status: "delta", Added: 1}, nil
	}}}, blockedReport, queueDir, time.Now())
	if err == nil {
		t.Fatal("expected combined report write failure")
	}
	summary, err := queue.Summarize(queueDir, 10, time.Now())
	if err != nil || summary.Pending != 1 {
		t.Fatalf("lost queued source=%+v err=%v", summary, err)
	}
}

func TestIncompleteSourceIsReportedWithoutBlockingOtherSources(t *testing.T) {
	dir := t.TempDir()
	continued := false
	report, err := Run(context.Background(), []Source{
		{Name: "mail", ReportPath: filepath.Join(dir, "mail.md"), Sync: func(context.Context) (Result, error) {
			return Result{Status: "incomplete", Detail: "one message awaiting bounded retry"}, nil
		}},
		{Name: "healthy", ReportPath: filepath.Join(dir, "healthy.md"), Sync: func(context.Context) (Result, error) { continued = true; return Result{Status: "no-changes"}, nil }},
	}, filepath.Join(dir, "monitor.md"), filepath.Join(dir, "queue"), time.Now())
	if !IsRunError(err) || !continued || report.Pending.Pending != 0 || len(report.Pending.Failures) != 1 {
		t.Fatalf("report=%+v continued=%t err=%v", report, continued, err)
	}
}

func TestSourceTimeoutPersistsFailureAndContinuesWithFreshDeadline(t *testing.T) {
	dir := t.TempDir()
	continued := false
	var expired context.Context
	report, err := Run(context.Background(), []Source{
		{Name: "slow", ReportPath: filepath.Join(dir, "slow.md"), Timeout: 10 * time.Millisecond, Sync: func(ctx context.Context) (Result, error) {
			expired = ctx
			<-ctx.Done()
			// Even a collector that accidentally returns nil cannot claim no-changes.
			return Result{Status: "no-changes"}, nil
		}},
		{Name: "healthy", ReportPath: filepath.Join(dir, "healthy.md"), Sync: func(ctx context.Context) (Result, error) {
			continued = true
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > 5*time.Minute || time.Until(deadline) < 4*time.Minute || ctx.Err() != nil {
				t.Fatalf("invalid fresh default deadline: %v %v", deadline, ctx.Err())
			}
			if expired.Err() != context.DeadlineExceeded {
				t.Fatal("slow source was not canceled")
			}
			return Result{Status: "no-changes"}, nil
		}},
	}, filepath.Join(dir, "monitor.md"), filepath.Join(dir, "queue"), time.Now())
	if !IsRunError(err) || !continued || report.Results[0].Status != "failed" || report.Results[0].Detail != context.DeadlineExceeded.Error() {
		t.Fatalf("report=%+v continued=%v err=%v", report, continued, err)
	}
	if report.Pending.Pending != 0 || len(report.Pending.Failures) != 1 {
		t.Fatalf("timeout was not durable: %+v", report.Pending)
	}
}
