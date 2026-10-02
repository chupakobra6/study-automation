package queue

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSplitReportsSurviveCrashAndRemainBounded(t *testing.T) {
	for _, recoverCrash := range []bool{false, true} {
		t.Run(fmt.Sprint(recoverCrash), func(t *testing.T) {
			dir := t.TempDir()
			report := filepath.Join(dir, "latest.md")
			store, err := Open(filepath.Join(dir, "queue"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if err := store.Start("telegram", report, time.Now()); err != nil {
				t.Fatal(err)
			}
			var body strings.Builder
			for i := 0; i < 10; i++ {
				fmt.Fprintf(&body, "<!-- study-monitor-part {\"required_evidence\":[\"/evidence/%d.pdf\"]} -->\n# Delta %d\nunique-message-%d\n", i, i, i)
			}
			if err := os.WriteFile(report, []byte(body.String()), 0o600); err != nil {
				t.Fatal(err)
			}
			if recoverCrash {
				err = store.Recover()
			} else {
				err = store.Complete(Outcome{Source: "telegram", Status: "ok", Added: 3000})
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Recover(); err != nil {
				t.Fatal(err)
			}
			summary, err := store.Summary(MaxAnalysisBatch, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if summary.Pending != 10 || len(summary.Items) != 8 || !summary.Truncated {
				t.Fatalf("batch %+v", summary)
			}
			for _, item := range summary.Items {
				data, err := os.ReadFile(item.ReportPath)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Count(string(data), "unique-message-") != 1 || len(item.RequiredEvidence) != 1 {
					t.Fatalf("part: %+v %s", item, data)
				}
			}
		})
	}
}

func TestMalformedSplitReportIsNotAcknowledgedAsWhole(t *testing.T) {
	if _, err := splitReport([]byte(partPrefix+"broken -->\nbody"), nil); err == nil {
		t.Fatal("invalid metadata accepted")
	}
}
