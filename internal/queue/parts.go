package queue

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// The collector's Markdown envelope travels with the report, so crash recovery
// splits exactly the same immutable input as normal completion does.
const partPrefix = "<!-- study-monitor-part "

type reportPart struct {
	Body             []byte
	RequiredEvidence []string `json:"required_evidence"`
}

func splitReport(body []byte, evidence []string) ([]reportPart, error) {
	if !bytes.HasPrefix(body, []byte(partPrefix)) {
		return []reportPart{{Body: body, RequiredEvidence: evidence}}, nil
	}
	var parts []reportPart
	for len(body) > 0 {
		lineEnd := bytes.IndexByte(body, '\n')
		if lineEnd < 0 || !bytes.HasPrefix(body, []byte(partPrefix)) || !bytes.HasSuffix(body[:lineEnd], []byte(" -->")) {
			return nil, fmt.Errorf("invalid review-part envelope")
		}
		var part reportPart
		if err := json.Unmarshal(body[len(partPrefix):lineEnd-4], &part); err != nil {
			return nil, fmt.Errorf("invalid review-part metadata: %w", err)
		}
		body = body[lineEnd+1:]
		next := bytes.Index(body, []byte("\n"+partPrefix))
		if next < 0 {
			part.Body, body = body, nil
		} else {
			part.Body, body = body[:next+1], body[next+1:]
		}
		if len(bytes.TrimSpace(part.Body)) == 0 {
			return nil, fmt.Errorf("empty review part")
		}
		parts = append(parts, part)
	}
	return parts, nil
}
