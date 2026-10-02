package queue

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func put(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func putReview(t *testing.T, path string, outcomes map[string]string, evidence []string) {
	t.Helper()
	decisions := []map[string]string{}
	for id, outcome := range outcomes {
		decisions = append(decisions, map[string]string{"kind": "event", "key": id, "outcome": outcome})
	}
	body, err := json.Marshal(map[string]any{"decisions": decisions, "evidence": []map[string]any{{"paths": evidence}}})
	if err != nil {
		t.Fatal(err)
	}
	put(t, path, string(body))
}

func collect(t *testing.T, dir, source, path, body string, outcome Outcome, now time.Time) {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Recover(); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(source, path, now); err != nil {
		t.Fatal(err)
	}
	put(t, path, body)
	outcome.Source = source
	if err := s.Complete(outcome); err != nil {
		t.Fatal(err)
	}
}

func TestPendingReportSurvivesLatestReplacementAndExplicitAck(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "queue")
	latest := filepath.Join(t.TempDir(), "latest.md")
	first := "Generated at: 2026-09-06T10:00:00Z\nPRIVATE REPORT BODY: assignment #42\n"
	collect(t, dir, "mail", latest, first, Outcome{Status: "delta", Added: 1}, time.Unix(10, 0))
	collect(t, dir, "mail", latest, "No new messages.\n", Outcome{Status: "no-changes"}, time.Unix(20, 0))
	summary, err := Summarize(dir, 10, time.Now())
	if err != nil || summary.Pending != 1 || summary.BySource["mail"] != 1 || !summary.OldestAt.Equal(time.Unix(10, 0)) {
		t.Fatalf("summary=%+v err=%v", summary, err)
	}
	data, _ := json.Marshal(summary)
	if strings.Contains(string(data), "PRIVATE REPORT BODY") {
		t.Fatal("summary loads full report text")
	}
	item, err := Inspect(dir, summary.Items[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(item.ReportPath)
	if err != nil || string(body) != first {
		t.Fatalf("saved=%q err=%v", body, err)
	}
	receipt := filepath.Join(t.TempDir(), "receipt.md")
	putReview(t, receipt, map[string]string{item.ID: "updated"}, nil)
	summary, err = RecordReview(dir, []string{item.ID}, nil, receipt, nil, time.Unix(30, 0))
	if err != nil || summary.Pending != 0 {
		t.Fatalf("ack=%+v err=%v", summary, err)
	}
	item, err = Inspect(dir, item.ID)
	if err != nil || !item.Acknowledged || item.ReceiptPath == receipt {
		t.Fatalf("item=%+v err=%v", item, err)
	}
	if _, err := RecordReview(dir, []string{item.ID}, nil, receipt, nil, time.Unix(40, 0)); err != nil {
		t.Fatalf("same ack retry was not idempotent: %v", err)
	}
	putReview(t, receipt, map[string]string{item.ID: "no-action"}, nil)
	if _, err := RecordReview(dir, []string{item.ID}, nil, receipt, nil, time.Now()); err == nil {
		t.Fatal("replaced an existing acknowledgment receipt")
	}
}

func TestAckBatchValidatesAllIDsAndRequiresReceipt(t *testing.T) {
	dir, latest := filepath.Join(t.TempDir(), "queue"), filepath.Join(t.TempDir(), "latest.md")
	collect(t, dir, "mail", latest, "message A", Outcome{Status: "delta", Added: 1}, time.Unix(10, 0))
	collect(t, dir, "mail", latest, "message B", Outcome{Status: "delta", Added: 1}, time.Unix(20, 0))
	summary, err := Summarize(dir, 10, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	compact, err := Summarize(dir, 1, time.Now())
	if err != nil || compact.Pending != 2 || len(compact.Items) != 1 || !compact.Truncated || compact.BySource["mail"] != 2 {
		t.Fatalf("compact summary=%+v err=%v", compact, err)
	}
	receipt := filepath.Join(t.TempDir(), "receipt.md")
	if _, err := RecordReview(dir, []string{summary.Items[0].ID}, nil, receipt, nil, time.Now()); err == nil {
		t.Fatal("accepted missing receipt")
	}
	putReview(t, receipt, map[string]string{summary.Items[0].ID: "no-action", summary.Items[1].ID: "no-action"}, nil)
	if _, err := RecordReview(dir, nil, nil, receipt, nil, time.Now()); err == nil {
		t.Fatal("accepted implicit/all acknowledgment")
	}
	if _, err := RecordReview(dir, []string{summary.Items[0].ID, strings.Repeat("0", 32) + "-report"}, nil, receipt, nil, time.Now()); err == nil {
		t.Fatal("accepted unknown ID")
	}
	after, err := Summarize(dir, 10, time.Now())
	if err != nil || after.Pending != 2 {
		t.Fatalf("acknowledged part of invalid batch: %+v err=%v", after, err)
	}
	after, err = RecordReview(dir, []string{summary.Items[0].ID, summary.Items[1].ID}, nil, receipt, nil, time.Now())
	if err != nil || after.Pending != 0 {
		t.Fatalf("valid batch=%+v err=%v", after, err)
	}
}

func TestValidIDAcceptsSplitReportParts(t *testing.T) {
	base := "0123456789abcdef0123456789abcdef"
	for _, id := range []string{base + "-report", base + "-report-000002"} {
		if !validID(id) {
			t.Fatalf("valid split report ID rejected: %s", id)
		}
	}
	for _, id := range []string{base + "-report-2", base + "-report-00000x", base + "-report-0000000"} {
		if validID(id) {
			t.Fatalf("invalid split report ID accepted: %s", id)
		}
	}
}

func TestAckRequiresDispositionForEveryEventAndBoundsBatch(t *testing.T) {
	dir, latest := filepath.Join(t.TempDir(), "queue"), filepath.Join(t.TempDir(), "latest.md")
	for index := 0; index < MaxAnalysisBatch+1; index++ {
		collect(t, dir, "mail", latest, fmt.Sprintf("message %d", index), Outcome{Status: "delta", Added: 1}, time.Unix(int64(index+1), 0))
	}
	summary, err := Summarize(dir, 50, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	receipt := filepath.Join(t.TempDir(), "receipt.md")
	putReview(t, receipt, map[string]string{}, nil)
	if _, err := RecordReview(dir, []string{summary.Items[0].ID}, nil, receipt, nil, time.Now()); err == nil || !strings.Contains(err.Error(), "exactly one event disposition") {
		t.Fatalf("missing disposition was not rejected: %v", err)
	}
	put(t, receipt, fmt.Sprintf(`{"decisions":[{"kind":"event","key":%q,"outcome":"updated"},{"kind":"event","key":%q,"outcome":"no-action"}]}`, summary.Items[0].ID, summary.Items[0].ID))
	if _, err := RecordReview(dir, []string{summary.Items[0].ID}, nil, receipt, nil, time.Now()); err == nil || !strings.Contains(err.Error(), "exactly one event disposition") {
		t.Fatalf("ambiguous disposition was not rejected: %v", err)
	}
	ids := make([]string, 0, len(summary.Items))
	outcomes := map[string]string{}
	for _, item := range summary.Items {
		ids = append(ids, item.ID)
		outcomes[item.ID] = "no-action"
	}
	putReview(t, receipt, outcomes, nil)
	if _, err := RecordReview(dir, ids, nil, receipt, nil, time.Now()); err == nil || !strings.Contains(err.Error(), "maximum") {
		t.Fatalf("oversized acknowledgment was not rejected: %v", err)
	}
}

func TestAckCannotSkipRequiredEvidenceReview(t *testing.T) {
	dir, latest := filepath.Join(t.TempDir(), "queue"), filepath.Join(t.TempDir(), "latest.md")
	evidence := filepath.Join(t.TempDir(), "assignment.docx")
	put(t, evidence, "document contents")
	collect(t, dir, "telegram", latest, "New assignment with attachment", Outcome{
		Status: "delta", Added: 1, Attachments: 1, RequiredEvidence: []string{evidence},
	}, time.Unix(10, 0))
	summary, err := Summarize(dir, 10, time.Now())
	if err != nil || len(summary.Items) != 1 || len(summary.Items[0].RequiredEvidence) != 1 {
		t.Fatalf("required evidence was not preserved: summary=%+v err=%v", summary, err)
	}
	receipt := filepath.Join(t.TempDir(), "receipt.md")
	putReview(t, receipt, map[string]string{summary.Items[0].ID: "updated"}, nil)
	if _, err := RecordReview(dir, []string{summary.Items[0].ID}, nil, receipt, nil, time.Now()); err == nil {
		t.Fatal("acknowledged an event without its required evidence")
	}
	putReview(t, receipt, map[string]string{summary.Items[0].ID: "updated"}, []string{evidence})
	if _, err := RecordReview(dir, []string{summary.Items[0].ID}, nil, receipt, nil, time.Now()); err == nil {
		t.Fatal("accepted a receipt citation without an explicit reviewed-file argument")
	}
	after, err := RecordReview(dir, []string{summary.Items[0].ID}, nil, receipt, []string{evidence}, time.Now())
	if err != nil || after.Pending != 0 {
		t.Fatalf("reviewed evidence could not be acknowledged: summary=%+v err=%v", after, err)
	}
}

func TestReportReplayDeduplicatesOnlyCollectorMetadata(t *testing.T) {
	dir, latest := filepath.Join(t.TempDir(), "queue"), filepath.Join(t.TempDir(), "latest.md")
	collect(t, dir, "mail", latest, "Generated at: yesterday\n- Date: source date\nMessage #42\n", Outcome{Added: 1}, time.Unix(10, 0))
	collect(t, dir, "mail", latest, "Generated at: today\n- Date: source date\nMessage #42\n", Outcome{Added: 1}, time.Unix(20, 0))
	summary, err := Summarize(dir, 10, time.Now())
	if err != nil || summary.Pending != 1 {
		t.Fatalf("duplicate report=%+v err=%v", summary, err)
	}
	collect(t, dir, "mail", latest, "Generated at: today\n- Date: changed source date\nMessage #42\n", Outcome{Added: 1}, time.Unix(30, 0))
	summary, err = Summarize(dir, 10, time.Now())
	if err != nil || summary.Pending != 2 {
		t.Fatalf("suppressed substantive change=%+v err=%v", summary, err)
	}
}

func TestSourceAvailabilityStaysOutsideAcademicQueue(t *testing.T) {
	dir, latest := filepath.Join(t.TempDir(), "queue"), filepath.Join(t.TempDir(), "latest.md")
	failure := Outcome{Status: "failed", Detail: "authentication expired", Failed: true}
	collect(t, dir, "mail", latest, "", failure, time.Unix(10, 0))
	collect(t, dir, "mail", latest, "", failure, time.Unix(20, 0))
	summary, err := Summarize(dir, 10, time.Now())
	if err != nil || summary.Pending != 0 || len(summary.Failures) != 1 || summary.Failures[0].Detail != "authentication expired" {
		t.Fatalf("summary=%+v err=%v", summary, err)
	}
	collect(t, dir, "mail", latest, "", failure, time.Unix(30, 0))
	summary, err = Summarize(dir, 10, time.Now())
	if err != nil || summary.Pending != 0 || len(summary.Failures) != 1 {
		t.Fatalf("repeated failure entered academic queue=%+v err=%v", summary, err)
	}
	collect(t, dir, "mail", latest, "No changes", Outcome{Status: "no-changes", Detail: "access restored"}, time.Unix(40, 0))
	summary, err = Summarize(dir, 10, time.Now())
	if err != nil || summary.Pending != 0 || len(summary.Failures) != 0 {
		t.Fatalf("recovery=%+v err=%v", summary, err)
	}
	collect(t, dir, "mail", latest, "No changes", Outcome{Status: "no-changes"}, time.Unix(50, 0))
	summary, err = Summarize(dir, 10, time.Now())
	if err != nil || summary.Pending != 0 {
		t.Fatalf("duplicate recovery=%+v err=%v", summary, err)
	}
	collect(t, dir, "mail", latest, "No changes", failure, time.Unix(60, 0))
	summary, err = Summarize(dir, 10, time.Now())
	if err != nil || summary.Pending != 0 || len(summary.Failures) != 1 {
		t.Fatalf("suppressed new failure episode=%+v err=%v", summary, err)
	}
}

func TestFailedSourceStillQueuesConfirmedAcademicDelta(t *testing.T) {
	dir, latest := filepath.Join(t.TempDir(), "queue"), filepath.Join(t.TempDir(), "latest.md")
	collect(t, dir, "lms", latest, "Assignment due tomorrow", Outcome{
		Status: "incomplete", Detail: "later page unavailable", Failed: true,
		NeedsAnalysis: true, AnalysisDecisionKnown: true,
	}, time.Unix(10, 0))
	summary, err := Summarize(dir, 10, time.Now())
	if err != nil || summary.Pending != 1 || len(summary.Failures) != 1 || summary.Items[0].Kind != "report" {
		t.Fatalf("partial academic delta or source gap was lost: %+v err=%v", summary, err)
	}
}

func TestHistoricalOperationalEventsReceiveMachineReceipt(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "queue")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"failure", "recovery"} {
		prefix := "a"
		if kind == "recovery" {
			prefix = "b"
		}
		id := strings.Repeat(prefix, 32) + "-" + kind
		if err := s.enqueue(Item{ID: id, Source: "mail", Kind: kind, CreatedAt: time.Unix(10, 0)}, []byte(kind)); err != nil {
			t.Fatal(err)
		}
	}
	reportID := strings.Repeat("c", 32) + "-report"
	if err := s.enqueue(Item{ID: reportID, Source: "mail", Kind: "report", CreatedAt: time.Unix(20, 0)}, []byte("assignment")); err != nil {
		t.Fatal(err)
	}
	s.Close()
	first, err := Summarize(dir, 10, time.Now())
	if err != nil || first.Pending != 1 || first.Items[0].ID != reportID {
		t.Fatalf("machine retirement lost academic event: %+v err=%v", first, err)
	}
	second, err := Summarize(dir, 10, time.Now())
	if err != nil || second.Pending != 1 {
		t.Fatalf("machine retirement repeated: %+v err=%v", second, err)
	}
	for _, id := range []string{strings.Repeat("a", 32) + "-failure", strings.Repeat("b", 32) + "-recovery"} {
		item, err := Inspect(dir, id)
		if err != nil || !item.Acknowledged {
			t.Fatalf("operational event %s was not retained and retired: %+v err=%v", id, item, err)
		}
	}
}

func TestRestartReplaysCompletedInflightExactlyOnce(t *testing.T) {
	dir, latest := filepath.Join(t.TempDir(), "queue"), filepath.Join(t.TempDir(), "latest.md")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start("mail", latest, time.Unix(10, 0)); err != nil {
		t.Fatal(err)
	}
	var pending inflight
	if err := readJSON(filepath.Join(dir, "inflight.json"), &pending); err != nil {
		t.Fatal(err)
	}
	pending.Outcome = &Outcome{Source: "mail", Status: "delta", Added: 1}
	pending.Report = []byte("Exact report committed before enqueue.\n")
	if err := writeJSON(filepath.Join(dir, "inflight.json"), pending); err != nil {
		t.Fatal(err)
	}
	s.Close()
	summary, err := Summarize(dir, 10, time.Now())
	if err != nil || summary.Pending != 1 {
		t.Fatalf("restart=%+v err=%v", summary, err)
	}
	// A crash after immutable event publication but before marker removal replays the same ID.
	if err := writeJSON(filepath.Join(dir, "inflight.json"), pending); err != nil {
		t.Fatal(err)
	}
	again, err := Summarize(dir, 10, time.Now())
	if err != nil || again.Pending != 1 || again.Items[0].ID != summary.Items[0].ID {
		t.Fatalf("replayed duplicate=%+v err=%v", again, err)
	}
}

func TestUnpublishedBundlesDoNotCreateEventsOrAcknowledgeAnything(t *testing.T) {
	dir, latest := filepath.Join(t.TempDir(), "queue"), filepath.Join(t.TempDir(), "latest.md")
	collect(t, dir, "mail", latest, "new message", Outcome{Added: 1}, time.Unix(10, 0))
	summary, err := Summarize(dir, 10, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(dir, "events", ".tmp-crashed", "item.json"), "not complete JSON")
	data, _ := json.Marshal(acknowledgment{IDs: []string{summary.Items[0].ID}})
	put(t, filepath.Join(dir, "acks", ".tmp-crashed", "ack.json"), string(data))
	again, err := Summarize(dir, 10, time.Now())
	if err != nil || again.Pending != 1 {
		t.Fatalf("unpublished bundle became visible=%+v err=%v", again, err)
	}
}

func TestConcurrentWriterFailsFastAndClosedLockCanBeReopened(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "queue")
	first, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := Open(dir); err == nil {
		second.Close()
		t.Fatal("allowed concurrent collector")
	}
	first.Close()
	third, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	third.Close()
}
