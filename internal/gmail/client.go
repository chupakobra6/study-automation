package gmail

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const defaultAPIBase = "https://gmail.googleapis.com/gmail/v1"

type Client struct {
	HTTP        *http.Client
	APIBase     string
	AccessToken string
}

type MessageRef struct {
	ID       string `json:"id"`
	ThreadID string `json:"threadId"`
}

type Message struct {
	ID           string   `json:"id"`
	ThreadID     string   `json:"threadId"`
	HistoryID    string   `json:"historyId"`
	InternalDate string   `json:"internalDate"`
	LabelIDs     []string `json:"labelIds"`
	Snippet      string   `json:"snippet"`
	Payload      Part     `json:"payload"`
}

type Part struct {
	PartID   string   `json:"partId"`
	MIMEType string   `json:"mimeType"`
	Filename string   `json:"filename"`
	Headers  []Header `json:"headers"`
	Body     Body     `json:"body"`
	Parts    []Part   `json:"parts"`
}

type Header struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type Body struct {
	AttachmentID string `json:"attachmentId"`
	Size         int64  `json:"size"`
	Data         string `json:"data"`
}

type Profile struct {
	EmailAddress  string `json:"emailAddress"`
	MessagesTotal int64  `json:"messagesTotal"`
	HistoryID     string `json:"historyId"`
}

func (c *Client) base() string {
	if strings.TrimSpace(c.APIBase) != "" {
		return strings.TrimRight(c.APIBase, "/")
	}
	return defaultAPIBase
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) Profile(ctx context.Context) (Profile, error) {
	var profile Profile
	err := c.getJSON(ctx, c.base()+"/users/me/profile", &profile)
	return profile, err
}

func (c *Client) ListUnseenMessages(ctx context.Context, query string, limit int, seen map[string]bool) ([]MessageRef, error) {
	if limit <= 0 {
		return nil, nil
	}
	refs := make([]MessageRef, 0, min(limit, 100))
	pageToken := ""
	visitedTokens := make(map[string]bool)
	listedIDs := make(map[string]bool)
	for len(refs) < limit {
		remaining := limit - len(refs)
		pageSize := min(remaining, 500)
		values := url.Values{
			"maxResults": {fmt.Sprintf("%d", pageSize)},
			"q":          {query},
		}
		if pageToken != "" {
			values.Set("pageToken", pageToken)
		}
		var response struct {
			Messages      []MessageRef `json:"messages"`
			NextPageToken string       `json:"nextPageToken"`
		}
		if err := c.getJSON(ctx, c.base()+"/users/me/messages?"+values.Encode(), &response); err != nil {
			return nil, err
		}
		for _, ref := range response.Messages {
			if ref.ID != "" && !seen[ref.ID] && !listedIDs[ref.ID] {
				refs = append(refs, ref)
				listedIDs[ref.ID] = true
			}
		}
		if response.NextPageToken == "" {
			break
		}
		pageToken = response.NextPageToken
		if visitedTokens[pageToken] {
			return nil, fmt.Errorf("Gmail list returned a repeated page token")
		}
		visitedTokens[pageToken] = true
	}
	if len(refs) > limit {
		refs = refs[:limit]
	}
	return refs, nil
}

func (c *Client) GetMessage(ctx context.Context, id string) (Message, error) {
	var message Message
	endpoint := c.base() + "/users/me/messages/" + url.PathEscape(id) + "?format=full"
	err := c.getJSON(ctx, endpoint, &message)
	return message, err
}

func (c *Client) GetAttachment(ctx context.Context, messageID, attachmentID string) ([]byte, error) {
	endpoint := c.base() + "/users/me/messages/" + url.PathEscape(messageID) +
		"/attachments/" + url.PathEscape(attachmentID)
	var response struct {
		Data string `json:"data"`
	}
	if err := c.getJSON(ctx, endpoint, &response); err != nil {
		return nil, err
	}
	return decodeBase64URL(response.Data)
}

func (c *Client) getJSON(ctx context.Context, endpoint string, target any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.AccessToken)
	req.Header.Set("Accept", "application/json")
	response, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("Gmail API request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("Gmail API %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		return fmt.Errorf("decode Gmail API response: %w", err)
	}
	return nil
}
