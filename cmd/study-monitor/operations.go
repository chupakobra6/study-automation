package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/chupakobra6/study-automation/internal/config"
	"github.com/chupakobra6/study-automation/internal/gmail"
	"github.com/chupakobra6/study-automation/internal/queue"
	"github.com/chupakobra6/study-automation/internal/yandexmail"
)

type taskBoard struct {
	Version int         `json:"version"`
	Tasks   []studyTask `json:"tasks"`
}
type studyTask struct {
	ID          string     `json:"id"`
	Priority    string     `json:"priority"`
	Status      string     `json:"status"`
	Title       string     `json:"title"`
	Aliases     []string   `json:"aliases,omitempty"`
	Facts       string     `json:"facts"`
	Action      string     `json:"action"`
	NextCheckAt string     `json:"next_check_at"`
	Dates       []taskDate `json:"dates,omitempty"`
	Evidence    []string   `json:"evidence"`
}
type taskDate struct {
	At    string `json:"at"`
	Kind  string `json:"kind"`
	Label string `json:"label"`
}
type briefTask struct {
	ID          string     `json:"id"`
	Priority    string     `json:"priority"`
	Title       string     `json:"title"`
	Action      string     `json:"action"`
	NextCheckAt string     `json:"next_check_at,omitempty"`
	Dates       []taskDate `json:"dates,omitempty"`
}
type importantDate struct {
	TaskID   string `json:"task_id"`
	Priority string `json:"priority"`
	At       string `json:"at"`
	Kind     string `json:"kind"`
	Label    string `json:"label"`
	Overdue  bool   `json:"overdue"`
}

type pendingOverview struct {
	Pending           int            `json:"pending_count"`
	Deferred          int            `json:"deferred_count,omitempty"`
	Waiting           int            `json:"waiting_dependency_count,omitempty"`
	OldestAt          *time.Time     `json:"oldest_at,omitempty"`
	BySource          map[string]int `json:"by_source"`
	Truncated         bool           `json:"truncated"`
	UnverifiedSources []string       `json:"unverified_sources,omitempty"`
	WaitingSources    []string       `json:"waiting_sources,omitempty"`
}

type coverageTopic struct {
	Key              string   `json:"key"`
	MatchedAlias     string   `json:"matched_alias"`
	CandidateTaskIDs []string `json:"candidate_task_ids"`
}

type coverageDecision struct {
	TaskID string `json:"task_id"`
	Title  string `json:"title"`
}

type coverageManifest struct {
	Topics        []coverageTopic    `json:"subjects_or_topics"`
	Deadlines     []string           `json:"deadlines"`
	UserDecisions []coverageDecision `json:"user_decisions"`
	DueTaskIDs    []string           `json:"due_task_ids"`
}

type reviewItem struct {
	ID         string          `json:"id"`
	Source     string          `json:"source"`
	Kind       string          `json:"kind"`
	Status     string          `json:"status"`
	Detail     string          `json:"detail"`
	CreatedAt  time.Time       `json:"created_at"`
	ReportPath string          `json:"report_path"`
	Deferred   *queue.Deferral `json:"deferred,omitempty"`
}

type reviewBatch struct {
	MaxEvents        int              `json:"max_events"`
	EventCount       int              `json:"event_count"`
	RemainingEvents  int              `json:"remaining_event_count"`
	Items            []reviewItem     `json:"items"`
	RequiredEvidence []string         `json:"required_evidence"`
	Coverage         coverageManifest `json:"coverage"`
}

type validationResult struct {
	Valid            bool             `json:"valid"`
	EventCount       int              `json:"event_count"`
	RequiredEvidence []string         `json:"required_evidence"`
	Notification     string           `json:"notification"`
	CompletedIDs     []string         `json:"completed_ids,omitempty"`
	Deferred         []queue.Deferral `json:"deferred,omitempty"`
}

