package main

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/chupakobra6/study-automation/internal/monitor"
)

func TestCollectionRunPreservesFailedSourceAndCompletedPeers(t *testing.T) {
	started := time.Date(2026, 9, 24, 4, 0, 0, 0, time.UTC)
	sources := []monitor.Source{{Name: "gmail"}, {Name: "telegram"}}
	run := newCollectionRun(started, sources, true)
	run = applyCollectionProgress(run, monitor.Progress{Source: "gmail", Phase: "started"})
	if run.Sources[0].Status != "running" || run.Sources[1].Status != "pending" {
		t.Fatalf("source progress not isolated: %+v", run.Sources)
	}
	dir := filepath.Join(t.TempDir(), "runs")
	if err := saveCollectionRun(dir, run, false); err != nil {
		t.Fatal(err)
	}
	before, err := loadCollectionRun(dir)
	if err != nil || before.Status != "running" || len(before.Sources) != 2 {
		t.Fatalf("running collection not observable: %+v, %v", before, err)
	}
	aborted := finishCollectionRun(run, errors.New("queue write failed"), started.Add(time.Second))
	if status := collectionBrief(aborted, nil, started.Add(2*time.Second)); status["ready"] != false {
		t.Fatalf("interrupted full collection must not look ready: %+v", status)
	}
	run = applyCollectionProgress(run, monitor.Progress{Source: "gmail", Phase: "finished", Status: "failed", Detail: "credential rejected", Duration: 20 * time.Millisecond})
	run = applyCollectionProgress(run, monitor.Progress{Source: "telegram", Phase: "finished", Status: "delta", Duration: 1200 * time.Millisecond})
	run = finishCollectionRun(run, errors.New("gmail failed"), started.Add(2*time.Second))
	if err := saveCollectionRun(dir, run, true); err != nil {
		t.Fatal(err)
	}
	after, err := loadCollectionRun(dir)
	if err != nil || after.Status != "partial" || after.Sources[0].Detail != "credential rejected" || after.Sources[1].DurationMillis != 1200 {
		t.Fatalf("collection result lost source state: %+v, %v", after, err)
	}
	status := collectionBrief(after, nil, started.Add(3*time.Second))
	if status["ready"] != true {
		t.Fatalf("partial collection should be ready for analysis: %+v", status)
	}
	status = collectionBrief(after, nil, started.Add(24*time.Hour))
	if status["ready"] != false {
		t.Fatalf("yesterday's collection should not look current: %+v", status)
	}
	after.FullScope = false
	status = collectionBrief(after, nil, started.Add(3*time.Second))
	if status["ready"] != false {
		t.Fatalf("single-source diagnostic should not replace full collection: %+v", status)
	}
}
