package schedule

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/chupakobra6/study-automation/internal/config"
	"github.com/chupakobra6/study-automation/internal/monitor"
)

const maxFileBytes = 32 << 20

type Client struct {
	HTTP    *http.Client
	BaseURL string
}

type File struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	Type      string `json:"type"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
	Modified  string `json:"modified"`
	LocalPath string `json:"local_path"`
}

type snapshot struct {
	PublicKey   string          `json:"public_key"`
	GeneratedAt time.Time       `json:"generated_at"`
	Files       map[string]File `json:"files"`
}

func Validate(cfg config.Schedule) error {
	key, err := url.Parse(cfg.PublicKey)
	if err != nil || key.Scheme != "https" || key.Host == "" || len(cfg.Folders) == 0 {
		return errors.New("schedule requires an HTTPS public_key and explicit folders")
	}
	for _, folder := range cfg.Folders {
		if !strings.HasPrefix(folder.Path, "/") || path.Clean(folder.Path) != folder.Path || len(folder.Include) == 0 {
			return errors.New("each schedule folder requires a canonical absolute path and filename include filters")
		}
		for _, pattern := range folder.Include {
			if _, err := regexp.Compile(pattern); err != nil {
				return fmt.Errorf("schedule filename filter: %w", err)
			}
		}
	}
	return nil
}

func (c Client) endpoint(cfg config.Schedule, resourcePath, suffix string, offset int) string {
	base := c.BaseURL
	if base == "" {
		base = "https://cloud-api.yandex.net/v1/disk/public/resources"
	}
	return base + suffix + "?" + url.Values{"public_key": {cfg.PublicKey}, "path": {resourcePath}, "limit": {"100"}, "offset": {fmt.Sprint(offset)}}.Encode()
}

func (c Client) get(ctx context.Context, endpoint string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 45 * time.Second}
	}
	response, err := client.Do(req)
	if err != nil {
		if requestError, ok := err.(*url.Error); ok {
			err = requestError.Err
		}
		return nil, fmt.Errorf("schedule HTTP request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("schedule HTTP status %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err == nil && int64(len(data)) > limit {
		err = errors.New("schedule response exceeds size limit")
	}
	return data, err
}

func (c Client) list(ctx context.Context, cfg config.Schedule) (map[string]File, error) {
	files := make(map[string]File)
	for _, folder := range cfg.Folders {
		patterns := make([]*regexp.Regexp, 0, len(folder.Include))
		for _, pattern := range folder.Include {
			patterns = append(patterns, regexp.MustCompile(pattern))
		}
		matched := 0
		for offset := 0; ; offset += 100 {
			if offset >= 10000 {
				return nil, errors.New("schedule folder exceeds 100 metadata pages")
			}
			data, err := c.get(ctx, c.endpoint(cfg, folder.Path, "", offset), 8<<20)
			if err != nil {
				return nil, err
			}
			var response struct {
				Embedded *struct {
					Items []File `json:"items"`
				} `json:"_embedded"`
			}
			if err := json.Unmarshal(data, &response); err != nil {
				return nil, err
			}
			if response.Embedded == nil {
				return nil, fmt.Errorf("schedule folder %q has no item listing", folder.Path)
			}
			for _, item := range response.Embedded.Items {
				if item.Type != "file" {
					continue
				}
				include := false
				for _, pattern := range patterns {
					include = include || pattern.MatchString(item.Name)
				}
				if !include {
					continue
				}
				if path.Dir(item.Path) != folder.Path || path.Base(item.Path) != item.Name || strings.ContainsAny(item.Name, "\\\r\n") {
					return nil, errors.New("schedule file is outside its configured folder")
				}
				digest, err := hex.DecodeString(item.SHA256)
				if err != nil || len(digest) != sha256.Size || item.Size <= 0 || item.Size > maxFileBytes {
					return nil, fmt.Errorf("schedule file %q has invalid hash or size", item.Name)
				}
				files[item.Path] = item
				matched++
			}
			if len(response.Embedded.Items) < 100 {
				break
			}
		}
		if matched == 0 {
			return nil, fmt.Errorf("schedule folder %q has no files matching configured filters", folder.Path)
		}
	}
	return files, nil
}

func Sync(ctx context.Context, cfg config.Schedule, client Client, now time.Time) (result monitor.Result, err error) {
	result = monitor.Result{Source: "schedule", Status: "failed", Report: cfg.LatestReport}
	defer func() {
		if err != nil {
			result.Detail = err.Error()
		}
	}()
	if err = Validate(cfg); err != nil {
		return result, err
	}
	previous := snapshot{}
	data, readErr := os.ReadFile(cfg.Checkpoint)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return result, readErr
	}
	if readErr == nil {
		if err = json.Unmarshal(data, &previous); err != nil {
			return result, err
		}
	}
	baseline := previous.PublicKey != cfg.PublicKey
	if baseline {
		previous.Files = nil
	}
	files, err := client.list(ctx, cfg)
	if err != nil {
		return result, err
	}
	paths := make([]string, 0, len(files))
	for key := range files {
		paths = append(paths, key)
	}
	sort.Strings(paths)
	changes := make([]string, 0)
	for _, key := range paths {
		item := files[key]
		item.LocalPath = filepath.Join(cfg.StateDir, "raw", item.SHA256, item.Name)
		old, seen := previous.Files[key]
		info, statErr := os.Stat(item.LocalPath)
		if seen && old.SHA256 == item.SHA256 && statErr == nil && info.Size() == item.Size {
			cached, readErr := os.ReadFile(item.LocalPath)
			digest := sha256.Sum256(cached)
			if readErr == nil && int64(len(cached)) == item.Size && strings.EqualFold(hex.EncodeToString(digest[:]), item.SHA256) {
				files[key] = item
				continue
			}
		}
		data, err = client.get(ctx, client.endpoint(cfg, key, "/download", 0), 1<<20)
		if err != nil {
			return result, err
		}
		var download struct {
			Href string `json:"href"`
		}
		if err = json.Unmarshal(data, &download); err != nil {
			return result, err
		}
		data, err = client.get(ctx, download.Href, maxFileBytes)
		if err != nil {
			return result, err
		}
		digest := sha256.Sum256(data)
		if int64(len(data)) != item.Size || !strings.EqualFold(hex.EncodeToString(digest[:]), item.SHA256) {
			return result, fmt.Errorf("schedule file %q changed during download or failed integrity check", item.Name)
		}
		if err = writeAtomic(item.LocalPath, data); err != nil {
			return result, err
		}
		files[key] = item
		label := "Changed"
		if !seen {
			label = "Added"
			result.Added++
		}
		result.Attachments++
		result.RequiredEvidence = append(result.RequiredEvidence, item.LocalPath)
		changes = append(changes, fmt.Sprintf("- %s: %s\n  - Source path: `%s`\n  - SHA-256: `%s`; modified: %s\n  - Raw file: `%s`", label, item.Name, key, item.SHA256, item.Modified, item.LocalPath))
	}
	removed := make([]string, 0)
	for key := range previous.Files {
		if _, exists := files[key]; !exists {
			removed = append(removed, key)
		}
	}
	sort.Strings(removed)
	for _, key := range removed {
		changes = append(changes, fmt.Sprintf("- Removed from source: `%s` (local file retained)", key))
	}
	result.Status = "no-changes"
	if baseline {
		result.Status = "baseline"
	} else if len(changes) > 0 {
		result.Status = "delta"
	}
	result.NeedsAnalysis = len(changes) > 0
	result.Detail = fmt.Sprintf("%d tracked files; %d downloaded; %d removed", len(files), result.Attachments, len(removed))
	body := strings.Join(changes, "\n")
	if body == "" {
		body = "No schedule changes."
	}
	report := fmt.Sprintf("# Official Schedule Updates\n\nGenerated at: %s\nStatus: `%s`\nPublic source: %s\n\n%s\n\nRead changed raw workbooks; select only the confirmed course and group for each tracked subject. Other sheets do not imply personal applicability.\n", now.UTC().Format(time.RFC3339), result.Status, cfg.PublicKey, body)
	if err = writeAtomic(cfg.LatestReport, []byte(report)); err != nil {
		result.Status = "failed"
		return result, err
	}
	data, err = json.MarshalIndent(snapshot{PublicKey: cfg.PublicKey, GeneratedAt: now.UTC(), Files: files}, "", "  ")
	if err == nil {
		err = writeAtomic(cfg.Checkpoint, append(data, '\n'))
	}
	if err != nil {
		result.Status = "failed"
	}
	return result, err
}

func writeAtomic(target string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(target), ".schedule-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), target)
}
