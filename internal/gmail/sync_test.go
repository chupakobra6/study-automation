package gmail

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chupakobra6/study-automation/internal/config"
)

func TestSyncDeduplicatesQueriesDownloadsAttachmentAndCheckpoints(t *testing.T) {
	attachment := []byte("%PDF-1.4\nfixture")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer access-token" {
			t.Fatalf("missing bearer token")
		}
		switch {
		case request.URL.Path == "/gmail/v1/users/me/messages":
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"messages": []map[string]string{{"id": "m2", "threadId": "t2"}, {"id": "m1", "threadId": "t1"}},
			})
		case request.URL.Path == "/gmail/v1/users/me/messages/m1":
			_ = json.NewEncoder(writer).Encode(testMessage("m1", "t1", "1000", "First", "body one"))
		case request.URL.Path == "/gmail/v1/users/me/messages/m2":
			message := testMessage("m2", "t2", "2000", "Second", "")
			message["payload"] = map[string]any{
				"mimeType": "multipart/mixed",
				"headers":  message["payload"].(map[string]any)["headers"],
				"parts": []map[string]any{
					{"mimeType": "text/plain", "body": map[string]any{"data": base64.RawURLEncoding.EncodeToString([]byte("body two"))}},
					{"mimeType": "application/pdf", "filename": "task.pdf", "body": map[string]any{"attachmentId": "a1", "size": len(attachment)}},
				},
			}
			_ = json.NewEncoder(writer).Encode(message)
		case request.URL.Path == "/gmail/v1/users/me/messages/m2/attachments/a1":
			_ = json.NewEncoder(writer).Encode(map[string]string{"data": base64.RawURLEncoding.EncodeToString(attachment)})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	cfg := testConfig(t)
	client := &Client{HTTP: server.Client(), APIBase: server.URL + "/gmail/v1", AccessToken: "access-token"}
	first, err := Sync(context.Background(), cfg, client, time.Unix(10, 0))
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != "baseline" || first.Added != 2 {
		t.Fatalf("first report = status %q added %d", first.Status, first.Added)
	}
	if len(first.Records[0].Attachments)+len(first.Records[1].Attachments) != 1 {
		t.Fatalf("attachment missing from records")
	}
	var savedAttachment Attachment
	for _, record := range first.Records {
		if len(record.Attachments) > 0 {
			savedAttachment = record.Attachments[0]
		}
	}
	data, err := os.ReadFile(savedAttachment.LocalPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(attachment) || savedAttachment.FileName != "task.pdf" {
		t.Fatalf("attachment was not preserved")
	}

	second, err := Sync(context.Background(), cfg, client, time.Unix(20, 0))
	if err != nil {
		t.Fatal(err)
	}
	if second.Status != "no-changes" || second.Added != 0 {
		t.Fatalf("second report = status %q added %d", second.Status, second.Added)
	}
	records, err := readRecords(cfg.MessagesJSONL)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("saved records = %d, want 2", len(records))
	}
	checkpointData, err := os.ReadFile(cfg.Checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(checkpointData), `"m1"`) || !strings.Contains(string(checkpointData), `"m2"`) {
		t.Fatalf("checkpoint does not contain both message IDs")
	}
}

