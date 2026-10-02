package config

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/chupakobra6/study-automation/internal/club404"
	"github.com/chupakobra6/study-automation/internal/githubstudy"
)

type Config struct {
	Manual struct {
		InboxDir string `toml:"inbox_dir"`
	} `toml:"manual"`
	GitHub     githubstudy.Config `toml:"github"`
	ConfigPath string             `toml:"-"`
	Paths      Paths              `toml:"paths"`
	Updates    Updates            `toml:"updates"`
	Monitoring Monitoring         `toml:"monitoring"`
	Gmail      Gmail              `toml:"gmail"`
	YandexMail YandexMail         `toml:"yandex_mail"`
	Telegram   Telegram           `toml:"telegram"`
	SmartLMS   SmartLMS           `toml:"smart_lms"`
	Netology   Netology           `toml:"netology"`
	Club404    club404.Config     `toml:"club404"`
	Schedule   Schedule           `toml:"schedule"`
}

type Paths struct {
	CredentialHelper string `toml:"credential_helper"`
	StateDir         string `toml:"state_dir"`
	StudyData        string `toml:"study_data"`
	TelegramHarvest  string `toml:"telegram_harvest"`
	HSELMSHarvest    string `toml:"hse_lms_harvest"`
}

type Updates struct {
	StateDir string `toml:"state_dir"`
}

type Monitoring struct {
	QueueDir     string `toml:"queue_dir"`
	TasksFile    string `toml:"tasks_file"`
	LatestReport string `toml:"latest_report"`
}

type Schedule struct {
	PublicKey    string           `toml:"public_key"`
	Folders      []ScheduleFolder `toml:"folders"`
	StateDir     string           `toml:"state_dir"`
	Checkpoint   string           `toml:"checkpoint"`
	LatestReport string           `toml:"latest_report"`
}

type ScheduleFolder struct {
	Path    string   `toml:"path"`
	Include []string `toml:"include"`
}

type Telegram struct {
	ExpectedUsername string         `toml:"expected_username"`
	ExpectedUserID   int64          `toml:"expected_user_id"`
	StateDir         string         `toml:"state_dir"`
	MessagesJSONL    string         `toml:"messages_jsonl"`
	Chats            []TelegramChat `toml:"chats"`
}

type TelegramChat struct {
	ID   string `toml:"id"`
	Name string `toml:"name"`
}

type SmartLMS struct {
	Courses     []SmartLMSCourse `toml:"courses"`
	CatalogScan bool             `toml:"catalog_scan"`
}

type SmartLMSCourse struct {
	Slug  string `toml:"slug"`
	Title string `toml:"title"`
	URL   string `toml:"url"`
}

type Netology struct {
	Courses []SmartLMSCourse `toml:"courses"`
}

type Gmail struct {
	Account            string   `toml:"account"`
	OAuthClientJSON    string   `toml:"oauth_client_json"`
	StateDir           string   `toml:"state_dir"`
	MessagesJSONL      string   `toml:"messages_jsonl"`
	AttachmentsDir     string   `toml:"attachments_dir"`
	Checkpoint         string   `toml:"checkpoint"`
	LatestReport       string   `toml:"latest_report"`
	KeychainService    string   `toml:"keychain_service"`
	Queries            []string `toml:"queries"`
	MaxMessagesPerSync int      `toml:"max_messages_per_sync"`
}

type YandexMail struct {
	Account            string   `toml:"account"`
	Host               string   `toml:"host"`
	Port               int      `toml:"port"`
	Folders            []string `toml:"folders"`
	Since              string   `toml:"since"`
	StateDir           string   `toml:"state_dir"`
	MessagesJSONL      string   `toml:"messages_jsonl"`
	AttachmentsDir     string   `toml:"attachments_dir"`
	Checkpoint         string   `toml:"checkpoint"`
	LatestReport       string   `toml:"latest_report"`
	KeychainService    string   `toml:"keychain_service"`
	MaxMessagesPerSync int      `toml:"max_messages_per_sync"`
}

