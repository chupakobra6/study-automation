package club404

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/chupakobra6/study-automation/internal/monitor"
)

type Config struct {
	Subjects     []string `toml:"subjects"`
	Profile      string   `toml:"profile"`
	StateDir     string   `toml:"state_dir"`
	Checkpoint   string   `toml:"checkpoint"`
	LatestReport string   `toml:"latest_report"`
}

type Runner interface {
	Run(ctx context.Context, dir, name string, args ...string) (stdout, stderr string, err error)
}

type link struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

type item struct {
	Kind               string `json:"kind"`
	URL                string `json:"url"`
	Title              string `json:"title"`
	Subject            string `json:"subject"`
	Deadline           string `json:"deadline"`
	Body               string `json:"body"`
	Links              []link `json:"links"`
	DescriptionMissing bool   `json:"description_missing,omitempty"`
}

type export struct {
	Version  int      `json:"version"`
	BaseURL  string   `json:"base_url"`
	Subjects []string `json:"subjects"`
	Counts   struct {
		Subjects  int `json:"subjects"`
		Tasks     int `json:"tasks"`
		Materials int `json:"materials"`
	} `json:"counts"`
	Items []item `json:"items"`
}

type checkpoint struct {
	Version int             `json:"version"`
	Items   map[string]item `json:"items"`
}

func Validate(cfg Config) error {
	if len(cfg.Subjects) == 0 || cfg.Profile == "" || cfg.StateDir == "" || cfg.Checkpoint == "" || cfg.LatestReport == "" {
		return errors.New("club404 needs subjects, profile, state, checkpoint and report paths")
	}
	seen := map[string]bool{}
	for _, slug := range cfg.Subjects {
		if slug == "" || seen[slug] {
			return errors.New("club404 subjects must be unique nonempty slugs")
		}
		for _, r := range slug {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return errors.New("club404 subject slug contains an invalid character")
			}
		}
		seen[slug] = true
	}
	return nil
}

func Sync(ctx context.Context, cfg Config, harvestDir string, runner Runner, now time.Time) (result monitor.Result, err error) {
	result = monitor.Result{Source: "club404", Status: "failed", Report: cfg.LatestReport}
	if err := Validate(cfg); err != nil {
		result.Detail = err.Error()
		return result, err
	}
	binary := filepath.Join(harvestDir, ".venv", "bin", "hse-lms-harvest")
	if info, statErr := os.Stat(binary); statErr != nil || !info.Mode().IsRegular() {
		err := errors.New("club404 browser adapter is missing")
		result.Detail = err.Error()
		return result, err
	}
	scanPath := filepath.Join(cfg.StateDir, "scan.json")
	args := []string{"club404-export", "--headless", "--profile", cfg.Profile, "--out", scanPath}
	for _, subject := range cfg.Subjects {
		args = append(args, "--subject", subject)
	}
	_, _, runErr := runner.Run(ctx, harvestDir, binary, args...)
	if runErr != nil {
		result.Detail = "club404 browser scan failed: " + runErr.Error()
		return result, runErr
	}
	data, readErr := os.ReadFile(scanPath)
	if readErr != nil {
		result.Detail = "club404 scan output is missing"
		return result, readErr
	}
	var current export
	if err := json.Unmarshal(data, &current); err != nil {
		result.Detail = "club404 scan output is invalid"
		return result, err
	}
	if err := validateExport(current, cfg.Subjects); err != nil {
		result.Detail = err.Error()
		return result, err
	}
	previous := checkpoint{Items: map[string]item{}}
	data, readErr = os.ReadFile(cfg.Checkpoint)
	if readErr == nil {
		if err := json.Unmarshal(data, &previous); err != nil || previous.Version != 1 {
			result.Detail = "club404 checkpoint is invalid"
			return result, errors.New(result.Detail)
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		result.Detail = "club404 checkpoint is unreadable"
		return result, readErr
	}
	baseline := errors.Is(readErr, os.ErrNotExist)
	next := checkpoint{Version: 1, Items: map[string]item{}}
	for _, item := range current.Items {
		next.Items[item.URL] = item
	}
	var changes []string
	var evidence []string
	if !baseline {
		urls := make([]string, 0, len(next.Items))
		for url := range next.Items {
			urls = append(urls, url)
		}
		sort.Strings(urls)
		for _, url := range urls {
			item := next.Items[url]
			old, exists := previous.Items[url]
			if exists && digest(item) == digest(old) {
				continue
			}
			label := "Updated"
			if !exists {
				label = "New"
			}
			path := filepath.Join(cfg.StateDir, "evidence", digest(item)+".md")
			if err := writeAtomic(path, []byte(renderItem(item))); err != nil {
				result.Detail = "club404 evidence write failed"
				return result, err
			}
			changes = append(changes, fmt.Sprintf("- %s %s: [%s](%s); evidence: `%s`", label, item.Kind, item.Title, url, path))
			evidence = append(evidence, path)
		}
		for url, old := range previous.Items {
			if _, exists := next.Items[url]; !exists {
				path := filepath.Join(cfg.StateDir, "evidence", "removed-"+digest(old)+".md")
				if err := writeAtomic(path, []byte("Removed from the current club404 scan; verify in the primary source.\n\n"+renderItem(old))); err != nil {
					result.Detail = "club404 removed-item evidence write failed"
					return result, err
				}
				changes = append(changes, fmt.Sprintf("- Removed %s: [%s](%s); previous evidence: `%s`; verify in primary source", old.Kind, old.Title, url, path))
				evidence = append(evidence, path)
			}
		}
		sort.Strings(changes)
	}
	status := "no-changes"
	if baseline {
		status = "baseline"
	} else if len(changes) > 0 {
		status = "delta"
	}
	report := renderReport(now, current, status, changes)
	if err := writeAtomic(cfg.LatestReport, []byte(report)); err != nil {
		result.Detail = "club404 report write failed"
		return result, err
	}
	encoded, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return result, err
	}
	if err := writeAtomic(cfg.Checkpoint, append(encoded, '\n')); err != nil {
		result.Detail = "club404 checkpoint write failed"
		return result, err
	}
	result.Status = status
	result.Detail = fmt.Sprintf("%d subjects; %d tasks; %d materials; %d changes", current.Counts.Subjects, current.Counts.Tasks, current.Counts.Materials, len(changes))
	result.Added = len(changes)
	result.RequiredEvidence = evidence
	result.Attachments = len(evidence)
	result.NeedsAnalysis = len(changes) > 0
	result.AnalysisDecisionKnown = true
	return result, nil
}

