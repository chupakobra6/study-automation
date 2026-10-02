package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chupakobra6/study-automation/internal/monitor"
	"github.com/chupakobra6/study-automation/internal/queue"
)

func TestReviewBatchBuildsExactEvidenceAndCoverageManifest(t *testing.T) {
	root := t.TempDir()
	report := filepath.Join(root, "report.md")
	evidenceA := filepath.Join(root, "brief.pdf")
	evidenceB := filepath.Join(root, "rules.docx")
	body := "Физическая культура: выбрать формат. Английский факультатив не нужен.\nСрок сдачи: 14 сентября 2026, 23:59.\n"
	if err := os.WriteFile(report, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	board := taskBoard{Version: 1, Tasks: []studyTask{
		{ID: "physical", Status: "open", Title: "Физкультура", Aliases: []string{"физкультура", "физическая культура"}},
		{ID: "english-refusal", Status: "closed_user_decision", Title: "Отказ от факультатива", Aliases: []string{"английский"}},
		{ID: "due-task", Status: "open", Title: "Наступившая задача"},
	}}
	item := queue.Item{ID: strings.Repeat("a", 32) + "-report", ReportPath: report, RequiredEvidence: []string{evidenceB, evidenceA}}
	batch, err := buildReviewBatch([]queue.Item{item}, 3, board, []studyTask{board.Tasks[2]})
	if err != nil {
		t.Fatal(err)
	}
	if batch.EventCount != 1 || batch.RemainingEvents != 3 || batch.MaxEvents != queue.MaxAnalysisBatch {
		t.Fatalf("unexpected batch bounds: %+v", batch)
	}
	if strings.Join(batch.RequiredEvidence, "|") != evidenceA+"|"+evidenceB {
		t.Fatalf("evidence manifest is not exact and sorted: %+v", batch.RequiredEvidence)
	}
	specs := decisionSpecs(batch)
	keys := []string{}
	for _, spec := range specs {
		keys = append(keys, spec.Kind+":"+spec.Key)
	}
	markers := strings.Join(keys, "\n")
	for _, want := range []string{
		"event:" + item.ID,
		"subject:физическая культура",
		"subject:английский",
		"deadline:14 сентября 2026, 23:59",
		"decision:english-refusal",
		"due:due-task",
	} {
		if !strings.Contains(markers, want) {
			t.Fatalf("coverage manifest omitted %s: %s", want, markers)
		}
	}
}

func TestDeadlineCoverageKeepsShortAndRelativeDates(t *testing.T) {
	deadlines := detectDeadlines("Сдать до 14.09 18:00.\nДедлайн завтра до 12:00.\nСрок: понедельник, 14 сентября 2026, 23:59.\nДата письма: 10.09.2026.\n")
	joined := strings.Join(deadlines, "|")
	if !strings.Contains(joined, "14.09 18:00") || !strings.Contains(joined, "завтра") || !strings.Contains(joined, "14 сентября 2026, 23:59") || strings.Contains(joined, "понедельник") || strings.Contains(joined, "10.09.2026") {
		t.Fatalf("unexpected deadline coverage: %+v", deadlines)
	}
}

func TestNewForumSubjectGetsCoverageWithoutExistingTaskAlias(t *testing.T) {
	coverage := detectCoverage("- 2026-09-04 19:38 | Study / Архитектуры ВС и ОП | Author: формула\n- 2026-09-04 19:39 | Study / Чатик 🌴 | Author: мем\n", taskBoard{}, nil)
	if len(coverage.Topics) != 1 || coverage.Topics[0].Key != "архитектуры вс и оп" {
		t.Fatalf("missing new subject: %+v", coverage)
	}
}

func TestStructuredReviewRequiresEveryDecisionAndBuildsNotice(t *testing.T) {
	f := newReviewFixture(t)
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	evidence := filepath.Join(f.root, "physical-rules.pdf")
	if err := os.WriteFile(evidence, []byte("rules"), 0600); err != nil {
		t.Fatal(err)
	}
	f.collect("telegram", "Физическая культура. Срок сдачи: 14 сентября 2026, 23:59.", []string{evidence}, now)
	if err := os.WriteFile(f.tasks, []byte(`{"version":1,"tasks":[{"id":"physical","status":"open","title":"Физкультура","aliases":["физическая культура"]}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	path, input := f.prepare(now)
	if len(input.Batch.Coverage.Topics) != 1 || len(input.Batch.Coverage.Deadlines) != 1 {
		t.Fatal(input.Batch.Coverage)
	}
	draft := reviewDraft{Decisions: decisionSpecs(input.Batch), Evidence: evidenceSpecs(input.Files)}
	write := func() {
		raw, err := json.Marshal(draft)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	draft.Decisions = draft.Decisions[:1]
	draft.Decisions[0].Outcome = "updated"
	draft.Decisions[0].Finding = "Опубликованы правила выбора формата."
	draft.Decisions[0].Notice = "Физкультура: опубликованы правила выбора формата."
	write()
	if _, err := validatePreparedReview(f.dir, path, taskBoard{}, now); err == nil || !strings.Contains(err.Error(), "missing decision") {
		t.Fatalf("missing coverage accepted: %v", err)
	}
	draft.Decisions = decisionSpecs(input.Batch)
	for i := range draft.Decisions {
		draft.Decisions[i].Outcome = "updated"
		draft.Decisions[i].Finding = "Правила и срок прочитаны."
		draft.Decisions[i].Notice = "Физкультура: выбрать формат до 14 сентября 2026, 23:59."
		draft.Decisions[i].Action = "Выбрать формат занятия."
	}
	draft.Evidence[0].Finding = "Правила выбора формата физкультуры применимы к текущему году."
	write()
	validated, err := validatePreparedReview(f.dir, path, taskBoard{}, now)
	if err != nil || !validated.Valid {
		t.Fatal(validated, err)
	}
	if !strings.Contains(validated.Notification, "Физкультура") || strings.Contains(validated.Notification, "telegram-event") {
		t.Fatal(validated.Notification)
	}
	for i := range draft.Decisions {
		if draft.Decisions[i].Kind == "subject" {
			draft.Decisions[i].Outcome = "blocked"
			write()
			if _, err := validatePreparedReview(f.dir, path, taskBoard{}, now); err == nil || !strings.Contains(err.Error(), "requires a blocked event") {
				t.Fatalf("blocked subject was acknowledged by its event: %v", err)
			}
			draft.Decisions[i].Outcome = "updated"
			write()
			break
		}
	}
	remaining, err := queue.RecordReview(f.dir, validated.CompletedIDs, validated.Deferred, path, validated.RequiredEvidence, now)
	if err != nil || remaining.Pending != 0 {
		t.Fatal(remaining, err)
	}
}

func TestStructuredNoticePreservesDueActionAndRejectsContradiction(t *testing.T) {
	specs := []reviewDecision{{Kind: "subject", Key: "алгоритмы"}, {Kind: "due", Key: "algorithms-team"}}
	decisions := map[string]reviewDecision{
		decisionKey("subject", "алгоритмы"):   {Kind: "subject", Key: "алгоритмы", Outcome: "updated", Finding: "Появилось задание.", Notice: "Новых учебных изменений нет."},
		decisionKey("due", "algorithms-team"): {Kind: "due", Key: "algorithms-team", Outcome: "no-action", Finding: "Проверка проведена."},
	}
	if _, err := buildReviewNotification(specs, decisions); err == nil {
		t.Fatal("contradiction accepted")
	}
	decisions[decisionKey("subject", "алгоритмы")] = reviewDecision{Kind: "subject", Key: "алгоритмы", Outcome: "updated", Finding: "Появилось задание.", Notice: "Алгоритмы: задание опубликовано."}
	if _, err := buildReviewNotification(specs, decisions); err == nil {
		t.Fatal("due action omitted")
	}
	decisions[decisionKey("due", "algorithms-team")] = reviewDecision{Kind: "due", Key: "algorithms-team", Outcome: "no-action", Finding: "Проверка проведена.", Action: "Алгоритмы: согласовать название и отправить состав команды."}
	notice, err := buildReviewNotification(specs, decisions)
	if err != nil || !strings.Contains(notice, "согласовать название") {
		t.Fatal(notice, err)
	}
}

func TestDeadlineOnlyUpdateDoesNotClaimNoChanges(t *testing.T) {
	spec := reviewDecision{Kind: "deadline", Key: "06.10 23:59"}
	decision := reviewDecision{Kind: spec.Kind, Key: spec.Key, Outcome: "updated", Finding: "Срок подтверждён в задании.", Notice: "Базы данных: первый этап до 6 октября, 23:59."}
	notice, err := buildReviewNotification([]reviewDecision{spec}, map[string]reviewDecision{decisionKey(spec.Kind, spec.Key): decision})
	if err != nil || strings.Contains(notice, "Новых учебных изменений нет") || !strings.Contains(notice, "6 октября") {
		t.Fatal(notice, err)
	}
}

func TestDueTasksRespectUserClosuresAndFutureChecks(t *testing.T) {
	now := time.Date(2026, 9, 6, 18, 0, 0, 0, time.UTC)
	board := taskBoard{Version: 1, Tasks: []studyTask{
		{ID: "english", Status: "closed_user_decision", NextCheckAt: "2026-09-06T10:00:00+03:00"},
		{ID: "db-sent", Status: "waiting_official"},
		{ID: "tuition", Status: "open", NextCheckAt: "2026-09-23T10:00:00+03:00"},
		{ID: "due", Status: "open", NextCheckAt: "2026-09-06T10:00:00+03:00"},
	}}
	due, err := dueTasks(board, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].ID != "due" {
		t.Fatalf("unexpected bootstrap tasks: %+v", due)
	}
	board.Tasks = append(board.Tasks, studyTask{ID: "broken-date", Status: "open", NextCheckAt: "soon"})
	if _, err := dueTasks(board, now); err == nil {
		t.Fatal("invalid due date silently dropped")
	}
}

func TestBriefSummariesExposeOpenActionsAndSortedImportantDates(t *testing.T) {
	now := time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC)
	board := taskBoard{Version: 1, Tasks: []studyTask{
		{ID: "later", Priority: "P2", Status: "open", Title: "Later", Action: "Do later", Dates: []taskDate{{At: "2026-09-26T23:59:00+03:00", Kind: "deadline", Label: "Later deadline"}}},
		{ID: "urgent", Priority: "P1", Status: "open", Title: "Urgent", Action: "Do now", Dates: []taskDate{{At: "2026-09-10T10:00:00+03:00", Kind: "deadline", Label: "Urgent deadline"}}},
		{ID: "waiting", Priority: "P1", Status: "waiting_official", Title: "Wait", Action: "Wait for a signal"},
		{ID: "closed", Priority: "P0", Status: "closed_confirmed", Title: "Closed", Action: "Do not repeat", Dates: []taskDate{{At: "2026-09-01T10:00:00+03:00", Kind: "deadline", Label: "Old"}}},
	}}
	actions := openActions(board)
	if len(actions) != 2 || actions[0].ID != "urgent" || actions[1].ID != "later" {
		t.Fatalf("unexpected open actions: %+v", actions)
	}
	dates := importantDates(board, now)
	if len(dates) != 2 || dates[0].TaskID != "urgent" || !dates[0].Overdue || dates[1].TaskID != "later" || dates[1].Overdue {
		t.Fatalf("unexpected important dates: %+v", dates)
	}
}

func TestNoChangeBriefOmitsRepeatedTaskDetails(t *testing.T) {
	now := time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC)
	board := taskBoard{Version: 1, Tasks: []studyTask{{
		ID: "later", Priority: "P2", Status: "open", Title: "Later", Action: "Do later",
		Dates: []taskDate{{At: "2026-09-26T23:59:00+03:00", Kind: "deadline", Label: "Later deadline"}},
	}}}
	brief, err := buildBrief(queue.Summary{BySource: map[string]int{}, Items: []queue.Item{}, Failures: []queue.Failure{}}, board, "tasks.json", "state", now)
	if err != nil {
		t.Fatal(err)
	}
	if brief["analysis_needed"] != false || brief["open_action_count"] != 1 || brief["important_date_count"] != 1 {
		t.Fatalf("unexpected compact brief: %+v", brief)
	}
	for _, key := range []string{"open_actions", "important_dates", "tasks_file", "context_file"} {
		if _, exists := brief[key]; exists {
			t.Fatalf("no-change brief repeats %s: %+v", key, brief)
		}
	}
}

func TestBriefSeparatesSourceHealthFromStudentAnalysis(t *testing.T) {
	now := time.Date(2026, 9, 25, 8, 0, 0, 0, time.UTC)
	pending := queue.Summary{
		BySource: map[string]int{}, Items: []queue.Item{},
		Failures:       []queue.Failure{{Source: "netology", Detail: "captcha and credential internals"}},
		WaitingSources: []queue.DependencyWait{{Source: "smart_lms", Reason: "internal retry condition"}},
	}
	brief, err := buildBrief(pending, taskBoard{}, "tasks.json", "state", now)
	if err != nil || brief["analysis_needed"] != false {
		t.Fatalf("source health started academic review: %+v err=%v", brief, err)
	}
	encoded, err := json.Marshal(brief)
	if err != nil || strings.Contains(string(encoded), "captcha and credential internals") || strings.Contains(string(encoded), "internal retry condition") {
		t.Fatalf("operational details entered model brief: %s err=%v", encoded, err)
	}
	overview := brief["pending"].(pendingOverview)
	if len(overview.UnverifiedSources) != 1 || overview.UnverifiedSources[0] != "netology" || len(overview.WaitingSources) != 1 || overview.WaitingSources[0] != "smart_lms" {
		t.Fatalf("coverage names were lost: %+v", overview)
	}
}

func TestSignalBriefIncludesAddressableDetails(t *testing.T) {
	now := time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC)
	board := taskBoard{Version: 1, Tasks: []studyTask{{
		ID: "due", Priority: "P1", Status: "open", Title: "Due", Action: "Act",
		NextCheckAt: "2026-09-10T10:00:00+03:00",
	}}}
	brief, err := buildBrief(queue.Summary{BySource: map[string]int{}, Items: []queue.Item{}, Failures: []queue.Failure{}}, board, "tasks.json", "state", now)
	if err != nil {
		t.Fatal(err)
	}
	if brief["analysis_needed"] != true {
		t.Fatalf("signal was lost: %+v", brief)
	}
	for _, key := range []string{"due_tasks", "tasks_file", "context_file"} {
		if _, exists := brief[key]; !exists {
			t.Fatalf("signal brief is missing %s: %+v", key, brief)
		}
	}
	for _, key := range []string{"open_actions", "important_dates"} {
		if _, exists := brief[key]; exists {
			t.Fatalf("signal brief repeats stable backlog in %s: %+v", key, brief)
		}
	}
}

func TestTaskSearchPrefersIDsTitlesAndExplicitAliases(t *testing.T) {
	board := taskBoard{Version: 1, Tasks: []studyTask{
		{ID: "physical-education", Title: "Физическая культура", Aliases: []string{"физра", "физре"}, Facts: "Требования второго курса"},
		{ID: "other", Title: "Другая задача", Facts: "Обсуждение физры и алгоритмов"},
		{ID: "algorithms", Title: "Алгоритмы", Facts: "Основная задача"},
	}}
	physical := findTasks(board, "физра")
	if len(physical) != 1 || physical[0].ID != "physical-education" {
		t.Fatalf("alias search leaked incidental facts: %+v", physical)
	}
	physical = findTasks(board, "физре")
	if len(physical) != 1 || physical[0].ID != "physical-education" {
		t.Fatalf("inflected alias search failed: %+v", physical)
	}
	algorithms := findTasks(board, "алгоритм")
	if len(algorithms) != 1 || algorithms[0].ID != "algorithms" {
		t.Fatalf("title search leaked incidental facts: %+v", algorithms)
	}
	fallback := findTasks(board, "второго курса")
	if len(fallback) != 1 || fallback[0].ID != "physical-education" {
		t.Fatalf("facts fallback failed: %+v", fallback)
	}
}

func TestSyncProgressOutputMakesTheHeldQueueLockObservable(t *testing.T) {
	var output bytes.Buffer
	writeSyncProgress(&output, monitor.Progress{Source: "telegram", Phase: "started", Timeout: 5 * time.Minute})
	writeSyncProgress(&output, monitor.Progress{Source: "telegram", Phase: "finished", Status: "ok", Detail: "4 chats synchronized"})
	text := output.String()
	if !strings.Contains(text, "source=telegram phase=started timeout=5m0s") || !strings.Contains(text, "source=telegram phase=finished status=ok") {
		t.Fatalf("progress is not actionable: %q", text)
	}
}
