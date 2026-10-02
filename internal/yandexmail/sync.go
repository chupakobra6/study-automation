package yandexmail

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	_ "github.com/emersion/go-message/charset"
	"github.com/emersion/go-message/mail"

	"github.com/chupakobra6/study-automation/internal/config"
	"github.com/chupakobra6/study-automation/internal/maildata"
)

type Report struct {
	Source           string                 `json:"source"`
	GeneratedAt      string                 `json:"generated_at"`
	Account          string                 `json:"account"`
	Status           string                 `json:"status"`
	Added            int                    `json:"added"`
	Incomplete       bool                   `json:"incomplete"`
	PendingItems     int                    `json:"pending_items"`
	BacklogRemaining int                    `json:"backlog_remaining"`
	FailedItems      int                    `json:"failed_items"`
	ItemFailures     []maildata.ItemFailure `json:"item_failures,omitempty"`
	Records          []maildata.Record      `json:"records"`
	Messages         string                 `json:"messages_jsonl"`
	Checkpoint       string                 `json:"checkpoint"`
	Report           string                 `json:"report"`
}

type checkpoint struct {
	GeneratedAt         string                          `json:"generated_at"`
	FolderUIDs          map[string]uint32               `json:"folder_uids"`
	FolderUIDValidities map[string]uint32               `json:"folder_uid_validities,omitempty"`
	ItemFailures        map[string]maildata.ItemFailure `json:"item_failures,omitempty"`
}

var (
	unsafeFilename = regexp.MustCompile(`[\x00-\x1f/:*?"<>|]+`)
	htmlTags       = regexp.MustCompile(`(?s)<[^>]*>`)
	htmlSpaces     = regexp.MustCompile(`\s+`)
)

func Sync(ctx context.Context, cfg config.YandexMail, client Client, now time.Time) (Report, error) {
	if cfg.Account == "" {
		return Report{}, errors.New("yandex_mail.account is not configured")
	}
	if len(cfg.Folders) == 0 {
		return Report{}, errors.New("yandex_mail.folders is empty")
	}
	since, err := time.Parse("2006-01-02", cfg.Since)
	if err != nil {
		return Report{}, fmt.Errorf("invalid yandex_mail.since %q: %w", cfg.Since, err)
	}
	previous, existed, err := readCheckpoint(cfg.Checkpoint)
	if err != nil {
		return Report{}, err
	}
	nextUIDs := make(map[string]uint32, len(previous.FolderUIDs))
	for folder, uid := range previous.FolderUIDs {
		nextUIDs[folder] = uid
	}
	failures := previous.ItemFailures
	if failures == nil {
		failures = make(map[string]maildata.ItemFailure)
	}
	added := make([]maildata.Record, 0)
	backlog := 0
	validities := make(map[string]uint32, len(previous.FolderUIDValidities))
	for folder, value := range previous.FolderUIDValidities {
		validities[folder] = value
	}
	for _, folder := range cfg.Folders {
		if err := ctx.Err(); err != nil {
			return Report{}, err
		}
		if folder == "" {
			continue
		}
		uids, err := client.Search(folder, since)
		if err != nil {
			return Report{}, err
		}
		previousUID := previous.FolderUIDs[folder]
		validity := client.UIDValidity()
		if validity == 0 {
			return Report{}, fmt.Errorf("folder %q has no UIDVALIDITY; refusing an unverifiable cursor", folder)
		}
		if old := validities[folder]; old != 0 && old != validity {
			return Report{}, fmt.Errorf("folder %q UIDVALIDITY changed (%d -> %d); stop and reconcile this mailbox before reusing its UID checkpoint", folder, old, validity)
		}
		validities[folder] = validity
		unseen := make([]uint32, 0, len(uids))
		selected := make(map[uint32]bool)
		// Retry explicitly from persisted state, including UIDs below the cursor
		// or no longer returned by SEARCH. SEARCH also selects the folder read-only.
		for _, failure := range failures {
			if failure.Folder == folder && failure.Due(now) {
				unseen = append(unseen, failure.UID)
				selected[failure.UID] = true
			}
		}
		for _, uid := range uids {
			_, failed := failures[itemKey(folder, uid)]
			if uid > previousUID && !failed && !selected[uid] {
				unseen = append(unseen, uid)
				selected[uid] = true
			}
		}
		sort.Slice(unseen, func(i, j int) bool { return unseen[i] < unseen[j] })
		if len(unseen) > cfg.MaxMessagesPerSync {
			backlog += len(unseen) - cfg.MaxMessagesPerSync
			unseen = unseen[:cfg.MaxMessagesPerSync]
		}
		for _, uid := range unseen {
			if err := ctx.Err(); err != nil {
				return Report{}, err
			}
			key := itemKey(folder, uid)
			stage := "fetch"
			raw, err := client.Fetch(uid)
			var record maildata.Record
			if err == nil {
				stage = "parse"
				record, err = parseRecord(cfg, folder, uid, raw)
			}
			if err != nil {
				if ctx.Err() != nil {
					return Report{}, ctx.Err()
				}
				failure := failures[key].Failed(fmt.Sprintf("%s/%d", folder, uid), stage, err, now)
				failure.Folder, failure.UID = folder, uid
				failures[key] = failure
			} else {
				added = append(added, record)
				delete(failures, key)
			}
			// This cursor is saved atomically with failures: advancing past an
			// unsuccessful UID never loses the information needed to retry it.
			if uid > nextUIDs[folder] {
				nextUIDs[folder] = uid
			}
		}
	}

	existing, err := readRecords(cfg.MessagesJSONL)
	if err != nil {
		return Report{}, err
	}
	merged := make(map[string]maildata.Record, len(existing)+len(added))
	for _, record := range append(existing, added...) {
		merged[recordKey(record)] = record
	}
	records := make([]maildata.Record, 0, len(merged))
	for _, record := range merged {
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].Folder == records[j].Folder {
			return records[i].UID < records[j].UID
		}
		return records[i].Folder < records[j].Folder
	})

	status := "no-changes"
	if !existed || len(previous.FolderUIDs) == 0 {
		status = "baseline"
	} else if len(added) > 0 {
		status = "delta"
	}
	pending, failed, items := maildata.FailureSummary(failures)
	if len(items) > 0 || backlog > 0 {
		status = "incomplete"
	}
	generatedAt := now.UTC().Format(time.RFC3339)
	report := Report{
		Source: "yandex_mail", GeneratedAt: generatedAt, Account: cfg.Account,
		Status: status, Added: len(added), Records: added, Messages: cfg.MessagesJSONL,
		Incomplete: len(items) > 0 || backlog > 0, PendingItems: pending, FailedItems: failed, ItemFailures: items, BacklogRemaining: backlog,
		Checkpoint: cfg.Checkpoint, Report: cfg.LatestReport,
	}
	if err := writeRecords(cfg.MessagesJSONL, records); err != nil {
		return Report{}, err
	}
	if err := writeAtomic(cfg.LatestReport, []byte(renderReport(report)), 0o600); err != nil {
		return Report{}, fmt.Errorf("write Yandex Mail report: %w", err)
	}
	next := checkpoint{GeneratedAt: generatedAt, FolderUIDs: nextUIDs, FolderUIDValidities: validities, ItemFailures: failures}
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return Report{}, err
	}
	if err := writeAtomic(cfg.Checkpoint, append(data, '\n'), 0o600); err != nil {
		return Report{}, fmt.Errorf("write Yandex Mail checkpoint: %w", err)
	}
	return report, nil
}

