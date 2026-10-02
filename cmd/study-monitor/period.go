package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/mail"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/chupakobra6/study-automation/internal/config"
)

// A period is an index over source event timestamps in saved records. It does
// not reconstruct a remote service's earlier state or mutate sync checkpoints.
type periodRecord struct {
	Source     string `json:"source"`
	ID         string `json:"id"`
	At         string `json:"at"`
	Title      string `json:"title"`
	Preview    string `json:"preview,omitempty"`
	SourceFile string `json:"source_file"`
	SourceLine int    `json:"source_line"`
	DateBasis  string `json:"date_basis"`
	SnapshotAt string `json:"snapshot_at,omitempty"`
}

type periodCoverage struct {
	Source       string `json:"source"`
	Mode         string `json:"mode"`
	Detail       string `json:"detail"`
	Count        int    `json:"count"`
	SnapshotPath string `json:"current_snapshot_path,omitempty"`
}

type periodTaskDate struct {
	TaskID string `json:"task_id"`
	Title  string `json:"title"`
	Status string `json:"current_status"`
	At     string `json:"at"`
	Kind   string `json:"kind"`
	Label  string `json:"label"`
}

func runPeriod(args []string) error {
	if len(args) > 0 && args[0] == "inspect" {
		return runPeriodInspect(args[1:])
	}
	f := flag.NewFlagSet("period", flag.ContinueOnError)
	configPath := f.String("config", defaultConfig, "configuration")
	date := f.String("date", "", "source event date, YYYY-MM-DD")
	from := f.String("from", "", "first source event date, inclusive")
	to := f.String("to", "", "last source event date, inclusive")
	limit := f.Int("limit", 50, "maximum records in one page (1..200)")
	offset := f.Int("offset", 0, "record offset")
	if err := f.Parse(args); err != nil {
		return err
	}
	if (*date != "" && (*from != "" || *to != "")) || (*date == "" && (*from == "" || *to == "")) {
		return errors.New("use --date YYYY-MM-DD or both --from YYYY-MM-DD --to YYYY-MM-DD")
	}
	if *limit < 1 || *limit > 200 || *offset < 0 {
		return errors.New("--limit must be 1..200 and --offset must be nonnegative")
	}
	if *date != "" {
		*from, *to = *date, *date
	}
	moscow, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		return err
	}
	start, err := time.ParseInLocation("2006-01-02", *from, moscow)
	if err != nil || start.Format("2006-01-02") != *from {
		return fmt.Errorf("invalid --from date %q", *from)
	}
	last, err := time.ParseInLocation("2006-01-02", *to, moscow)
	if err != nil || last.Format("2006-01-02") != *to || last.Before(start) {
		return fmt.Errorf("invalid --to date %q", *to)
	}
	end := last.AddDate(0, 0, 1)
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	records, coverage, err := collectPeriodRecords(cfg, start, end, *configPath)
	if err != nil {
		return err
	}
	taskDates, taskDateError := currentTaskDates(cfg.Monitoring.TasksFile, start, end)
	sort.Slice(records, func(i, j int) bool {
		if records[i].At != records[j].At {
			return records[i].At < records[j].At
		}
		if records[i].Source != records[j].Source {
			return records[i].Source < records[j].Source
		}
		return records[i].ID < records[j].ID
	})
	total := len(records)
	page := []periodRecord{}
	if *offset < total {
		until := *offset + *limit
		if until > total {
			until = total
		}
		page = records[*offset:until]
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"period":   map[string]string{"from": *from, "to": *to, "timezone": "Europe/Moscow", "basis": "source_event_date"},
		"coverage": coverage, "total_records": total, "offset": *offset, "limit": *limit,
		"next_offset": func() any {
			if *offset+len(page) < total {
				return *offset + len(page)
			}
			return nil
		}(),
		"records":                  page,
		"current_task_dates":       taskDates,
		"current_task_dates_error": taskDateError,
		"meaning":                  "Read-only index of saved messages by message date, classes by class day, and assignments by due date. A snapshot without a source timestamp cannot date its status change. All records still require semantic review; incomplete coverage is not evidence of no activity.",
	})
}

