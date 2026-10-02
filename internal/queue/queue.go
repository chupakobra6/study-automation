package queue

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

const maxReportBytes = 16 << 20

// MaxAnalysisBatch bounds the number of immutable events a model may review and
// acknowledge in one pass. Larger backlogs remain pending for later runs.
const MaxAnalysisBatch = 8
const MaxAnalysisBatchBytes = 64 << 10

type Item struct {
	ID               string    `json:"id"`
	Source           string    `json:"source"`
	Kind             string    `json:"kind"`
	Status           string    `json:"status"`
	Detail           string    `json:"detail"`
	CreatedAt        time.Time `json:"created_at"`
	ReportPath       string    `json:"report_path"`
	ReportSHA256     string    `json:"report_sha256"`
	RequiredEvidence []string  `json:"required_evidence,omitempty"`
	Acknowledged     bool      `json:"acknowledged"`
	ReceiptPath      string    `json:"receipt_path,omitempty"`
	Deferred         *Deferral `json:"deferred,omitempty"`
}

type Failure struct {
	Source  string `json:"source"`
	Status  string `json:"status"`
	Detail  string `json:"detail"`
	EventID string `json:"event_id"`
}

type Summary struct {
	Pending        int              `json:"pending_count"`
	Deferred       int              `json:"deferred_count,omitempty"`
	Waiting        int              `json:"waiting_dependency_count,omitempty"`
	OldestAt       *time.Time       `json:"oldest_at,omitempty"`
	BySource       map[string]int   `json:"by_source"`
	Items          []Item           `json:"items"`
	Truncated      bool             `json:"truncated"`
	Failures       []Failure        `json:"active_failures"`
	WaitingSources []DependencyWait `json:"waiting_sources,omitempty"`
}

type Outcome struct {
	Source                string   `json:"source"`
	Status                string   `json:"status"`
	Detail                string   `json:"detail"`
	Added                 int      `json:"added"`
	Attachments           int      `json:"attachments"`
	RequiredEvidence      []string `json:"required_evidence,omitempty"`
	NeedsAnalysis         bool     `json:"needs_analysis"`
	AnalysisDecisionKnown bool     `json:"analysis_decision_known,omitempty"`
	Failed                bool     `json:"failed"`
}

type inflight struct {
	ID         string    `json:"id"`
	Source     string    `json:"source"`
	ReportPath string    `json:"report_path"`
	BeforeHash string    `json:"before_hash"`
	StartedAt  time.Time `json:"started_at"`
	Outcome    *Outcome  `json:"outcome,omitempty"`
	Report     []byte    `json:"report,omitempty"`
}

type sourceState struct {
	Source             string   `json:"source"`
	ReportFingerprint  string   `json:"report_fingerprint"`
	FailureFingerprint string   `json:"failure_fingerprint"`
	Failure            *Failure `json:"failure,omitempty"`
}

type acknowledgment struct {
	IDs              []string   `json:"ids"`
	CreatedAt        time.Time  `json:"created_at"`
	OriginalReceipt  string     `json:"original_receipt"`
	ReceiptSHA256    string     `json:"receipt_sha256"`
	ReviewedEvidence []string   `json:"reviewed_evidence,omitempty"`
	Deferred         []Deferral `json:"deferred,omitempty"`
	Sources          []string   `json:"sources,omitempty"`
}

// Store serializes collection and queue mutations. The OS releases the lock on crash.
type Store struct {
	dir  string
	lock *os.File
}

func Open(dir string) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("queue directory is required")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(abs, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("monitoring queue is in use: %w", err)
	}
	return &Store{dir: abs, lock: lock}, nil
}

func (s *Store) Close() error { return s.lock.Close() }

