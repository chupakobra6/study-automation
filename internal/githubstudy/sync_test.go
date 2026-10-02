package githubstudy

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fixture struct {
	t       *testing.T
	data    map[string]any
	fail    map[string]error
	headers map[string]http.Header
	calls   map[string]int
}

func newFixture(t *testing.T) *fixture {
	return &fixture{t: t, data: map[string]any{}, fail: map[string]error{}, headers: map[string]http.Header{}, calls: map[string]int{}}
}

func (f *fixture) get(_ context.Context, endpoint string) ([]byte, http.Header, error) {
	f.calls[endpoint]++
	if err := f.fail[endpoint]; err != nil {
		return nil, nil, err
	}
	value, ok := f.data[endpoint]
	if !ok {
		f.t.Errorf("unexpected request %s", endpoint)
		return nil, nil, errors.New("unexpected endpoint")
	}
	data, err := json.Marshal(value)
	return data, f.headers[endpoint], err
}

func testConfig(t *testing.T) Config {
	dir := t.TempDir()
	return Config{Repositories: []Repository{{Name: "course/tasks", Branches: []string{"main"}, BranchPrefixes: []string{"main-task-"}, Include: []string{`(^|/)README\.md$`}}}, Projects: []Project{{Owner: "course", Number: 2}}, StateDir: dir, Checkpoint: filepath.Join(dir, "checkpoint.json"), LatestReport: filepath.Join(dir, "latest.md")}
}

const mainHead = "1111111111111111111111111111111111111111"
const taskHead = "2222222222222222222222222222222222222222"

func (f *fixture) content(head, body string) {
	hash := sha1.Sum(append(fmt.Appendf(nil, "blob %d\x00", len(body)), []byte(body)...))
	sha := hex.EncodeToString(hash[:])
	f.data["repos/course/tasks/git/trees/"+head+"?recursive=1"] = map[string]any{"truncated": false, "tree": []any{map[string]any{"path": "README.md", "type": "blob", "sha": sha, "size": len(body)}, map[string]any{"path": "some-team.md", "type": "blob", "sha": mainHead, "size": 100}}}
	f.data["repos/course/tasks/git/blobs/"+sha] = map[string]any{"encoding": "base64", "sha": sha, "size": len(body), "content": base64.StdEncoding.EncodeToString([]byte(body))}
}

func (f *fixture) seed() {
	f.data["repos/course/tasks/branches?per_page=100"] = []any{map[string]any{"name": "main", "commit": map[string]string{"sha": mainHead}}, map[string]any{"name": "main-task-0", "commit": map[string]string{"sha": taskHead}}, map[string]any{"name": "other-team-task-0", "commit": map[string]string{"sha": mainHead}}}
	f.content(mainHead, "# Course instructions")
	f.content(taskHead, "# Task 0\nImplement an algorithm.")
	f.data["repos/course/tasks/issues?state=all&per_page=100"] = []any{map[string]any{"id": 1, "number": 1, "title": "Assignment", "body": "Solve it", "html_url": "https://github.com/course/tasks/issues/1", "state": "open", "updated_at": "2026-09-01T00:00:00Z"}, map[string]any{"id": 2, "title": "Team submission", "pull_request": map[string]string{"url": "https://api.github.com/x"}}}
	f.data["orgs/course/projectsV2/2"] = map[string]any{"id": 10, "title": "Algorithms 26/27", "state": "open", "updated_at": "2026-09-01T00:00:00Z"}
	f.data["orgs/course/projectsV2/2/fields?per_page=100"] = []any{map[string]any{"id": 20, "name": "Deadline (exclusive)", "data_type": "date"}, map[string]any{"id": 21, "name": "Status", "data_type": "single_select"}}
	f.projectItem("2026-09-12", "Backlog", "Read the assignment.", "2026-09-01T00:00:00Z")
}

const itemsEndpoint = "orgs/course/projectsV2/2/items?fields=20%2C21&per_page=100"

func (f *fixture) projectItem(deadline, status, body, updated string) {
	f.data[itemsEndpoint] = []any{map[string]any{"id": 30, "content_type": "DraftIssue", "content": map[string]any{"title": "Task 0", "body": body}, "updated_at": updated, "fields": []any{map[string]any{"name": "Deadline (exclusive)", "data_type": "date", "value": deadline}, map[string]any{"name": "Status", "data_type": "single_select", "value": map[string]any{"name": map[string]string{"raw": status, "html": status}, "color": "RED"}}}}}
}

