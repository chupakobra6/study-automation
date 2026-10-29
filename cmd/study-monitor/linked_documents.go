package main

import (
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
)

type linkedDocument struct {
	URL     string                 `json:"url"`
	Sources []linkedDocumentSource `json:"sources"`
}

type linkedDocumentSource struct {
	EventID string `json:"event_id"`
	Context string `json:"message_context"`
}

var reportURL = regexp.MustCompile(`https?://[^\s<>"\x60]+`)
var telegramMessageHeader = regexp.MustCompile(`(?m)^- \d{4}-\d{2}-\d{2} \d{2}:\d{2} \| ([^\n|]+) \| `)

// This is a document-service allowlist, not a judgement about the linked content.
// In particular, a captionless document still needs a separate review finding.
func sharedDocumentURL(raw, context string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil {
		return false
	}
	documentExtension := false
	for _, value := range []string{u.Path, u.Query().Get("filename")} {
		switch strings.ToLower(path.Ext(value)) {
		case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".heic", ".svg", ".mp4", ".mov", ".avi", ".webm", ".mp3", ".ogg", ".wav":
			return false
		case ".pdf", ".doc", ".docx", ".odt", ".rtf", ".txt", ".xls", ".xlsx", ".ods", ".csv", ".tsv", ".ppt", ".pptx", ".odp":
			documentExtension = true
		}
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) == 0 {
		return false
	}
	switch strings.ToLower(u.Hostname()) {
	case "docs.yandex.ru":
		return (parts[0] == "edit" && len(parts) > 1) || (parts[0] == "showcaptchafast" && u.RawQuery != "")
	case "docs.google.com":
		return len(parts) > 2 && (parts[0] == "spreadsheets" || parts[0] == "document") && parts[1] == "d"
	case "disk.yandex.ru", "disk.yandex.com", "disk.360.yandex.ru":
		public := ((parts[0] == "d" || parts[0] == "i") && len(parts) > 1) || (parts[0] == "public" && u.Query().Get("hash") != "")
		return public && (documentExtension || documentContext(context))
	}
	return false
}

func documentContext(context string) bool {
	context = telegramMessageHeader.ReplaceAllString(context, "")
	context = reportURL.ReplaceAllString(context, "")
	words := strings.Fields(normalizeSearch(context))
	for _, word := range words {
		for _, stem := range []string{"таблиц", "ведомост", "документ", "методич", "конспект", "презентац"} {
			if strings.HasPrefix(word, stem) {
				return true
			}
		}
		switch word {
		case "document", "spreadsheet", "table", "pdf", "docx", "xlsx":
			return true
		}
	}
	normalized := strings.Join(words, " ")
	return strings.Contains(normalized, "условия задания") || strings.Contains(normalized, "условие задания") || strings.Contains(normalized, "текст задания") || strings.Contains(normalized, "assignment instructions")
}

func collectLinkedDocuments(documents map[string]*linkedDocument, eventID, report string) {
	headers := telegramMessageHeader.FindAllStringSubmatchIndex(report, -1)
	for _, position := range reportURL.FindAllStringIndex(report, -1) {
		// Trim report/Markdown punctuation while keeping the original source URL,
		// including captcha-wrapper parameters and signatures.
		raw := strings.TrimRight(report[position[0]:position[1]], ").,;!?:]}")
		context := linkedDocumentContext(report, position[0], headers)
		// Opaque Disk links can lead to videos or folders. Only explicit document
		// context or a document filename makes them mandatory document references.
		if !sharedDocumentURL(raw, context) {
			continue
		}
		document := documents[raw]
		if document == nil {
			document = &linkedDocument{URL: raw, Sources: []linkedDocumentSource{}}
			documents[raw] = document
		}
		source := linkedDocumentSource{EventID: eventID, Context: context}
		duplicate := false
		for _, existing := range document.Sources {
			if existing == source {
				duplicate = true
				break
			}
		}
		if !duplicate {
			document.Sources = append(document.Sources, source)
		}
	}
}

func linkedDocumentContext(report string, position int, headers [][]int) string {
	start, end := 0, len(report)
	for index, header := range headers {
		if header[0] > position {
			break
		}
		start = header[0]
		if index > 0 {
			previous := headers[index-1]
			// Keep the preceding announcement when a separate message only shares
			// its URL. Never join messages from different chats/forum topics.
			if report[previous[2]:previous[3]] == report[header[2]:header[3]] {
				start = previous[0]
			}
		}
		end = len(report)
		if index+1 < len(headers) {
			end = headers[index+1][0]
		}
	}
	context := []rune(strings.TrimSpace(report[start:end]))
	const maxContext = 2400
	if len(context) > maxContext {
		// The exact URL and report path remain separately available.
		context = append(context[:maxContext], []rune("… [see event report]")...)
	}
	return string(context)
}

func sortedLinkedDocuments(documents map[string]*linkedDocument) []linkedDocument {
	keys := make([]string, 0, len(documents))
	for key := range documents {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]linkedDocument, 0, len(keys))
	for _, key := range keys {
		result = append(result, *documents[key])
	}
	return result
}
