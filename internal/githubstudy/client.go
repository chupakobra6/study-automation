// Package githubstudy collects allowlisted academic GitHub sources through REST.
package githubstudy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"
)

const maxResponseBytes = 16 << 20

// Client uses the existing gh authentication. Tokens never enter argv or reports.
// Get is injectable for deterministic fixtures; production always uses GET.
type Client struct {
	Get func(context.Context, string) ([]byte, http.Header, error)
}

type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > maxResponseBytes {
		return 0, errors.New("GitHub response exceeds 16 MiB")
	}
	return b.Buffer.Write(p)
}

func ghGet(ctx context.Context, endpoint string) ([]byte, http.Header, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "api", "--hostname", "github.com", "--method", "GET", "--include", "-H", "Accept: application/vnd.github+json", "-H", "X-GitHub-Api-Version: 2026-03-10", endpoint)
	cmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1")
	var stdout limitedBuffer
	cmd.Stdout = &stdout
	// gh errors can contain raw API bodies; expose only the status below.
	cmd.Stderr = io.Discard
	runErr := cmd.Run()
	response, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(stdout.Bytes())), nil)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		if runErr != nil {
			return nil, nil, fmt.Errorf("GitHub CLI unavailable or authentication failed: %w", runErr)
		}
		return nil, nil, errors.New("GitHub CLI returned invalid HTTP response")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		detail := ""
		if response.StatusCode == 401 || response.StatusCode == 403 || response.StatusCode == 404 {
			detail = "; source unavailable or access denied (not an empty source)"
		}
		if response.Header.Get("X-RateLimit-Remaining") == "0" {
			detail = "; rate limit exhausted; reset=" + response.Header.Get("X-RateLimit-Reset")
		}
		return nil, response.Header, fmt.Errorf("GitHub HTTP %d%s", response.StatusCode, detail)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if len(data) > maxResponseBytes {
		return nil, nil, errors.New("GitHub response exceeds 16 MiB")
	}
	return data, response.Header, err
}

func (c Client) get(ctx context.Context, endpoint string, target any) (http.Header, error) {
	get := c.Get
	if get == nil {
		get = ghGet
	}
	data, header, err := get(ctx, endpoint)
	if err != nil {
		return header, err
	}
	if err := json.Unmarshal(data, target); err != nil {
		return header, fmt.Errorf("invalid GitHub JSON: %w", err)
	}
	return header, nil
}

// Follow only pagination on the original allowlisted endpoint, never links in content.
func (c Client) list(ctx context.Context, endpoint string) ([]json.RawMessage, error) {
	base, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	var all []json.RawMessage
	seen := map[string]bool{}
	for page := 0; page < 100; page++ {
		if seen[endpoint] {
			return nil, errors.New("GitHub repeated pagination cursor")
		}
		seen[endpoint] = true
		var items []json.RawMessage
		header, err := c.get(ctx, endpoint, &items)
		if err != nil {
			return nil, err
		}
		if items == nil { // JSON null is not an authoritative empty list.
			return nil, errors.New("GitHub returned null instead of a list")
		}
		all = append(all, items...)
		next := ""
		for _, link := range strings.Split(header.Get("Link"), ",") {
			if !strings.Contains(link, `rel="next"`) {
				continue
			}
			start, end := strings.Index(link, "<"), strings.Index(link, ">")
			if start < 0 || end <= start {
				return nil, errors.New("invalid GitHub pagination link")
			}
			u, err := url.Parse(link[start+1 : end])
			if err != nil || u.Scheme != "https" || u.Host != "api.github.com" || strings.TrimPrefix(u.Path, "/") != base.Path {
				return nil, errors.New("GitHub pagination escaped the configured source")
			}
			next = strings.TrimPrefix(u.RequestURI(), "/")
		}
		if next == "" {
			return all, nil
		}
		endpoint = next
	}
	return nil, errors.New("GitHub source exceeds 100 pages; coverage incomplete")
}
