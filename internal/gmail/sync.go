package gmail

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"mime"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/chupakobra6/study-automation/internal/config"
	"github.com/chupakobra6/study-automation/internal/maildata"
)

type Attachment = maildata.Attachment
type Record = maildata.Record

type Report struct {
	Source           string                 `json:"source"`
	GeneratedAt      string                 `json:"generated_at"`
	Account          string                 `json:"account"`
	Status           string                 `json:"status"`
	Added            int                    `json:"added"`
	Incomplete       bool                   `json:"incomplete"`
	PendingItems     int                    `json:"pending_items"`
	BacklogRemaining bool                   `json:"backlog_remaining"`
	FailedItems      int                    `json:"failed_items"`
	ItemFailures     []maildata.ItemFailure `json:"item_failures,omitempty"`
	Records          []Record               `json:"records"`
	Messages         string                 `json:"messages_jsonl"`
	Checkpoint       string                 `json:"checkpoint"`
	Report           string                 `json:"report"`
}

type checkpoint struct {
	GeneratedAt      string                          `json:"generated_at"`
	QueryFingerprint string                          `json:"query_fingerprint"`
	SeenIDs          []string                        `json:"seen_ids"`
	ItemFailures     map[string]maildata.ItemFailure `json:"item_failures,omitempty"`
	CoverageAnchor   string                          `json:"coverage_anchor,omitempty"`
}

var (
	relativeDays   = regexp.MustCompile(`\bnewer_than:(\d+)d\b`)
	unsafeFilename = regexp.MustCompile(`[\x00-\x1f/:*?"<>|]+`)
	htmlTags       = regexp.MustCompile(`(?s)<[^>]*>`)
	htmlSpaces     = regexp.MustCompile(`\s+`)
)

// Keep the previous successful listing window open across outages/backlogs.
// Academic query terms are preserved; this never expands to the whole inbox.
func catchupQuery(query, anchor, previous string, now time.Time) string {
	if anchor == "" {
		anchor = previous
	}
	start, err := time.Parse(time.RFC3339, anchor)
	if err != nil || !start.Before(now) {
		return query
	}
	return relativeDays.ReplaceAllStringFunc(query, func(value string) string {
		match := relativeDays.FindStringSubmatch(value)
		days, err := strconv.Atoi(match[1])
		if err != nil {
			return value
		}
		return "after:" + start.AddDate(0, 0, -days-1).UTC().Format("2006/01/02")
	})
}

