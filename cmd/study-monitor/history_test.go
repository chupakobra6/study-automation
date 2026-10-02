package main

import (
	"testing"
	"time"

	"github.com/chupakobra6/study-automation/internal/queue"
)

func TestHistoryFiltersObservedMoscowDayWithoutDroppingAcknowledgedEvents(t *testing.T) {
	moscow, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 9, 24, 0, 0, 0, 0, moscow)
	end := start.AddDate(0, 0, 1)
	items := []queue.Item{
		{ID: "before", CreatedAt: start.Add(-time.Nanosecond)},
		{ID: "first", CreatedAt: start, Acknowledged: true, ReceiptPath: "/receipt.md"},
		{ID: "last", CreatedAt: end.Add(-time.Nanosecond)},
		{ID: "after", CreatedAt: end},
	}
	events := eventsInPeriod(items, start, end)
	if len(events) != 2 || events[0].ID != "first" || events[1].ID != "last" || !events[0].Acknowledged || events[0].ReceiptPath != "/receipt.md" || !events[0].CollectionStartedAt.Equal(start) {
		t.Fatalf("wrong historical boundary or acknowledgment: %+v", events)
	}
}