func currentTaskDates(path string, start, end time.Time) ([]periodTaskDate, string) {
	board, err := readBoard(path)
	if err != nil {
		return nil, err.Error()
	}
	dates := []periodTaskDate{}
	for _, task := range board.Tasks {
		for _, date := range task.Dates {
			at, err := time.Parse(time.RFC3339, date.At)
			if err != nil || at.Before(start) || !at.Before(end) {
				continue
			}
			dates = append(dates, periodTaskDate{TaskID: task.ID, Title: task.Title, Status: task.Status, At: at.UTC().Format(time.RFC3339), Kind: date.Kind, Label: date.Label})
		}
	}
	sort.Slice(dates, func(i, j int) bool {
		if dates[i].At != dates[j].At {
			return dates[i].At < dates[j].At
		}
		return dates[i].TaskID < dates[j].TaskID
	})
	return dates, ""
}

func runPeriodInspect(args []string) error {
	f := flag.NewFlagSet("period inspect", flag.ContinueOnError)
	configPath := f.String("config", defaultConfig, "configuration")
	source := f.String("source", "", "gmail, yandex_mail, telegram, schedule, smart_lms or netology")
	id := f.String("id", "", "record ID from period result")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *id == "" {
		return errors.New("--id required")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if *source == "schedule" || *source == "smart_lms" || *source == "netology" {
		moscow, err := time.LoadLocation("Europe/Moscow")
		if err != nil {
			return err
		}
		start := time.Date(1900, 1, 1, 0, 0, 0, 0, moscow)
		end := time.Date(2101, 1, 1, 0, 0, 0, 0, moscow)
		records, coverage, err := readDatedSourceAdapter(cfg, *configPath, *source, start, end)
		if err != nil {
			return err
		}
		for _, record := range records {
			if record.ID == *id {
				return json.NewEncoder(os.Stdout).Encode(map[string]any{"source": *source, "id": *id, "record": record, "coverage": coverage})
			}
		}
		return fmt.Errorf("record %s not found in %s current saved snapshot (%s)", *id, *source, coverage.Mode)
	}
	path := ""
	chats := map[string]bool(nil)
	switch *source {
	case "gmail":
		path = cfg.Gmail.MessagesJSONL
	case "yandex_mail":
		path = cfg.YandexMail.MessagesJSONL
	case "telegram":
		path = cfg.Telegram.MessagesJSONL
		chats = map[string]bool{}
		for _, chat := range cfg.Telegram.Chats {
			chats[chat.ID] = true
		}
	default:
		return errors.New("--source must be gmail, yandex_mail or telegram")
	}
	record, line, err := findPeriodRecord(path, *id, chats)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"source": *source, "id": *id, "source_file": path, "source_line": line, "record": record})
}

