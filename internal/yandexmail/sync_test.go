package yandexmail

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chupakobra6/study-automation/internal/config"
)

type fakeClient struct {
	messages   map[uint32][]byte
	searches   []string
	fetched    []uint32
	fetchErr   map[uint32]error
	searchOmit map[uint32]bool
	validity   uint32
}

func (f *fakeClient) UIDValidity() uint32 {
	if f.validity == 0 {
		return 1
	}
	return f.validity
}

func (f *fakeClient) Search(folder string, _ time.Time) ([]uint32, error) {
	f.searches = append(f.searches, folder)
	uids := make([]uint32, 0, len(f.messages))
	for uid := range f.messages {
		if !f.searchOmit[uid] {
			uids = append(uids, uid)
		}
	}
	return uids, nil
}

func (f *fakeClient) Fetch(uid uint32) ([]byte, error) {
	f.fetched = append(f.fetched, uid)
	if err := f.fetchErr[uid]; err != nil {
		return nil, err
	}
	return f.messages[uid], nil
}

func (*fakeClient) Close() error { return nil }

func TestSyncUsesCheckpointAndDownloadsAttachment(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir)
	client := &fakeClient{messages: map[uint32][]byte{10: testMessage("First", false)}, fetchErr: map[uint32]error{}}

	first, err := Sync(context.Background(), cfg, client, time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != "baseline" || first.Added != 1 {
		t.Fatalf("unexpected first report: %+v", first)
	}
	client.messages[11] = testMessage("Second", true)
	client.fetched = nil
	second, err := Sync(context.Background(), cfg, client, time.Date(2026, 9, 6, 13, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if second.Status != "delta" || second.Added != 1 || len(second.Records) != 1 {
		t.Fatalf("unexpected second report: %+v", second)
	}
	if len(client.fetched) != 1 || client.fetched[0] != 11 {
		t.Fatalf("expected only UID 11, got %v", client.fetched)
	}
	attachment := second.Records[0].Attachments[0]
	if attachment.FileName != "task.pdf" || attachment.MIMEType != "application/pdf" {
		t.Fatalf("unexpected attachment: %+v", attachment)
	}
	if data, err := os.ReadFile(attachment.LocalPath); err != nil || !strings.HasPrefix(string(data), "%PDF") {
		t.Fatalf("attachment not written correctly: %q, %v", data, err)
	}
	third, err := Sync(context.Background(), cfg, client, time.Date(2026, 9, 6, 14, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if third.Status != "no-changes" || third.Added != 0 {
		t.Fatalf("unexpected third report: %+v", third)
	}
}

func TestSyncIsolatesFailuresAndRetriesBelowCursor(t *testing.T) {
	for _, stage := range []string{"fetch", "parse"} {
		t.Run(stage, func(t *testing.T) {
			cfg := testConfig(t.TempDir())
			client := &fakeClient{messages: map[uint32][]byte{10: testMessage("Broken", true), 11: testMessage("Good", false)}, fetchErr: map[uint32]error{}}
			if stage == "fetch" {
				client.fetchErr[10] = errors.New("network stopped")
			} else {
				client.messages[10] = []byte(strings.ReplaceAll(string(client.messages[10]), "JVBERi0xLjQK", "%invalid-base64%"))
			}
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
			if err != nil || cp.FolderUIDs["INBOX"] != 11 || len(cp.ItemFailures) != 1 || cp.ItemFailures[itemKey("INBOX", 10)].UID != 10 {
				t.Fatalf("cursor must retain a recoverable failed UID: %+v err=%v", cp, err)
			}
			// Recovery must use the persisted failure even if SEARCH no longer lists it.
			client.searchOmit = map[uint32]bool{10: true}
			client.messages[12] = testMessage("New", false)
			run(now.Add(time.Minute), 1, 1, 0)
			if len(client.fetched) != 3 || client.fetched[2] != 12 {
				t.Fatalf("bad UID blocked a later message or retried too early: %v", client.fetched)
			}
			run(now.Add(15*time.Minute), 0, 1, 0)
			run(now.Add(74*time.Minute), 0, 1, 0)
			if len(client.fetched) != 4 {
				t.Fatalf("second backoff not respected: %v", client.fetched)
			}
			terminal := run(now.Add(75*time.Minute), 0, 0, 1)
			if !terminal.ItemFailures[0].Terminal || terminal.ItemFailures[0].Attempts != 3 {
				t.Fatalf("failure=%+v", terminal.ItemFailures)
			}
			run(now.Add(30*24*time.Hour), 0, 0, 1)
			if len(client.fetched) != 5 {
				t.Fatalf("terminal item retried automatically: %v", client.fetched)
			}
			client.messages[10] = testMessage("Repaired", true)
			delete(client.fetchErr, 10)
			if err := ResetItemFailure(cfg.Checkpoint, "INBOX", 10); err != nil {
				t.Fatal(err)
			}
			recovered := run(now.Add(31*24*time.Hour), 1, 0, 0)
			if len(recovered.Records[0].Attachments) != 1 {
				t.Fatal("recovered attachment missing")
			}
			run(now.Add(32*24*time.Hour), 0, 0, 0)
			records, err := readRecords(cfg.MessagesJSONL)
			if err != nil || len(records) != 3 || len(client.fetched) != 6 || client.fetched[5] != 10 {
				t.Fatalf("duplicate/lost records: records=%d fetched=%v err=%v", len(records), client.fetched, err)
			}
		})
	}
}

func TestSyncDrainsBacklogWithoutSkippingOlderUIDs(t *testing.T) {
	cfg := testConfig(t.TempDir())
	cfg.MaxMessagesPerSync = 2
	client := &fakeClient{messages: map[uint32][]byte{
		10: testMessage("First", false), 11: testMessage("Second", false),
		12: testMessage("Third", false), 13: testMessage("Fourth", false),
		14: testMessage("Fifth", false),
	}}
	for run, wantAdded := range []int{2, 2, 1, 0} {
		report, err := Sync(context.Background(), cfg, client, time.Now())
		if err != nil || report.Added != wantAdded {
			t.Fatalf("run %d: report=%+v err=%v", run, report, err)
		}
		if report.Incomplete != (run < 2) || (report.BacklogRemaining > 0) != (run < 2) {
			t.Fatalf("hidden backlog on run %d: %+v", run, report)
		}
	}
	if len(client.fetched) != 5 {
		t.Fatalf("fetched=%v, want all five UIDs exactly once", client.fetched)
	}
	for index, uid := range client.fetched {
		if uid != uint32(10+index) {
			t.Fatalf("fetched=%v, want oldest-first contiguous batches", client.fetched)
		}
	}
}

func TestSyncBackoffDoesNotConsumeUIDBatch(t *testing.T) {
	cfg := testConfig(t.TempDir())
	cfg.MaxMessagesPerSync = 1
	client := &fakeClient{messages: map[uint32][]byte{
		10: testMessage("Broken", false), 11: testMessage("Good", false), 12: testMessage("New", false),
	}, fetchErr: map[uint32]error{10: errors.New("unavailable")}}
	now := time.Now()
	for run, wantAdded := range []int{0, 1, 1, 0} {
		report, err := Sync(context.Background(), cfg, client, now.Add(time.Duration(run)*time.Minute))
		if err != nil || report.Added != wantAdded || report.PendingItems != 1 {
			t.Fatalf("run=%d report=%+v err=%v", run, report, err)
		}
	}
	if len(client.fetched) != 3 || client.fetched[0] != 10 || client.fetched[1] != 11 || client.fetched[2] != 12 {
		t.Fatalf("failed UID blocked following messages or retried too early: %v", client.fetched)
	}
}

func TestMailboxUIDResetCannotSilentlyReuseOldCursor(t *testing.T) {
	cfg := testConfig(t.TempDir())
	client := &fakeClient{messages: map[uint32][]byte{10: testMessage("old", false)}, validity: 5}
	if _, err := Sync(context.Background(), cfg, client, time.Now()); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(cfg.Checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	client.validity = 6
	client.messages = map[uint32][]byte{1: testMessage("new after UID reset", false)}
	if _, err := Sync(context.Background(), cfg, client, time.Now()); err == nil || !strings.Contains(err.Error(), "UIDVALIDITY changed") {
		t.Fatalf("reset err=%v", err)
	}
	after, err := os.ReadFile(cfg.Checkpoint)
	if err != nil || string(before) != string(after) {
		t.Fatalf("cursor overwritten after reset: %v", err)
	}
}

func TestParsePreservesAttachmentsWithEqualFilenames(t *testing.T) {
	raw := string(testMessage("Two attachments", true))
	raw = strings.Replace(raw, "--BOUNDARY--", "--BOUNDARY\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=task.pdf\r\n\r\nsecond attachment\r\n--BOUNDARY--", 1)
	record, err := parseRecord(testConfig(t.TempDir()), "INBOX", 10, []byte(raw))
	if err != nil || len(record.Attachments) != 2 {
		t.Fatalf("attachments=%+v err=%v", record.Attachments, err)
	}
	first, second := record.Attachments[0], record.Attachments[1]
	firstData, firstErr := os.ReadFile(first.LocalPath)
	secondData, secondErr := os.ReadFile(second.LocalPath)
	if firstErr != nil || secondErr != nil || first.LocalPath == second.LocalPath || string(firstData) == string(secondData) {
		t.Fatalf("equal filenames overwrote attachment content: %+v %+v", first, second)
	}
}

func testConfig(dir string) config.YandexMail {
	return config.YandexMail{
		Account: "student@example.edu", Host: "imap.yandex.ru", Port: 993,
		Folders: []string{"INBOX"}, Since: "2026-07-01", StateDir: filepath.Join(dir, "mail"),
		MessagesJSONL:      filepath.Join(dir, "mail", "messages.jsonl"),
		AttachmentsDir:     filepath.Join(dir, "mail", "attachments"),
		Checkpoint:         filepath.Join(dir, "updates", "yandex_mail.json"),
		LatestReport:       filepath.Join(dir, "updates", "yandex_mail-latest.md"),
		MaxMessagesPerSync: 1000,
	}
}

func testMessage(subject string, attachment bool) []byte {
	if !attachment {
		return []byte("From: Teacher <teacher@example.edu>\r\nTo: student@example.edu\r\nDate: Fri, 4 Sep 2026 07:28:00 +0300\r\nSubject: " + subject + "\r\nMessage-ID: <one@example.edu>\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nУчебное сообщение и дедлайн.\r\n")
	}
	return []byte("From: Teacher <teacher@example.edu>\r\nTo: student@example.edu\r\nDate: Fri, 4 Sep 2026 07:28:00 +0300\r\nSubject: " + subject + "\r\nMessage-ID: <two@example.edu>\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=BOUNDARY\r\n\r\n--BOUNDARY\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nМатериалы.\r\n--BOUNDARY\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=task.pdf\r\nContent-Transfer-Encoding: base64\r\n\r\nJVBERi0xLjQK\r\n--BOUNDARY--\r\n")
}
