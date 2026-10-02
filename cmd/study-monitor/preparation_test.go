package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chupakobra6/study-automation/internal/queue"
)

func writeReview(t *testing.T, path string, draft reviewDraft) {
	t.Helper()
	raw, err := json.Marshal(draft)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestPreparedReviewDoesNotAcceptListedButUnreadEvidence(t *testing.T) {
	f := newReviewFixture(t)
	now := time.Now()
	file := filepath.Join(f.root, "photo.jpg")
	if err := os.WriteFile(file, []byte("image"), 0600); err != nil {
		t.Fatal(err)
	}
	f.collect("telegram", "new attachment", []string{file}, now)
	path, input := f.prepare(now)
	draft := reviewDraft{Decisions: decisionSpecs(input.Batch), Evidence: evidenceSpecs(input.Files)}
	for i := range draft.Decisions {
		draft.Decisions[i].Outcome = "no-action"
		draft.Decisions[i].Finding = "Письмо и вложение проверены."
	}
	writeReview(t, path, draft)
	if _, err := validatePreparedReview(f.dir, path, taskBoard{}, now); err == nil || !strings.Contains(err.Error(), "content/context") {
		t.Fatalf("listed path accepted: %v", err)
	}
	draft.Evidence[0].Finding = "Фото грамматики относится к факультативу, от которого пользователь отказался; контекст соседнего сообщения подтверждает английский."
	writeReview(t, path, draft)
	if _, err := validatePreparedReview(f.dir, path, taskBoard{}, now); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("changed image"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := validatePreparedReview(f.dir, path, taskBoard{}, now); err == nil {
		t.Fatal("changed evidence accepted")
	}
}

func TestIdenticalEvidenceNeedsOneFindingButEveryPathStaysVerified(t *testing.T) {
	f := newReviewFixture(t)
	now := time.Now()
	a, b := filepath.Join(f.root, "copy-a.pdf"), filepath.Join(f.root, "copy-b.pdf")
	for _, path := range []string{a, b} {
		if err := os.WriteFile(path, []byte("same content"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	f.collect("telegram", "two copies", []string{a, b}, now)
	path, input := f.prepare(now)
	draft := reviewDraft{Decisions: decisionSpecs(input.Batch), Evidence: evidenceSpecs(input.Files)}
	if len(draft.Evidence) != 1 || len(draft.Evidence[0].Paths) != 2 {
		t.Fatal(draft.Evidence)
	}
	for i := range draft.Decisions {
		draft.Decisions[i].Outcome = "no-action"
		draft.Decisions[i].Finding = "Содержимое проверено."
	}
	draft.Evidence[0].Finding = "Одинаковые PDF-копии содержат условия задания."
	writeReview(t, path, draft)
	if _, err := validatePreparedReview(f.dir, path, taskBoard{}, now); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := validatePreparedReview(f.dir, path, taskBoard{}, now); err == nil {
		t.Fatal("changed copy accepted")
	}
}

func TestPreparedReviewSettlesDueCheckWithoutLosingNotice(t *testing.T) {
	f := newReviewFixture(t)
	now := time.Now()
	task := studyTask{ID: "db", Title: "Базы данных", Status: "open", NextCheckAt: now.Add(-time.Hour).Format(time.RFC3339)}
	board := taskBoard{Version: 1, Tasks: []studyTask{task}}
	raw, _ := json.Marshal(board)
	if err := os.WriteFile(f.tasks, raw, 0600); err != nil {
		t.Fatal(err)
	}
	path, input := f.prepare(now)
	draft := reviewDraft{Decisions: decisionSpecs(input.Batch), Evidence: evidenceSpecs(input.Files)}
	draft.Decisions[0].Outcome = "no-action"
	draft.Decisions[0].Finding = "Нового ответа нет; дальше проверять по новому сообщению."
	draft.Decisions[0].Action = "Базы данных: написать преподавателю в Max для обсуждения темы."
	writeReview(t, path, draft)
	if _, err := validatePreparedReview(f.dir, path, board, now); err == nil || !strings.Contains(err.Error(), "stale next_check_at") {
		t.Fatalf("stale loop accepted: %v", err)
	}
	board.Tasks[0].NextCheckAt = ""
	if _, err := validatePreparedReview(f.dir, path, board, now); err != nil {
		t.Fatal(err)
	}
	draft.Decisions[0].Action = ""
	writeReview(t, path, draft)
	if _, err := validatePreparedReview(f.dir, path, board, now); err == nil {
		t.Fatal("due action vanished")
	}
}

func TestClosedRegistrationDoesNotCreateOverdueOrDueWork(t *testing.T) {
	now := time.Now()
	board := taskBoard{Version: 1, Tasks: []studyTask{{
		ID: "registration", Status: "closed_confirmed", Title: "Регистрация завершена",
		NextCheckAt: now.Add(-time.Hour).Format(time.RFC3339),
		Dates:       []taskDate{{At: now.Add(-time.Hour).Format(time.RFC3339), Kind: "deadline", Label: "Срок регистрации"}},
	}}}
	due, err := dueTasks(board, now)
	if err != nil || len(due) != 0 || len(importantDates(board, now)) != 0 || len(openActions(board)) != 0 {
		t.Fatalf("completed registration became outstanding: due=%v err=%v", due, err)
	}
	batch, err := buildReviewBatch(nil, 0, board, due)
	if err != nil || len(decisionSpecs(batch)) != 0 {
		t.Fatal(batch, err)
	}
}

func TestPreparedReviewAcknowledgesOnlyCompletedNeighbour(t *testing.T) {
	f := newReviewFixture(t)
	now := time.Now()
	f.collect("gmail", "letter", nil, now)
	missing := filepath.Join(f.root, "missing.jpg")
	f.collect("telegram", "attachment", []string{missing}, now.Add(time.Second))
	reviewedAt := now.Add(2 * time.Second)
	path, input := f.prepare(reviewedAt)
	blocked := map[string]string{}
	for _, item := range input.Batch.Items {
		if item.Source == "telegram" {
			blocked[item.ID] = `{"evidence_change":true}`
		}
	}
	f.receipt(path, input, blocked)
	result, err := validatePreparedReview(f.dir, path, taskBoard{}, reviewedAt)
	if err != nil || len(result.CompletedIDs) != 1 || len(result.Deferred) != 1 {
		t.Fatal(result, err)
	}
	remaining, err := queue.RecordReview(f.dir, result.CompletedIDs, result.Deferred, path, result.RequiredEvidence, reviewedAt)
	if err != nil || remaining.Pending != 1 || remaining.Deferred != 1 || len(remaining.Items) != 0 {
		t.Fatal(remaining, err)
	}
}
