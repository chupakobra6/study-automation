package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chupakobra6/study-automation/internal/queue"
)

func TestSharedDocumentURLsKeepTargetedServiceBoundary(t *testing.T) {
	for _, raw := range []string{
		"https://docs.yandex.ru/edit/d/fictional-sheet?from_public=1",
		"https://docs.yandex.ru/showcaptchafast?d=fictional&retpath=encoded&s=signature",
		"https://docs.google.com/spreadsheets/d/fictional/edit#gid=0",
		"https://docs.google.com/document/d/fictional/edit",
		"https://disk.yandex.ru/d/fictional",
		"https://disk.yandex.com/i/fictional",
		"https://disk.360.yandex.ru/d/fictional",
		"https://disk.yandex.ru/public?hash=fictional",
	} {
		if !sharedDocumentURL(raw, "Таблица тем задания") {
			t.Errorf("shared document URL missed: %s", raw)
		}
	}
	for _, raw := range []string{
		"https://t.me/c/12345/67", "https://example.org/meme",
		"https://video.example.org/watch/fictional", "https://lms.example.org/course/42",
		"https://docs.google.com/forms/d/fictional/viewform",
		"https://docs.google.com/spreadsheets", "https://docs.yandex.ru/",
		"https://docs.yandex.ru.attacker.example/edit/d/fictional",
		"https://disk.yandex.ru/client/disk", "https://disk.yandex.ru/d/fictional/meme.jpg",
		"https://disk.yandex.ru/d/fictional/movie.mp4", "https://disk.yandex.ru/d/fictional?filename=photo.png",
	} {
		if sharedDocumentURL(raw, "Таблица тем задания") {
			t.Errorf("unrelated/media URL became required document: %s", raw)
		}
	}
}

func TestOpaqueDiskLinksRequireDocumentContentRatherThanVideoOrTopic(t *testing.T) {
	documents := map[string]*linkedDocument{}
	collectLinkedDocuments(documents, "fictional-video-event",
		"- 2026-10-20 14:00 | Fictional study / Документы | Teacher: Видеозаписи занятий\n"+
			"- 2026-10-20 14:01 | Fictional study / Документы | Teacher: https://disk.yandex.ru/d/video-folder\n")
	collectLinkedDocuments(documents, "fictional-sheet-event",
		"- 2026-10-20 14:00 | Fictional study / Projects | Teacher: Таблица инициативных тем\n"+
			"- 2026-10-20 14:01 | Fictional study / Projects | Teacher: https://disk.yandex.ru/d/opaque-sheet\n")
	if len(documents) != 1 || documents["https://disk.yandex.ru/d/opaque-sheet"] == nil {
		t.Fatalf("video or topic became document context, or sheet was omitted: %+v", documents)
	}
	for _, raw := range []string{
		"https://docs.yandex.ru/edit/d/opaque", "https://docs.yandex.ru/showcaptchafast?d=opaque&s=signature",
		"https://docs.google.com/document/d/opaque/edit", "https://docs.google.com/spreadsheets/d/opaque/edit",
		"https://disk.yandex.ru/d/opaque/rules.pdf", "https://disk.yandex.ru/d/opaque?filename=topics.xlsx",
	} {
		if !sharedDocumentURL(raw, "") {
			t.Errorf("document service or filename unnecessarily required caption: %s", raw)
		}
	}
	for _, context := range []string{"", "видеозаписи занятий", "записи занятий", "https://disk.yandex.ru/d/document"} {
		if sharedDocumentURL("https://disk.yandex.ru/d/video-folder", context) {
			t.Errorf("opaque media link became document: %q", context)
		}
	}
}