func Load(path string) (Config, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Config{}, fmt.Errorf("resolve config path: %w", err)
	}
	var cfg Config
	if _, err := toml.DecodeFile(abs, &cfg); err != nil {
		return Config{}, fmt.Errorf("read config %s: %w", abs, err)
	}
	base := filepath.Dir(abs)
	cfg.ConfigPath = abs
	cfg.Paths.StateDir = resolve(base, defaultString(cfg.Paths.StateDir, ".study_state"))
	cfg.Paths.CredentialHelper = resolve(base, defaultString(cfg.Paths.CredentialHelper, filepath.Join(cfg.Paths.StateDir, "bin", "study-keychain")))
	cfg.Paths.StudyData = resolve(base, defaultString(cfg.Paths.StudyData, "tools/study-data"))
	cfg.Paths.TelegramHarvest = resolve(base, defaultString(cfg.Paths.TelegramHarvest, "../telegram-harvest"))
	cfg.Paths.HSELMSHarvest = resolve(base, defaultString(cfg.Paths.HSELMSHarvest, "../hse-lms-harvest"))
	cfg.Updates.StateDir = resolve(base, defaultString(
		cfg.Updates.StateDir,
		filepath.Join(cfg.Paths.StateDir, "updates"),
	))
	cfg.Monitoring.LatestReport = resolve(base, defaultString(
		cfg.Monitoring.LatestReport,
		filepath.Join(cfg.Updates.StateDir, "monitoring-latest.md"),
	))
	cfg.Monitoring.QueueDir = resolve(base, defaultString(cfg.Monitoring.QueueDir, filepath.Join(cfg.Paths.StateDir, "queue")))
	cfg.Monitoring.TasksFile = resolve(base, defaultString(cfg.Monitoring.TasksFile, filepath.Join(cfg.Paths.StateDir, "academic-monitoring", "tasks.json")))
	cfg.Manual.InboxDir = resolve(base, defaultString(cfg.Manual.InboxDir, filepath.Join(cfg.Paths.StateDir, "manual", "inbox")))
	cfg.GitHub.StateDir = resolve(base, defaultString(cfg.GitHub.StateDir, filepath.Join(cfg.Paths.StateDir, "github")))
	cfg.GitHub.Checkpoint = resolve(base, defaultString(cfg.GitHub.Checkpoint, filepath.Join(cfg.GitHub.StateDir, "checkpoint.json")))
	cfg.GitHub.LatestReport = resolve(base, defaultString(cfg.GitHub.LatestReport, filepath.Join(cfg.Updates.StateDir, "github-latest.md")))
	cfg.Telegram.ExpectedUsername = strings.TrimPrefix(strings.TrimSpace(cfg.Telegram.ExpectedUsername), "@")
	if len(cfg.Telegram.Chats) > 0 && (cfg.Telegram.ExpectedUsername == "" || cfg.Telegram.ExpectedUserID <= 0) {
		return Config{}, fmt.Errorf("telegram.expected_username and positive telegram.expected_user_id are required when chats are configured")
	}
	cfg.Telegram.StateDir = resolve(base, defaultString(cfg.Telegram.StateDir, filepath.Join(cfg.Paths.StateDir, "telegram")))
	cfg.Telegram.MessagesJSONL = resolve(base, defaultString(cfg.Telegram.MessagesJSONL, filepath.Join(cfg.Telegram.StateDir, "messages.jsonl")))
	cfg.Gmail.Account = strings.TrimSpace(cfg.Gmail.Account)
	cfg.Gmail.OAuthClientJSON = resolve(base, defaultString(
		cfg.Gmail.OAuthClientJSON,
		filepath.Join(cfg.Paths.StateDir, "credentials", "gmail-oauth-client.json"),
	))
	cfg.Gmail.StateDir = resolve(base, defaultString(
		cfg.Gmail.StateDir,
		filepath.Join(cfg.Paths.StateDir, "raw", "gmail"),
	))
	cfg.Gmail.MessagesJSONL = resolve(base, defaultString(
		cfg.Gmail.MessagesJSONL,
		filepath.Join(cfg.Gmail.StateDir, "messages.jsonl"),
	))
	cfg.Gmail.AttachmentsDir = resolve(base, defaultString(
		cfg.Gmail.AttachmentsDir,
		filepath.Join(cfg.Paths.StateDir, "attachments", "gmail"),
	))
	cfg.Gmail.Checkpoint = resolve(base, defaultString(
		cfg.Gmail.Checkpoint,
		filepath.Join(cfg.Paths.StateDir, "checkpoints", "gmail.json"),
	))
	cfg.Gmail.LatestReport = resolve(base, defaultString(
		cfg.Gmail.LatestReport,
		filepath.Join(cfg.Updates.StateDir, "gmail-latest.md"),
	))
	cfg.Gmail.KeychainService = defaultString(cfg.Gmail.KeychainService, "codex-study-gmail-oauth")
	if cfg.Gmail.MaxMessagesPerSync <= 0 {
		cfg.Gmail.MaxMessagesPerSync = 1000
	}
	for index := range cfg.Gmail.Queries {
		cfg.Gmail.Queries[index] = strings.TrimSpace(cfg.Gmail.Queries[index])
	}
	cfg.YandexMail.Account = strings.TrimSpace(cfg.YandexMail.Account)
	cfg.YandexMail.Host = defaultString(cfg.YandexMail.Host, "imap.yandex.ru")
	if cfg.YandexMail.Port <= 0 {
		cfg.YandexMail.Port = 993
	}
	if len(cfg.YandexMail.Folders) == 0 {
		cfg.YandexMail.Folders = []string{"INBOX"}
	}
	for index := range cfg.YandexMail.Folders {
		cfg.YandexMail.Folders[index] = strings.TrimSpace(cfg.YandexMail.Folders[index])
	}
	cfg.YandexMail.Since = defaultString(cfg.YandexMail.Since, "2026-07-01")
	cfg.YandexMail.StateDir = resolve(base, defaultString(
		cfg.YandexMail.StateDir,
		filepath.Join(cfg.Paths.StateDir, "yandex-mail"),
	))
	cfg.YandexMail.MessagesJSONL = resolve(base, defaultString(
		cfg.YandexMail.MessagesJSONL,
		filepath.Join(cfg.YandexMail.StateDir, "messages.jsonl"),
	))
	cfg.YandexMail.AttachmentsDir = resolve(base, defaultString(
		cfg.YandexMail.AttachmentsDir,
		filepath.Join(cfg.YandexMail.StateDir, "attachments"),
	))
	cfg.YandexMail.Checkpoint = resolve(base, defaultString(
		cfg.YandexMail.Checkpoint,
		filepath.Join(cfg.Updates.StateDir, "yandex_mail.json"),
	))
	cfg.YandexMail.LatestReport = resolve(base, defaultString(
		cfg.YandexMail.LatestReport,
		filepath.Join(cfg.Updates.StateDir, "yandex_mail-latest.md"),
	))
	cfg.YandexMail.KeychainService = defaultString(cfg.YandexMail.KeychainService, "codex-study-yandex-imap")
	if cfg.YandexMail.MaxMessagesPerSync <= 0 {
		cfg.YandexMail.MaxMessagesPerSync = 1000
	}
	cfg.Schedule.StateDir = resolve(base, defaultString(cfg.Schedule.StateDir, filepath.Join(cfg.Paths.StateDir, "schedule")))
	cfg.Schedule.Checkpoint = resolve(base, defaultString(cfg.Schedule.Checkpoint, filepath.Join(cfg.Paths.StateDir, "checkpoints", "schedule.json")))
	cfg.Schedule.LatestReport = resolve(base, defaultString(cfg.Schedule.LatestReport, filepath.Join(cfg.Updates.StateDir, "schedule-latest.md")))
	if len(cfg.Club404.Subjects) > 0 {
		cfg.Club404.Profile = resolve(base, defaultString(cfg.Club404.Profile, filepath.Join(cfg.Paths.StateDir, "club404", "browser-profile")))
		cfg.Club404.StateDir = resolve(base, defaultString(cfg.Club404.StateDir, filepath.Join(cfg.Paths.StateDir, "club404")))
		cfg.Club404.Checkpoint = resolve(base, defaultString(cfg.Club404.Checkpoint, filepath.Join(cfg.Club404.StateDir, "checkpoint.json")))
		cfg.Club404.LatestReport = resolve(base, defaultString(cfg.Club404.LatestReport, filepath.Join(cfg.Updates.StateDir, "club404-latest.md")))
	}
	return cfg, nil
}

func resolve(base, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Clean(filepath.Join(base, path))
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
