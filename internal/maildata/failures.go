package maildata

import (
	"encoding/base64"
	"errors"
	"net"
	"os"
	"sort"
	"time"
)

// ItemFailure remains in the source checkpoint until the item succeeds or is
// explicitly retried after repair. Error codes never contain message bodies,
// HTTP response bodies, URLs or credentials.
type ItemFailure struct {
	ID          string    `json:"id"`
	Folder      string    `json:"folder,omitempty"`
	UID         uint32    `json:"uid,omitempty"`
	Attempts    int       `json:"attempts"`
	Stage       string    `json:"stage"`
	ErrorCode   string    `json:"error_code"`
	FirstFailed time.Time `json:"first_failed_at"`
	LastFailed  time.Time `json:"last_failed_at"`
	NextRetryAt time.Time `json:"next_retry_at,omitzero"`
	Terminal    bool      `json:"terminal"`
}

func (failure ItemFailure) Due(now time.Time) bool {
	return !failure.Terminal && !now.Before(failure.NextRetryAt)
}

func (failure ItemFailure) Failed(id, stage string, err error, now time.Time) ItemFailure {
	failure.ID = id
	failure.Attempts++
	failure.Stage = stage
	failure.ErrorCode = errorCode(err)
	if failure.FirstFailed.IsZero() {
		failure.FirstFailed = now.UTC()
	}
	failure.LastFailed = now.UTC()
	failure.Terminal = failure.Attempts >= 3
	failure.NextRetryAt = time.Time{}
	if !failure.Terminal {
		delay := 15 * time.Minute
		if failure.Attempts == 2 {
			delay = time.Hour
		}
		failure.NextRetryAt = now.UTC().Add(delay)
	}
	return failure
}

// Reset makes one repaired item eligible on the next sync, with a fresh bounded
// retry budget. Keeping the entry is essential for IMAP UIDs below the cursor.
func (failure ItemFailure) Reset() ItemFailure {
	failure.Attempts = 0
	failure.Terminal = false
	failure.NextRetryAt = time.Time{}
	return failure
}

func FailureSummary(failures map[string]ItemFailure) (pending, terminal int, items []ItemFailure) {
	for _, failure := range failures {
		items = append(items, failure)
		if failure.Terminal {
			terminal++
		} else {
			pending++
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return pending, terminal, items
}

func errorCode(err error) string {
	var pathErr *os.PathError
	var netErr net.Error
	var base64Err base64.CorruptInputError
	switch {
	case errors.As(err, &pathErr):
		return "local-storage"
	case errors.As(err, &netErr):
		return "network"
	case errors.As(err, &base64Err):
		return "invalid-base64"
	default:
		return "item-processing-failed"
	}
}
