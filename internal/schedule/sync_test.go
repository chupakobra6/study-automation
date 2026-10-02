package schedule

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chupakobra6/study-automation/internal/config"
	"github.com/chupakobra6/study-automation/internal/monitor"
)

type fixture struct {
	items         []File
	bodies        map[string][]byte
	fail          map[string]bool
	downloads     []string
	metadataCalls int
	client        Client
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{bodies: map[string][]byte{}, fail: map[string]bool{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "" {
			t.Errorf("unexpected write or authorization: %s", r.Method)
		}
		key := r.URL.Query().Get("path")
		switch r.URL.Path {
		case "/resources":
			f.metadataCalls++
			offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
			end := min(offset+100, len(f.items))
			_ = json.NewEncoder(w).Encode(map[string]any{"_embedded": map[string]any{"items": f.items[offset:end]}})
		case "/resources/download":
			_ = json.NewEncoder(w).Encode(map[string]string{"href": "http://" + r.Host + "/file?" + url.Values{"path": {key}}.Encode()})
		case "/file":
			f.downloads = append(f.downloads, key)
			if f.fail[key] {
				http.Error(w, "offline", http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write(f.bodies[key])
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	f.client = Client{HTTP: server.Client(), BaseURL: server.URL + "/resources"}
	return f
}

func (f *fixture) file(name, body string) File {
	data := []byte(body)
	digest := sha256.Sum256(data)
	key := "/program/" + name
	f.bodies[key] = data
	return File{Name: name, Path: key, Type: "file", Size: int64(len(data)), SHA256: hex.EncodeToString(digest[:]), Modified: "2026-09-04T12:00:00Z"}
}

func testConfig(t *testing.T) config.Schedule {
	t.Helper()
	dir := t.TempDir()
	return config.Schedule{
		PublicKey: "https://disk.example/schedule", StateDir: filepath.Join(dir, "schedule"),
		Checkpoint: filepath.Join(dir, "checkpoints", "schedule.json"), LatestReport: filepath.Join(dir, "updates", "schedule-latest.md"),
		Folders: []config.ScheduleFolder{{Path: "/program", Include: []string{`^Расписание занятий 2 курс .*\.xls[x]?$`}}},
	}
}

func TestSyncTracksScopedRawFilesAndSkipsUnchangedDownloads(t *testing.T) {
	cfg, f := testConfig(t), newFixture(t)
	current := f.file("Расписание занятий 2 курс 03.09.xls", "binary Excel fixture\x00first")
	otherYear := f.file("Расписание занятий 1 курс 03.09.xls", "first-year data")
	f.items = []File{otherYear, current}
	first, err := Sync(context.Background(), cfg, f.client, time.Now())
	if err != nil || first.Status != "baseline" || first.Added != 1 || first.Attachments != 1 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	originalPath := filepath.Join(cfg.StateDir, "raw", current.SHA256, current.Name)
	raw, err := os.ReadFile(originalPath)
	if err != nil || string(raw) != string(f.bodies[current.Path]) {
		t.Fatalf("raw source altered: %q %v", raw, err)
	}
	second, err := Sync(context.Background(), cfg, f.client, time.Now())
	if err != nil || second.NeedsAnalysis || second.Status != "no-changes" || len(f.downloads) != 1 {
		t.Fatalf("second=%+v downloads=%v err=%v", second, f.downloads, err)
	}
	current = f.file(current.Name, "binary Excel fixture\x00updated")
	f.items[1] = current
	changed, err := Sync(context.Background(), cfg, f.client, time.Now())
	if err != nil || !changed.NeedsAnalysis || changed.Status != "delta" || changed.Added != 0 || changed.Attachments != 1 {
		t.Fatalf("changed=%+v err=%v", changed, err)
	}
	next := f.file("Расписание занятий 2 курс 04.09.xlsx", "new workbook")
	f.items = append(f.items, next)
	added, err := Sync(context.Background(), cfg, f.client, time.Now())
	if err != nil || added.Added != 1 {
		t.Fatalf("added=%+v err=%v", added, err)
	}
	f.items = []File{otherYear, next}
	removed, err := Sync(context.Background(), cfg, f.client, time.Now())
	if err != nil || !removed.NeedsAnalysis || removed.Attachments != 0 {
		t.Fatalf("removed=%+v err=%v", removed, err)
	}
	if _, err := os.Stat(originalPath); err != nil {
		t.Fatal("historical raw file was removed")
	}
	for _, downloaded := range f.downloads {
		if downloaded == otherYear.Path {
			t.Fatal("downloaded first-year workbook")
		}
	}
	if err := os.Remove(filepath.Join(cfg.StateDir, "raw", next.SHA256, next.Name)); err != nil {
		t.Fatal(err)
	}
	recovered, err := Sync(context.Background(), cfg, f.client, time.Now())
	if err != nil || recovered.Attachments != 1 {
		t.Fatalf("missing raw recovery=%+v err=%v", recovered, err)
	}
}

func TestSyncRepairsSameSizeCachedCorruption(t *testing.T) {
	cfg, f := testConfig(t), newFixture(t)
	item := f.file("Расписание занятий 2 курс 03.09.xls", "original binary workbook")
	f.items = []File{item}
	if _, err := Sync(context.Background(), cfg, f.client, time.Now()); err != nil {
		t.Fatal(err)
	}
	rawPath := filepath.Join(cfg.StateDir, "raw", item.SHA256, item.Name)
	if err := os.WriteFile(rawPath, []byte(strings.Repeat("x", int(item.Size))), 0o600); err != nil {
		t.Fatal(err)
	}
	repaired, err := Sync(context.Background(), cfg, f.client, time.Now())
	if err != nil || repaired.Attachments != 1 || !repaired.NeedsAnalysis || len(f.downloads) != 2 {
		t.Fatalf("repaired=%+v downloads=%v err=%v", repaired, f.downloads, err)
	}
	raw, err := os.ReadFile(rawPath)
	if err != nil || string(raw) != string(f.bodies[item.Path]) {
		t.Fatalf("raw source was not restored: %q err=%v", raw, err)
	}
	unchanged, err := Sync(context.Background(), cfg, f.client, time.Now())
	if err != nil || unchanged.Status != "no-changes" || len(f.downloads) != 2 {
		t.Fatalf("unchanged=%+v downloads=%v err=%v", unchanged, f.downloads, err)
	}
}

func TestSyncFailurePreservesCheckpointAndOtherSourcesContinue(t *testing.T) {
	cfg, f := testConfig(t), newFixture(t)
	old := f.file("Расписание занятий 2 курс 03.09.xls", "first")
	f.items = []File{old}
	if _, err := Sync(context.Background(), cfg, f.client, time.Now()); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(cfg.Checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	changed := f.file(old.Name, "updated")
	newFile := f.file("Расписание занятий 2 курс 04.09.xls", "second")
	f.items = []File{changed, newFile}
	f.fail[newFile.Path] = true
	continued := false
	_, err = monitor.Run(context.Background(), []monitor.Source{
		{Name: "schedule", ReportPath: cfg.LatestReport, Sync: func(ctx context.Context) (monitor.Result, error) { return Sync(ctx, cfg, f.client, time.Now()) }},
		{Name: "healthy", ReportPath: filepath.Join(t.TempDir(), "healthy.md"), Sync: func(context.Context) (monitor.Result, error) {
			continued = true
			return monitor.Result{Status: "no-changes"}, nil
		}},
	}, filepath.Join(t.TempDir(), "combined.md"), filepath.Join(t.TempDir(), "queue"), time.Now())
	if !monitor.IsRunError(err) || !continued {
		t.Fatalf("err=%v continued=%t", err, continued)
	}
	after, err := os.ReadFile(cfg.Checkpoint)
	if err != nil || string(before) != string(after) {
		t.Fatal("checkpoint advanced after partial download failure")
	}
	f.fail[newFile.Path] = false
	retry, err := Sync(context.Background(), cfg, f.client, time.Now())
	if err != nil || retry.Attachments != 2 || retry.Added != 1 {
		t.Fatalf("retry=%+v err=%v", retry, err)
	}
}

func TestSyncRejectsBadHashAndReportWriteFailureWithoutCheckpoint(t *testing.T) {
	for _, kind := range []string{"hash", "report"} {
		t.Run(kind, func(t *testing.T) {
			cfg, f := testConfig(t), newFixture(t)
			item := f.file("Расписание занятий 2 курс 03.09.xls", "real content")
			f.items = []File{item}
			if kind == "hash" {
				f.bodies[item.Path] = []byte("wrong content")
			} else {
				if err := os.MkdirAll(cfg.LatestReport, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			result, err := Sync(context.Background(), cfg, f.client, time.Now())
			if err == nil || result.Status != "failed" {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if _, err := os.Stat(cfg.Checkpoint); !os.IsNotExist(err) {
				t.Fatalf("checkpoint exists: %v", err)
			}
		})
	}
}

func TestSyncFindsCurrentFileBeyondFirstMetadataPage(t *testing.T) {
	cfg, f := testConfig(t), newFixture(t)
	for i := 0; i < 100; i++ {
		f.items = append(f.items, File{Name: "other course", Type: "file"})
	}
	f.items = append(f.items, f.file("Расписание занятий 2 курс 03.09.xls", "current workbook"))
	result, err := Sync(context.Background(), cfg, f.client, time.Now())
	if err != nil || result.Added != 1 || f.metadataCalls != 2 {
		t.Fatalf("result=%+v metadata=%d err=%v", result, f.metadataCalls, err)
	}
}

func TestSyncDoesNotTreatEmptySelectionAsNoChanges(t *testing.T) {
	cfg, f := testConfig(t), newFixture(t)
	f.items = []File{f.file("Расписание занятий 1 курс 03.09.xls", "unrelated")}
	result, err := Sync(context.Background(), cfg, f.client, time.Now())
	if err == nil || result.Status != "failed" || !strings.Contains(err.Error(), "no files matching") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