func (s *Store) Start(source, reportPath string, now time.Time) error {
	if source == "" {
		return errors.New("queue source is required")
	}
	if strings.TrimSpace(reportPath) == "" {
		return errors.New("latest source report path is required for crash recovery")
	}
	absoluteReport, err := filepath.Abs(reportPath)
	if err != nil {
		return err
	}
	reportPath = absoluteReport
	if _, err := os.Stat(filepath.Join(s.dir, "inflight.json")); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return err
		}
		return errors.New("recover the unfinished source before starting another")
	}
	data, err := readReport(reportPath)
	if err != nil {
		return err
	}
	id, err := newID()
	if err != nil {
		return err
	}
	return writeJSON(filepath.Join(s.dir, "inflight.json"), inflight{ID: id, Source: source, ReportPath: reportPath, BeforeHash: digest(data), StartedAt: now.UTC()})
}

func (s *Store) Complete(outcome Outcome) error {
	var pending inflight
	if err := readJSON(filepath.Join(s.dir, "inflight.json"), &pending); err != nil {
		return err
	}
	if outcome.Source != pending.Source {
		return errors.New("source result does not match inflight source")
	}
	body, err := readReport(pending.ReportPath)
	if err != nil {
		return err
	}
	pending.Outcome, pending.Report = &outcome, body
	// Persist the result and exact report together before publishing an event.
	if err := writeJSON(filepath.Join(s.dir, "inflight.json"), pending); err != nil {
		return err
	}
	return s.finish(pending, false)
}

// Recover must run before any collector can replace a source's latest report.
func (s *Store) Recover() error {
	var pending inflight
	err := readJSON(filepath.Join(s.dir, "inflight.json"), &pending)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if pending.Outcome != nil {
		return s.finish(pending, false)
	}
	body, err := readReport(pending.ReportPath)
	if err != nil {
		return err
	}
	if len(body) > 0 && digest(body) != pending.BeforeHash {
		pending.Report = body
		pending.Outcome = &Outcome{Source: pending.Source, Status: "recovered", Detail: "Source run was interrupted; inspect the preserved report before acknowledging", NeedsAnalysis: true}
		if err := s.finish(pending, true); err != nil {
			return err
		}
		return nil
	}
	return s.clearInflight()
}

func (s *Store) finish(pending inflight, recovered bool) error {
	outcome := *pending.Outcome
	state := sourceState{Source: outcome.Source}
	statePath := filepath.Join(s.dir, "sources", digest([]byte(outcome.Source))+".json")
	if err := readJSON(statePath, &state); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Availability is operational state. Only a source-declared academic delta,
	// preserved item, or required evidence enters the model's review queue.
	review := outcome.NeedsAnalysis || outcome.Added > 0 || outcome.Attachments > 0 || len(outcome.RequiredEvidence) > 0
	reportEventID := ""
	if review {
		body := pending.Report
		if len(body) == 0 {
			body = []byte(fmt.Sprintf("# %s\n\nStatus: %s\n%s\n", outcome.Source, outcome.Status, outcome.Detail))
		}
		fingerprint := reportFingerprint(body)
		if fingerprint != state.ReportFingerprint {
			reportEventID = pending.ID + "-report"
			parts, err := splitReport(body, outcome.RequiredEvidence)
			if err != nil {
				return err
			}
			for index, part := range parts {
				id := reportEventID
				if index > 0 {
					id = fmt.Sprintf("%s-%06d", reportEventID, index+1)
				}
				if err := s.enqueue(Item{ID: id, Source: outcome.Source, Kind: "report", Status: outcome.Status, Detail: outcome.Detail, CreatedAt: pending.StartedAt, RequiredEvidence: part.RequiredEvidence}, part.Body); err != nil {
					return err
				}
			}
			state.ReportFingerprint = fingerprint
		}
	}
	if !recovered {
		if outcome.Failed {
			fingerprint := digest([]byte(outcome.Status + "\x00" + outcome.Detail))
			if fingerprint != state.FailureFingerprint {
				state.FailureFingerprint = fingerprint
				state.Failure = &Failure{Source: outcome.Source, Status: outcome.Status, Detail: outcome.Detail, EventID: reportEventID}
			}
		} else if state.Failure != nil {
			state.Failure, state.FailureFingerprint = nil, ""
		}
	}
	if err := writeJSON(statePath, state); err != nil {
		return err
	}
	return s.clearInflight()
}