func TestBriefLinkedDocumentsKeepCaptionlessMessageContextAndDeduplicate(t *testing.T) {
	f := newReviewFixture(t)
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	link := "https://docs.yandex.ru/showcaptchafast?d=fictional&retpath=opaque&s=signature"
	f.collect("telegram", "# Telegram\n"+
		"- 2026-10-20 14:00 | Fictional study / Projects | Teacher: Выберите личную тему в таблице\n"+
		"- 2026-10-20 14:01 | Fictional study / Projects | Teacher: "+link+"\n"+
		"https://t.me/c/12345/67 https://example.org/meme https://video.example.org/watch/1 https://lms.example.org/course/42\n"+
		"https://disk.yandex.ru/d/fictional/photo.jpg https://disk.yandex.ru/d/fictional/movie.mp4\n"+link, nil, now)
	f.collect("telegram", link, nil, now.Add(time.Second))
	f.collect("gmail", "https://docs.google.com/document/d/mail-only/edit", nil, now.Add(2*time.Second))
	summary, err := queue.Summarize(f.dir, queue.MaxAnalysisBatch, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	brief, err := buildBrief(summary, taskBoard{}, f.tasks, f.root, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	batch := brief["review_batch"].(reviewBatch)
	if len(batch.Coverage.LinkedDocuments) != 1 {
		t.Fatalf("wrong document coverage: %+v", batch.Coverage.LinkedDocuments)
	}
	reference := batch.Coverage.LinkedDocuments[0]
	if reference.URL != link || len(reference.Sources) != 2 {
		t.Fatalf("source URL or events were lost: %+v", reference)
	}
	if !strings.Contains(reference.Sources[0].Context, "Выберите личную тему в таблице") || !strings.Contains(reference.Sources[0].Context, link) {
		t.Fatalf("separate link lost preceding announcement: %+v", reference)
	}
	for _, source := range reference.Sources {
		item, err := queue.Inspect(f.dir, source.EventID)
		if err != nil || item.Source != "telegram" {
			t.Fatalf("wrong source association: %+v %v", source, err)
		}
	}
}

func TestPreparedReferenceNeedsDecisionAndFindingBeforeAcknowledgement(t *testing.T) {
	f := newReviewFixture(t)
	now := time.Now()
	link := "https://docs.google.com/spreadsheets/d/fictional-personal-rows/edit#gid=42"
	f.collect("telegram", link, nil, now)
	path, input := f.prepare(now)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var draft reviewDraft
	if err := json.Unmarshal(raw, &draft); err != nil {
		t.Fatal(err)
	}
	if len(draft.Decisions) != 2 || draft.Decisions[1].Kind != "reference" || draft.Decisions[1].Key != link {
		t.Fatalf("prepared receipt hides exact link: %+v", draft)
	}
	draft.Decisions[0].Outcome, draft.Decisions[0].Finding = "no-action", "Прочитано объявление."
	reference := draft.Decisions[1]
	draft.Decisions = draft.Decisions[:1]
	writeReview(t, path, draft)
	if _, err := validatePreparedReview(f.dir, path, taskBoard{}, now); err == nil || !strings.Contains(err.Error(), "missing decision reference/"+link) {
		t.Fatalf("generic no-action accepted without linked table: %v", err)
	}
	reference.Outcome = "no-action"
	draft.Decisions = append(draft.Decisions, reference)
	writeReview(t, path, draft)
	if _, err := validatePreparedReview(f.dir, path, taskBoard{}, now); err == nil || !strings.Contains(err.Error(), "concrete finding") {
		t.Fatalf("empty reference finding accepted: %v", err)
	}
	draft.Decisions[1].Finding = "Таблица прочитана, личная строка проверена; новых действий нет."
	writeReview(t, path, draft)
	if remaining := f.commit(path, now); remaining.Pending != 0 {
		t.Fatalf("examined table could not be acknowledged: %+v", remaining)
	}
	if input.Batch.Coverage.LinkedDocuments[0].URL != link {
		t.Fatal("prepared input lost reference")
	}
}

func TestBlockedReferenceRequiresEveryOwningEventRatherThanUnrelatedBlock(t *testing.T) {
	f := newReviewFixture(t)
	now := time.Now()
	link := "https://docs.yandex.ru/edit/d/fictional?from_public=1"
	f.collect("telegram", link, nil, now)
	f.collect("telegram", "Повтор таблицы: "+link, nil, now.Add(time.Second))
	f.collect("gmail", "unrelated letter", nil, now.Add(2*time.Second))
	reviewedAt := now.Add(time.Minute)
	path, input := f.prepare(reviewedAt)
	draft := reviewDraft{Decisions: decisionSpecs(input.Batch), Evidence: evidenceSpecs(input.Files)}
	for i := range draft.Decisions {
		d := &draft.Decisions[i]
		d.Outcome, d.Finding = "no-action", "Содержимое прочитано."
		if d.Kind == "reference" {
			d.Outcome, d.Finding, d.Notice = "blocked", "Таблица недоступна, личная строка не проверена.", "Таблица недоступна; нужна повторная проверка."
		}
		for _, event := range input.Batch.Items {
			if d.Kind == "event" && d.Key == event.ID && event.Source == "gmail" {
				d.Outcome, d.Notice, d.RetryAt = "blocked", "Письмо требует повторной проверки.", now.Add(time.Hour).Format(time.RFC3339)
			}
		}
	}
	writeReview(t, path, draft)
	if _, err := validatePreparedReview(f.dir, path, taskBoard{}, reviewedAt); err == nil || !strings.Contains(err.Error(), "requires its owning event") {
		t.Fatalf("unrelated blocked event covered unavailable table: %v", err)
	}
	for i := range draft.Decisions {
		d := &draft.Decisions[i]
		if d.Kind == "event" {
			d.Outcome, d.Notice, d.RetryAt = "blocked", "Нужна повторная проверка источника.", now.Add(time.Hour).Format(time.RFC3339)
		}
	}
	writeReview(t, path, draft)
	result, err := validatePreparedReview(f.dir, path, taskBoard{}, reviewedAt)
	if err != nil || len(result.CompletedIDs) != 0 || len(result.Deferred) != 3 {
		t.Fatalf("blocked references did not retain their events: %+v %v", result, err)
	}
}

func TestPreparedReferenceCoverageRemainsFrozenWhenAnotherEventArrives(t *testing.T) {
	f := newReviewFixture(t)
	now := time.Now()
	first := "https://docs.google.com/document/d/fictional-first/edit"
	second := "https://docs.google.com/spreadsheets/d/fictional-next/edit"
	f.collect("telegram", first, nil, now)
	path, input := f.prepare(now)
	f.collect("telegram", second, nil, now.Add(time.Second))
	f.receipt(path, input, nil)
	remaining := f.commit(path, now.Add(time.Minute))
	if remaining.Pending != 1 || len(remaining.Items) != 1 {
		t.Fatalf("old prepared receipt consumed new event: %+v", remaining)
	}
	_, next := f.prepare(now.Add(time.Minute))
	if len(next.Batch.Coverage.LinkedDocuments) != 1 || next.Batch.Coverage.LinkedDocuments[0].URL != second {
		t.Fatalf("new link did not require its own review: %+v", next.Batch.Coverage)
	}
}

func TestHistoricalPreparedInputWithoutLinkedDocumentsKeepsFrozenSpec(t *testing.T) {
	f := newReviewFixture(t)
	now := time.Now()
	f.collect("telegram", "https://docs.google.com/document/d/newly-visible/edit", nil, now)
	path, input := f.prepare(now)
	raw, err := os.ReadFile(path + ".input.json")
	if err != nil {
		t.Fatal(err)
	}
	var historical map[string]any
	if err := json.Unmarshal(raw, &historical); err != nil {
		t.Fatal(err)
	}
	delete(historical["batch"].(map[string]any)["coverage"].(map[string]any), "linked_documents")
	raw, err = json.Marshal(historical)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".input.json", raw, 0600); err != nil {
		t.Fatal(err)
	}
	input.Batch.Coverage.LinkedDocuments = nil
	f.receipt(path, input, nil)
	if remaining := f.commit(path, now); remaining.Pending != 0 {
		t.Fatalf("current report changed historical prepared decisions: %+v", remaining)
	}
}
