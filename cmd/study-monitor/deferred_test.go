package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chupakobra6/study-automation/internal/queue"
)

type reviewFixture struct {
	t                *testing.T
	root, dir, tasks string
	sequence         int
}

func newReviewFixture(t *testing.T) *reviewFixture {
	t.Helper()
	root := t.TempDir()
	tasks := filepath.Join(root, "tasks.json")
	if err := os.WriteFile(tasks, []byte(`{"version":1,"tasks":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	return &reviewFixture{t: t, root: root, dir: filepath.Join(root, "queue"), tasks: tasks}
}
func (f *reviewFixture) collect(source, body string, evidence []string, now time.Time) {
	f.t.Helper()
	s, err := queue.Open(f.dir)
	if err != nil {
		f.t.Fatal(err)
	}
	defer s.Close()
	latest := filepath.Join(f.root, "latest.md")
	if err := s.Start(source, latest, now); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(latest, []byte(body), 0600); err != nil {
		f.t.Fatal(err)
	}
	if err := s.Complete(queue.Outcome{Source: source, Status: "delta", Added: 1, RequiredEvidence: evidence}); err != nil {
		f.t.Fatal(err)
	}
}
func (f *reviewFixture) prepare(now time.Time) (string, reviewInput) {
	f.t.Helper()
	f.sequence++
	path := filepath.Join(f.root, fmt.Sprintf("review-%d.json", f.sequence))
	if _, err := prepareReview(f.dir, f.tasks, path, now); err != nil {
		f.t.Fatal(err)
	}
	raw, err := os.ReadFile(path + ".input.json")
	if err != nil {
		f.t.Fatal(err)
	}
	var input reviewInput
	if err := json.Unmarshal(raw, &input); err != nil {
		f.t.Fatal(err)
	}
	return path, input
}
func (f *reviewFixture) receipt(path string, input reviewInput, blocked map[string]string) {
	f.t.Helper()
	draft := reviewDraft{Decisions: decisionSpecs(input.Batch), Evidence: evidenceSpecs(input.Files)}
	for i := range draft.Decisions {
		decision := &draft.Decisions[i]
		decision.Outcome = "no-action"
		decision.Finding = "Содержимое проверено; дополнительного действия нет."
		if decision.Kind == "event" {
			if condition, ok := blocked[decision.Key]; ok {
				decision.Outcome = "blocked"
				decision.Finding = "Вложение недоступно; содержимое пока не прочитано."
				decision.Notice = "Вложение недоступно, учебный сигнал пока не проверен."
				var retry struct {
					At       string `json:"at"`
					Evidence bool   `json:"evidence_change"`
				}
				if err := json.Unmarshal([]byte(condition), &retry); err != nil {
					f.t.Fatal(err)
				}
				decision.RetryAt = retry.At
				decision.EvidenceChange = retry.Evidence
			}
		}
	}
	for i := range draft.Evidence {
		if !strings.HasPrefix(draft.Evidence[i].ID, "unavailable:") {
			draft.Evidence[i].Finding = "Условия прочитаны полностью; дополнительных обязанностей нет."
		}
	}
	raw, err := json.Marshal(draft)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		f.t.Fatal(err)
	}
}
func (f *reviewFixture) commit(path string, now time.Time) queue.Summary {
	f.t.Helper()
	validated, err := validatePreparedReview(f.dir, path, taskBoard{}, now)
	if err != nil {
		f.t.Fatal(err)
	}
	summary, err := queue.RecordReview(f.dir, validated.CompletedIDs, validated.Deferred, path, validated.RequiredEvidence, now)
	if err != nil {
		f.t.Fatal(err)
	}
	return summary
}

func TestBlockedBatchProgressesAndRecoversThroughPreparedReview(t *testing.T) {
	f := newReviewFixture(t)
	now := time.Now()
	file := filepath.Join(f.root, "missing.txt")
	for i := 0; i < queue.MaxAnalysisBatch; i++ {
		f.collect("telegram", fmt.Sprintf("material %d", i), []string{file}, now.Add(time.Duration(i)*time.Second))
	}
	path, input := f.prepare(now.Add(time.Minute))
	if len(input.Batch.Items) != queue.MaxAnalysisBatch {
		t.Fatal(input.Batch)
	}
	blocked := map[string]string{}
	for _, item := range input.Batch.Items {
		blocked[item.ID] = `{"evidence_change":true}`
	}
	f.receipt(path, input, blocked)
	remaining := f.commit(path, now.Add(time.Minute))
	if remaining.Pending != 8 || len(remaining.Items) != 0 || remaining.Deferred != 1 || remaining.Waiting != 7 || len(remaining.WaitingSources) != 1 {
		t.Fatal(remaining)
	}
	// Repeating the command cannot acknowledge blocked data or create extra state.
	f.commit(path, now.Add(2*time.Minute))
	bundles, _ := filepath.Glob(filepath.Join(f.dir, "acks", "*", "ack.json"))
	if len(bundles) != 1 {
		t.Fatal(bundles)
	}
	for id := range blocked {
		item, err := queue.Inspect(f.dir, id)
		if err != nil || item.Acknowledged || item.Deferred == nil {
			t.Fatal(item, err)
		}
	}
	f.collect("gmail", "independent letter", nil, now.Add(4*time.Minute))
	path, input = f.prepare(now.Add(5 * time.Minute))
	if len(input.Batch.Items) != 1 || input.Batch.Items[0].Source != "gmail" {
		t.Fatal(input.Batch)
	}
	f.receipt(path, input, nil)
	remaining = f.commit(path, now.Add(5*time.Minute))
	brief, err := buildBrief(remaining, taskBoard{}, f.tasks, f.root, now.Add(5*time.Minute))
	if err != nil || brief["analysis_needed"] != false || remaining.Waiting != 7 {
		t.Fatal(brief, remaining, err)
	}
	// Restore the exact attachment after all handles/process-style commands closed.
	if err := os.WriteFile(file, []byte("restored material"), 0600); err != nil {
		t.Fatal(err)
	}
	path, input = f.prepare(now.Add(6 * time.Minute))
	if len(input.Batch.Items) != 8 {
		t.Fatal(input.Batch)
	}
	f.receipt(path, input, nil)
	remaining = f.commit(path, now.Add(6*time.Minute))
	if remaining.Pending != 0 {
		t.Fatal(remaining)
	}
	f.commit(path, now.Add(8*time.Minute))
}

func TestTimedRetryAndSourceOrderUseTheSameReviewContract(t *testing.T) {
	f := newReviewFixture(t)
	now := time.Now()
	f.collect("manual", "first delta", nil, now)
	path, input := f.prepare(now)
	id := input.Batch.Items[0].ID
	retry := now.Add(time.Hour).UTC().Format(time.RFC3339)
	f.receipt(path, input, map[string]string{id: fmt.Sprintf(`{"at":%q}`, retry)})
	f.commit(path, now)
	_, waiting := f.prepare(now.Add(30 * time.Second))
	if len(waiting.Batch.Items) != 0 {
		t.Fatal("unchanged timed deferral became ready", waiting.Batch)
	}
	f.collect("manual", "second delta", nil, now.Add(time.Minute))
	path, input = f.prepare(now.Add(2 * time.Minute))
	if len(input.Batch.Items) != 2 || input.Batch.Items[0].ID != id {
		t.Fatal("new source event did not reopen predecessor", input.Batch)
	}
	// Re-blocking the predecessor cannot close its later delta in the same batch.
	f.receipt(path, input, map[string]string{id: fmt.Sprintf(`{"at":%q}`, now.Add(3*time.Hour).UTC().Format(time.RFC3339))})
	if _, err := validatePreparedReview(f.dir, path, taskBoard{}, now.Add(2*time.Minute)); err == nil || !strings.Contains(err.Error(), "earlier source event") {
		t.Fatalf("out-of-order completion accepted: %v", err)
	}
	f.receipt(path, input, nil)
	remaining := f.commit(path, now.Add(2*time.Minute))
	if remaining.Pending != 0 {
		t.Fatal(remaining)
	}
}

func TestRestoredLargeRetryGetsATurnAmidFreshWork(t *testing.T) {
	f := newReviewFixture(t)
	now := time.Now()
	f.collect("telegram", strings.Repeat("x", queue.MaxAnalysisBatchBytes+1), nil, now)
	path, input := f.prepare(now)
	id := input.Batch.Items[0].ID
	f.receipt(path, input, map[string]string{id: fmt.Sprintf(`{"at":%q}`, now.Add(time.Minute).UTC().Format(time.RFC3339))})
	f.commit(path, now)
	f.collect("gmail", "fresh one", nil, now.Add(time.Minute))
	path, input = f.prepare(now.Add(2 * time.Minute))
	if len(input.Batch.Items) != 1 || input.Batch.Items[0].Source != "gmail" {
		t.Fatal(input.Batch)
	}
	f.receipt(path, input, nil)
	f.commit(path, now.Add(2*time.Minute))
	f.collect("gmail", "fresh two", nil, now.Add(3*time.Minute))
	path, input = f.prepare(now.Add(3 * time.Minute))
	if len(input.Batch.Items) != 1 || input.Batch.Items[0].ID != id {
		t.Fatal(input.Batch)
	}
	f.receipt(path, input, nil)
	remaining := f.commit(path, now.Add(3*time.Minute))
	if remaining.Pending != 1 || remaining.Items[0].Source != "gmail" {
		t.Fatal(remaining)
	}
}

func TestRecoveryBetweenPreparationAndDeferralIsNotSwallowed(t *testing.T) {
	f := newReviewFixture(t)
	now := time.Now()
	file := filepath.Join(f.root, "missing.txt")
	f.collect("telegram", "attachment", []string{file}, now)
	path, input := f.prepare(now)
	f.receipt(path, input, map[string]string{input.Batch.Items[0].ID: `{"evidence_change":true}`})
	if err := os.WriteFile(file, []byte("restored while reviewing"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := validatePreparedReview(f.dir, path, taskBoard{}, now); err == nil || !strings.Contains(err.Error(), "changed since preparation") {
		t.Fatalf("restoration swallowed: %v", err)
	}
	current, err := queue.Summarize(f.dir, queue.MaxAnalysisBatch, now)
	if err != nil || len(current.Items) != 1 || current.Deferred != 0 {
		t.Fatal(current, err)
	}
	path, input = f.prepare(now.Add(time.Minute))
	f.receipt(path, input, nil)
	if remaining := f.commit(path, now.Add(time.Minute)); remaining.Pending != 0 {
		t.Fatal(remaining)
	}
}

func TestOlderReceiptCannotOverwriteNewerDeferral(t *testing.T) {
	f := newReviewFixture(t)
	now := time.Now()
	f.collect("telegram", "attachment", nil, now)
	old, input := f.prepare(now)
	id := input.Batch.Items[0].ID
	f.receipt(old, input, map[string]string{id: fmt.Sprintf(`{"at":%q}`, now.Add(time.Minute).UTC().Format(time.RFC3339))})
	f.commit(old, now)
	next, newInput := f.prepare(now.Add(2 * time.Minute))
	f.receipt(next, newInput, map[string]string{id: fmt.Sprintf(`{"at":%q}`, now.Add(time.Hour).UTC().Format(time.RFC3339))})
	f.commit(next, now.Add(2*time.Minute))
	if _, err := validatePreparedReview(f.dir, old, taskBoard{}, now.Add(3*time.Minute)); err == nil {
		t.Fatal("older blocked review accepted")
	}
	item, err := queue.Inspect(f.dir, id)
	if err != nil || item.Deferred == nil || !item.Deferred.ReviewedAt.Equal(newInput.PreparedAt) {
		t.Fatal(item, err)
	}
}

func TestNewSourceEventReopensDeferralOnlyOnce(t *testing.T) {
	f := newReviewFixture(t)
	now := time.Now()
	f.collect("netology", "source unavailable", nil, now)
	path, input := f.prepare(now.Add(time.Minute))
	first := input.Batch.Items[0].ID
	f.receipt(path, input, map[string]string{first: fmt.Sprintf(`{"at":%q}`, now.Add(4*time.Hour).UTC().Format(time.RFC3339))})
	f.commit(path, now.Add(time.Minute))
	f.collect("netology", "new assignment status", nil, now.Add(2*time.Minute))
	path, input = f.prepare(now.Add(3 * time.Minute))
	if len(input.Batch.Items) != 2 || input.Batch.Items[0].ID != first {
		t.Fatal("new source event did not reopen blocked predecessor", input.Batch)
	}
	blocked := map[string]string{}
	for _, item := range input.Batch.Items {
		blocked[item.ID] = fmt.Sprintf(`{"at":%q}`, now.Add(4*time.Hour).UTC().Format(time.RFC3339))
	}
	f.receipt(path, input, blocked)
	f.commit(path, now.Add(3*time.Minute))
	_, quiet := f.prepare(now.Add(4 * time.Minute))
	if len(quiet.Batch.Items) != 0 {
		t.Fatal("same source delta caused a retry storm", quiet.Batch)
	}
}