func validateExport(current export, subjects []string) error {
	if current.Version != 1 || current.BaseURL != "https://club404.ru" || current.Counts.Subjects != len(subjects) || len(current.Subjects) != len(subjects) || len(current.Items) == 0 || len(current.Items) != current.Counts.Tasks+current.Counts.Materials {
		return errors.New("club404 scan is incomplete or has an unexpected schema")
	}
	allowed := map[string]bool{}
	for _, slug := range subjects {
		allowed[slug] = true
	}
	seen := map[string]bool{}
	for _, slug := range current.Subjects {
		if !allowed[slug] {
			return errors.New("club404 scan includes an unconfigured subject")
		}
		if seen[slug] {
			return errors.New("club404 scan repeats a subject")
		}
		seen[slug] = true
	}
	seen = map[string]bool{}
	tasks, materials := 0, 0
	for _, item := range current.Items {
		if item.DescriptionMissing && item.Kind != "task" {
			return errors.New("club404 missing description is only valid for a task card")
		}
		if !allowed[item.Subject] || item.Title == "" || item.Body == "" || seen[item.URL] || (item.Kind != "task" && item.Kind != "material") {
			return errors.New("club404 scan contains an invalid item")
		}
		prefix := "https://club404.ru/course/2/" + item.Kind + "s/"
		if !strings.HasPrefix(item.URL, prefix) {
			return errors.New("club404 item URL is outside the configured course")
		}
		seen[item.URL] = true
		if item.Kind == "task" {
			tasks++
		} else {
			materials++
		}
	}
	if tasks != current.Counts.Tasks || materials != current.Counts.Materials {
		return errors.New("club404 scan item counts do not match")
	}
	return nil
}

func renderItem(item item) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\nSource: secondary club404\nSubject: %s\nKind: %s\nURL: %s\n", item.Title, item.Subject, item.Kind, item.URL)
	if item.Deadline != "" {
		fmt.Fprintf(&b, "Displayed deadline: %s\n", item.Deadline)
	}
	if item.DescriptionMissing {
		fmt.Fprintln(&b, "Description missing: only the visible task card header is available; assignment requirements are unknown.")
	}
	if strings.Contains(strings.ToLower(item.Body), "произвольная дата") || strings.Contains(strings.ToLower(item.Body), "точного дедлайна пока что нет") {
		fmt.Fprintln(&b, "Deadline warning: club404 says its displayed date is arbitrary; do not use it as a due date.")
	}
	fmt.Fprint(&b, "\n## Content\n\n")
	fmt.Fprintln(&b, item.Body)
	if len(item.Links) > 0 {
		fmt.Fprint(&b, "\n## Links and attachments\n\n")
		for _, link := range item.Links {
			fmt.Fprintf(&b, "- %s: %s\n", link.Label, link.URL)
		}
	}
	return b.String()
}

func renderReport(now time.Time, current export, status string, changes []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# club404 — secondary study source\n\nCollected: %s\nStatus: %s\nSubjects: %d\nTasks: %d\nMaterials: %d\n\n", now.Format(time.RFC3339), status, current.Counts.Subjects, current.Counts.Tasks, current.Counts.Materials)
	fmt.Fprint(&b, "Dates, completion and obligations on this student-run platform require confirmation in LMS, Netology, official schedule, teacher messages or GitHub. A club404 status is not the user's submission status. Content may cover another group.\n\n")
	if len(changes) > 0 {
		fmt.Fprint(&b, "## Changes\n\n")
		for _, change := range changes {
			fmt.Fprintln(&b, change)
		}
	}
	return b.String()
}

var relativeBadge = regexp.MustCompile(`([0-9]{2}:[0-9]{2})[0-9]+[[:space:]]+(сек\.|мин\.|час\.|дн\.|день|дня|дней)`)

func digest(value item) string {
	value.Body = relativeBadge.ReplaceAllString(value.Body, "$1")
	value.Links = append([]link(nil), value.Links...)
	for index := range value.Links {
		value.Links[index].Label = relativeBadge.ReplaceAllString(value.Links[index].Label, "$1")
	}
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".club404-*.tmp")
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
