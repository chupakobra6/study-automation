package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/chupakobra6/study-automation/internal/config"
)

func TestOSRunnerCancellationTerminatesWrapperAndChildPipes(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "child.pid")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	childPID := make(chan int, 1)
	go func() {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			if data, err := os.ReadFile(ready); err == nil {
				pid, _ := strconv.Atoi(string(data))
				childPID <- pid
				cancel()
				return
			}
			select {
			case <-ctx.Done():
				childPID <- 0
				return
			case <-ticker.C:
			}
		}
	}()
	started := time.Now()
	_, _, err := (OSRunner{}).Run(ctx, t.TempDir(), os.Args[0], "-test.run=^TestOSRunnerProcess$", "--", "parent", ready)
	pid := <-childPID
	if pid > 0 {
		defer syscall.Kill(pid, syscall.SIGKILL)
	}
	if pid == 0 || !errors.Is(err, context.Canceled) || time.Since(started) > 2*time.Second {
		t.Fatalf("process tree did not stop promptly: pid=%d err=%v duration=%v", pid, err, time.Since(started))
	}
}

// The test binary itself supplies both wrapper and child; no shell fixture.
func TestOSRunnerProcess(t *testing.T) {
	separator := -1
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 {
		return
	}
	args := os.Args[separator+1:]
	if len(args) != 2 {
		os.Exit(2)
	}
	if args[0] == "child" {
		if err := os.WriteFile(args[1], []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
			os.Exit(2)
		}
		fmt.Fprintln(os.Stdout, "fixture child holds the inherited output pipe")
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestOSRunnerProcess$", "--", "child", args[1])
	child.Stdout, child.Stderr = os.Stdout, os.Stderr
	if err := child.Run(); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

type fakeRunner struct {
	stdout string
	stderr string
	err    error
	calls  [][]string
}

func (f *fakeRunner) Run(_ context.Context, dir, name string, args ...string) (string, string, error) {
	f.calls = append(f.calls, append([]string{dir, name}, args...))
	return f.stdout, f.stderr, f.err
}

func telegramConfig(t *testing.T) config.Config {
	t.Helper()
	dir := t.TempDir()
	binary := filepath.Join(dir, "telegram-harvest", "bin", "telegram-harvest")
	if err := osWriteExecutable(binary); err != nil {
		t.Fatal(err)
	}
	return config.Config{
		ConfigPath: filepath.Join(dir, "study_data.local.toml"),
		Paths: config.Paths{
			TelegramHarvest: filepath.Join(dir, "telegram-harvest"),
			StudyData:       filepath.Join(dir, "tools", "study-data"),
		},
		Telegram: config.Telegram{
			ExpectedUsername: "study_reader", ExpectedUserID: 424242,
			Chats: []config.TelegramChat{{ID: "1", Name: "study"}},
		},
		Updates: config.Updates{StateDir: filepath.Join(dir, "updates")},
	}
}

func writeTelegramDelta(t *testing.T, cfg config.Config, visible, digestOmitted, messagesOmitted int, required, gaps []string) {
	t.Helper()
	if err := os.MkdirAll(cfg.Updates.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]any{
		"status": "ok", "generated_at": time.Now().UTC().Format(time.RFC3339),
		"configured_chat_count": len(cfg.Telegram.Chats), "visible_added": visible,
		"digest_omitted": digestOmitted, "messages_omitted": messagesOmitted,
		"review_required_paths": required, "review_gaps": gaps,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.Updates.StateDir, "telegram-latest.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestTelegramRejectsWrongAccountBeforeSync(t *testing.T) {
	cfg := telegramConfig(t)
	runner := &fakeRunner{stdout: `{"id":13,"username":"other_account"}`}
	result, err := SyncTelegram(context.Background(), cfg, runner, false)
	if err == nil || result.Status != "blocked" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if !strings.Contains(result.Detail, "expected @study_reader") {
		t.Fatalf("detail=%q", result.Detail)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("calls=%d, want identity check only", len(runner.calls))
	}
}

func TestTelegramChecksIdentityThenUsesStudyDataAdapter(t *testing.T) {
	cfg := telegramConfig(t)
	writeTelegramDelta(t, cfg, 0, 0, 0, nil, nil)
	runner := &sequencedRunner{responses: []fakeResponse{
		{stdout: `{"id":424242,"username":"study_reader"}`},
		{stdout: "telegram delta: visible_added=0 status=no-changes"},
	}}
	result, err := SyncTelegram(context.Background(), cfg, runner, false)
	if err != nil || result.Status != "ok" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if len(runner.calls) != 2 || !containsAll(runner.calls[1], "sync", "telegram", "--apply") {
		t.Fatalf("unexpected calls: %#v", runner.calls)
	}
}

func TestTelegramCarriesRequiredEvidenceIntoMonitorResult(t *testing.T) {
	cfg := telegramConfig(t)
	evidence := filepath.Join(t.TempDir(), "physical-education.docx")
	if err := os.WriteFile(evidence, []byte("document"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeTelegramDelta(t, cfg, 1, 0, 0, []string{evidence}, nil)
	runner := &sequencedRunner{responses: []fakeResponse{
		{stdout: `{"id":424242,"username":"study_reader"}`},
		{stdout: "telegram sync complete"},
	}}
	result, err := SyncTelegram(context.Background(), cfg, runner, false)
	if err != nil || result.Added != 1 || result.Attachments != 1 || len(result.RequiredEvidence) != 1 || result.RequiredEvidence[0] != evidence {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestTelegramMissingRequiredEvidenceIsIncomplete(t *testing.T) {
	cfg := telegramConfig(t)
	writeTelegramDelta(t, cfg, 1, 0, 0, nil, []string{"study topic #42: document:assignment.docx"})
	runner := &sequencedRunner{responses: []fakeResponse{
		{stdout: `{"id":424242,"username":"study_reader"}`},
		{stdout: "telegram sync complete"},
	}}
	result, err := SyncTelegram(context.Background(), cfg, runner, false)
	if err == nil || result.Status != "incomplete" || !strings.Contains(result.Detail, "no local file") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestTelegramShortlistTruncationIsCompleteWhenFullReportIsIntact(t *testing.T) {
	cfg := telegramConfig(t)
	writeTelegramDelta(t, cfg, 26, 1, 0, nil, nil)
	runner := &sequencedRunner{responses: []fakeResponse{
		{stdout: `{"id":424242,"username":"study_reader"}`},
		{stdout: "telegram sync complete"},
	}}
	result, err := SyncTelegram(context.Background(), cfg, runner, false)
	if err != nil || result.Status != "ok" || !result.NeedsAnalysis {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestTelegramTruncatedFullReportIsIncomplete(t *testing.T) {
	cfg := telegramConfig(t)
	writeTelegramDelta(t, cfg, 301, 0, 1, nil, nil)
	runner := &sequencedRunner{responses: []fakeResponse{
		{stdout: `{"id":424242,"username":"study_reader"}`},
		{stdout: "telegram sync complete"},
	}}
	result, err := SyncTelegram(context.Background(), cfg, runner, false)
	if err == nil || result.Status != "incomplete" || !strings.Contains(result.Detail, "omitted 1 new message") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestSmartLMSWithEmptyAllowlistIsPausedWithoutCommand(t *testing.T) {
	runner := &fakeRunner{err: errors.New("must not run")}
	result, err := SyncSmartLMS(context.Background(), config.Config{}, runner, false)
	if err != nil || result.Status != "paused" || result.Detail != "0 configured courses" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("runner was called: %#v", runner.calls)
	}
}

func TestNetologyUsesFreshStructuredDeltaAndCurrentModuleScope(t *testing.T) {
	state := t.TempDir()
	cfg := config.Config{ConfigPath: filepath.Join(state, "study_data.local.toml"),
		Updates:  config.Updates{StateDir: state},
		Netology: config.Netology{Courses: []config.SmartLMSCourse{{Slug: "os"}, {Slug: "networks"}}}}
	evidence := filepath.Join(state, "assignment.md")
	if err := os.WriteFile(evidence, []byte("full assignment"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]any{
		"status": "baseline", "generated_at": time.Now().UTC().Format(time.RFC3339),
		"subject_count": 2, "needs_analysis": true, "review_required_paths": []string{evidence},
		"coverage_status": "complete", "coverage": map[string]any{
			"os":       map[string]any{"version": 1, "status": "complete", "remaining_urls": []string{}},
			"networks": map[string]any{"version": 1, "status": "complete", "remaining_urls": []string{}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "netology-latest.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	result, err := SyncNetology(context.Background(), cfg, runner, false)
	if err != nil || result.Source != "netology" || result.Status != "baseline" || !result.NeedsAnalysis ||
		!result.AnalysisDecisionKnown || len(result.RequiredEvidence) != 1 || result.RequiredEvidence[0] != evidence {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if len(runner.calls) != 1 || !slices.Contains(runner.calls[0], "netology") {
		t.Fatalf("wrong sync command: %v", runner.calls)
	}
}

func TestSmartLMSUsesFreshStructuredDeltaForAnalysis(t *testing.T) {
	for _, status := range []string{"baseline", "delta", "no-changes"} {
		t.Run(status, func(t *testing.T) {
			cfg := config.Config{Updates: config.Updates{StateDir: t.TempDir()}, SmartLMS: config.SmartLMS{Courses: []config.SmartLMSCourse{{Slug: "databases"}}}}
			wantAnalysis := status != "no-changes"
			data, err := json.Marshal(map[string]any{
				"status": status, "generated_at": time.Now().UTC().Format(time.RFC3339),
				"subject_count": 1, "needs_analysis": wantAnalysis,
				"coverage_status": "complete", "coverage": map[string]any{"databases": map[string]any{"version": 1, "status": "complete", "remaining_urls": []string{}}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cfg.Updates.StateDir, "smart_lms-latest.json"), data, 0o600); err != nil {
				t.Fatal(err)
			}
			result, err := SyncSmartLMS(context.Background(), cfg, &fakeRunner{stdout: "unstructured harvester logs"}, false)
			if err != nil || result.Status != status || result.NeedsAnalysis != wantAnalysis || !result.AnalysisDecisionKnown {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}
}

func TestSmartLMSRejectsMissingStaleOrIncompleteDelta(t *testing.T) {
	for _, report := range []string{
		"",
		`{"status":"no-changes","generated_at":"2020-01-01T00:00:00Z","subject_count":1,"needs_analysis":false}`,
		`{"status":"no-changes","subject_count":1}`,
	} {
		cfg := config.Config{Updates: config.Updates{StateDir: t.TempDir()}, SmartLMS: config.SmartLMS{Courses: []config.SmartLMSCourse{{Slug: "databases"}}}}
		if report != "" {
			if err := os.WriteFile(filepath.Join(cfg.Updates.StateDir, "smart_lms-latest.json"), []byte(report), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		result, err := SyncSmartLMS(context.Background(), cfg, &fakeRunner{}, false)
		if err == nil || result.Status != "failed" {
			t.Fatalf("report=%s result=%+v err=%v", report, result, err)
		}
	}
}

func TestSmartLMSRequiresFreshCatalogWhenEnabled(t *testing.T) {
	for _, tc := range []struct {
		name, catalogStatus, coverageStatus, wantStatus string
	}{
		{"complete", "complete", "complete", "no-changes"},
		{"unconfirmed", "unknown", "unknown", "incomplete"},
		{"missing", "", "complete", "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{Updates: config.Updates{StateDir: t.TempDir()}, SmartLMS: config.SmartLMS{
				CatalogScan: true, Courses: []config.SmartLMSCourse{{Slug: "databases"}},
			}}
			data, err := json.Marshal(map[string]any{
				"status": "no-changes", "generated_at": time.Now().UTC().Format(time.RFC3339),
				"subject_count": 1, "needs_analysis": false,
				"coverage_status": tc.coverageStatus, "catalog": map[string]any{"status": tc.catalogStatus},
				"coverage": map[string]any{"databases": map[string]any{"version": 1, "status": "complete", "remaining_urls": []string{}}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cfg.Updates.StateDir, "smart_lms-latest.json"), data, 0o600); err != nil {
				t.Fatal(err)
			}
			result, _ := SyncSmartLMS(context.Background(), cfg, &fakeRunner{}, false)
			if result.Status != tc.wantStatus {
				t.Fatalf("result=%+v, want %s", result, tc.wantStatus)
			}
		})
	}
}

type fakeResponse struct {
	stdout string
	stderr string
	err    error
}

type sequencedRunner struct {
	responses []fakeResponse
	calls     [][]string
}

func (f *sequencedRunner) Run(_ context.Context, dir, name string, args ...string) (string, string, error) {
	f.calls = append(f.calls, append([]string{dir, name}, args...))
	response := f.responses[len(f.calls)-1]
	return response.stdout, response.stderr, response.err
}

func containsAll(values []string, wanted ...string) bool {
	joined := strings.Join(values, "\x00")
	for _, value := range wanted {
		if !strings.Contains(joined, value) {
			return false
		}
	}
	return true
}

func osWriteExecutable(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte("fixture"), 0o755)
}

func TestSmartLMSRetainsConfirmedPartialDeltaDespiteCollectorExit(t *testing.T) {
	for _, needsAnalysis := range []bool{true, false} {
		cfg := config.Config{Updates: config.Updates{StateDir: t.TempDir()}, SmartLMS: config.SmartLMS{Courses: []config.SmartLMSCourse{{Slug: "databases"}}}}
		evidence := filepath.Join(t.TempDir(), "confirmed.md")
		if err := os.WriteFile(evidence, []byte("confirmed assignment"), 0o600); err != nil {
			t.Fatal(err)
		}
		paths := []string{}
		if needsAnalysis {
			paths = append(paths, evidence)
		}
		data, err := json.Marshal(map[string]any{
			"status": "delta", "generated_at": time.Now().UTC().Format(time.RFC3339),
			"subject_count": 1, "needs_analysis": needsAnalysis, "review_required_paths": paths,
			"coverage_status": "partial", "coverage": map[string]any{"databases": map[string]any{"version": 1, "status": "partial", "stop_reason": "max_pages", "remaining_urls": []string{"page-2"}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cfg.Updates.StateDir, "smart_lms-latest.json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
		result, err := SyncSmartLMS(context.Background(), cfg, &fakeRunner{err: errors.New("exit status 1")}, false)
		if err == nil || result.Status != "incomplete" || result.NeedsAnalysis != needsAnalysis || !result.AnalysisDecisionKnown || len(result.RequiredEvidence) != len(paths) || !strings.Contains(result.Detail, "1 remaining") {
			t.Fatalf("result=%+v err=%v", result, err)
		}
	}
}
