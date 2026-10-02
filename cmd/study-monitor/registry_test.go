package main

import (
	"context"
	"github.com/chupakobra6/study-automation/internal/queue"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestMalformedRegistryCannotSilentlyDropReminders(t *testing.T) {
	for _, body := range []string{`{"version":1,"task":[]}`, `{"version":1,"tasks":null}`, `{"version":1,"tasks":[{"id":"x","title":"X","status":"open","next_check_at":"soon"}]}`, `{"version":1,"tasks":[{"id":"x","title":"X","status":"open","dates":[{"at":"soon","kind":"deadline","label":"X"}]}]}`, `{"version":1,"tasks":[{"id":"x","title":"X","status":"open"},{"id":"x","title":"X","status":"open"}]}`} {
		p := filepath.Join(t.TempDir(), "tasks.json")
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readBoard(p); err == nil {
			t.Fatalf("invalid registry accepted: %s", body)
		}
	}
	p := filepath.Join(t.TempDir(), "tasks.json")
	if err := os.WriteFile(p, []byte(`{"version":1,"tasks":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readBoard(p); err != nil {
		t.Fatalf("explicit empty registry rejected: %v", err)
	}
}

func TestRetryItemRefusesConcurrentSourceWriter(t *testing.T) {
	root := t.TempDir()
	queuePath := filepath.Join(root, "queue")
	cfg := filepath.Join(root, "config.toml")
	if err := os.WriteFile(cfg, []byte("[monitoring]\nqueue_dir = "+strconv.Quote(queuePath)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := queue.Open(queuePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	err = runOperations(context.Background(), []string{"retry-item", "--config", cfg, "--source", "gmail", "--message-id", "message-1"})
	if err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("concurrent checkpoint mutation was not refused: %v", err)
	}
}
