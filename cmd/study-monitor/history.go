package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/chupakobra6/study-automation/internal/config"
	"github.com/chupakobra6/study-automation/internal/queue"
)

type historyEvent struct {
	ID                  string    `json:"id"`
	Source              string    `json:"source"`
	Status              string    `json:"status"`
	CollectionStartedAt time.Time `json:"collection_started_at"`
	ReportPath          string    `json:"report_path"`
	Acknowledged        bool      `json:"acknowledged"`
	ReceiptPath         string    `json:"receipt_path,omitempty"`
}

type historyDay struct {
	Date     string `json:"date"`
	Coverage string `json:"coverage"`
}

func runHistory(args []string) error {
	f := flag.NewFlagSet("history", flag.ContinueOnError)
	configPath := f.String("config", defaultConfig, "configuration")
	date := f.String("date", "", "one collection date, YYYY-MM-DD")
	from := f.String("from", "", "first collection date, inclusive")
	to := f.String("to", "", "last collection date, inclusive")
	if err := f.Parse(args); err != nil {
		return err
	}
	if (*date != "" && (*from != "" || *to != "")) || (*date == "" && (*from == "" || *to == "")) {
		return errors.New("use --date YYYY-MM-DD or both --from YYYY-MM-DD --to YYYY-MM-DD")
	}
	if *date != "" {
		*from, *to = *date, *date
	}
	moscow, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		return err
	}
	start, err := time.ParseInLocation("2006-01-02", *from, moscow)
	if err != nil {
		return fmt.Errorf("--from: %w", err)
	}
	endDay, err := time.ParseInLocation("2006-01-02", *to, moscow)
	if err != nil {
		return fmt.Errorf("--to: %w", err)
	}
	if endDay.Before(start) {
		return errors.New("--to must not precede --from")
	}
	end := endDay.AddDate(0, 0, 1)
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	paths, err := filepath.Glob(filepath.Join(collectionDir(cfg.Paths.StateDir), "*.json"))
	if err != nil {
		return err
	}
	runs := []collectionRun{}
	for _, path := range paths {
		if filepath.Base(path) == "latest.json" || filepath.Base(path) == "latest-full.json" {
			continue
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var run collectionRun
		if err := json.Unmarshal(body, &run); err != nil {
			return fmt.Errorf("read run %s: %w", path, err)
		}
		if !run.StartedAt.Before(start) && run.StartedAt.Before(end) {
			runs = append(runs, run)
		}
	}
	items, err := queue.ListEvents(cfg.Monitoring.QueueDir)
	if err != nil {
		return err
	}
	events := eventsInPeriod(items, start, end)
	days := []historyDay{}
	for day := start; day.Before(end); day = day.AddDate(0, 0, 1) {
		coverage := "unrecorded"
		for _, run := range runs {
			if run.StartedAt.In(moscow).Format("2006-01-02") == day.Format("2006-01-02") {
				if !run.FullScope {
					if coverage == "unrecorded" {
						coverage = "targeted-only"
					}
					continue
				}
				if run.Status == "complete" {
					coverage = "complete"
				} else if coverage != "complete" {
					coverage = "partial"
				}
			}
		}
		days = append(days, historyDay{Date: day.Format("2006-01-02"), Coverage: coverage})
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"period":  map[string]string{"from": *from, "to": *to, "timezone": "Europe/Moscow"},
		"meaning": "Events grouped by collection start time; source content may describe an earlier date. Missing runs do not prove no study activity.",
		"days":    days, "runs": runs, "events": events,
	})
}

func eventsInPeriod(items []queue.Item, start, end time.Time) []historyEvent {
	events := []historyEvent{}
	for _, item := range items {
		if item.CreatedAt.Before(start) || !item.CreatedAt.Before(end) {
			continue
		}
		events = append(events, historyEvent{ID: item.ID, Source: item.Source, Status: item.Status, CollectionStartedAt: item.CreatedAt, ReportPath: item.ReportPath, Acknowledged: item.Acknowledged, ReceiptPath: item.ReceiptPath})
	}
	return events
}