func TestSemanticDeltaAndBranchDiscovery(t *testing.T) {
	cfg := testConfig(t)
	f := newFixture(t)
	f.seed()
	client := Client{Get: f.get}
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	first, err := Sync(context.Background(), cfg, client, now)
	if err != nil || !first.NeedsAnalysis || first.Added != 5 || first.Status != "baseline" {
		t.Fatalf("baseline: %+v %v", first, err)
	}
	text, _ := os.ReadFile(cfg.LatestReport)
	if !strings.Contains(string(text), "Deadline (exclusive)=2026-09-12") || !strings.Contains(string(text), "main-task-0") || strings.Contains(string(text), "other-team") {
		t.Fatalf("unexpected report %s", text)
	}
	firstTreeCalls := f.calls["repos/course/tasks/git/trees/"+taskHead+"?recursive=1"]
	f.projectItem("2026-09-12", "Backlog", "Read the assignment.", "2026-09-06T00:00:00Z")
	second, err := Sync(context.Background(), cfg, client, now.Add(time.Hour))
	if err != nil || second.NeedsAnalysis || second.Status != "no-changes" {
		t.Fatalf("timestamps are not academic changes: %+v %v", second, err)
	}
	if f.calls["repos/course/tasks/git/trees/"+taskHead+"?recursive=1"] != firstTreeCalls {
		t.Fatal("unchanged branch fetched its tree again")
	}
	f.projectItem("2026-09-13", "In progress", "Updated assignment body.", "2026-09-06T01:00:00Z")
	third, err := Sync(context.Background(), cfg, client, now.Add(2*time.Hour))
	if err != nil || !third.NeedsAnalysis || third.Added != 0 || third.Status != "delta" {
		t.Fatalf("meaningful changes missing: %+v %v", third, err)
	}
	text, _ = os.ReadFile(cfg.LatestReport)
	if !strings.Contains(string(text), "Deadline (exclusive)=2026-09-13") || !strings.Contains(string(text), "Status=In progress") {
		t.Fatalf("deadline/status missing: %s", text)
	}
	var saved snapshot
	data, _ := os.ReadFile(cfg.Checkpoint)
	json.Unmarshal(data, &saved)
	if saved.Scopes["project:course/2"].Records["30"].Body != "Updated assignment body." {
		t.Fatal("body was not preserved")
	}
}

func TestPartialFailureRetainsSnapshotAndDoesNotBlockOtherSources(t *testing.T) {
	cfg := testConfig(t)
	f := newFixture(t)
	f.seed()
	client := Client{Get: f.get}
	now := time.Now()
	if _, err := Sync(context.Background(), cfg, client, now); err != nil {
		t.Fatal(err)
	}
	f.fail["repos/course/tasks/issues?state=all&per_page=100"] = errors.New("GitHub HTTP 403; source unavailable or access denied")
	f.projectItem("2026-09-18", "Backlog", "Changed while issues failed.", "")
	result, err := Sync(context.Background(), cfg, client, now.Add(time.Hour))
	if err == nil || result.Status != "partial" || !result.NeedsAnalysis {
		t.Fatalf("partial: %+v %v", result, err)
	}
	var saved snapshot
	data, _ := os.ReadFile(cfg.Checkpoint)
	json.Unmarshal(data, &saved)
	if len(saved.Scopes["repo:course/tasks:issues"].Records) != 1 || saved.Scopes["project:course/2"].Records["30"].Body != "Changed while issues failed." {
		t.Fatal("successful updates or previous failed source were lost")
	}
	again, err := Sync(context.Background(), cfg, client, now.Add(2*time.Hour))
	if err == nil || again.NeedsAnalysis {
		t.Fatalf("unchanged outage must stay visible without new analysis: %+v %v", again, err)
	}
	delete(f.fail, "repos/course/tasks/issues?state=all&per_page=100")
	recovered, err := Sync(context.Background(), cfg, client, now.Add(3*time.Hour))
	if err != nil || !recovered.NeedsAnalysis {
		t.Fatalf("recovery: %+v %v", recovered, err)
	}
}

func TestProjectRedactionAndTruncatedTreeAreCoverageGaps(t *testing.T) {
	cfg := testConfig(t)
	f := newFixture(t)
	f.seed()
	f.data[itemsEndpoint] = []any{map[string]any{"id": 30, "content_type": "REDACTED", "content": nil}}
	f.data["repos/course/tasks/git/trees/"+taskHead+"?recursive=1"] = map[string]any{"truncated": true, "tree": []any{}}
	result, err := Sync(context.Background(), cfg, Client{Get: f.get}, time.Now())
	if err == nil || result.Status != "partial" || !strings.Contains(err.Error(), "redacted") || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("missing gaps: %+v %v", result, err)
	}
}

func TestPaginationIsCompleteAndCannotLeaveAllowlist(t *testing.T) {
	f := newFixture(t)
	first := "orgs/course/projectsV2/2/items?per_page=100"
	second := "orgs/course/projectsV2/2/items?after=abc&per_page=100"
	f.data[first] = []any{map[string]int{"id": 1}}
	f.data[second] = []any{map[string]int{"id": 2}}
	f.headers[first] = http.Header{"Link": {`<https://api.github.com/` + second + `>; rel="next"`}}
	client := Client{Get: f.get}
	items, err := client.list(context.Background(), first)
	if err != nil || len(items) != 2 {
		t.Fatalf("pagination: %v %v", items, err)
	}
	f.headers[first] = http.Header{"Link": {`<https://api.github.com/repos/unrelated/private/issues>; rel="next"`}}
	if _, err := client.list(context.Background(), first); err == nil {
		t.Fatal("accepted source escape")
	}
	f.headers[first] = http.Header{"Link": {`<https://api.github.com/` + first + `>; rel="next"`}}
	if _, err := client.list(context.Background(), first); err == nil || !strings.Contains(err.Error(), "repeated") {
		t.Fatalf("cursor loop: %v", err)
	}
}

func TestMissingConfiguredBranchIsNotEmpty(t *testing.T) {
	cfg := testConfig(t)
	f := newFixture(t)
	f.seed()
	f.data["repos/course/tasks/branches?per_page=100"] = []any{}
	result, err := Sync(context.Background(), cfg, Client{Get: f.get}, time.Now())
	if err == nil || result.Status != "partial" || !strings.Contains(err.Error(), "configured branch") {
		t.Fatalf("missing branch: %+v %v", result, err)
	}
}