// ResetItemFailure retains the failed UID for retry even below the folder cursor.
func ResetItemFailure(path, folder string, uid uint32) error {
	value, _, err := readCheckpoint(path)
	if err != nil {
		return err
	}
	key := itemKey(folder, uid)
	failure, ok := value.ItemFailures[key]
	if !ok {
		return fmt.Errorf("Yandex Mail item %s/%d has no saved failure", folder, uid)
	}
	value.ItemFailures[key] = failure.Reset()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, append(data, '\n'), 0o600)
}

func parseRecord(cfg config.YandexMail, folder string, uid uint32, raw []byte) (maildata.Record, error) {
	reader, err := mail.CreateReader(bytes.NewReader(raw))
	if err != nil {
		return maildata.Record{}, err
	}
	defer reader.Close()
	textParts := make([]string, 0)
	htmlParts := make([]string, 0)
	attachments := make([]maildata.Attachment, 0)
	partIndex := 0
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return maildata.Record{}, err
		}
		partIndex++
		content, err := io.ReadAll(io.LimitReader(part.Body, 64<<20))
		if err != nil {
			return maildata.Record{}, err
		}
		switch header := part.Header.(type) {
		case *mail.InlineHeader:
			contentType, _, _ := header.ContentType()
			value := strings.TrimSpace(string(content))
			if contentType == "text/plain" && value != "" {
				textParts = append(textParts, value)
			} else if contentType == "text/html" && value != "" {
				htmlParts = append(htmlParts, value)
			}
		case *mail.AttachmentHeader:
			filename, _ := header.Filename()
			filename = safeFilename(filename, fmt.Sprintf("attachment-%d.bin", partIndex))
			contentType, _, _ := header.ContentType()
			localPath := filepath.Join(cfg.AttachmentsDir, safeFilename(folder, "folder"), fmt.Sprintf("%d-%03d-%s", uid, partIndex, filename))
			if err := writeAtomic(localPath, content, 0o600); err != nil {
				return maildata.Record{}, err
			}
			digest := sha256.Sum256(content)
			attachments = append(attachments, maildata.Attachment{
				FileName: filename, MIMEType: contentType, Size: len(content),
				SHA256: hex.EncodeToString(digest[:]), LocalPath: localPath,
			})
		}
	}
	body := strings.TrimSpace(strings.Join(textParts, "\n\n"))
	if body == "" {
		converted := make([]string, 0, len(htmlParts))
		for _, value := range htmlParts {
			converted = append(converted, htmlToText(value))
		}
		body = strings.TrimSpace(strings.Join(converted, "\n\n"))
	}
	return maildata.Record{
		Source: "yandex_mail", Account: cfg.Account, Folder: folder, UID: uid,
		MessageID: headerText(reader.Header, "Message-Id"), Date: headerText(reader.Header, "Date"),
		From: headerText(reader.Header, "From"), To: headerText(reader.Header, "To"),
		Subject: headerText(reader.Header, "Subject"), Snippet: truncate(body, 360),
		BodyText: body, Attachments: attachments,
	}, nil
}