func Sync(ctx context.Context, cfg config.Gmail, client *Client, now time.Time) (Report, error) {
	if cfg.Account == "" {
		return Report{}, errors.New("gmail.account is not configured")
	}
	if len(cfg.Queries) == 0 {
		return Report{}, errors.New("gmail.queries is empty")
	}
	previous, existed, err := readCheckpoint(cfg.Checkpoint)
	if err != nil {
		return Report{}, err
	}
	seen := make(map[string]bool, len(previous.SeenIDs))
	for _, id := range previous.SeenIDs {
		seen[id] = true
	}
	failures := previous.ItemFailures
	if failures == nil {
		failures = make(map[string]maildata.ItemFailure)
	}
	// Failed IDs have their own retry schedule. Exclude them from listing limits
	// so an unavailable or terminal item cannot hide the following messages.
	listed := make(map[string]bool, len(seen)+len(failures))
	for id := range seen {
		listed[id] = true
	}
	retries := make([]string, 0)
	for id, failure := range failures {
		listed[id] = true
		if failure.Due(now) {
			retries = append(retries, id)
		}
	}
	sort.Strings(retries)
	refs := make([]MessageRef, 0)
	for _, id := range retries {
		refs = append(refs, MessageRef{ID: id})
	}
	for _, query := range cfg.Queries {
		if strings.TrimSpace(query) == "" {
			continue
		}
		remaining := cfg.MaxMessagesPerSync + 1 - len(refs)
		if remaining <= 0 {
			break
		}
		matches, err := client.ListUnseenMessages(ctx, catchupQuery(query, previous.CoverageAnchor, previous.GeneratedAt, now), remaining, listed)
		if err != nil {
			return Report{}, fmt.Errorf("list Gmail messages for query %q: %w", query, err)
		}
		for _, ref := range matches {
			if ref.ID == "" || listed[ref.ID] {
				continue
			}
			listed[ref.ID] = true
			refs = append(refs, ref)
		}
	}
	unseen := make([]MessageRef, 0, len(refs))
	for _, ref := range refs {
		if !seen[ref.ID] {
			unseen = append(unseen, ref)
		}
	}
	backlog := len(unseen) > cfg.MaxMessagesPerSync
	if backlog {
		unseen = unseen[:cfg.MaxMessagesPerSync]
	}
	added := make([]Record, 0, len(unseen))
	for _, ref := range unseen {
		if err := ctx.Err(); err != nil {
			return Report{}, err
		}
		message, err := client.GetMessage(ctx, ref.ID)
		if err != nil {
			if ctx.Err() != nil {
				return Report{}, ctx.Err()
			}
			failures[ref.ID] = failures[ref.ID].Failed(ref.ID, "fetch", err, now)
			continue
		}
		if message.ID != ref.ID {
			failures[ref.ID] = failures[ref.ID].Failed(ref.ID, "parse", errors.New("message ID mismatch"), now)
			continue
		}
		record, err := buildRecord(ctx, cfg, client, message)
		if err != nil {
			if ctx.Err() != nil {
				return Report{}, ctx.Err()
			}
			failures[ref.ID] = failures[ref.ID].Failed(ref.ID, "parse", err, now)
			continue
		}
		added = append(added, record)
		delete(failures, ref.ID)
		seen[ref.ID] = true
	}
	existing, err := readRecords(cfg.MessagesJSONL)
	if err != nil {
		return Report{}, err
	}
	merged := make(map[string]Record, len(existing)+len(added))
	for _, record := range append(existing, added...) {
		if record.MessageID != "" {
			merged[record.MessageID] = record
		}
	}
	records := make([]Record, 0, len(merged))
	for _, record := range merged {
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].InternalDate == records[j].InternalDate {
			return records[i].MessageID < records[j].MessageID
		}
		return records[i].InternalDate < records[j].InternalDate
	})
	status := "no-changes"
	if !existed {
		status = "baseline"
	} else if len(added) > 0 {
		status = "delta"
	}
	pending, failed, items := maildata.FailureSummary(failures)
	if len(items) > 0 || backlog {
		status = "incomplete"
	}
	generatedAt := now.UTC().Format(time.RFC3339)
	report := Report{
		Source:      "gmail",
		GeneratedAt: generatedAt,
		Account:     cfg.Account,
		Status:      status,
		Added:       len(added),
		Incomplete:  len(items) > 0 || backlog, PendingItems: pending, FailedItems: failed, ItemFailures: items, BacklogRemaining: backlog,
		Records:    added,
		Messages:   cfg.MessagesJSONL,
		Checkpoint: cfg.Checkpoint,
		Report:     cfg.LatestReport,
	}
	if err := writeRecords(cfg.MessagesJSONL, records); err != nil {
		return Report{}, err
	}
	if err := writeAtomic(cfg.LatestReport, []byte(renderReport(report)), 0o600); err != nil {
		return Report{}, fmt.Errorf("write Gmail report: %w", err)
	}
	seenIDs := make([]string, 0, len(seen))
	for id := range seen {
		seenIDs = append(seenIDs, id)
	}
	sort.Strings(seenIDs)
	next := checkpoint{
		GeneratedAt:      generatedAt,
		QueryFingerprint: queryFingerprint(cfg.Queries),
		SeenIDs:          seenIDs,
		ItemFailures:     failures,
		CoverageAnchor:   generatedAt,
	}
	if backlog {
		next.CoverageAnchor = previous.CoverageAnchor
		if next.CoverageAnchor == "" {
			next.CoverageAnchor = previous.GeneratedAt
		}
		if next.CoverageAnchor == "" {
			next.CoverageAnchor = generatedAt
		}
	}
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return Report{}, err
	}
	data = append(data, '\n')
	if err := writeAtomic(cfg.Checkpoint, data, 0o600); err != nil {
		return Report{}, fmt.Errorf("write Gmail checkpoint: %w", err)
	}
	return report, nil
}

// ResetItemFailure retries one item after the underlying problem is repaired.
func ResetItemFailure(path, messageID string) error {
	value, _, err := readCheckpoint(path)
	if err != nil {
		return err
	}
	failure, ok := value.ItemFailures[messageID]
	if !ok {
		return fmt.Errorf("Gmail item %q has no saved failure", messageID)
	}
	value.ItemFailures[messageID] = failure.Reset()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, append(data, '\n'), 0o600)
}

