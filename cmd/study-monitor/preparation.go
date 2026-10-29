package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/chupakobra6/study-automation/internal/queue"
)

// The input is owned by the collector. The review contains only decisions and
// findings; paths, event IDs and hashes are never trusted from the review.
type reviewInput struct {
	Batch      reviewBatch       `json:"batch"`
	Due        []studyTask       `json:"due_tasks"`
	Files      map[string]string `json:"sha256"`
	PreparedAt time.Time         `json:"prepared_at"`
}

type reviewDecision struct {
	Kind           string `json:"kind"`
	Key            string `json:"key"`
	Outcome        string `json:"outcome"`
	Finding        string `json:"finding"`
	Notice         string `json:"notice,omitempty"`
	Action         string `json:"action,omitempty"`
	Timing         string `json:"timing,omitempty"`
	RetryAt        string `json:"retry_at,omitempty"`
	EvidenceChange bool   `json:"evidence_change,omitempty"`
}

type reviewEvidence struct {
	ID      string   `json:"id"`
	Paths   []string `json:"paths"`
	Finding string   `json:"finding"`
}

type reviewDraft struct {
	Decisions []reviewDecision `json:"decisions"`
	Evidence  []reviewEvidence `json:"evidence"`
}

func evidenceHash(path string) (string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(body)
	return hex.EncodeToString(hash[:]), nil
}

