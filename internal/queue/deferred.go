package queue

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// A deferral records an unsuccessful review, never an acknowledgment. Evidence,
// a justified time, or a newer event from the same source can reopen review.
type Deferral struct {
	ID         string            `json:"id"`
	Reason     string            `json:"reason"`
	RetryAt    *time.Time        `json:"retry_at,omitempty"`
	Evidence   map[string]string `json:"evidence,omitempty"`
	ReviewedAt time.Time         `json:"reviewed_at"`
}

func EvidenceFingerprint(path string) string {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "unavailable"
	}
	file, err := os.Open(path)
	if err != nil {
		return "unavailable"
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "unavailable"
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func (d Deferral) ready(now time.Time) bool {
	if d.RetryAt != nil && !now.Before(*d.RetryAt) {
		return true
	}
	for path, fingerprint := range d.Evidence {
		if EvidenceFingerprint(path) != fingerprint {
			return true
		}
	}
	return false
}

func (s *Store) deferrals() (map[string]Deferral, map[string]time.Time, error) {
	lastReviews := map[string]time.Time{}
	result := map[string]Deferral{}
	paths, err := filepath.Glob(filepath.Join(s.dir, "acks", "*", "ack.json"))
	if err != nil {
		return nil, nil, err
	}
	for _, path := range paths {
		if strings.HasPrefix(filepath.Base(filepath.Dir(path)), ".") {
			continue
		}
		var ack acknowledgment
		if err := readJSON(path, &ack); err != nil {
			return nil, nil, err
		}
		for _, source := range ack.Sources {
			if ack.CreatedAt.After(lastReviews[source]) {
				lastReviews[source] = ack.CreatedAt
			}
		}
		for _, value := range ack.Deferred {
			old, exists := result[value.ID]
			if !exists || value.ReviewedAt.After(old.ReviewedAt) {
				result[value.ID] = value
			}
		}
	}
	return result, lastReviews, nil
}

type DependencyWait struct {
	Source         string     `json:"source"`
	EventID        string     `json:"event_id"`
	Reason         string     `json:"reason"`
	RetryAt        *time.Time `json:"retry_at,omitempty"`
	EvidenceChange bool       `json:"evidence_change,omitempty"`
}

func (s *Store) selectReady(result *Summary, limit int, now time.Time) error {
	deferred, lastReviews, err := s.deferrals()
	if err != nil {
		return err
	}
	lanes := map[string][]Item{}
	blockedSources := map[string]bool{}
	latestSourceEvent := map[string]time.Time{}
	for _, item := range result.Items {
		if item.CreatedAt.After(latestSourceEvent[item.Source]) {
			latestSourceEvent[item.Source] = item.CreatedAt
		}
	}
	for _, item := range result.Items {
		if blockedSources[item.Source] {
			result.Waiting++
			continue
		}
		if d, ok := deferred[item.ID]; ok {
			item.Deferred = &d
			if !d.ready(now) && !latestSourceEvent[item.Source].After(d.ReviewedAt) {
				result.Deferred++
				blockedSources[item.Source] = true
				result.WaitingSources = append(result.WaitingSources, DependencyWait{Source: item.Source, EventID: item.ID, Reason: compactDetail(d.Reason), RetryAt: d.RetryAt, EvidenceChange: len(d.Evidence) > 0})
				continue
			}
		}
		lanes[item.Source] = append(lanes[item.Source], item)
	}
	// The queue already aggregates each source. Rotate those source lanes by the
	// last committed review, then round-robin within the unchanged batch budget.
	// This lets fresh work and restored retries progress even when a single large
	// report consumes the byte budget. Each lane retains chronological order.
	sources := []string{}
	for source := range lanes {
		sources = append(sources, source)
	}
	sort.Slice(sources, func(i, j int) bool {
		a, b := lastReviews[sources[i]], lastReviews[sources[j]]
		if !a.Equal(b) {
			return a.Before(b)
		}
		a, b = lanes[sources[i]][0].CreatedAt, lanes[sources[j]][0].CreatedAt
		if !a.Equal(b) {
			return a.Before(b)
		}
		return lanes[sources[i]][0].ID < lanes[sources[j]][0].ID
	})
	result.Items = []Item{}
	for more := true; more; {
		more = false
		for _, source := range sources {
			if len(lanes[source]) > 0 {
				result.Items = append(result.Items, lanes[source][0])
				lanes[source] = lanes[source][1:]
				more = true
			}
		}
	}
	result.Truncated = len(result.Items) > limit
	return nil
}

func (s *Store) validateDeferrals(completed []string, deferred []Deferral, receipt []byte, now time.Time) error {
	seen := map[string]bool{}
	for _, id := range completed {
		seen[id] = true
	}
	for i := range deferred {
		d := &deferred[i]
		if seen[d.ID] {
			return errors.New("duplicate review event ID")
		}
		seen[d.ID] = true
		item, err := s.inspect(d.ID)
		if err != nil {
			return err
		}
		if item.Deferred != nil && item.Deferred.ReviewedAt.After(d.ReviewedAt) {
			return fmt.Errorf("event %s has a newer review; prepare again", d.ID)
		}
		if item.Acknowledged {
			return fmt.Errorf("acknowledged event %s cannot become blocked", d.ID)
		}
		if strings.TrimSpace(d.Reason) == "" {
			return fmt.Errorf("blocked event %s needs a reason and disposition", d.ID)
		}
		if len(d.Evidence) == 0 && d.RetryAt == nil {
			return fmt.Errorf("blocked event %s needs an evidence change or retry time", d.ID)
		}
		// A repeated identical receipt is idempotent even after its retry time.
		same := item.Deferred != nil && item.Deferred.ReviewedAt.Equal(d.ReviewedAt)
		if d.RetryAt != nil && !d.RetryAt.After(now) && !same {
			return fmt.Errorf("blocked event %s retry time must be in the future", d.ID)
		}
		allowed := map[string]bool{}
		for _, path := range item.RequiredEvidence {
			allowed[filepath.Clean(path)] = true
		}
		for path, fingerprint := range d.Evidence {
			if !allowed[path] {
				return fmt.Errorf("retry evidence is outside event %s", d.ID)
			}
			if EvidenceFingerprint(path) != fingerprint {
				return fmt.Errorf("retry evidence changed during review: %s", path)
			}
		}
	}
	// Refuse a later completion while an earlier delta of that source remains
	// pending. Consecutive completed events in this same transaction are allowed.
	paths, err := filepath.Glob(filepath.Join(s.dir, "events", "*", "item.json"))
	if err != nil {
		return err
	}
	acks, err := s.acknowledged()
	if err != nil {
		return err
	}
	items := []Item{}
	for _, path := range paths {
		if strings.HasPrefix(filepath.Base(filepath.Dir(path)), ".") {
			continue
		}
		var item Item
		if err := readJSON(path, &item); err != nil {
			return err
		}
		if _, ok := acks[item.ID]; !ok {
			items = append(items, item)
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].ID < items[j].ID
		}
		return items[i].CreatedAt.Before(items[j].CreatedAt)
	})
	done := map[string]bool{}
	for _, id := range completed {
		done[id] = true
	}
	prior := map[string]string{}
	for _, item := range items {
		if done[item.ID] {
			if id := prior[item.Source]; id != "" {
				return fmt.Errorf("event %s must wait for earlier source event %s", item.ID, id)
			}
		} else if prior[item.Source] == "" {
			prior[item.Source] = item.ID
		}
	}
	return nil
}

// ValidateReviewState uses the same locked state checks as the committing path.
func ValidateReviewState(dir string, completed []string, deferred []Deferral, receipt []byte, now time.Time) error {
	s, err := Open(dir)
	if err != nil {
		return err
	}
	defer s.Close()
	return s.validateDeferrals(completed, deferred, receipt, now)
}