func findPeriodRecord(path, id string, chats map[string]bool) (json.RawMessage, int, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 16<<20)
	var found json.RawMessage
	foundLine := 0
	for line := 1; scanner.Scan(); line++ {
		var item struct {
			MessageID any `json:"message_id"`
			Chat      struct {
				ID any `json:"id"`
			} `json:"chat"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &item); err != nil {
			return nil, 0, fmt.Errorf("%s line %d: %w", path, line, err)
		}
		itemID := periodString(item.MessageID)
		if chats != nil {
			chatID := periodString(item.Chat.ID)
			if !chats[chatID] {
				continue
			}
			itemID = chatID + ":" + itemID
		}
		if itemID == id {
			found = append(found[:0], scanner.Bytes()...)
			foundLine = line
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, 0, err
	}
	if foundLine == 0 {
		return nil, 0, fmt.Errorf("record %q not found in %s", id, path)
	}
	return found, foundLine, nil
}

func collectPeriodRecords(cfg config.Config, start, end time.Time, configPath string) ([]periodRecord, []periodCoverage, error) {
	records := []periodRecord{}
	coverage := []periodCoverage{}
	mailSources := []struct{ name, path string }{{"gmail", cfg.Gmail.MessagesJSONL}, {"yandex_mail", cfg.YandexMail.MessagesJSONL}}
	for _, source := range mailSources {
		found, missingDates, exists, err := readPeriodJSONL(source.name, source.path, start, end, nil)
		if err != nil {
			return nil, nil, err
		}
		records = append(records, found...)
		mode := "saved-records"
		detail := "Filtered saved messages by message date; source query/folder scope and collection gaps still apply."
		if !exists {
			mode, detail = "missing", "No saved message archive."
		}
		if missingDates > 0 {
			detail += fmt.Sprintf(" %d records had no usable date.", missingDates)
		}
		coverage = append(coverage, periodCoverage{Source: source.name, Mode: mode, Detail: detail, Count: len(found)})
	}
	chatIDs := map[string]bool{}
	for _, chat := range cfg.Telegram.Chats {
		chatIDs[chat.ID] = true
	}
	telegramPath := cfg.Telegram.MessagesJSONL
	found, missingDates, exists, err := readPeriodJSONL("telegram", telegramPath, start, end, chatIDs)
	if err != nil {
		return nil, nil, err
	}
	records = append(records, found...)
	mode, detail := "saved-records", fmt.Sprintf("Filtered saved messages from %d configured chats by message date; coverage begins with the saved archive.", len(chatIDs))
	if !exists {
		mode, detail = "missing", "No saved Telegram archive."
	}
	if missingDates > 0 {
		detail += fmt.Sprintf(" %d records had no usable date.", missingDates)
	}
	coverage = append(coverage, periodCoverage{Source: "telegram", Mode: mode, Detail: detail, Count: len(found)})
	for _, source := range []string{"schedule", "smart_lms", "netology"} {
		found, row, err := readDatedSourceAdapter(cfg, configPath, source, start, end)
		if err != nil {
			return nil, nil, err
		}
		records = append(records, found...)
		coverage = append(coverage, row)
	}
	snapshots := []struct{ source, path string }{
		{"github", cfg.GitHub.Checkpoint},
		{"manual", filepath.Join(cfg.Paths.StateDir, "manual", "checkpoint.json")},
	}
	if len(cfg.Club404.Subjects) > 0 {
		snapshots = append(snapshots, struct{ source, path string }{"club404", cfg.Club404.Checkpoint})
	}
	for _, item := range snapshots {
		row := periodCoverage{Source: item.source, Mode: "unindexed", Detail: "No reliable source-event date index; the linked current snapshot is not a historical view."}
		if info, err := os.Stat(item.path); err == nil && info.Mode().IsRegular() {
			row.SnapshotPath = item.path
		}
		coverage = append(coverage, row)
	}
	return records, coverage, nil
}

func readPeriodJSONL(source, path string, start, end time.Time, chats map[string]bool) ([]periodRecord, int, bool, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, false, nil
	}
	if err != nil {
		return nil, 0, false, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 16<<20)
	byID := map[string]periodRecord{}
	missingDates := 0
	for line := 1; scanner.Scan(); line++ {
		var item struct {
			MessageID    any    `json:"message_id"`
			Date         string `json:"date"`
			InternalDate string `json:"internal_date"`
			Subject      string `json:"subject"`
			Snippet      string `json:"snippet"`
			Text         string `json:"text"`
			Chat         struct {
				ID    any    `json:"id"`
				Title string `json:"title"`
			} `json:"chat"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &item); err != nil {
			return nil, 0, true, fmt.Errorf("%s line %d: %w", path, line, err)
		}
		chatID := periodString(item.Chat.ID)
		if chats != nil && !chats[chatID] {
			continue
		}
		at, basis, ok := parseSourceDate(item.Date, item.InternalDate)
		if !ok {
			missingDates++
			continue
		}
		if at.Before(start) || !at.Before(end) {
			continue
		}
		id := periodString(item.MessageID)
		if id == "" {
			return nil, 0, true, fmt.Errorf("%s line %d: missing message_id", path, line)
		}
		if chats != nil {
			id = chatID + ":" + id
		}
		title, preview := item.Subject, item.Snippet
		if chats != nil {
			title, preview = item.Chat.Title, item.Text
		}
		byID[id] = periodRecord{Source: source, ID: id, At: at.UTC().Format(time.RFC3339), Title: compactPeriodText(title, 100), Preview: compactPeriodText(preview, 180), SourceFile: path, SourceLine: line, DateBasis: basis}
	}
	if err := scanner.Err(); err != nil {
		return nil, 0, true, fmt.Errorf("read %s: %w", path, err)
	}
	result := make([]periodRecord, 0, len(byID))
	for _, record := range byID {
		result = append(result, record)
	}
	return result, missingDates, true, nil
}

func parseSourceDate(date, internal string) (time.Time, string, bool) {
	if at, err := time.Parse(time.RFC3339Nano, date); err == nil {
		return at, "message_date", true
	}
	if at, err := mail.ParseDate(date); err == nil {
		return at, "message_date", true
	}
	if at, err := time.Parse(time.RFC3339Nano, internal); err == nil {
		return at, "internal_date", true
	}
	return time.Time{}, "", false
}

func periodString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case float64:
		return strconv.FormatInt(int64(typed), 10)
	default:
		return ""
	}
}

func compactPeriodText(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return value
}
