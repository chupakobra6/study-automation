package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadResolvesGmailPathsAndDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "study_data.local.toml")
	data := []byte(`[paths]
state_dir = ".state"

[gmail]
account = "student@example.com"
queries = ["from:example.edu", " deadline "]
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.Gmail.StateDir, filepath.Join(dir, ".state", "raw", "gmail"); got != want {
		t.Fatalf("state dir = %q, want %q", got, want)
	}
	if got, want := cfg.Gmail.OAuthClientJSON, filepath.Join(dir, ".state", "credentials", "gmail-oauth-client.json"); got != want {
		t.Fatalf("oauth client = %q, want %q", got, want)
	}
	if got, want := cfg.Gmail.LatestReport, filepath.Join(dir, ".state", "updates", "gmail-latest.md"); got != want {
		t.Fatalf("latest report = %q, want %q", got, want)
	}
	if got, want := cfg.Gmail.AttachmentsDir, filepath.Join(dir, ".state", "attachments", "gmail"); got != want {
		t.Fatalf("attachments = %q, want %q", got, want)
	}
	if got, want := cfg.Gmail.Checkpoint, filepath.Join(dir, ".state", "checkpoints", "gmail.json"); got != want {
		t.Fatalf("checkpoint = %q, want %q", got, want)
	}
	if cfg.Gmail.MaxMessagesPerSync != 1000 {
		t.Fatalf("max messages = %d", cfg.Gmail.MaxMessagesPerSync)
	}
	if cfg.Gmail.Queries[1] != "deadline" {
		t.Fatalf("query was not trimmed: %q", cfg.Gmail.Queries[1])
	}
	if got, want := cfg.Monitoring.LatestReport, filepath.Join(dir, ".state", "updates", "monitoring-latest.md"); got != want {
		t.Fatalf("monitoring report = %q, want %q", got, want)
	}
	if got, want := cfg.YandexMail.Checkpoint, filepath.Join(dir, ".state", "updates", "yandex_mail.json"); got != want {
		t.Fatalf("Yandex checkpoint = %q, want %q", got, want)
	}
	if got, want := cfg.YandexMail.LatestReport, filepath.Join(dir, ".state", "updates", "yandex_mail-latest.md"); got != want {
		t.Fatalf("Yandex report = %q, want %q", got, want)
	}
	if cfg.Telegram.ExpectedUsername != "" || cfg.Telegram.ExpectedUserID != 0 {
		t.Fatalf("Telegram identity = @%s/%d", cfg.Telegram.ExpectedUsername, cfg.Telegram.ExpectedUserID)
	}
	if got, want := cfg.Schedule.StateDir, filepath.Join(dir, ".state", "schedule"); got != want {
		t.Fatalf("schedule state=%q want=%q", got, want)
	}
	if got, want := cfg.Schedule.Checkpoint, filepath.Join(dir, ".state", "checkpoints", "schedule.json"); got != want {
		t.Fatalf("schedule checkpoint=%q want=%q", got, want)
	}
}

func TestTelegramIdentityMustBeExplicit(t *testing.T) {
	for _, test := range []struct {
		name, identity string
		valid          bool
	}{
		{"missing", "", false},
		{"username-only", "expected_username = \"study_reader\"", false},
		{"id-only", "expected_user_id = 424242", false},
		{"negative-id", "expected_username = \"study_reader\"\nexpected_user_id = -1", false},
		{"explicit", "expected_username = \"@study_reader\"\nexpected_user_id = 424242", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			data := "[telegram]\n" + test.identity + "\n[[telegram.chats]]\nid = \"100\"\nname = \"Course\"\n"
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if test.valid {
				if err != nil || cfg.Telegram.ExpectedUsername != "study_reader" || cfg.Telegram.ExpectedUserID != 424242 {
					t.Fatalf("explicit identity not retained: %v", err)
				}
			} else if err == nil {
				t.Fatal("configured Telegram chats accepted without explicit identity")
			}
		})
	}
}
