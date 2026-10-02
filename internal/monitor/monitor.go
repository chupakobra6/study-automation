package monitor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/chupakobra6/study-automation/internal/queue"
)

type Result struct {
	Source                string
	Status                string
	Detail                string
	Report                string
	Added                 int
	Attachments           int
	RequiredEvidence      []string
	NeedsAnalysis         bool
	AnalysisDecisionKnown bool
	DurationMillis        int64
}

type Source struct {
	Name       string
	ReportPath string
	Timeout    time.Duration
	Sync       func(context.Context) (Result, error)
	Progress   func(Progress)
}

type Progress struct {
	Source   string
	Phase    string
	Status   string
	Detail   string
	Timeout  time.Duration
	Duration time.Duration
}

type Report struct {
	GeneratedAt   time.Time
	Results       []Result
	NeedsAnalysis bool
	Path          string
	Pending       queue.Summary
}

type RunError struct {
	Failures []string
}

func (e *RunError) Error() string {
	return "monitoring failed: " + strings.Join(e.Failures, "; ")
}

func Run(ctx context.Context, sources []Source, reportPath, queueDir string, now time.Time) (Report, error) {
	report := Report{GeneratedAt: now.UTC(), Path: reportPath}
	store, err := queue.Open(queueDir)
	if err != nil {
		return report, err
	}
	defer store.Close()
	if err := store.Recover(); err != nil {
		return report, fmt.Errorf("recover pending source report: %w", err)
	}
	failures := make([]string, 0)
	for _, source := range sources {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		if err := store.Start(source.Name, source.ReportPath, now); err != nil {
			return report, fmt.Errorf("prepare durable source %s: %w", source.Name, err)
		}
		notify(source, Progress{Source: source.Name, Phase: "started", Timeout: sourceTimeout(source)})
		started := time.Now()
		result, err := syncSource(ctx, source)
		result.DurationMillis = time.Since(started).Milliseconds()
		if result.Source == "" {
			result.Source = source.Name
		}
		failed := err != nil || result.Status == "failed" || result.Status == "blocked" || result.Status == "incomplete"
		if failed {
			if result.Status == "" {
				result.Status = "failed"
			}
			if result.Detail == "" {
				if err != nil {
					result.Detail = err.Error()
				} else {
					result.Detail = "source reported " + result.Status
				}
			}
			failures = append(failures, fmt.Sprintf("%s: %s", source.Name, result.Detail))
		}
		report.Results = append(report.Results, result)
		if err := store.Complete(queue.Outcome{Source: source.Name, Status: result.Status, Detail: result.Detail, Added: result.Added, Attachments: result.Attachments, RequiredEvidence: result.RequiredEvidence, NeedsAnalysis: result.NeedsAnalysis, AnalysisDecisionKnown: result.AnalysisDecisionKnown, Failed: failed}); err != nil {
			return report, fmt.Errorf("preserve source %s in pending queue: %w", source.Name, err)
		}
		notify(source, Progress{Source: source.Name, Phase: "finished", Status: result.Status, Detail: result.Detail, Duration: time.Duration(result.DurationMillis) * time.Millisecond})
	}
	report.Pending, err = store.Summary(10, time.Now())
	if err != nil {
		return report, err
	}
	report.NeedsAnalysis = len(report.Pending.Items) > 0
	if err := writeAtomic(reportPath, []byte(render(report)), 0o600); err != nil {
		return report, err
	}
	if len(failures) > 0 {
		return report, &RunError{Failures: failures}
	}
	return report, nil
}

// Sync runs synchronously: its writes must finish before the queue snapshots
// its report. Collectors must honor cancellation through their I/O adapters.
func syncSource(ctx context.Context, source Source) (Result, error) {
	sourceCtx, cancel := context.WithTimeout(ctx, sourceTimeout(source))
	defer cancel()
	result, err := source.Sync(sourceCtx)
	if contextErr := sourceCtx.Err(); contextErr != nil {
		err = contextErr
		result.Status = "failed"
		result.Detail = contextErr.Error()
	}
	return result, err
}

func sourceTimeout(source Source) time.Duration {
	if source.Timeout > 0 {
		return source.Timeout
	}
	return 5 * time.Minute
}

func notify(source Source, progress Progress) {
	if source.Progress != nil {
		source.Progress(progress)
	}
}

func render(report Report) string {
	var output strings.Builder
	fmt.Fprintln(&output, "# Study Monitoring")
	fmt.Fprintln(&output)
	fmt.Fprintf(&output, "Generated at: %s\n", report.GeneratedAt.Format(time.RFC3339))
	fmt.Fprintf(&output, "Needs analysis: `%t`\n\n", report.NeedsAnalysis)
	fmt.Fprintf(&output, "Pending analysis: %d\n", report.Pending.Pending)
	if report.Pending.OldestAt != nil {
		fmt.Fprintf(&output, "Oldest pending: %s\n", report.Pending.OldestAt.Format(time.RFC3339))
	}
	names := make([]string, 0, len(report.Pending.BySource))
	for source := range report.Pending.BySource {
		names = append(names, source)
	}
	sort.Strings(names)
	for _, source := range names {
		fmt.Fprintf(&output, "- %s: %d pending\n", source, report.Pending.BySource[source])
	}
	for _, item := range report.Pending.Items {
		fmt.Fprintf(&output, "- `%s` | %s | %s | %s | `%s`\n", item.ID, item.Source, item.Kind, item.CreatedAt.Format(time.RFC3339), item.ReportPath)
	}
	if report.Pending.Truncated {
		fmt.Fprintln(&output, "More pending items exist; use the queue summary command.")
	}
	fmt.Fprintln(&output)
	for _, result := range report.Results {
		fmt.Fprintf(&output, "## %s\n\n", result.Source)
		fmt.Fprintf(&output, "- Status: `%s`\n", result.Status)
		fmt.Fprintf(&output, "- Detail: %s\n", result.Detail)
		if result.Added > 0 {
			fmt.Fprintf(&output, "- Added: %d\n", result.Added)
		}
		if result.Attachments > 0 {
			fmt.Fprintf(&output, "- Attachments: %d\n", result.Attachments)
		}
		if len(result.RequiredEvidence) > 0 {
			fmt.Fprintf(&output, "- Required evidence review: %d file(s)\n", len(result.RequiredEvidence))
		}
		if result.Report != "" {
			fmt.Fprintf(&output, "- Source report: `%s`\n", result.Report)
		}
		fmt.Fprintln(&output)
	}
	return output.String()
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".monitoring-*.tmp")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(mode); err != nil {
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
	if err := os.Rename(tempName, path); err != nil {
		return err
	}
	return nil
}

func IsRunError(err error) bool {
	var target *RunError
	return errors.As(err, &target)
}