func (s *Store) clearInflight() error {
	if err := os.Remove(filepath.Join(s.dir, "inflight.json")); err != nil {
		return err
	}
	return syncDir(s.dir)
}

func (s *Store) enqueue(item Item, body []byte) error {
	item.ReportPath = filepath.Join(s.dir, "events", item.ID, "report.md")
	item.ReportSHA256 = digest(body)
	return s.bundle("events", item.ID, "item.json", item, "report.md", body)
}

func Summarize(dir string, limit int, now time.Time) (Summary, error) {
	s, err := Open(dir)
	if err != nil {
		return Summary{}, err
	}
	defer s.Close()
	if err := s.Recover(); err != nil {
		return Summary{}, err
	}
	return s.Summary(limit, now)
}

// ListEvents returns the immutable observations, including acknowledged ones.
// It does not acquire the writer lock and never changes queue state.
func ListEvents(dir string) ([]Item, error) {
	acks, err := (&Store{dir: dir}).acknowledged()
	if err != nil {
		return nil, err
	}
	paths, err := filepath.Glob(filepath.Join(dir, "events", "*", "item.json"))
	if err != nil {
		return nil, err
	}
	items := make([]Item, 0, len(paths))
	for _, path := range paths {
		if strings.HasPrefix(filepath.Base(filepath.Dir(path)), ".") {
			continue
		}
		var item Item
		if err := readJSON(path, &item); err != nil {
			return nil, fmt.Errorf("read event %s: %w", path, err)
		}
		if receipt, ok := acks[item.ID]; ok {
			item.Acknowledged = true
			item.ReceiptPath = receipt
		}
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].ID < items[j].ID
		}
		return items[i].CreatedAt.Before(items[j].CreatedAt)
	})
	return items, nil
}

func (s *Store) Summary(limit int, now time.Time) (Summary, error) {
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	result := Summary{BySource: map[string]int{}, Items: []Item{}, Failures: []Failure{}}
	if err := s.retireOperationalEvents(now); err != nil {
		return result, err
	}
	acks, err := s.acknowledged()
	if err != nil {
		return result, err
	}
	paths, err := filepath.Glob(filepath.Join(s.dir, "events", "*", "item.json"))
	if err != nil {
		return result, err
	}
	for _, path := range paths {
		if strings.HasPrefix(filepath.Base(filepath.Dir(path)), ".") {
			continue
		}
		var item Item
		if err := readJSON(path, &item); err != nil {
			return result, err
		}
		if _, ok := acks[item.ID]; ok {
			continue
		}
		result.Pending++
		result.BySource[item.Source]++
		item.Detail = compactDetail(item.Detail)
		result.Items = append(result.Items, item)
	}
	sort.Slice(result.Items, func(i, j int) bool {
		if result.Items[i].CreatedAt.Equal(result.Items[j].CreatedAt) {
			return result.Items[i].ID < result.Items[j].ID
		}
		return result.Items[i].CreatedAt.Before(result.Items[j].CreatedAt)
	})
	if len(result.Items) > 0 {
		value := result.Items[0].CreatedAt
		result.OldestAt = &value
	}
	if err := s.selectReady(&result, limit, now); err != nil {
		return result, err
	}
	if len(result.Items) > limit {
		result.Truncated = true
		result.Items = result.Items[:limit]
	}
	reviewBytes := 0
	for index, item := range result.Items {
		body, err := readReport(item.ReportPath)
		if err != nil {
			return result, err
		}
		if digest(body) != item.ReportSHA256 {
			return result, fmt.Errorf("immutable queue report %s is missing or altered", item.ID)
		}
		if limit == MaxAnalysisBatch && index > 0 && reviewBytes+len(body) > MaxAnalysisBatchBytes {
			result.Items = result.Items[:index]
			result.Truncated = true
			break
		}
		reviewBytes += len(body)
	}
	paths, err = filepath.Glob(filepath.Join(s.dir, "sources", "*.json"))
	if err != nil {
		return result, err
	}
	for _, path := range paths {
		var state sourceState
		if err := readJSON(path, &state); err != nil {
			return result, err
		}
		if state.Failure != nil {
			failure := *state.Failure
			failure.Detail = compactDetail(failure.Detail)
			result.Failures = append(result.Failures, failure)
		}
	}
	sort.Slice(result.Failures, func(i, j int) bool { return result.Failures[i].Source < result.Failures[j].Source })
	return result, nil
}

