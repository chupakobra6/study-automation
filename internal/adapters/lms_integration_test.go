package adapters

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chupakobra6/study-automation/internal/config"
	"github.com/chupakobra6/study-automation/internal/queue"
)

// Explicit integration with the adjacent owner, using its real CLI and browser.
// All source content, profiles, dumps and queue state belong to this test.
func TestLocalLMSHarvestStudyQueue(t *testing.T) {
	harvester := os.Getenv("HSE_LMS_HARVEST")
	if harvester == "" {
		t.Skip("set HSE_LMS_HARVEST to run the local browser integration")
	}
	studyRoot, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	var phase, revision atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := phase.Load()
		var body string
		switch r.URL.Path {
		case "/course/view.php":
			links := []string{"a", "b"}
			if current > 0 {
				links = []string{"a", "c", "b"}
			}
			if current == 4 {
				links = []string{"a", "c"}
			}
			body = "<h1>Учебный курс</h1><p>Основные блоки контента</p>"
			for _, id := range links {
				body += fmt.Sprintf(`<a href="/mod/page/view.php?id=%s">Работа %s</a>`, id, id)
			}
		case "/mod/page/view.php":
			id := r.URL.Query().Get("id")
			if id == "b" && current == 2 {
				http.Error(w, "temporarily unavailable", 503)
				return
			}
			text := "Задание " + id
			if id == "a" && current > 0 {
				text += fmt.Sprintf(" изменено, версия %d", revision.Load())
			}
			body = fmt.Sprintf("<h1>Работа %s</h1><p>%s</p><p>Срок сдачи</p><p>20 сентября</p><p>Состояние ответа на задание</p><p>Ответ не добавлен</p>", id, text)
		default:
			http.NotFound(w, r)
			return
		}
		body = "<!doctype html><meta charset=utf-8><title>Мои курсы</title>" + body
		etag := fmt.Sprintf(`"%x"`, sha256.Sum256([]byte(body)))
		w.Header().Set("ETag", etag)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if r.Method != http.MethodHead {
			fmt.Fprint(w, body)
		}
	}))
	defer server.Close()
	root := t.TempDir()
	configPath := filepath.Join(root, "study.toml")
	state := filepath.Join(root, "updates")
	cfg := config.Config{ConfigPath: configPath, Paths: config.Paths{StudyData: filepath.Join(studyRoot, "tools/study-data")}, Updates: config.Updates{StateDir: state}, SmartLMS: config.SmartLMS{Courses: []config.SmartLMSCourse{{Slug: "course"}}}}
	setLimit := func(limit int) {
		body := fmt.Sprintf(`[paths]
state_dir = %q
hse_lms_harvest = %q
[updates]
state_dir = %q
[smart_lms]
profile = %q
dumps = %q
auto_login = false
courses = [{slug="course", title="Учебный курс", url=%q, max_pages=%d}]
`, root, harvester, state, filepath.Join(root, "profile"), filepath.Join(root, "dumps"), server.URL+"/course/view.php?id=1", limit)
		if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	queueDir := filepath.Join(root, "queue")
	reportPath := filepath.Join(state, "smart_lms-latest.md")
	run := func(want string, analysis bool) map[string]any {
		t.Helper()
		store, err := queue.Open(queueDir)
		if err != nil {
			t.Fatal(err)
		}
		if err = store.Start("smart_lms", reportPath, time.Now()); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		result, runErr := SyncSmartLMS(ctx, cfg, OSRunner{}, false)
		if result.Status != want || result.NeedsAnalysis != analysis || (runErr != nil) != (want == "incomplete") {
			store.Close()
			t.Fatalf("result=%+v err=%v\n%s", result, runErr, result.Output)
		}
		err = store.Complete(queue.Outcome{Source: "smart_lms", Status: result.Status, Detail: result.Detail, NeedsAnalysis: result.NeedsAnalysis, AnalysisDecisionKnown: result.AnalysisDecisionKnown, RequiredEvidence: result.RequiredEvidence, Attachments: result.Attachments, Failed: runErr != nil})
		store.Close()
		if err != nil {
			t.Fatal(err)
		}
		body, err := os.ReadFile(filepath.Join(state, "smart_lms-latest.json"))
		if err != nil {
			t.Fatal(err)
		}
		var report map[string]any
		if err := json.Unmarshal(body, &report); err != nil {
			t.Fatal(err)
		}
		t.Logf("phase=%d status=%s coverage=%v analysis=%t", phase.Load(), result.Status, report["coverage_status"], result.NeedsAnalysis)
		return report
	}
	ack := func() queue.Summary {
		t.Helper()
		summary, err := queue.Summarize(queueDir, 100, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if summary.Pending == 0 {
			return summary
		}
		ids, evidence := []string{}, []string{}
		decisions := []map[string]string{}
		for _, item := range summary.Items {
			ids = append(ids, item.ID)
			evidence = append(evidence, item.RequiredEvidence...)
			decisions = append(decisions, map[string]string{"kind": "event", "key": item.ID, "outcome": "updated"})
		}
		receiptPath := filepath.Join(root, fmt.Sprintf("review-%d.json", time.Now().UnixNano()))
		receipt, err := json.Marshal(map[string]any{"decisions": decisions, "evidence": []map[string]any{{"paths": evidence}}})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(receiptPath, receipt, 0o600); err != nil {
			t.Fatal(err)
		}
		summary, err = queue.RecordReview(queueDir, ids, nil, receiptPath, evidence, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return summary
	}
	setLimit(10)
	run("baseline", true)
	ack()
	phase.Store(1)
	setLimit(3)
	report := run("incomplete", true)
	changes := report["changes"].(map[string]any)
	if changes["total_added_pages"].(float64) < 1 || changes["total_removed_pages"].(float64) != 0 {
		t.Fatalf("partial=%v", changes)
	}
	if summary := ack(); summary.Pending != 0 || len(summary.Failures) != 1 {
		t.Fatalf("partial ack erased remaining work: %+v", summary)
	}
	phase.Store(2)
	run("incomplete", false)
	ack()
	run("incomplete", false)
	if summary, err := queue.Summarize(queueDir, 100, time.Now()); err != nil || summary.Pending != 0 || len(summary.Failures) != 1 {
		t.Fatalf("repeat duplicated changes or lost gap: %+v err=%v", summary, err)
	}
	revision.Store(1)
	run("incomplete", true)
	if summary := ack(); len(summary.Failures) != 1 {
		t.Fatalf("new delta erased collection gap: %+v", summary)
	}
	phase.Store(3)
	run("no-changes", false)
	if summary := ack(); len(summary.Failures) != 0 {
		t.Fatalf("complete did not clear gap: %+v", summary)
	}
	phase.Store(4)
	report = run("delta", true)
	if report["changes"].(map[string]any)["total_removed_pages"].(float64) != 1 {
		t.Fatalf("real removal missing: %v", report["changes"])
	}
	ack()
	run("no-changes", false)
}