func createPrivate(path string, body []byte) error {
	if !filepath.IsAbs(path) {
		return errors.New("review must be an absolute path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(body)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func decisionSpecs(batch reviewBatch) []reviewDecision {
	result := []reviewDecision{}
	for _, item := range batch.Items {
		result = append(result, reviewDecision{Kind: "event", Key: item.ID})
	}
	for _, item := range batch.Coverage.Topics {
		result = append(result, reviewDecision{Kind: "subject", Key: item.Key})
	}
	for _, key := range batch.Coverage.Deadlines {
		result = append(result, reviewDecision{Kind: "deadline", Key: key})
	}
	for _, item := range batch.Coverage.UserDecisions {
		result = append(result, reviewDecision{Kind: "decision", Key: item.TaskID})
	}
	for _, item := range batch.Coverage.LinkedDocuments {
		result = append(result, reviewDecision{Kind: "reference", Key: item.URL})
	}
	for _, key := range batch.Coverage.DueTaskIDs {
		result = append(result, reviewDecision{Kind: "due", Key: key})
	}
	return result
}

func evidenceSpecs(files map[string]string) []reviewEvidence {
	groups := map[string]*reviewEvidence{}
	for path, hash := range files {
		id := hash
		if hash == "unavailable" {
			id = "unavailable:" + path
		}
		if groups[id] == nil {
			groups[id] = &reviewEvidence{ID: id, Paths: []string{}}
		}
		groups[id].Paths = append(groups[id].Paths, path)
	}
	ids := make([]string, 0, len(groups))
	for id := range groups {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	result := make([]reviewEvidence, 0, len(ids))
	for _, id := range ids {
		sort.Strings(groups[id].Paths)
		result = append(result, *groups[id])
	}
	return result
}

func prepareReview(queueDir, tasksFile, receipt string, now time.Time) (map[string]any, error) {
	if !filepath.IsAbs(receipt) || filepath.Ext(receipt) != ".json" {
		return nil, errors.New("--receipt requires an absolute new .json path")
	}
	if _, err := os.Stat(receipt); !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("review already exists or cannot be inspected")
	}
	board, err := readBoard(tasksFile)
	if err != nil {
		return nil, err
	}
	summary, err := queue.Summarize(queueDir, queue.MaxAnalysisBatch, now)
	if err != nil {
		return nil, err
	}
	due, err := dueTasks(board, now)
	if err != nil {
		return nil, err
	}
	batch, err := buildReviewBatch(summary.Items, summary.Pending-len(summary.Items), board, due)
	if err != nil {
		return nil, err
	}
	input := reviewInput{Batch: batch, Due: due, Files: map[string]string{}, PreparedAt: now.UTC()}
	for _, path := range batch.RequiredEvidence {
		hash, err := evidenceHash(path)
		if err != nil {
			hash = "unavailable"
		}
		input.Files[path] = hash
	}
	draft := reviewDraft{Decisions: decisionSpecs(batch), Evidence: evidenceSpecs(input.Files)}
	encodedInput, err := json.MarshalIndent(input, "", "  ")
	if err != nil {
		return nil, err
	}
	encodedDraft, err := json.MarshalIndent(draft, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := createPrivate(receipt+".input.json", encodedInput); err != nil {
		return nil, err
	}
	if err := createPrivate(receipt, encodedDraft); err != nil {
		return nil, err
	}
	return map[string]any{"receipt": receipt, "event_count": batch.EventCount, "evidence_path_count": len(input.Files), "unique_evidence_count": len(draft.Evidence), "due_task_count": len(due)}, nil
}

func validOutcome(value string) bool {
	return value == "updated" || value == "no-action" || value == "blocked"
}

func substantive(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && !strings.Contains(strings.ToLower(value), "todo")
}

func decisionKey(kind, key string) string { return kind + "\x00" + key }

func validatePreparedReview(queueDir, receipt string, board taskBoard, now time.Time) (validationResult, error) {
	var input reviewInput
	raw, err := os.ReadFile(receipt + ".input.json")
	if err != nil {
		return validationResult{}, fmt.Errorf("run queue prepare before review: %w", err)
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return validationResult{}, err
	}
	if len(input.Batch.Items) > queue.MaxAnalysisBatch {
		return validationResult{}, errors.New("review batch exceeds limit")
	}
	raw, err = os.ReadFile(receipt)
	if err != nil {
		return validationResult{}, err
	}
	var draft reviewDraft
	if err := json.Unmarshal(raw, &draft); err != nil {
		return validationResult{}, fmt.Errorf("invalid review JSON: %w", err)
	}
	specs := decisionSpecs(input.Batch)
	expectedDecisions := map[string]bool{}
	for _, spec := range specs {
		expectedDecisions[decisionKey(spec.Kind, spec.Key)] = true
	}
	decisions := map[string]reviewDecision{}
	for _, decision := range draft.Decisions {
		key := decisionKey(decision.Kind, decision.Key)
		if !expectedDecisions[key] {
			return validationResult{}, fmt.Errorf("unexpected decision %s/%s", decision.Kind, decision.Key)
		}
		if _, exists := decisions[key]; exists {
			return validationResult{}, fmt.Errorf("duplicate decision %s/%s", decision.Kind, decision.Key)
		}
		if !validOutcome(decision.Outcome) || !substantive(decision.Finding) {
			return validationResult{}, fmt.Errorf("decision %s/%s needs an outcome and concrete finding", decision.Kind, decision.Key)
		}
		decisions[key] = decision
	}
	for _, spec := range specs {
		if _, ok := decisions[decisionKey(spec.Kind, spec.Key)]; !ok {
			return validationResult{}, fmt.Errorf("missing decision %s/%s", spec.Kind, spec.Key)
		}
	}
	blockedCoverage := false
	blockedEvent := false
	for _, decision := range decisions {
		if decision.Outcome != "blocked" {
			continue
		}
		if decision.Kind == "event" {
			blockedEvent = true
		} else if decision.Kind != "due" {
			blockedCoverage = true
		}
	}
	if blockedCoverage && !blockedEvent {
		return validationResult{}, errors.New("blocked source coverage requires a blocked event")
	}
	for _, reference := range input.Batch.Coverage.LinkedDocuments {
		if decisions[decisionKey("reference", reference.URL)].Outcome != "blocked" {
			continue
		}
		for _, source := range reference.Sources {
			if decisions[decisionKey("event", source.EventID)].Outcome != "blocked" {
				return validationResult{}, fmt.Errorf("blocked reference %s requires its owning event %s to be blocked", reference.URL, source.EventID)
			}
		}
	}
	groups := evidenceSpecs(input.Files)
	if len(draft.Evidence) != len(groups) {
		return validationResult{}, fmt.Errorf("review needs %d unique evidence groups, got %d", len(groups), len(draft.Evidence))
	}
	evidence := map[string]reviewEvidence{}
	for _, group := range draft.Evidence {
		if _, exists := evidence[group.ID]; exists {
			return validationResult{}, fmt.Errorf("duplicate evidence group %s", group.ID)
		}
		evidence[group.ID] = group
	}
	for _, expected := range groups {
		actual, ok := evidence[expected.ID]
		if !ok || !sameStrings(actual.Paths, expected.Paths) {
			return validationResult{}, fmt.Errorf("evidence group changed: %s", expected.ID)
		}
	}
	for _, original := range input.Due {
		current, ok := taskByID(board, original.ID)
		if !ok {
			return validationResult{}, fmt.Errorf("due task %s was deleted instead of resolved", original.ID)
		}
		decision := decisions[decisionKey("due", original.ID)]
		if decision.Outcome != "blocked" && !strings.HasPrefix(current.Status, "closed") && !strings.HasPrefix(current.Status, "cancelled") && current.NextCheckAt != "" {
			at, err := time.Parse(time.RFC3339, current.NextCheckAt)
			if err != nil || !at.After(now) {
				return validationResult{}, fmt.Errorf("due task %s still has a stale next_check_at", original.ID)
			}
		}
	}
	result := validationResult{Valid: true}
	seen := map[string]bool{}
	for _, expected := range input.Batch.Items {
		item, err := queue.Inspect(queueDir, expected.ID)
		if err != nil {
			return validationResult{}, err
		}
		if item.ReportPath != expected.ReportPath {
			return validationResult{}, errors.New("prepared event no longer matches queue")
		}
		decision := decisions[decisionKey("event", item.ID)]
		if decision.Outcome == "blocked" {
			deferred, err := parseDeferral(decision, item, input.Files, input.PreparedAt, now)
			if err != nil {
				return validationResult{}, err
			}
			result.Deferred = append(result.Deferred, deferred)
			continue
		}
		for _, path := range item.RequiredEvidence {
			hash, err := evidenceHash(path)
			if err != nil || hash != input.Files[path] {
				return validationResult{}, fmt.Errorf("evidence missing/changed since preparation: %s", path)
			}
			if !substantive(evidence[hash].Finding) {
				return validationResult{}, fmt.Errorf("evidence requires a content/context finding: %s", path)
			}
			if !seen[path] {
				result.RequiredEvidence = append(result.RequiredEvidence, path)
				seen[path] = true
			}
		}
		result.CompletedIDs = append(result.CompletedIDs, item.ID)
	}
	if err := queue.ValidateReviewState(queueDir, result.CompletedIDs, result.Deferred, raw, now); err != nil {
		return validationResult{}, err
	}
	result.EventCount = len(result.CompletedIDs)
	result.Notification, err = buildReviewNotification(specs, decisions)
	if err != nil {
		return validationResult{}, err
	}
	return result, nil
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func parseDeferral(decision reviewDecision, item queue.Item, files map[string]string, preparedAt, now time.Time) (queue.Deferral, error) {
	result := queue.Deferral{ID: item.ID, Reason: strings.TrimSpace(decision.Finding), ReviewedAt: preparedAt}
	if preparedAt.IsZero() {
		return result, errors.New("blocked event needs current prepared input")
	}
	if decision.RetryAt != "" {
		at, err := time.Parse(time.RFC3339, decision.RetryAt)
		same := item.Deferred != nil && item.Deferred.ReviewedAt.Equal(preparedAt)
		if err != nil || (!at.After(now) && !same) {
			return result, fmt.Errorf("blocked event %s needs a future retry time", item.ID)
		}
		result.RetryAt = &at
	}
	if decision.EvidenceChange {
		result.Evidence = map[string]string{}
		for _, path := range item.RequiredEvidence {
			expected, ok := files[path]
			if !ok || queue.EvidenceFingerprint(path) != expected {
				return result, fmt.Errorf("retry evidence changed since preparation; prepare again: %s", path)
			}
			result.Evidence[filepath.Clean(path)] = expected
		}
	}
	if len(result.Evidence) == 0 && result.RetryAt == nil {
		return result, fmt.Errorf("blocked event %s needs evidence_change or retry_at", item.ID)
	}
	return result, nil
}

func buildReviewNotification(specs []reviewDecision, decisions map[string]reviewDecision) (string, error) {
	changes, actions, timings := []string{}, []string{}, []string{}
	changed := false
	for _, spec := range specs {
		decision := decisions[decisionKey(spec.Kind, spec.Key)]
		if decision.Outcome != "no-action" {
			changed = true
		}
		if decision.Outcome != "no-action" && !substantive(decision.Notice) {
			return "", fmt.Errorf("%s/%s needs a user-facing notice", spec.Kind, spec.Key)
		}
		if spec.Kind == "due" && !substantive(decision.Action) {
			return "", fmt.Errorf("due task %s needs a concrete action", spec.Key)
		}
		if substantive(decision.Notice) {
			if spec.Kind == "deadline" {
				timings = append(timings, decision.Notice)
			} else {
				changes = append(changes, decision.Notice)
			}
		}
		if substantive(decision.Action) {
			actions = append(actions, decision.Action)
		}
		if substantive(decision.Timing) {
			timings = append(timings, decision.Timing)
		}
	}
	if changed {
		for _, change := range changes {
			if strings.Contains(normalizeSearch(change), "новых учебных изменений нет") {
				return "", errors.New("notification contradicts updated or blocked decisions")
			}
		}
	}
	for _, line := range append(append(append([]string{}, changes...), actions...), timings...) {
		lower := strings.ToLower(line)
		for _, internal := range []string{"open_action_count", "next_important_date", "remaining_event_count", "review_batch", "required_evidence", "queue ack", "receipt marker"} {
			if strings.Contains(lower, internal) {
				return "", fmt.Errorf("notification exposes internal term %q", internal)
			}
		}
	}
	if len(changes) == 0 {
		if changed {
			changes = []string{"Изменения по срокам приведены ниже."}
		} else {
			changes = []string{"Новых учебных изменений нет."}
		}
	}
	if len(actions) == 0 {
		actions = []string{"Новых подтверждённых действий нет."}
	}
	if len(timings) == 0 {
		timings = []string{"Новых сроков нет."}
	}
	return "### Что изменилось\n\n" + noticeLines(changes) + "\n\n### Что сделать\n\n" + noticeLines(actions) + "\n\n### Сроки и ожидания\n\n" + noticeLines(timings), nil
}

func noticeLines(items []string) string {
	seen := map[string]bool{}
	lines := []string{}
	for _, item := range items {
		item = strings.TrimSpace(item)
		if !seen[item] {
			seen[item] = true
			lines = append(lines, "- "+item)
		}
	}
	return strings.Join(lines, "\n")
}