// Historical availability events remain immutable, but a machine receipt
// removes them from academic review. Current health lives in source state.
func (s *Store) retireOperationalEvents(now time.Time) error {
	acked, err := s.acknowledged()
	if err != nil {
		return err
	}
	paths, err := filepath.Glob(filepath.Join(s.dir, "events", "*", "item.json"))
	if err != nil {
		return err
	}
	ids := []string{}
	sources := map[string]bool{}
	for _, path := range paths {
		if strings.HasPrefix(filepath.Base(filepath.Dir(path)), ".") {
			continue
		}
		var item Item
		if err := readJSON(path, &item); err != nil {
			return err
		}
		if item.Kind != "failure" && item.Kind != "recovery" {
			continue
		}
		if _, exists := acked[item.ID]; exists {
			continue
		}
		ids = append(ids, item.ID)
		sources[item.Source] = true
	}
	if len(ids) == 0 {
		return nil
	}
	sort.Strings(ids)
	sourceNames := make([]string, 0, len(sources))
	for source := range sources {
		sourceNames = append(sourceNames, source)
	}
	sort.Strings(sourceNames)
	body := []byte("Source availability is recorded by the collector; no academic facts were reviewed.\n")
	ackID := digest([]byte("operational\x00" + strings.Join(ids, "\x00")))
	ack := acknowledgment{IDs: ids, CreatedAt: now.UTC(), OriginalReceipt: "machine-owned", ReceiptSHA256: digest(body), Sources: sourceNames}
	return s.bundle("acks", ackID, "ack.json", ack, "receipt.md", body)
}

func Inspect(dir, id string) (Item, error) {
	s, err := Open(dir)
	if err != nil {
		return Item{}, err
	}
	defer s.Close()
	return s.inspect(id)
}

func (s *Store) inspect(id string) (Item, error) {
	var item Item
	if !validID(id) {
		return item, errors.New("invalid queue item ID")
	}
	if err := readJSON(filepath.Join(s.dir, "events", id, "item.json"), &item); err != nil {
		return item, err
	}
	body, err := readReport(item.ReportPath)
	if err != nil {
		return item, err
	}
	if digest(body) != item.ReportSHA256 {
		return item, errors.New("immutable queue report is missing or altered")
	}
	acks, err := s.acknowledged()
	if err != nil {
		return item, err
	}
	item.ReceiptPath, item.Acknowledged = acks[id]
	if !item.Acknowledged {
		deferred, _, err := s.deferrals()
		if err != nil {
			return item, err
		}
		if value, ok := deferred[id]; ok {
			item.Deferred = &value
		}
	}
	return item, nil
}