func runOperations(ctx context.Context, args []string) error {
	op := args[0]
	args = args[1:]
	sub := ""
	if op == "queue" {
		if len(args) == 0 {
			return errors.New("queue list|inspect <id>|prepare|ack --receipt <path>")
		}
		sub = args[0]
		args = args[1:]
	}
	id := ""
	if sub == "inspect" && len(args) > 0 {
		id = args[0]
		args = args[1:]
	}
	f := flag.NewFlagSet(op, flag.ContinueOnError)
	query := f.String("query", "", "find related tasks by ID, title or facts")
	configPath := f.String("config", defaultConfig, "configuration")
	receipt := f.String("receipt", "", "existing completed analysis receipt")
	source := f.String("source", "", "gmail or yandex_mail")
	messageID := f.String("message-id", "", "Gmail message ID")
	folder := f.String("folder", "", "IMAP folder")
	uid := f.Uint("uid", 0, "IMAP UID")
	limit := f.Int("limit", queue.MaxAnalysisBatch, "maximum pending items in compact output")
	if err := f.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if op == "retry-item" {
		store, e := queue.Open(cfg.Monitoring.QueueDir)
		if e != nil {
			return e
		}
		defer store.Close()
		switch *source {
		case "gmail":
			if *messageID == "" {
				return errors.New("--message-id required")
			}
			return gmail.ResetItemFailure(cfg.Gmail.Checkpoint, *messageID)
		case "yandex_mail":
			if *folder == "" || *uid == 0 || uint64(*uid) > uint64(^uint32(0)) {
				return errors.New("valid --folder and --uid required")
			}
			return yandexmail.ResetItemFailure(cfg.YandexMail.Checkpoint, *folder, uint32(*uid))
		default:
			return errors.New("--source gmail|yandex_mail required")
		}
	}
	if *limit < 1 || *limit > 50 {
		return errors.New("--limit must be 1..50")
	}
	if op == "brief" && *limit > queue.MaxAnalysisBatch {
		return fmt.Errorf("brief batch limit cannot exceed %d", queue.MaxAnalysisBatch)
	}
	var result any
	switch {
	case op == "task":
		if strings.TrimSpace(*query) == "" {
			return errors.New("--query is required")
		}
		board, e := readBoard(cfg.Monitoring.TasksFile)
		if e != nil {
			return e
		}
		result = findTasks(board, *query)
	case op == "board":
		board, e := readBoard(cfg.Monitoring.TasksFile)
		if e != nil {
			return e
		}
		var b strings.Builder
		b.WriteString("# Учебные задачи\n\nСформировано из `tasks.json`; изменять только JSON.\n\n")
		for _, task := range board.Tasks {
			if strings.HasPrefix(task.Status, "closed") || strings.HasPrefix(task.Status, "cancelled") {
				continue
			}
			fmt.Fprintf(&b, "## %s\n\n%s · %s · `%s`\n\n%s\n\n%s\n\n", task.Title, task.Priority, task.Status, task.ID, task.Facts, task.Action)
			if len(task.Dates) > 0 {
				b.WriteString("Важные даты:\n\n")
				for _, date := range task.Dates {
					fmt.Fprintf(&b, "- %s — %s (`%s`)\n", date.At, date.Label, date.Kind)
				}
				b.WriteString("\n")
			}
			fmt.Fprintf(&b, "Следующая проверка: %s\n\n", task.NextCheckAt)
			for _, ref := range task.Evidence {
				fmt.Fprintf(&b, "- %s\n", ref)
			}
			b.WriteString("\n")
		}
		path := filepath.Join(filepath.Dir(cfg.Monitoring.TasksFile), "status-board.md")
		if err := writeManualPrivate(path, []byte(b.String()), false); err != nil {
			return err
		}
		result = map[string]string{"board": path}
	case op == "brief":
		now := time.Now()
		collection, collectionErr := loadCollectionRun(collectionDir(cfg.Paths.StateDir))
		pending, e := queue.Summarize(cfg.Monitoring.QueueDir, *limit, time.Now())
		if e != nil {
			collectionState := collectionBrief(collection, collectionErr, now)
			collectionState["ready"] = false
			collectionState["reason"] = "queue unavailable: " + e.Error()
			result = map[string]any{"collection": collectionState, "analysis_needed": false, "queue_error": e.Error()}
			break
		}
		board, e := readBoard(cfg.Monitoring.TasksFile)
		if e != nil {
			result = map[string]any{"pending": pending, "tasks_error": e.Error(), "tasks_file": cfg.Monitoring.TasksFile, "analysis_needed": true, "collection": collectionBrief(collection, collectionErr, now)}
			break
		}
		var brief map[string]any
		brief, err = buildBrief(pending, board, cfg.Monitoring.TasksFile, cfg.Paths.StateDir, now)
		if err == nil {
			brief["collection"] = collectionBrief(collection, collectionErr, now)
			result = brief
		}
	case sub == "list":
		result, err = queue.Summarize(cfg.Monitoring.QueueDir, *limit, time.Now())
	case sub == "inspect":
		if id == "" {
			return errors.New("event ID required")
		}
		result, err = queue.Inspect(cfg.Monitoring.QueueDir, id)
	case sub == "ack":
		if *receipt == "" {
			return errors.New("--receipt required")
		}
		board, e := readBoard(cfg.Monitoring.TasksFile)
		if e != nil {
			return e
		}
		validated, e := validatePreparedReview(cfg.Monitoring.QueueDir, *receipt, board, time.Now())
		if e != nil {
			return e
		}
		var summary queue.Summary
		if len(validated.CompletedIDs)+len(validated.Deferred) == 0 {
			summary, err = queue.Summarize(cfg.Monitoring.QueueDir, queue.MaxAnalysisBatch, time.Now())
		} else {
			summary, err = queue.RecordReview(cfg.Monitoring.QueueDir, validated.CompletedIDs, validated.Deferred, *receipt, validated.RequiredEvidence, time.Now())
		}
		result = map[string]any{"queue": summary, "notification": validated.Notification}
	case sub == "prepare":
		result, err = prepareReview(cfg.Monitoring.QueueDir, cfg.Monitoring.TasksFile, *receipt, time.Now())
	default:
		return errors.New("queue list|inspect <id>|prepare|ack --receipt <path>")
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}

func findTasks(board taskBoard, rawQuery string) []studyTask {
	query := strings.ToLower(strings.TrimSpace(rawQuery))
	primary := []studyTask{}
	for _, task := range board.Tasks {
		searchable := strings.ToLower(task.ID + " " + task.Title + " " + strings.Join(task.Aliases, " "))
		if strings.Contains(searchable, query) {
			primary = append(primary, task)
		}
	}
	if len(primary) > 0 {
		return primary
	}
	fallback := []studyTask{}
	for _, task := range board.Tasks {
		if strings.Contains(strings.ToLower(task.Facts), query) {
			fallback = append(fallback, task)
		}
	}
	return fallback
}

func buildBrief(pending queue.Summary, board taskBoard, tasksFile, stateDir string, now time.Time) (map[string]any, error) {
	due, err := dueTasks(board, now)
	if err != nil {
		return nil, err
	}
	actions := openActions(board)
	dates := importantDates(board, now)
	analysisNeeded := len(pending.Items) > 0 || len(due) > 0
	overview := pendingOverview{
		Pending: pending.Pending, OldestAt: pending.OldestAt, BySource: pending.BySource,
		Truncated: pending.Truncated, Deferred: pending.Deferred, Waiting: pending.Waiting,
	}
	for _, failure := range pending.Failures {
		overview.UnverifiedSources = append(overview.UnverifiedSources, failure.Source)
	}
	for _, waiting := range pending.WaitingSources {
		overview.WaitingSources = append(overview.WaitingSources, waiting.Source)
	}
	result := map[string]any{
		"analysis_needed":      analysisNeeded,
		"pending":              overview,
		"due_task_count":       len(due),
		"open_action_count":    len(actions),
		"important_date_count": len(dates),
	}
	if len(dates) > 0 {
		result["next_important_date"] = dates[0]
	}
	if !analysisNeeded {
		return result, nil
	}
	batch, err := buildReviewBatch(pending.Items, pending.Pending-len(pending.Items), board, due)
	if err != nil {
		return nil, err
	}
	result["review_batch"] = batch
	result["due_tasks"] = due
	result["tasks_file"] = tasksFile
	result["context_file"] = filepath.Join(stateDir, "academic-monitoring", "memory.md")
	return result, nil
}

var deadlineAbsolute = regexp.MustCompile(`20\d{2}-\d{2}-\d{2}|\d{1,2}[./]\d{1,2}`)

func taskByID(board taskBoard, id string) (studyTask, bool) {
	for _, task := range board.Tasks {
		if task.ID == id {
			return task, true
		}
	}
	return studyTask{}, false
}

func buildReviewBatch(items []queue.Item, remaining int, board taskBoard, due []studyTask) (reviewBatch, error) {
	if len(items) > queue.MaxAnalysisBatch {
		return reviewBatch{}, fmt.Errorf("review batch has %d events; maximum is %d", len(items), queue.MaxAnalysisBatch)
	}
	batch := reviewBatch{
		MaxEvents: queue.MaxAnalysisBatch, EventCount: len(items), RemainingEvents: max(0, remaining),
		Items: []reviewItem{}, RequiredEvidence: []string{},
		Coverage: coverageManifest{Topics: []coverageTopic{}, Deadlines: []string{}, UserDecisions: []coverageDecision{}, DueTaskIDs: []string{}},
	}
	evidence := map[string]bool{}
	var reports strings.Builder
	for _, item := range items {
		batch.Items = append(batch.Items, reviewItem{
			ID: item.ID, Source: item.Source, Kind: item.Kind, Status: item.Status,
			Detail: item.Detail, CreatedAt: item.CreatedAt, ReportPath: item.ReportPath, Deferred: item.Deferred,
		})
		body, err := os.ReadFile(item.ReportPath)
		if err != nil {
			return reviewBatch{}, fmt.Errorf("read queue report %s: %w", item.ID, err)
		}
		reports.Write(body)
		reports.WriteByte('\n')
		for _, path := range item.RequiredEvidence {
			evidence[filepath.Clean(path)] = true
		}
	}
	for path := range evidence {
		batch.RequiredEvidence = append(batch.RequiredEvidence, path)
	}
	sort.Strings(batch.RequiredEvidence)
	batch.Coverage = detectCoverage(reports.String(), board, due)
	return batch, nil
}

func detectCoverage(report string, board taskBoard, due []studyTask) coverageManifest {
	coverage := coverageManifest{Topics: []coverageTopic{}, Deadlines: detectDeadlines(report), UserDecisions: []coverageDecision{}, DueTaskIDs: []string{}}
	normalizedReport := normalizeSearch(report)
	topics := map[string]*coverageTopic{}
	decisions := map[string]coverageDecision{}
	// A new forum subject must remain visible before a task/alias exists.
	for _, match := range telegramTopicLine.FindAllStringSubmatch(report, -1) {
		title := strings.TrimSpace(match[1])
		key := normalizeSearch(title)
		if key == "" || key == "general" || key == "общий чат" || key == "болталка" || key == "чат" || strings.HasPrefix(key, "чатик") {
			continue
		}
		topics[key] = &coverageTopic{Key: key, MatchedAlias: title, CandidateTaskIDs: []string{}}
	}
	for _, task := range board.Tasks {
		matched := longestMatch(normalizedReport, task.Aliases)
		if matched == "" {
			continue
		}
		key := normalizeSearch(matched)
		topic := topics[key]
		if topic == nil {
			topic = &coverageTopic{Key: key, MatchedAlias: matched, CandidateTaskIDs: []string{}}
			topics[key] = topic
		}
		topic.CandidateTaskIDs = append(topic.CandidateTaskIDs, task.ID)
		if strings.HasPrefix(task.Status, "closed") {
			decisions[task.ID] = coverageDecision{TaskID: task.ID, Title: task.Title}
		}
	}
	keys := make([]string, 0, len(topics))
	for key := range topics {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		topic := *topics[key]
		sort.Strings(topic.CandidateTaskIDs)
		coverage.Topics = append(coverage.Topics, topic)
	}
	decisionIDs := make([]string, 0, len(decisions))
	for id := range decisions {
		decisionIDs = append(decisionIDs, id)
	}
	sort.Strings(decisionIDs)
	for _, id := range decisionIDs {
		coverage.UserDecisions = append(coverage.UserDecisions, decisions[id])
	}
	for _, task := range due {
		coverage.DueTaskIDs = append(coverage.DueTaskIDs, task.ID)
	}
	sort.Strings(coverage.DueTaskIDs)
	return coverage
}

var telegramTopicLine = regexp.MustCompile(`(?m)^- \d{4}-\d{2}-\d{2} \d{2}:\d{2} \| [^\n|]+ / ([^\n|]+) \| `)

func longestMatch(normalizedReport string, aliases []string) string {
	best := ""
	for _, alias := range aliases {
		normalized := normalizeSearch(alias)
		if normalized != "" && containsPhrase(normalizedReport, normalized) && len([]rune(normalized)) > len([]rune(normalizeSearch(best))) {
			best = strings.TrimSpace(alias)
		}
	}
	return best
}

func normalizeSearch(value string) string {
	value = strings.ToLower(strings.ReplaceAll(value, "ё", "е"))
	return strings.Join(strings.FieldsFunc(value, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }), " ")
}

