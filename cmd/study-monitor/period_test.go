package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chupakobra6/study-automation/internal/config"
)

func TestPeriodIndexesSourceDatesAcrossMoscowBoundaryWithoutOtherChats(t *testing.T) {
	root := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	gmail := write("gmail.jsonl", `{"message_id":"g1","date":"Wed, 23 Sep 2026 23:45:00 +0000","subject":"Домашняя работа","snippet":"Первое письмо"}`+"\n")
	yandex := write("yandex.jsonl", `{"message_id":"y1","date":"not a date","internal_date":"2026-09-24T21:01:00Z","subject":"Проект"}`+"\n")
	telegram := write("telegram.jsonl", `{"chat":{"id":42,"title":"Учебный чат"},"message_id":7,"date":"2026-09-24T20:58:00Z","text":"старый текст"}`+"\n"+
		`{"chat":{"id":99,"title":"Другой чат"},"message_id":8,"date":"2026-09-24T20:58:00Z","text":"не учитывать"}`+"\n"+
		`{"chat":{"id":42,"title":"Учебный чат"},"message_id":7,"date":"2026-09-24T20:58:00Z","text":"исправленный срок"}`+"\n")
	cfg := config.Config{}
	cfg.Gmail.MessagesJSONL = gmail
	cfg.YandexMail.MessagesJSONL = yandex
	cfg.Telegram.MessagesJSONL = telegram
	cfg.Telegram.Chats = []config.TelegramChat{{ID: "42"}}
	moscow, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 9, 24, 0, 0, 0, 0, moscow)
	records, coverage, err := collectPeriodRecords(cfg, start, start.AddDate(0, 0, 1), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || len(coverage) != 8 {
		t.Fatalf("wrong period index or source coverage: %+v %+v", records, coverage)
	}
	bySource := map[string]periodRecord{}
	for _, record := range records {
		bySource[record.Source] = record
	}
	if bySource["gmail"].ID != "g1" || bySource["telegram"].ID != "42:7" || bySource["telegram"].Preview != "исправленный срок" || bySource["telegram"].SourceLine != 3 {
		t.Fatalf("wrong source dates, chat selection or revision: %+v", records)
	}
	if coverage[1].Count != 0 || coverage[3].Mode != "unindexed" {
		t.Fatalf("coverage hides missing index: %+v", coverage)
	}
	cfg.Club404.Subjects = []string{"net"}
	cfg.Club404.Checkpoint = write("club404.json", `{"version":1,"items":{}}`)
	_, withClub, err := collectPeriodRecords(cfg, start, start.AddDate(0, 0, 1), "")
	if err != nil || len(withClub) != 9 || withClub[8].Source != "club404" || withClub[8].Mode != "unindexed" || withClub[8].SnapshotPath != cfg.Club404.Checkpoint {
		t.Fatalf("club404 coverage must name its current snapshot only: %+v, %v", withClub, err)
	}
	full, line, err := findPeriodRecord(telegram, "42:7", map[string]bool{"42": true})
	if err != nil || line != 3 || !strings.Contains(string(full), "исправленный срок") {
		t.Fatalf("cannot inspect selected latest record: line=%d err=%v record=%s", line, err, full)
	}
	if _, basis, ok := parseSourceDate("not a date", "2026-09-24T20:59:00Z"); !ok || basis != "internal_date" {
		t.Fatal("internal date fallback lost")
	}
	tasks := write("tasks.json", `{"version":1,"tasks":[{"id":"work","title":"Практика","status":"open","dates":[{"at":"2026-09-24T23:59:00+03:00","kind":"deadline","label":"Сдать работу"}]}]}`)
	dates, dateError := currentTaskDates(tasks, start, start.AddDate(0, 0, 1))
	if dateError != "" || len(dates) != 1 || dates[0].TaskID != "work" || dates[0].Kind != "deadline" {
		t.Fatalf("current task calendar missing from period: %+v %s", dates, dateError)
	}
}