func TestSyncIsolatesFailuresAndBoundsRetries(t *testing.T) {
	for _, stage := range []string{"fetch", "parse"} {
		t.Run(stage, func(t *testing.T) {
			broken, listed, fetched := true, []string{"bad", "good"}, map[string]int{}
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/gmail/v1/users/me/messages" {
					refs := make([]MessageRef, 0, len(listed))
					for _, id := range listed {
						refs = append(refs, MessageRef{ID: id})
					}
					_ = json.NewEncoder(writer).Encode(map[string]any{"messages": refs})
					return
				}
				id := strings.TrimPrefix(request.URL.Path, "/gmail/v1/users/me/messages/")
				fetched[id]++
				if id == "bad" && broken && stage == "fetch" {
					http.Error(writer, "private response must not enter checkpoint", http.StatusServiceUnavailable)
					return
				}
				message := testMessage(id, "thread", "1000", id, "body")
				if id == "bad" {
					payload := message["payload"].(map[string]any)
					data := base64.RawURLEncoding.EncodeToString([]byte("attachment"))
					if broken {
						data = "%invalid-base64%"
					}
					payload["parts"] = []map[string]any{
						{"mimeType": "application/pdf", "filename": "task.pdf", "body": map[string]any{"data": base64.RawURLEncoding.EncodeToString([]byte("first attachment"))}},
						{"mimeType": "application/pdf", "filename": "task.pdf", "body": map[string]any{"data": data}},
					}
				}
				_ = json.NewEncoder(writer).Encode(message)
			}))
			defer server.Close()
			cfg := testConfig(t)
			client := &Client{HTTP: server.Client(), APIBase: server.URL + "/gmail/v1"}
			now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
			run := func(at time.Time, wantAdded, wantPending, wantFailed int) Report {
				t.Helper()
				report, err := Sync(context.Background(), cfg, client, at)
				if err != nil || report.Added != wantAdded || report.PendingItems != wantPending || report.FailedItems != wantFailed {
					t.Fatalf("report=%+v err=%v", report, err)
				}
				if report.Incomplete != (wantPending+wantFailed > 0) || (report.Incomplete && report.Status != "incomplete") {
					t.Fatalf("incorrect completeness: %+v", report)
				}
				return report
			}
			first := run(now, 1, 1, 0)
			if first.ItemFailures[0].Stage != stage || first.ItemFailures[0].Attempts != 1 {
				t.Fatalf("failure=%+v", first.ItemFailures)
			}
			cp, _, err := readCheckpoint(cfg.Checkpoint)
			if err != nil || len(cp.SeenIDs) != 1 || cp.SeenIDs[0] != "good" || len(cp.ItemFailures) != 1 {
				t.Fatalf("successful and failed IDs must be persisted separately: %+v %v", cp, err)
			}
			// The failure is still retried even after it leaves the query results.
			listed = []string{"new", "good"}
			run(now.Add(time.Minute), 1, 1, 0)
			if fetched["bad"] != 1 {
				t.Fatal("retried before backoff elapsed")
			}
			run(now.Add(15*time.Minute), 0, 1, 0)
			run(now.Add(74*time.Minute), 0, 1, 0)
			if fetched["bad"] != 2 {
				t.Fatalf("second attempt backoff not respected: %v", fetched)
			}
			terminal := run(now.Add(75*time.Minute), 0, 0, 1)
			if !terminal.ItemFailures[0].Terminal || terminal.ItemFailures[0].Attempts != 3 {
				t.Fatalf("failure=%+v", terminal.ItemFailures)
			}
			run(now.Add(30*24*time.Hour), 0, 0, 1)
			if fetched["bad"] != 3 {
				t.Fatalf("terminal item retried automatically: %v", fetched)
			}
			data, _ := os.ReadFile(cfg.Checkpoint)
			if strings.Contains(string(data), "private response") {
				t.Fatal("raw response leaked into persisted error")
			}
			broken = false
			if err := ResetItemFailure(cfg.Checkpoint, "bad"); err != nil {
				t.Fatal(err)
			}
			recovered := run(now.Add(31*24*time.Hour), 1, 0, 0)
			if len(recovered.Records[0].Attachments) != 2 || recovered.Records[0].Attachments[0].LocalPath == recovered.Records[0].Attachments[1].LocalPath {
				t.Fatal("attachments with equal filenames were not preserved independently")
			}
			run(now.Add(32*24*time.Hour), 0, 0, 0)
			records, err := readRecords(cfg.MessagesJSONL)
			if err != nil || len(records) != 3 || fetched["good"] != 1 || fetched["new"] != 1 || fetched["bad"] != 4 {
				t.Fatalf("duplicate/lost records: records=%d fetched=%v err=%v", len(records), fetched, err)
			}
		})
	}
}

func TestSyncDrainsBacklogPastPreviouslySeenListPages(t *testing.T) {
	ids := []string{"m5", "m4", "m3", "m2", "m1"}
	fetched := make(map[string]int)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/gmail/v1/users/me/messages" {
			start, _ := strconv.Atoi(request.URL.Query().Get("pageToken"))
			limit, _ := strconv.Atoi(request.URL.Query().Get("maxResults"))
			end := min(start+limit, len(ids))
			refs := make([]MessageRef, 0, end-start)
			for _, id := range ids[start:end] {
				refs = append(refs, MessageRef{ID: id})
			}
			next := ""
			if end < len(ids) {
				next = strconv.Itoa(end)
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{"messages": refs, "nextPageToken": next})
			return
		}
		id := strings.TrimPrefix(request.URL.Path, "/gmail/v1/users/me/messages/")
		fetched[id]++
		_ = json.NewEncoder(writer).Encode(testMessage(id, "thread", "1000", id, "Учебное письмо"))
	}))
	defer server.Close()
	cfg := testConfig(t)
	cfg.MaxMessagesPerSync = 2
	client := &Client{HTTP: server.Client(), APIBase: server.URL + "/gmail/v1"}
	for run, wantAdded := range []int{2, 2, 1, 0} {
		report, err := Sync(context.Background(), cfg, client, time.Now())
		if err != nil || report.Added != wantAdded {
			t.Fatalf("run %d: report=%+v err=%v", run, report, err)
		}
		if report.Incomplete != (run < 2) || report.BacklogRemaining != (run < 2) {
			t.Fatalf("hidden backlog on run %d: %+v", run, report)
		}
	}
	for _, id := range ids {
		if fetched[id] != 1 {
			t.Fatalf("message %s fetched %d times, want once", id, fetched[id])
		}
	}
	records, err := readRecords(cfg.MessagesJSONL)
	if err != nil || len(records) != len(ids) {
		t.Fatalf("records=%d err=%v, want all five messages", len(records), err)
	}
}

