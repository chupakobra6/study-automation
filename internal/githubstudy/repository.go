package githubstudy

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
)

func (c Client) repository(ctx context.Context, repo Repository, previous, next map[string]scope) {
	prefix := "repo:" + repo.Name + ":"
	issues, err := c.issues(ctx, repo.Name)
	next[prefix+"issues"] = withError(scope{Records: issues}, previous[prefix+"issues"], err)
	branches, err := c.list(ctx, "repos/"+repo.Name+"/branches?per_page=100")
	if err != nil {
		next[prefix+"branches"] = withError(scope{}, previous[prefix+"branches"], err)
		for key, old := range previous {
			if strings.HasPrefix(key, prefix+"branch:") {
				next[key] = withError(scope{}, old, err)
			}
		}
		return
	}
	next[prefix+"branches"] = scope{Records: map[string]record{}}
	heads := map[string]string{}
	for _, raw := range branches {
		var branch struct {
			Name   string
			Commit struct{ SHA string }
		}
		if err := json.Unmarshal(raw, &branch); err != nil || branch.Name == "" || !validSHA(branch.Commit.SHA) {
			next[prefix+"branches"] = withError(scope{}, previous[prefix+"branches"], errors.New("invalid GitHub branch metadata"))
			for key, old := range previous {
				if strings.HasPrefix(key, prefix+"branch:") {
					next[key] = old
				}
			}
			return
		}
		for _, branchPrefix := range repo.BranchPrefixes {
			if strings.HasPrefix(branch.Name, branchPrefix) {
				heads[branch.Name] = branch.Commit.SHA
			}
		}
		for _, exact := range repo.Branches {
			if exact == branch.Name {
				heads[exact] = branch.Commit.SHA
			}
		}
	}
	for _, exact := range repo.Branches {
		if _, exists := heads[exact]; !exists {
			key := prefix + "branch:" + exact
			next[key] = withError(scope{}, previous[key], fmt.Errorf("configured branch %q is unavailable", exact))
		}
	}
	for branch, head := range heads {
		key := prefix + "branch:" + branch
		old := previous[key]
		if old.Head == head && old.Records != nil {
			old.Gap = ""
			next[key] = old
			continue
		}
		current, err := c.tree(ctx, repo, branch, head, old)
		next[key] = withError(current, old, err)
	}
	for key := range previous {
		if strings.HasPrefix(key, prefix+"branch:") {
			if _, exists := next[key]; !exists {
				next[key] = scope{Records: map[string]record{}}
			}
		}
	}
}

func validSHA(s string) bool {
	decoded, err := hex.DecodeString(s)
	return err == nil && len(decoded) == sha1.Size
}

func (c Client) tree(ctx context.Context, repo Repository, branch, head string, old scope) (scope, error) {
	var tree struct {
		Truncated bool
		Tree      *[]struct {
			Path, Type, SHA string
			Size            int
		}
	}
	if _, err := c.get(ctx, "repos/"+repo.Name+"/git/trees/"+head+"?recursive=1", &tree); err != nil {
		return scope{}, err
	}
	if tree.Truncated || tree.Tree == nil {
		return scope{}, errors.New("GitHub tree is missing or truncated; coverage incomplete")
	}
	patterns := make([]*regexp.Regexp, 0, len(repo.Include))
	for _, pattern := range repo.Include {
		patterns = append(patterns, regexp.MustCompile(pattern))
	}
	current := scope{Head: head, Records: map[string]record{}}
	for _, file := range *tree.Tree {
		if file.Type != "blob" {
			continue
		}
		include := false
		for _, pattern := range patterns {
			include = include || pattern.MatchString(file.Path)
		}
		if !include {
			continue
		}
		if path.Clean(file.Path) != file.Path || strings.HasPrefix(file.Path, "/") || strings.HasPrefix(file.Path, "../") || !validSHA(file.SHA) || file.Size < 0 || file.Size > 2<<20 {
			return scope{}, errors.New("GitHub content has invalid path, hash or size")
		}
		if previous, exists := old.Records[file.Path]; exists && previous.BlobSHA == file.SHA {
			current.Records[file.Path] = previous
			continue
		}
		var blob struct {
			Encoding, Content, SHA string
			Size                   int
		}
		if _, err := c.get(ctx, "repos/"+repo.Name+"/git/blobs/"+file.SHA, &blob); err != nil {
			return scope{}, err
		}
		if blob.Encoding != "base64" || blob.SHA != file.SHA {
			return scope{}, errors.New("GitHub blob has unexpected encoding or hash")
		}
		body, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(blob.Content, "\n", ""))
		if err != nil || len(body) != file.Size || len(body) != blob.Size {
			return scope{}, errors.New("GitHub blob content failed size or encoding validation")
		}
		gitObject := fmt.Appendf(nil, "blob %d\x00", len(body))
		digest := sha1.Sum(append(gitObject, body...))
		if hex.EncodeToString(digest[:]) != file.SHA {
			return scope{}, errors.New("GitHub blob content failed hash validation")
		}
		current.Records[file.Path] = record{Kind: "file", Title: repo.Name + " / " + branch + " / " + file.Path, URL: "https://github.com/" + repo.Name + "/blob/" + url.PathEscape(branch) + "/" + escapePath(file.Path), Body: string(body), BlobSHA: file.SHA}
	}
	if len(current.Records) == 0 {
		return scope{}, fmt.Errorf("branch %q has no files matching configured include patterns", branch)
	}
	return current, nil
}

func escapePath(s string) string {
	parts := strings.Split(s, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

type issue struct {
	ID                 int64
	Number             int
	Title, Body, State string
	HTMLURL            string          `json:"html_url"`
	StateReason        string          `json:"state_reason"`
	PullRequest        json.RawMessage `json:"pull_request"`
	Labels             []struct{ Name string }
	Assignees          []struct{ Login string }
	Milestone          *struct {
		Title, State string
		DueOn        string `json:"due_on"`
	}
}

func (item issue) record() record {
	fields := map[string]string{}
	var labels, assignees []string
	for _, label := range item.Labels {
		labels = append(labels, label.Name)
	}
	for _, assignee := range item.Assignees {
		assignees = append(assignees, assignee.Login)
	}
	sort.Strings(labels)
	sort.Strings(assignees)
	if len(labels) > 0 {
		fields["labels"] = strings.Join(labels, ", ")
	}
	if len(assignees) > 0 {
		fields["assignees"] = strings.Join(assignees, ", ")
	}
	if item.StateReason != "" {
		fields["state_reason"] = item.StateReason
	}
	if item.Milestone != nil {
		fields["milestone"] = item.Milestone.Title
		fields["milestone_due"] = item.Milestone.DueOn
		fields["milestone_state"] = item.Milestone.State
	}
	return record{Kind: "issue", Title: item.Title, URL: item.HTMLURL, Body: item.Body, State: item.State, Fields: fields}
}

func (c Client) issues(ctx context.Context, repo string) (map[string]record, error) {
	items, err := c.list(ctx, "repos/"+repo+"/issues?state=all&per_page=100")
	if err != nil {
		return nil, err
	}
	records := map[string]record{}
	for _, raw := range items {
		var item issue
		if err := json.Unmarshal(raw, &item); err != nil || item.ID == 0 || item.Title == "" {
			return nil, errors.New("invalid GitHub issue")
		}
		if len(item.PullRequest) > 0 && string(item.PullRequest) != "null" {
			continue
		}
		records[fmt.Sprint(item.ID)] = item.record()
	}
	return records, nil
}