func containsPhrase(text, phrase string) bool {
	if len([]rune(phrase)) >= 5 {
		return strings.Contains(text, phrase)
	}
	return strings.Contains(" "+text+" ", " "+phrase+" ")
}

var deadlineKeyword = regexp.MustCompile(`(?i)(deadline|due|дедлайн|срок(?:а|и|ом|у|ов)?|сда(?:ть|ча|чи)|до\s)`)
var deadlineDate = regexp.MustCompile(`(?i)20\d{2}-\d{2}-\d{2}(?:[T ]\d{1,2}:\d{2})?|\d{1,2}[./]\d{1,2}(?:[./](?:20)?\d{2})?(?:\s+\d{1,2}:\d{2})?|\d{1,2}\s+(?:января|февраля|марта|апреля|мая|июня|июля|августа|сентября|октября|ноября|декабря)(?:\s+20\d{2})?(?:[,\s]+\d{1,2}:\d{2})?|(?:сегодня|завтра|послезавтра|понедельник(?:а)?|вторник(?:а)?|сред(?:а|ы)|четверг(?:а)?|пятниц(?:а|ы)|суббот(?:а|ы)|воскресень(?:е|я))(?:\s+(?:до\s+)?\d{1,2}:\d{2})?`)

func detectDeadlines(report string) []string {
	seen := map[string]bool{}
	result := []string{}
	for _, line := range strings.Split(report, "\n") {
		for _, keyword := range deadlineKeyword.FindAllStringIndex(line, -1) {
			end := min(len(line), keyword[1]+160)
			values := deadlineDate.FindAllString(line[keyword[1]:end], -1)
			if len(values) == 0 {
				continue
			}
			absolute := []string{}
			for _, value := range values {
				if isAbsoluteDeadline(value) {
					absolute = append(absolute, value)
				}
			}
			if len(absolute) > 0 {
				values = absolute
			}
			for _, value := range values {
				key := strings.Join(strings.Fields(strings.ToLower(value)), " ")
				if !seen[key] {
					seen[key] = true
					result = append(result, key)
				}
			}
		}
	}
	sort.Strings(result)
	return result
}

