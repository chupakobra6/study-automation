package githubstudy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

func (c Client) project(ctx context.Context, project Project) (scope, error) {
	endpoint := fmt.Sprintf("orgs/%s/projectsV2/%d", project.Owner, project.Number)
	webURL := fmt.Sprintf("https://github.com/orgs/%s/projects/%d", project.Owner, project.Number)
	var metadata struct {
		ID                        int64
		Title, Description, State string
		ShortDescription          string `json:"short_description"`
	}
	if _, err := c.get(ctx, endpoint, &metadata); err != nil {
		return scope{}, err
	}
	if metadata.ID == 0 || metadata.Title == "" {
		return scope{}, errors.New("GitHub project metadata is missing")
	}
	fields, err := c.list(ctx, endpoint+"/fields?per_page=100")
	if err != nil {
		return scope{}, err
	}
	var ids []string
	for _, raw := range fields {
		var field struct {
			ID       int64
			DataType string `json:"data_type"`
		}
		if err := json.Unmarshal(raw, &field); err != nil || field.ID == 0 {
			return scope{}, errors.New("invalid GitHub project field")
		}
		switch field.DataType {
		case "text", "date", "single_select", "number", "iteration":
			ids = append(ids, fmt.Sprint(field.ID))
		}
	}
	sort.Strings(ids)
	query := url.Values{"per_page": {"100"}}
	if len(ids) > 0 {
		query.Set("fields", strings.Join(ids, ","))
	}
	items, err := c.list(ctx, endpoint+"/items?"+query.Encode())
	if err != nil {
		return scope{}, err
	}
	current := scope{Records: map[string]record{
		"project": {Kind: "project", Title: metadata.Title, URL: webURL, Body: metadata.Description + "\n" + metadata.ShortDescription, State: metadata.State},
	}}
	for _, raw := range items {
		var item struct {
			ID          int64
			ContentType string `json:"content_type"`
			Content     *issue
			ArchivedAt  string `json:"archived_at"`
			Fields      []struct {
				Name     string
				DataType string `json:"data_type"`
				Value    json.RawMessage
			}
		}
		if err := json.Unmarshal(raw, &item); err != nil || item.ID == 0 || item.Content == nil || item.Content.Title == "" {
			return scope{}, errors.New("GitHub project contains unreadable or redacted content; coverage incomplete")
		}
		r := item.Content.record()
		r.Kind = "project item"
		if r.URL == "" {
			r.URL = webURL + "?pane=issue&itemId=" + fmt.Sprint(item.ID)
		}
		r.Fields["content_type"] = item.ContentType
		if item.ArchivedAt != "" {
			r.Fields["archived"] = "true"
		}
		for _, field := range item.Fields {
			switch field.DataType {
			case "text", "date", "single_select", "number", "iteration":
				var value any
				if err := json.Unmarshal(field.Value, &value); err != nil {
					return scope{}, errors.New("invalid GitHub project field value")
				}
				if field.DataType == "single_select" {
					if selected, ok := value.(map[string]any); ok {
						value = selected["name"]
					}
				}
				if rich, ok := value.(map[string]any); ok {
					if raw, exists := rich["raw"]; exists {
						value = raw
					}
				}
				text, ok := value.(string)
				if !ok {
					normalized, _ := json.Marshal(value)
					text = string(normalized)
				}
				r.Fields[field.Name] = text
			}
		}
		current.Records[fmt.Sprint(item.ID)] = r
	}
	return current, nil
}