func RecordReview(dir string, ids []string, deferred []Deferral, receiptPath string, reviewedEvidence []string, now time.Time) (Summary, error) {
	s, err := Open(dir)
	if err != nil {
		return Summary{}, err
	}
	defer s.Close()
	if len(ids)+len(deferred) == 0 || strings.TrimSpace(receiptPath) == "" {
		return Summary{}, errors.New("acknowledgment requires explicit item IDs and a receipt file")
	}
	if len(ids)+len(deferred) > MaxAnalysisBatch {
		return Summary{}, fmt.Errorf("acknowledgment batch has %d events; maximum is %d", len(ids)+len(deferred), MaxAnalysisBatch)
	}
	body, err := readReport(receiptPath)
	if err != nil {
		return Summary{}, err
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return Summary{}, errors.New("acknowledgment receipt is missing or empty")
	}
	if err := s.validateDeferrals(ids, deferred, body, now); err != nil {
		return Summary{}, err
	}
	var decisionReceipt struct {
		Decisions []struct {
			Kind    string `json:"kind"`
			Key     string `json:"key"`
			Outcome string `json:"outcome"`
		} `json:"decisions"`
	}
	if err := json.Unmarshal(body, &decisionReceipt); err != nil {
		return Summary{}, fmt.Errorf("invalid structured review: %w", err)
	}
	missingDispositions := []string{}
	for _, id := range ids {
		count := 0
		for _, decision := range decisionReceipt.Decisions {
			if decision.Kind == "event" && decision.Key == id {
				count++
				if decision.Outcome == "blocked" {
					return Summary{}, fmt.Errorf("blocked event %s cannot be acknowledged", id)
				}
				if decision.Outcome != "updated" && decision.Outcome != "no-action" {
					return Summary{}, fmt.Errorf("invalid event outcome for %s", id)
				}
			}
		}
		if count != 1 {
			missingDispositions = append(missingDispositions, id)
		}
	}
	if len(missingDispositions) > 0 {
		return Summary{}, fmt.Errorf("analysis receipt needs exactly one event disposition for: %s", strings.Join(missingDispositions, ", "))
	}
	reviewed, err := validateReviewedEvidence(reviewedEvidence, body)
	if err != nil {
		return Summary{}, err
	}
	sources := map[string]bool{}
	unique := map[string]bool{}
	selected := make([]string, 0, len(ids))
	required := map[string]bool{}
	for _, id := range ids {
		if unique[id] {
			return Summary{}, errors.New("duplicate acknowledgment ID")
		}
		unique[id] = true
		item, err := s.inspect(id)
		if err != nil {
			return Summary{}, err
		}
		if item.Acknowledged {
			old, err := readReport(item.ReceiptPath)
			if err != nil {
				return Summary{}, err
			}
			if digest(old) != digest(body) {
				return Summary{}, fmt.Errorf("item %s was already acknowledged with a different receipt", id)
			}
			continue
		}
		sources[item.Source] = true
		selected = append(selected, id)
		for _, path := range item.RequiredEvidence {
			required[filepath.Clean(path)] = true
		}
	}
	missingRequired := []string{}
	for path := range required {
		if !reviewed[path] {
			missingRequired = append(missingRequired, path)
		}
	}
	if len(missingRequired) > 0 {
		sort.Strings(missingRequired)
		return Summary{}, fmt.Errorf("event acknowledgment is missing required reviewed evidence: %s", strings.Join(missingRequired, ", "))
	}
	for _, d := range deferred {
		item, err := s.inspect(d.ID)
		if err != nil {
			return Summary{}, err
		}
		sources[item.Source] = true
	}
	if len(selected)+len(deferred) > 0 {
		sort.Strings(selected)
		reviewedPaths := make([]string, 0, len(reviewed))
		for path := range reviewed {
			reviewedPaths = append(reviewedPaths, path)
		}
		sort.Strings(reviewedPaths)
		ackID := digest([]byte(receiptPath + "\n" + digest(body)))
		sourceNames := []string{}
		for source := range sources {
			sourceNames = append(sourceNames, source)
		}
		sort.Strings(sourceNames)
		ack := acknowledgment{IDs: selected, CreatedAt: now.UTC(), OriginalReceipt: receiptPath, ReceiptSHA256: digest(body), ReviewedEvidence: reviewedPaths, Deferred: deferred, Sources: sourceNames}
		if err := s.bundle("acks", ackID, "ack.json", ack, "receipt.md", body); err != nil {
			return Summary{}, err
		}
	}
	return s.Summary(10, now)
}