func isAbsoluteDeadline(value string) bool {
	normalized := strings.ToLower(value)
	if deadlineAbsolute.MatchString(normalized) {
		return true
	}
	for _, month := range []string{
		"января", "февраля", "марта", "апреля", "мая", "июня", "июля", "августа", "сентября", "октября", "ноября", "декабря",
	} {
		if strings.Contains(normalized, month) {
			return true
		}
	}
	return false
}

func readBoard(path string) (taskBoard, error) {
	var board taskBoard
	data, err := os.ReadFile(path)
	if err != nil {
		return board, err
	}
	if err = json.Unmarshal(data, &board); err != nil {
		return board, err
	}
	seen := map[string]bool{}
	for _, task := range board.Tasks {
		if task.ID == "" || task.Title == "" || task.Status == "" || seen[task.ID] {
			return board, errors.New("tasks require unique IDs, title and status")
		}
		seen[task.ID] = true
		if task.NextCheckAt != "" {
			if _, e := time.Parse(time.RFC3339, task.NextCheckAt); e != nil {
				return board, fmt.Errorf("invalid next_check_at for %s", task.ID)
			}
		}
		for _, date := range task.Dates {
			if date.At == "" || date.Kind == "" || date.Label == "" {
				return board, fmt.Errorf("task %s dates require at, kind and label", task.ID)
			}
			if _, e := time.Parse(time.RFC3339, date.At); e != nil {
				return board, fmt.Errorf("invalid task date for %s", task.ID)
			}
		}
	}
	if board.Tasks == nil {
		return board, errors.New("tasks.json requires a tasks array (empty [] is allowed)")
	}
	if board.Version != 1 {
		return board, errors.New("unsupported tasks.json version")
	}
	return board, nil
}

