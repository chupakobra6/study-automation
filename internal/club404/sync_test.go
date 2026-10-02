package club404

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fixtureRunner struct{ scan export }

func (r fixtureRunner) Run(_ context.Context, _, _ string, args ...string) (string, string, error) {
	for i := range args {
		if args[i] == "--out" && i+1 < len(args) {
			data, _ := json.Marshal(r.scan)
			return "", "", os.WriteFile(args[i+1], data, 0o600)
		}
	}
	return "", "", os.ErrInvalid
}

func TestSecondarySourceBaselineAndChangedDeadline(t *testing.T) {
	dir := t.TempDir()
	harvest := filepath.Join(dir, "harvest")
	binary := filepath.Join(harvest, ".venv", "bin", "hse-lms-harvest")
	if err := os.MkdirAll(filepath.Dir(binary), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Subjects: []string{"net"}, Profile: filepath.Join(dir, "profile"), StateDir: filepath.Join(dir, "state"), Checkpoint: filepath.Join(dir, "state", "checkpoint.json"), LatestReport: filepath.Join(dir, "report.md")}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	scan := export{Version: 1, BaseURL: "https://club404.ru", Subjects: []string{"net"}}
	scan.Counts.Subjects, scan.Counts.Tasks = 1, 1
	scan.Items = []item{{Kind: "task", URL: "https://club404.ru/course/2/tasks/985", Title: "Сети", Subject: "net", Deadline: "1 окт 2026, 23:59", Body: "Точного дедлайна пока что нет, стоит произвольная дата", Links: []link{{Label: "ЛМС", URL: "https://netology.ru/task"}}}}
	now := time.Date(2026, 9, 30, 7, 0, 0, 0, time.UTC)
	baseline, err := Sync(context.Background(), cfg, harvest, fixtureRunner{scan}, now)
	if err != nil || baseline.Status != "baseline" || baseline.NeedsAnalysis {
		t.Fatalf("baseline = %+v, %v", baseline, err)
	}
	unchanged, err := Sync(context.Background(), cfg, harvest, fixtureRunner{scan}, now.Add(time.Hour))
	if err != nil || unchanged.Status != "no-changes" || unchanged.NeedsAnalysis {
		t.Fatalf("unchanged = %+v, %v", unchanged, err)
	}
	scan.Items[0].Body += "\nНовое пояснение"
	changed, err := Sync(context.Background(), cfg, harvest, fixtureRunner{scan}, now.Add(2*time.Hour))
	if err != nil || changed.Status != "delta" || !changed.NeedsAnalysis || len(changed.RequiredEvidence) != 1 {
		t.Fatalf("changed = %+v, %v", changed, err)
	}
	evidence, err := os.ReadFile(changed.RequiredEvidence[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"secondary club404", "Displayed deadline: 1 окт 2026, 23:59", "arbitrary", "https://netology.ru/task", "Новое пояснение"} {
		if !strings.Contains(string(evidence), want) {
			t.Fatalf("evidence omitted %q", want)
		}
	}
	report, err := os.ReadFile(cfg.LatestReport)
	if err != nil || !strings.Contains(string(report), "verify in primary") && !strings.Contains(string(report), "require confirmation") {
		t.Fatalf("secondary-source report missing provenance: %s, %v", report, err)
	}
	scan.Counts.Tasks, scan.Counts.Materials = 0, 1
	scan.Items = []item{{Kind: "material", URL: "https://club404.ru/course/2/materials/42", Title: "Лекция", Subject: "net", Body: "Текст лекции"}}
	removed, err := Sync(context.Background(), cfg, harvest, fixtureRunner{scan}, now.Add(3*time.Hour))
	if err != nil || removed.Status != "delta" || len(removed.RequiredEvidence) != 2 {
		t.Fatalf("removed item must retain previous content: %+v, %v", removed, err)
	}
	foundPrevious := false
	for _, path := range removed.RequiredEvidence {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		foundPrevious = foundPrevious || strings.Contains(string(data), "Removed from the current club404 scan") && strings.Contains(string(data), "произвольная дата")
	}
	if !foundPrevious {
		t.Fatal("removed item previous content was lost")
	}
}

func TestRejectsIncompleteScanWithoutAdvancingCheckpoint(t *testing.T) {
	dir := t.TempDir()
	harvest := filepath.Join(dir, "harvest")
	binary := filepath.Join(harvest, ".venv", "bin", "hse-lms-harvest")
	if err := os.MkdirAll(filepath.Dir(binary), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Subjects: []string{"net"}, Profile: filepath.Join(dir, "profile"), StateDir: filepath.Join(dir, "state"), Checkpoint: filepath.Join(dir, "state", "checkpoint.json"), LatestReport: filepath.Join(dir, "report.md")}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	scan := export{Version: 1, BaseURL: "https://club404.ru", Subjects: []string{"net"}}
	scan.Counts.Subjects, scan.Counts.Tasks = 1, 1
	_, err := Sync(context.Background(), cfg, harvest, fixtureRunner{scan}, time.Now())
	if err == nil {
		t.Fatal("incomplete scan was accepted")
	}
	if _, err := os.Stat(cfg.Checkpoint); !os.IsNotExist(err) {
		t.Fatalf("checkpoint advanced after incomplete scan: %v", err)
	}
}

func TestCountdownDoesNotChangeItemDigest(t *testing.T) {
	old := item{Body: "Дедлайн 30 сен 2026, 23:5922 час.", Links: []link{{Label: "Дедлайн 30 сен 2026, 23:5922 час.", URL: "https://club404.ru/course/2/tasks/1024"}}}
	new := item{Body: "Дедлайн 30 сен 2026, 23:5921 час.", Links: []link{{Label: "Дедлайн 30 сен 2026, 23:5921 час.", URL: "https://club404.ru/course/2/tasks/1024"}}}
	if digest(old) != digest(new) {
		t.Fatal("live countdown created a content change")
	}
	new.Body += " Новое условие"
	if digest(old) == digest(new) {
		t.Fatal("real content change was hidden")
	}
}