func validateReviewedEvidence(paths []string, receipt []byte) (map[string]bool, error) {
	result := map[string]bool{}
	problems := []string{}
	for _, value := range paths {
		path := filepath.Clean(strings.TrimSpace(value))
		if path == "." || !filepath.IsAbs(path) {
			problems = append(problems, fmt.Sprintf("not an absolute path: %q", value))
			continue
		}
		if result[path] {
			problems = append(problems, "duplicate path: "+path)
			continue
		}
		result[path] = true
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			problems = append(problems, "not an available regular file: "+path)
			continue
		}
		if !bytes.Contains(receipt, []byte(path)) {
			problems = append(problems, "not cited in receipt: "+path)
		}
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("reviewed evidence validation failed: %s", strings.Join(problems, "; "))
	}
	return result, nil
}

func (s *Store) acknowledged() (map[string]string, error) {
	result := map[string]string{}
	paths, err := filepath.Glob(filepath.Join(s.dir, "acks", "*", "ack.json"))
	if err != nil {
		return nil, err
	}
	for _, path := range paths {
		if strings.HasPrefix(filepath.Base(filepath.Dir(path)), ".") {
			continue
		}
		var ack acknowledgment
		if err := readJSON(path, &ack); err != nil {
			return nil, err
		}
		for _, id := range ack.IDs {
			result[id] = filepath.Join(filepath.Dir(path), "receipt.md")
		}
	}
	return result, nil
}

func (s *Store) bundle(kind, id, metadataName string, metadata any, bodyName string, body []byte) error {
	parent := filepath.Join(s.dir, kind)
	target := filepath.Join(parent, id)
	if _, err := os.Stat(target); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	temp, err := os.MkdirTemp(parent, ".tmp-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	if err := durableFile(filepath.Join(temp, metadataName), append(data, '\n'), 0o400); err != nil {
		return err
	}
	if err := durableFile(filepath.Join(temp, bodyName), body, 0o400); err != nil {
		return err
	}
	if err := syncDir(temp); err != nil {
		return err
	}
	if err := os.Rename(temp, target); err != nil {
		return err
	}
	return syncDir(parent)
}

func readJSON(path string, target any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}

func readReport(path string) ([]byte, error) {
	if path == "" {
		return nil, nil
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, maxReportBytes+1))
	if len(body) > maxReportBytes {
		return nil, errors.New("queue report or receipt exceeds 16 MiB")
	}
	return body, err
}

func writeJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".queue-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	temp.Close()
	defer os.Remove(name)
	if err := durableFile(name, append(data, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func durableFile(path string, data []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

func syncDir(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}
func digest(data []byte) string { value := sha256.Sum256(data); return hex.EncodeToString(value[:]) }
func newID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}
func validID(id string) bool {
	prefix, suffix, ok := strings.Cut(id, "-")
	data, err := hex.DecodeString(prefix)
	if !ok || err != nil || len(data) != 16 {
		return false
	}
	if suffix == "report" || suffix == "failure" || suffix == "recovery" {
		return true
	}
	if !strings.HasPrefix(suffix, "report-") || len(suffix) != len("report-")+6 {
		return false
	}
	for _, char := range suffix[len("report-"):] {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

// Ignore only collector-run metadata for deduplication; saved evidence stays byte-exact.
func reportFingerprint(data []byte) string {
	lines := strings.Split(string(data), "\n")
	stable := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.HasPrefix(line, "Generated at:") || strings.HasPrefix(line, "- Generated:") || strings.HasPrefix(line, "- Previous checkpoint:") {
			continue
		}
		stable = append(stable, line)
	}
	return digest([]byte(strings.Join(stable, "\n")))
}

func compactDetail(value string) string {
	runes := []rune(value)
	if len(runes) > 240 {
		return string(runes[:239]) + "…"
	}
	return value
}
