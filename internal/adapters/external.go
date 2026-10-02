package adapters

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/chupakobra6/study-automation/internal/config"
)

type Result struct {
	Source                string
	Status                string
	Detail                string
	Output                string
	Added                 int
	Attachments           int
	RequiredEvidence      []string
	NeedsAnalysis         bool
	AnalysisDecisionKnown bool
}

type CommandRunner interface {
	Run(ctx context.Context, dir, name string, args ...string) (stdout, stderr string, err error)
}

type OSRunner struct{}

func (OSRunner) Run(ctx context.Context, dir, name string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	// Browser wrappers spawn children that inherit stdout/stderr. Kill the whole
	// isolated process group on cancellation, then bound pipe cleanup as well.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		} else {
			return err
		}
	}
	cmd.WaitDelay = 5 * time.Second
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if cmd.Process != nil {
		// Stop remaining group members when a wrapper exits first and WaitDelay
		// closes inherited pipes, instead of leaving its children running.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	return stdout.String(), stderr.String(), err
}

type telegramIdentity struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

func TelegramIdentity(ctx context.Context, cfg config.Config, runner CommandRunner) (Result, error) {
	binary := filepath.Join(cfg.Paths.TelegramHarvest, "bin", "telegram-harvest")
	if _, err := os.Stat(binary); err != nil {
		return Result{Source: "telegram", Status: "blocked", Detail: "telegram-harvest binary is missing"}, err
	}
	stdout, stderr, err := runner.Run(ctx, cfg.Paths.TelegramHarvest, binary, "--profile", "study", "me", "-json")
	if err != nil {
		detail := strings.TrimSpace(stderr)
		if detail == "" {
			detail = err.Error()
		}
		return Result{Source: "telegram", Status: "blocked", Detail: detail}, err
	}
	var identity telegramIdentity
	if err := json.Unmarshal([]byte(stdout), &identity); err != nil {
		return Result{Source: "telegram", Status: "blocked", Detail: "invalid identity JSON"}, err
	}
	actualUsername := strings.TrimPrefix(identity.Username, "@")
	if !strings.EqualFold(actualUsername, cfg.Telegram.ExpectedUsername) || identity.ID != cfg.Telegram.ExpectedUserID {
		err := fmt.Errorf("authorized as @%s (ID %d), expected @%s (ID %d)", actualUsername, identity.ID, cfg.Telegram.ExpectedUsername, cfg.Telegram.ExpectedUserID)
		return Result{Source: "telegram", Status: "blocked", Detail: err.Error()}, err
	}
	return Result{Source: "telegram", Status: "ok", Detail: fmt.Sprintf("@%s (ID %d), %d configured chats", actualUsername, identity.ID, len(cfg.Telegram.Chats))}, nil
}