func headerText(header mail.Header, name string) string {
	value, err := header.Text(name)
	if err != nil && value == "" {
		return strings.TrimSpace(header.Get(name))
	}
	return strings.TrimSpace(value)
}

func safeFilename(value, fallback string) string {
	name := filepath.Base(strings.ReplaceAll(value, "\\", "/"))
	name = strings.Trim(unsafeFilename.ReplaceAllString(name, "_"), " .")
	if name == "" || name == "." {
		return fallback
	}
	runes := []rune(name)
	if len(runes) > 180 {
		name = string(runes[:180])
	}
	return name
}

func htmlToText(value string) string {
	value = strings.ReplaceAll(value, "<br>", "\n")
	value = strings.ReplaceAll(value, "<br/>", "\n")
	value = strings.ReplaceAll(value, "</p>", "\n")
	return strings.TrimSpace(htmlSpaces.ReplaceAllString(html.UnescapeString(htmlTags.ReplaceAllString(value, " ")), " "))
}

func truncate(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return strings.TrimSpace(string(runes[:limit-1])) + "…"
}

func recordKey(record maildata.Record) string {
	return itemKey(record.Folder, record.UID)
}

func itemKey(folder string, uid uint32) string {
	return fmt.Sprintf("%s\x00%d", folder, uid)
}

func readCheckpoint(path string) (checkpoint, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return checkpoint{FolderUIDs: map[string]uint32{}}, false, nil
	}
	if err != nil {
		return checkpoint{}, false, fmt.Errorf("read Yandex Mail checkpoint: %w", err)
	}
	var value checkpoint
	if err := json.Unmarshal(data, &value); err != nil {
		return checkpoint{}, false, fmt.Errorf("parse Yandex Mail checkpoint: %w", err)
	}
	if value.FolderUIDs == nil {
		value.FolderUIDs = map[string]uint32{}
	}
	return value, true, nil
}

func readRecords(path string) ([]maildata.Record, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	records := make([]maildata.Record, 0)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var record maildata.Record
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return nil, fmt.Errorf("parse Yandex Mail JSONL: %w", err)
		}
		records = append(records, record)
	}
	return records, scanner.Err()
}

func writeRecords(path string, records []maildata.Record) error {
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	for _, record := range records {
		if err := encoder.Encode(record); err != nil {
			return err
		}
	}
	return writeAtomic(path, output.Bytes(), 0o600)
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".study-monitor-*")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(mode); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempName, path)
}

func renderReport(report Report) string {
	var output strings.Builder
	fmt.Fprintf(&output, "# Yandex Educational Mail Updates\n\nGenerated at: %s\nAccount: `%s`\nStatus: `%s`\nAdded: %d\n\n",
		report.GeneratedAt, report.Account, report.Status, report.Added)
	if report.Incomplete {
		if report.BacklogRemaining > 0 {
			fmt.Fprintf(&output, "Collection budget reached; %d mail items remain for the next sync, not treated as checked.\n", report.BacklogRemaining)
		}
		fmt.Fprintf(&output, "Incomplete: %d waiting for retry; %d require repair and explicit retry. Details: `%s`.\n\n", report.PendingItems, report.FailedItems, report.Checkpoint)
	}
	if len(report.Records) == 0 {
		output.WriteString("No new messages.\n")
		return output.String()
	}
	for _, record := range report.Records {
		fmt.Fprintf(&output, "## %s\n\n- Date: %s\n- From: %s\n- Folder/UID: `%s/%d`\n- Snippet: %s\n",
			defaultString(record.Subject, "(no subject)"), defaultString(record.Date, "unknown"),
			defaultString(record.From, "unknown"), record.Folder, record.UID, record.Snippet)
		for _, attachment := range record.Attachments {
			fmt.Fprintf(&output, "- Attachment: `%s` (%s, %d bytes) at `%s`\n",
				attachment.FileName, attachment.MIMEType, attachment.Size, attachment.LocalPath)
		}
		output.WriteString("\n")
	}
	return output.String()
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