func openActions(board taskBoard) []briefTask {
	actions := []briefTask{}
	for _, task := range board.Tasks {
		if task.Status != "open" || strings.TrimSpace(task.Action) == "" {
			continue
		}
		actions = append(actions, briefTask{ID: task.ID, Priority: task.Priority, Title: task.Title, Action: task.Action, NextCheckAt: task.NextCheckAt, Dates: task.Dates})
	}
	sort.SliceStable(actions, func(i, j int) bool {
		if priorityRank(actions[i].Priority) != priorityRank(actions[j].Priority) {
			return priorityRank(actions[i].Priority) < priorityRank(actions[j].Priority)
		}
		return actions[i].ID < actions[j].ID
	})
	return actions
}

func importantDates(board taskBoard, now time.Time) []importantDate {
	dates := []importantDate{}
	for _, task := range board.Tasks {
		if strings.HasPrefix(task.Status, "closed") || strings.HasPrefix(task.Status, "cancelled") {
			continue
		}
		for _, date := range task.Dates {
			at, err := time.Parse(time.RFC3339, date.At)
			if err != nil {
				continue
			}
			// Past appointments/checks are history, not an overdue obligation.
			if !at.After(now) && (date.Kind == "check" || date.Kind == "class" || date.Kind == "meeting") {
				continue
			}
			dates = append(dates, importantDate{TaskID: task.ID, Priority: task.Priority, At: date.At, Kind: date.Kind, Label: date.Label, Overdue: !at.After(now)})
		}
	}
	sort.SliceStable(dates, func(i, j int) bool {
		left, _ := time.Parse(time.RFC3339, dates[i].At)
		right, _ := time.Parse(time.RFC3339, dates[j].At)
		return left.Before(right)
	})
	return dates
}

func priorityRank(priority string) int {
	switch priority {
	case "P0":
		return 0
	case "P1":
		return 1
	case "P2":
		return 2
	default:
		return 3
	}
}
func dueTasks(board taskBoard, now time.Time) ([]studyTask, error) {
	due := []studyTask{}
	for _, task := range board.Tasks {
		if task.NextCheckAt == "" || strings.HasPrefix(task.Status, "closed") || strings.HasPrefix(task.Status, "cancelled") {
			continue
		}
		at, err := time.Parse(time.RFC3339, task.NextCheckAt)
		if err != nil {
			return nil, fmt.Errorf("task %s has invalid next_check_at: %w", task.ID, err)
		}
		if !at.After(now) {
			due = append(due, task)
		}
	}
	return due, nil
}