func SyncTelegram(ctx context.Context, cfg config.Config, runner CommandRunner, dryRun bool) (Result, error) {
	identity, err := TelegramIdentity(ctx, cfg, runner)
	if err != nil {
		return identity, err
	}
	if len(cfg.Telegram.Chats) == 0 {
		err := errors.New("no Telegram study chats are configured")
		return Result{Source: "telegram", Status: "blocked", Detail: err.Error()}, err
	}
	mode := "--apply"
	if dryRun {
		mode = "--dry-run"
	}
	started := time.Now().UTC().Truncate(time.Second)
	stdout, stderr, err := runner.Run(ctx, filepath.Dir(cfg.ConfigPath), cfg.Paths.StudyData, "--config", cfg.ConfigPath, "sync", "telegram", mode)
	if err != nil {
		detail := strings.TrimSpace(stderr)
		if detail == "" {
			detail = err.Error()
		}
		return Result{Source: "telegram", Status: "failed", Detail: detail, Output: strings.TrimSpace(stdout)}, err
	}
	result := Result{Source: "telegram", Status: "ok", Output: strings.TrimSpace(stdout)}
	if dryRun {
		result.Detail = fmt.Sprintf("@%s; %d chats planned", cfg.Telegram.ExpectedUsername, len(cfg.Telegram.Chats))
		return result, nil
	}
	data, err := os.ReadFile(filepath.Join(cfg.Updates.StateDir, "telegram-latest.json"))
	var report struct {
		Status          string    `json:"status"`
		GeneratedAt     time.Time `json:"generated_at"`
		ConfiguredChats int       `json:"configured_chat_count"`
		VisibleAdded    int       `json:"visible_added"`
		DigestOmitted   int       `json:"digest_omitted"`
		MessagesOmitted int       `json:"messages_omitted"`
		ReviewRequired  []string  `json:"review_required_paths"`
		ReviewGaps      []string  `json:"review_gaps"`
	}
	if err == nil {
		err = json.Unmarshal(data, &report)
	}
	validStatus := report.Status == "ok" || report.Status == "baseline" || report.Status == "reset"
	if err == nil && (report.GeneratedAt.Before(started) || report.ConfiguredChats != len(cfg.Telegram.Chats) || report.VisibleAdded < 0 || report.DigestOmitted < 0 || report.MessagesOmitted < 0 || !validStatus) {
		err = errors.New("missing, stale or incomplete Telegram delta report")
	}
	if err == nil {
		for _, path := range report.ReviewRequired {
			info, statErr := os.Stat(path)
			if !filepath.IsAbs(path) || statErr != nil || !info.Mode().IsRegular() {
				err = fmt.Errorf("required Telegram evidence is unavailable: %s", path)
				break
			}
		}
	}
	result.Status = report.Status
	result.Added = report.VisibleAdded
	result.Attachments = len(report.ReviewRequired) + len(report.ReviewGaps)
	result.RequiredEvidence = report.ReviewRequired
	result.NeedsAnalysis = report.Status != "ok" || report.VisibleAdded > 0
	result.Detail = fmt.Sprintf("@%s; %d chats synchronized; %d new messages; %d file(s) require review", cfg.Telegram.ExpectedUsername, len(cfg.Telegram.Chats), result.Added, len(result.RequiredEvidence))
	if err == nil && len(report.ReviewGaps) > 0 {
		err = fmt.Errorf("%d Telegram attachment(s) require review but have no local file", len(report.ReviewGaps))
	}
	if err == nil && report.MessagesOmitted > 0 {
		err = fmt.Errorf("Telegram report omitted %d new message(s); inspect the agent view before acknowledging", report.MessagesOmitted)
	}
	if err != nil {
		result.Status = "incomplete"
		result.Detail = err.Error()
		return result, err
	}
	return result, nil
}

func SyncSmartLMS(ctx context.Context, cfg config.Config, runner CommandRunner, dryRun bool) (Result, error) {
	return syncLMS(ctx, cfg, runner, dryRun, "smart_lms", "Smart LMS", cfg.SmartLMS.Courses, cfg.SmartLMS.CatalogScan)
}

func SyncNetology(ctx context.Context, cfg config.Config, runner CommandRunner, dryRun bool) (Result, error) {
	return syncLMS(ctx, cfg, runner, dryRun, "netology", "Netology", cfg.Netology.Courses, false)
}

