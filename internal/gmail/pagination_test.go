package gmail

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestListingContinuesEmptyPageWithTokenAndDeduplicates(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var response any
		switch r.URL.Query().Get("pageToken") {
		case "":
			response = map[string]any{"nextPageToken": "two"}
		case "two":
			response = map[string]any{"messages": []MessageRef{{ID: "a"}, {ID: "a"}}, "nextPageToken": "three"}
		case "three":
			response = map[string]any{"messages": []MessageRef{{ID: "b"}}}
		default:
			t.Errorf("unexpected page")
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()
	client := &Client{APIBase: server.URL, HTTP: server.Client()}
	refs, err := client.ListUnseenMessages(context.Background(), "from:example.edu", 2, nil)
	if err != nil || len(refs) != 2 || refs[0].ID != "a" || refs[1].ID != "b" {
		t.Fatalf("refs=%+v err=%v", refs, err)
	}
}

func TestCatchupQueryRetainsAcademicWindowAfterOutage(t *testing.T) {
	now := time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)
	got := catchupQuery("from:(github.com) (invitation OR invited) newer_than:30d", "2026-07-01T00:00:00Z", "2026-09-15T00:00:00Z", now)
	if got != "from:(github.com) (invitation OR invited) after:2026/05/31" {
		t.Fatal(got)
	}
}