func TestSyncBackoffDoesNotConsumeListingLimit(t *testing.T) {
	ids, fetched := []string{"bad", "good", "older"}, map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/gmail/v1/users/me/messages" {
			start, _ := strconv.Atoi(request.URL.Query().Get("pageToken"))
			limit, _ := strconv.Atoi(request.URL.Query().Get("maxResults"))
			end := min(start+limit, len(ids))
			refs := make([]MessageRef, 0, end-start)
			for _, id := range ids[start:end] {
				refs = append(refs, MessageRef{ID: id})
			}
			next := ""
			if end < len(ids) {
				next = strconv.Itoa(end)
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{"messages": refs, "nextPageToken": next})
			return
		}
		id := strings.TrimPrefix(request.URL.Path, "/gmail/v1/users/me/messages/")
		fetched[id]++
		if id == "bad" {
			http.Error(writer, "unavailable", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(writer).Encode(testMessage(id, "thread", "1000", id, "body"))
	}))
	defer server.Close()
	cfg := testConfig(t)
	cfg.MaxMessagesPerSync = 1
	client := &Client{HTTP: server.Client(), APIBase: server.URL + "/gmail/v1"}
	now := time.Now()
	for run, wantAdded := range []int{0, 1, 1, 0} {
		report, err := Sync(context.Background(), cfg, client, now.Add(time.Duration(run)*time.Minute))
		if err != nil || report.Added != wantAdded || report.PendingItems != 1 {
			t.Fatalf("run=%d report=%+v err=%v", run, report, err)
		}
	}
	for _, id := range ids {
		if fetched[id] != 1 {
			t.Fatalf("failed ID hid following list pages or retried too early: %v", fetched)
		}
	}
}

func TestListRejectsRepeatedPaginationToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"messages": []MessageRef{{ID: "seen"}}, "nextPageToken": "repeated",
		})
	}))
	defer server.Close()
	client := &Client{HTTP: server.Client(), APIBase: server.URL}
	_, err := client.ListUnseenMessages(context.Background(), "study", 2, map[string]bool{"seen": true})
	if err == nil || !strings.Contains(err.Error(), "repeated page token") {
		t.Fatalf("err=%v, want a bounded pagination failure", err)
	}
}

func testConfig(t *testing.T) config.Gmail {
	t.Helper()
	dir := t.TempDir()
	return config.Gmail{
		Account: "student@example.com", Queries: []string{"from:example.edu", "deadline"}, MaxMessagesPerSync: 100,
		StateDir: filepath.Join(dir, "gmail"), MessagesJSONL: filepath.Join(dir, "gmail", "messages.jsonl"),
		AttachmentsDir: filepath.Join(dir, "gmail", "attachments"), Checkpoint: filepath.Join(dir, "updates", "gmail.json"),
		LatestReport: filepath.Join(dir, "updates", "gmail-latest.md"),
	}
}

func testMessage(id, threadID, internalDate, subject, body string) map[string]any {
	return map[string]any{
		"id": id, "threadId": threadID, "historyId": "history-" + id, "internalDate": internalDate,
		"snippet": "snippet " + id,
		"payload": map[string]any{
			"mimeType": "text/plain",
			"headers": []map[string]string{
				{"name": "Date", "value": "Sun, 6 Sep 2026 10:00:00 +0300"},
				{"name": "From", "value": "Teacher <teacher@example.edu>"},
				{"name": "To", "value": "student@example.com"},
				{"name": "Subject", "value": subject},
			},
			"body": map[string]any{"data": base64.RawURLEncoding.EncodeToString([]byte(body))},
		},
	}
}