func syncLMS(ctx context.Context, cfg config.Config, runner CommandRunner, dryRun bool, source, label string, courses []config.SmartLMSCourse, catalogScan bool) (Result, error) {
	if len(courses) == 0 {
		return Result{Source: source, Status: "paused", Detail: "0 configured courses"}, nil
	}
	mode := "--apply"
	if dryRun {
		mode = "--dry-run"
	}
	started := time.Now().UTC().Truncate(time.Second)
	stdout, stderr, runErr := runner.Run(ctx, filepath.Dir(cfg.ConfigPath), cfg.Paths.StudyData, "--config", cfg.ConfigPath, "sync", source, mode)
	result := Result{Source: source, Status: "ok", Detail: fmt.Sprintf("%d configured courses synchronized", len(courses)), Output: strings.TrimSpace(stdout)}
	if dryRun {
		return result, runErr
	}
	data, err := os.ReadFile(filepath.Join(cfg.Updates.StateDir, source+"-latest.json"))
	var report struct {
		Status         string    `json:"status"`
		GeneratedAt    time.Time `json:"generated_at"`
		SubjectCount   int       `json:"subject_count"`
		NeedsAnalysis  *bool     `json:"needs_analysis"`
		ReviewRequired []string  `json:"review_required_paths"`
		CoverageStatus string    `json:"coverage_status"`
		Catalog        struct {
			Status string `json:"status"`
		} `json:"catalog"`
		Coverage map[string]struct {
			Version    int      `json:"version"`
			Status     string   `json:"status"`
			StopReason string   `json:"stop_reason"`
			Remaining  []string `json:"remaining_urls"`
		} `json:"coverage"`
	}
	if err == nil {
		err = json.Unmarshal(data, &report)
	}
	if err == nil && (report.NeedsAnalysis == nil || report.GeneratedAt.Before(started) || report.SubjectCount != len(courses) || (report.Status != "baseline" && report.Status != "delta" && report.Status != "no-changes")) {
		err = fmt.Errorf("missing, stale or incomplete %s delta report", label)
	}
	if err == nil {
		coverageStatus := "complete"
		for _, course := range courses {
			coverage, exists := report.Coverage[course.Slug]
			if !exists || coverage.Version != 1 || (coverage.Status != "complete" && coverage.Status != "partial" && coverage.Status != "unknown") || (coverage.Status == "complete" && len(coverage.Remaining) > 0) {
				err = fmt.Errorf("missing or invalid %s coverage", label)
				break
			}
			if coverage.Status == "unknown" || (coverage.Status == "partial" && coverageStatus == "complete") {
				coverageStatus = coverage.Status
			}
		}
		if catalogScan {
			if report.Catalog.Status != "complete" && report.Catalog.Status != "unknown" {
				err = errors.New("missing or invalid Smart LMS catalog coverage")
			} else if report.Catalog.Status == "unknown" {
				coverageStatus = "unknown"
			}
		}
		if report.CoverageStatus != coverageStatus {
			err = fmt.Errorf("inconsistent %s coverage status", label)
		}
	}
	if err == nil {
		for _, path := range report.ReviewRequired {
			info, statErr := os.Stat(path)
			if !filepath.IsAbs(path) || statErr != nil || !info.Mode().IsRegular() {
				err = fmt.Errorf("required %s evidence is unavailable: %s", label, path)
				break
			}
		}
	}
	if err != nil {
		result.Status = "failed"
		result.Detail = fmt.Sprintf("read %s delta: %v", label, err)
		if runErr != nil {
			result.Detail += "; " + strings.TrimSpace(stderr)
		}
		return result, err
	}
	result.AnalysisDecisionKnown = true
	result.Status = report.Status
	result.NeedsAnalysis = *report.NeedsAnalysis
	result.Attachments = len(report.ReviewRequired)
	result.RequiredEvidence = report.ReviewRequired
	if report.CoverageStatus != "complete" {
		result.Status = "incomplete"
		var gaps []string
		for _, course := range courses {
			coverage := report.Coverage[course.Slug]
			if coverage.Status != "complete" {
				gaps = append(gaps, fmt.Sprintf("%s: %s (%s, %d remaining); continue collection", course.Slug, coverage.Status, coverage.StopReason, len(coverage.Remaining)))
			}
		}
		if catalogScan && report.Catalog.Status != "complete" {
			gaps = append(gaps, "enrolled-course catalog: unconfirmed; continue collection")
		}
		result.Detail = strings.Join(gaps, "; ")
		return result, errors.New(result.Detail)
	}
	if runErr != nil {
		result.Status = "failed"
		result.Detail = strings.TrimSpace(stderr)
		if result.Detail == "" {
			result.Detail = runErr.Error()
		}
		return result, runErr
	}
	return result, nil
}