func buildRecord(ctx context.Context, cfg config.Gmail, client *Client, message Message) (Record, error) {
	plain := make([]string, 0)
	htmlBody := make([]string, 0)
	attachments := make([]Attachment, 0)
	partIndex := 0
	var walk func(Part) error
	walk = func(part Part) error {
		partIndex++
		if part.Filename != "" {
			payload, err := partPayload(ctx, client, message.ID, part)
			if err != nil {
				return err
			}
			filename := safeFilename(decodeHeader(part.Filename), fmt.Sprintf("attachment-%d.bin", partIndex))
			localPath := filepath.Join(cfg.AttachmentsDir, fmt.Sprintf("%s-%03d-%s", message.ID, partIndex, filename))
			if err := writeAtomic(localPath, payload, 0o600); err != nil {
				return err
			}
			digest := sha256.Sum256(payload)
			attachments = append(attachments, Attachment{
				FileName: filename, MIMEType: part.MIMEType, Size: len(payload),
				SHA256: hex.EncodeToString(digest[:]), LocalPath: localPath,
			})
		} else if part.MIMEType == "text/plain" || part.MIMEType == "text/html" {
			payload, err := partPayload(ctx, client, message.ID, part)
			if err != nil {
				return err
			}
			text := strings.TrimSpace(string(payload))
			if text != "" {
				if part.MIMEType == "text/plain" {
					plain = append(plain, text)
				} else {
					htmlBody = append(htmlBody, text)
				}
			}
		}
		for _, child := range part.Parts {
			if err := walk(child); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(message.Payload); err != nil {
		return Record{}, err
	}
	body := strings.Join(plain, "\n\n")
	if body == "" {
		converted := make([]string, 0, len(htmlBody))
		for _, value := range htmlBody {
			converted = append(converted, htmlToText(value))
		}
		body = strings.Join(converted, "\n\n")
	}
	internalDate := message.InternalDate
	if milliseconds, err := strconv.ParseInt(message.InternalDate, 10, 64); err == nil {
		internalDate = time.UnixMilli(milliseconds).UTC().Format(time.RFC3339)
	}
	return Record{
		Source: "gmail", Account: cfg.Account, MessageID: message.ID, ThreadID: message.ThreadID,
		HistoryID: message.HistoryID, InternalDate: internalDate,
		Date: header(message.Payload.Headers, "Date"), From: header(message.Payload.Headers, "From"),
		To: header(message.Payload.Headers, "To"), Subject: header(message.Payload.Headers, "Subject"),
		Snippet: strings.TrimSpace(html.UnescapeString(message.Snippet)), BodyText: body,
		Attachments: attachments,
	}, nil
}

func partPayload(ctx context.Context, client *Client, messageID string, part Part) ([]byte, error) {
	if part.Body.Data != "" {
		return decodeBase64URL(part.Body.Data)
	}
	if part.Body.AttachmentID != "" {
		return client.GetAttachment(ctx, messageID, part.Body.AttachmentID)
	}
	return nil, nil
}

func decodeBase64URL(value string) ([]byte, error) {
	data, err := base64.RawURLEncoding.DecodeString(value)
	if err == nil {
		return data, nil
	}
	return base64.URLEncoding.DecodeString(value)
}

func header(headers []Header, name string) string {
	for _, item := range headers {
		if strings.EqualFold(item.Name, name) {
			return decodeHeader(item.Value)
		}
	}
	return ""
}

func decodeHeader(value string) string {
	decoded, err := new(mime.WordDecoder).DecodeHeader(value)
	if err != nil {
		return strings.TrimSpace(value)
	}
	return strings.TrimSpace(decoded)
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

func readRecords(path string) ([]Record, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	records := make([]Record, 0)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var record Record
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return nil, fmt.Errorf("parse Gmail JSONL: %w", err)
		}
		records = append(records, record)
	}
	return records, scanner.Err()
}

func writeRecords(path string, records []Record) error {
	var output strings.Builder
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	for _, record := range records {
		if err := encoder.Encode(record); err != nil {
			return err
		}
	}
	return writeAtomic(path, []byte(output.String()), 0o600)
}

func readCheckpoint(path string) (checkpoint, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return checkpoint{}, false, nil
	}
	if err != nil {
		return checkpoint{}, false, err
	}
	var value checkpoint
	if err := json.Unmarshal(data, &value); err != nil {
		return checkpoint{}, false, fmt.Errorf("parse Gmail checkpoint: %w", err)
	}
	return value, true, nil
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
	fmt.Fprintf(&output, "# Gmail Updates\n\nGenerated at: %s\nAccount: `%s`\nStatus: `%s`\nAdded: %d\n\n",
		report.GeneratedAt, report.Account, report.Status, report.Added)
	if report.Incomplete {
		if report.BacklogRemaining {
			fmt.Fprintln(&output, "Collection budget reached; more matching mail remains. It is retained for the next sync, not treated as checked.")
		}
		fmt.Fprintf(&output, "Incomplete: %d waiting for retry; %d require repair and explicit retry. Details: `%s`.\n\n", report.PendingItems, report.FailedItems, report.Checkpoint)
	}
	if len(report.Records) == 0 {
		output.WriteString("No new messages.\n")
		return output.String()
	}
	for _, record := range report.Records {
		fmt.Fprintf(&output, "## %s\n\n- Date: %s\n- From: %s\n- Message/thread: `%s` / `%s`\n- Snippet: %s\n",
			defaultText(record.Subject, "(no subject)"), defaultText(record.Date, "unknown"),
			defaultText(record.From, "unknown"), record.MessageID, record.ThreadID, record.Snippet)
		for _, attachment := range record.Attachments {
			fmt.Fprintf(&output, "- Attachment: `%s` (%s, %d bytes) at `%s`\n",
				attachment.FileName, attachment.MIMEType, attachment.Size, attachment.LocalPath)
		}
		output.WriteString("\n")
	}
	return output.String()
}

func queryFingerprint(queries []string) string {
	digest := sha256.Sum256([]byte(strings.Join(queries, "\x00")))
	return hex.EncodeToString(digest[:])
}

func defaultText(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
