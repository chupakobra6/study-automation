package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/chupakobra6/study-automation/internal/adapters"
	"github.com/chupakobra6/study-automation/internal/club404"
	"github.com/chupakobra6/study-automation/internal/config"
	"github.com/chupakobra6/study-automation/internal/credentials"
	"github.com/chupakobra6/study-automation/internal/githubstudy"
	"github.com/chupakobra6/study-automation/internal/gmail"
	"github.com/chupakobra6/study-automation/internal/maildata"
	"github.com/chupakobra6/study-automation/internal/monitor"
	"github.com/chupakobra6/study-automation/internal/schedule"
	"github.com/chupakobra6/study-automation/internal/yandexmail"
)

const defaultConfig = "../study_data.local.toml"

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usageError()
	}
	switch args[0] {
	case "period":
		return runPeriod(args[1:])
	case "history":
		return runHistory(args[1:])
	case "brief", "task", "board", "queue", "retry-item":
		return runOperations(ctx, args)
	case "doctor":
		return runDoctor(ctx, args[1:])
	case "auth":
		return runAuth(ctx, args[1:])
	case "sync":
		return runSync(ctx, args[1:])
	case "help", "-h", "--help":
		printUsage()
		return nil
	default:
		return fmt.Errorf("unknown command %q\n\n%s", args[0], usageText())
	}
}

type doctorRow struct {
	Status string `json:"status"`
	Label  string `json:"label"`
	Detail string `json:"detail"`
}

func runDoctor(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	configPath := flags.String("config", defaultConfig, "path to study_data.local.toml")
	jsonOutput := flags.Bool("json", false, "write machine-readable status")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	rows := []doctorRow{
		status(cfg.Gmail.Account != "", "Gmail account", defaultValue(cfg.Gmail.Account, "gmail.account is not configured")),
		status(len(cfg.Gmail.Queries) > 0, "Gmail queries", fmt.Sprintf("%d configured", len(cfg.Gmail.Queries))),
		status(cfg.YandexMail.Account != "", "Yandex Mail account", defaultValue(cfg.YandexMail.Account, "yandex_mail.account is not configured")),
	}
	githubMailReady := (len(cfg.GitHub.Repositories) == 0 && len(cfg.GitHub.Projects) == 0) || coversGitHubInvitations(cfg.Gmail.Queries)
	if len(cfg.GitHub.Repositories) > 0 || len(cfg.GitHub.Projects) > 0 {
		detail := "narrow invitation query configured"
		if !githubMailReady {
			detail = "missing query with GitHub and invitation/invited terms"
		}
		rows = append(rows, status(githubMailReady, "Gmail GitHub invitations", detail))
	}
	clientOK := false
	if _, err := gmail.LoadOAuthClient(cfg.Gmail.OAuthClientJSON); err == nil {
		clientOK = true
		rows = append(rows, doctorRow{Status: "ok", Label: "Gmail OAuth client", Detail: cfg.Gmail.OAuthClientJSON})
	} else {
		rows = append(rows, doctorRow{Status: "blocked", Label: "Gmail OAuth client", Detail: "missing or invalid; run auth gmail --client-json <downloaded.json>"})
	}
	store := credentials.Helper{Path: cfg.Paths.CredentialHelper}
	_, tokenErr := store.Get(ctx, cfg.Gmail.Account, cfg.Gmail.KeychainService)
	tokenOK := tokenErr == nil
	if tokenOK {
		rows = append(rows, doctorRow{Status: "ok", Label: "Gmail refresh token", Detail: "stored in macOS Keychain"})
	} else {
		rows = append(rows, doctorRow{Status: "blocked", Label: "Gmail refresh token", Detail: tokenErr.Error()})
	}
	_, yandexTokenErr := store.Get(ctx, cfg.YandexMail.Account, cfg.YandexMail.KeychainService)
	yandexTokenOK := yandexTokenErr == nil
	if yandexTokenOK {
		rows = append(rows, doctorRow{Status: "ok", Label: "Yandex app password", Detail: "stored in macOS Keychain"})
	} else {
		rows = append(rows, doctorRow{Status: "blocked", Label: "Yandex app password", Detail: yandexTokenErr.Error()})
	}
	telegramResult, telegramErr := adapters.TelegramIdentity(ctx, cfg, adapters.OSRunner{})
	if telegramErr == nil && len(cfg.Telegram.Chats) == 0 {
		telegramErr = errors.New("no Telegram study chats are configured")
		telegramResult.Status = "blocked"
		telegramResult.Detail = telegramErr.Error()
	}
	rows = append(rows, doctorRow{Status: telegramResult.Status, Label: "Telegram study identity", Detail: telegramResult.Detail})
	lmsReady := true
	if len(cfg.SmartLMS.Courses) == 0 {
		rows = append(rows, doctorRow{Status: "paused", Label: "Smart LMS", Detail: "0 configured courses"})
	} else if info, err := os.Stat(cfg.Paths.HSELMSHarvest); err == nil && info.IsDir() {
		rows = append(rows, doctorRow{Status: "ok", Label: "Smart LMS", Detail: fmt.Sprintf("%d configured courses", len(cfg.SmartLMS.Courses))})
	} else {
		lmsReady = false
		rows = append(rows, doctorRow{Status: "blocked", Label: "Smart LMS", Detail: "hse-lms-harvest project is missing"})
	}
	if len(cfg.SmartLMS.Courses) > 0 {
		if cfg.SmartLMS.CatalogScan {
			rows = append(rows, doctorRow{Status: "ok", Label: "Smart LMS catalog", Detail: "enrolled-course index checked on every sync"})
		} else {
			rows = append(rows, doctorRow{Status: "warn", Label: "Smart LMS catalog", Detail: "disabled; new courses can be missed"})
		}
	}
	if len(cfg.Netology.Courses) == 0 {
		rows = append(rows, doctorRow{Status: "paused", Label: "Netology", Detail: "0 configured current modules"})
	} else if info, err := os.Stat(cfg.Paths.HSELMSHarvest); err == nil && info.IsDir() {
		rows = append(rows, doctorRow{Status: "ok", Label: "Netology", Detail: fmt.Sprintf("%d configured current modules", len(cfg.Netology.Courses))})
	} else {
		lmsReady = false
		rows = append(rows, doctorRow{Status: "blocked", Label: "Netology", Detail: "hse-lms-harvest project is missing"})
	}
	clubReady := true
	if len(cfg.Club404.Subjects) > 0 {
		clubErr := club404.Validate(cfg.Club404)
		if clubErr == nil {
			_, clubErr = os.Stat(filepath.Join(cfg.Paths.HSELMSHarvest, ".venv", "bin", "hse-lms-harvest"))
		}
		clubReady = clubErr == nil
		detail := fmt.Sprintf("%d configured subjects; login and coverage verified by sync", len(cfg.Club404.Subjects))
		if clubErr != nil {
			detail = clubErr.Error()
		}
		rows = append(rows, status(clubReady, "club404", detail))
	}
	scheduleErr := schedule.Validate(cfg.Schedule)
	scheduleDetail := fmt.Sprintf("%d explicitly filtered public folders; access verified by sync", len(cfg.Schedule.Folders))
	if scheduleErr != nil {
		scheduleDetail = scheduleErr.Error()
	}
	rows = append(rows, status(scheduleErr == nil, "Official schedule", scheduleDetail))
	githubReady := true
	if len(cfg.GitHub.Repositories) > 0 || len(cfg.GitHub.Projects) > 0 {
		e := githubstudy.Validate(cfg.GitHub)
		if e == nil {
			_, e = exec.LookPath("gh")
		}
		githubReady = e == nil
		detail := fmt.Sprintf("%d repositories, %d projects; access verified by sync", len(cfg.GitHub.Repositories), len(cfg.GitHub.Projects))
		if e != nil {
			detail = e.Error()
		}
		rows = append(rows, status(githubReady, "GitHub", detail))
	}
	ready := githubReady && githubMailReady && clientOK && tokenOK && cfg.Gmail.Account != "" && len(cfg.Gmail.Queries) > 0 && cfg.YandexMail.Account != "" && yandexTokenOK && telegramErr == nil && len(cfg.Telegram.Chats) > 0 && lmsReady && clubReady && scheduleErr == nil
	if *jsonOutput {
		if err := json.NewEncoder(os.Stdout).Encode(map[string]any{"rows": rows, "ready": ready}); err != nil {
			return err
		}
		if !ready {
			return errors.New("study monitoring sources are not ready")
		}
		return nil
	}
	for _, row := range rows {
		fmt.Printf("%-8s %-24s %s\n", row.Status, row.Label, row.Detail)
	}
	if !ready {
		return errors.New("study monitoring sources are not ready")
	}
	return nil
}

func coversGitHubInvitations(queries []string) bool {
	for _, query := range queries {
		normalized := strings.ToLower(query)
		if strings.Contains(normalized, "github") && (strings.Contains(normalized, "invitation") || strings.Contains(normalized, "invited")) {
			return true
		}
	}
	return false
}

func runAuth(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "gmail" {
		return errors.New("usage: study-monitor auth gmail --client-json <downloaded.json> [--config <path>]")
	}
	flags := flag.NewFlagSet("auth gmail", flag.ContinueOnError)
	configPath := flags.String("config", defaultConfig, "path to study_data.local.toml")
	clientJSON := flags.String("client-json", "", "downloaded Google OAuth Desktop client JSON")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if strings.TrimSpace(*clientJSON) == "" {
		return errors.New("--client-json is required")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if cfg.Gmail.Account == "" {
		return errors.New("gmail.account is not configured")
	}
	if err := gmail.ImportOAuthClient(*clientJSON, cfg.Gmail.OAuthClientJSON); err != nil {
		return fmt.Errorf("import Gmail OAuth client: %w", err)
	}
	fmt.Printf("Opening Google authorization for %s with read-only Gmail access.\n", cfg.Gmail.Account)
	if err := gmail.Authorize(ctx, gmail.AuthorizeOptions{
		Account: cfg.Gmail.Account, KeychainService: cfg.Gmail.KeychainService,
		ClientPath: cfg.Gmail.OAuthClientJSON, Store: credentials.Helper{Path: cfg.Paths.CredentialHelper},
		HTTP: &http.Client{Timeout: 45 * time.Second}, Timeout: 5 * time.Minute,
	}); err != nil {
		return err
	}
	fmt.Println("Gmail read-only authorization saved in macOS Keychain.")
	return nil
}

func runSync(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("sync [source] --apply|--dry-run")
	}
	if !strings.HasPrefix(args[0], "-") {
		args = append([]string{"--source", args[0]}, args[1:]...)
	}
	return runUnifiedSync(ctx, args)
}

func collectGmail(ctx context.Context, cfg config.Config) (gmail.Report, error) {
	oauthClient, err := gmail.LoadOAuthClient(cfg.Gmail.OAuthClientJSON)
	if err != nil {
		return gmail.Report{}, err
	}
	if err := gmail.ValidateOAuthClient(oauthClient); err != nil {
		return gmail.Report{}, err
	}
	store := credentials.Helper{Path: cfg.Paths.CredentialHelper}
	refreshToken, err := store.Get(ctx, cfg.Gmail.Account, cfg.Gmail.KeychainService)
	if err != nil {
		return gmail.Report{}, fmt.Errorf("read Gmail refresh token: %w; run auth gmail", err)
	}
	httpClient := &http.Client{Timeout: 45 * time.Second}
	accessToken, err := gmail.RefreshAccessToken(ctx, httpClient, oauthClient, refreshToken)
	if err != nil {
		return gmail.Report{}, err
	}
	client := &gmail.Client{HTTP: httpClient, AccessToken: accessToken}
	profile, err := client.Profile(ctx)
	if err != nil {
		return gmail.Report{}, fmt.Errorf("verify Gmail profile: %w", err)
	}
	if !strings.EqualFold(profile.EmailAddress, cfg.Gmail.Account) {
		return gmail.Report{}, fmt.Errorf("authorized Gmail account is %q, expected %q", profile.EmailAddress, cfg.Gmail.Account)
	}
	report, err := gmail.Sync(ctx, cfg.Gmail, client, time.Now())
	if err != nil {
		return gmail.Report{}, err
	}
	return report, nil
}

func collectYandexMail(ctx context.Context, cfg config.Config) (yandexmail.Report, error) {
	store := credentials.Helper{Path: cfg.Paths.CredentialHelper}
	password, err := store.Get(ctx, cfg.YandexMail.Account, cfg.YandexMail.KeychainService)
	if err != nil {
		return yandexmail.Report{}, fmt.Errorf("read Yandex app password: %w", err)
	}
	client, err := yandexmail.Connect(ctx, cfg.YandexMail.Host, cfg.YandexMail.Port, cfg.YandexMail.Account, password)
	if err != nil {
		return yandexmail.Report{}, err
	}
	defer client.Close()
	report, err := yandexmail.Sync(ctx, cfg.YandexMail, client, time.Now())
	if err != nil {
		return yandexmail.Report{}, err
	}
	return report, nil
}

func runUnifiedSync(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("sync", flag.ContinueOnError)
	selected := flags.String("source", "", "collect only one source through the same durable queue")
	configPath := flags.String("config", defaultConfig, "path to study_data.local.toml")
	dryRun := flags.Bool("dry-run", false, "show the complete source plan without writes")
	apply := flags.Bool("apply", false, "synchronize all configured study sources")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *dryRun == *apply {
		return errors.New("exactly one of --dry-run or --apply is required")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if *dryRun {
		fmt.Printf("gmail: %s (%d queries)\n", cfg.Gmail.Account, len(cfg.Gmail.Queries))
		fmt.Printf("yandex_mail: %s (%d folders)\n", cfg.YandexMail.Account, len(cfg.YandexMail.Folders))
		fmt.Printf("schedule: %s (%d filtered folders)\n", cfg.Schedule.PublicKey, len(cfg.Schedule.Folders))
		fmt.Printf("telegram: @%s (ID %d), %d configured chats\n", cfg.Telegram.ExpectedUsername, cfg.Telegram.ExpectedUserID, len(cfg.Telegram.Chats))
		if len(cfg.SmartLMS.Courses) == 0 {
			fmt.Println("smart_lms: paused: 0 configured courses")
		} else {
			fmt.Printf("smart_lms: %d configured courses\n", len(cfg.SmartLMS.Courses))
		}
		fmt.Printf("netology: %d configured current modules\n", len(cfg.Netology.Courses))
		fmt.Printf("club404: %d configured subjects\n", len(cfg.Club404.Subjects))
		fmt.Printf("github: %d repositories, %d projects\nmanual: %s\nqueue: %s\n", len(cfg.GitHub.Repositories), len(cfg.GitHub.Projects), cfg.Manual.InboxDir, cfg.Monitoring.QueueDir)
		fmt.Printf("combined report: %s\n", cfg.Monitoring.LatestReport)
		return nil
	}
	runner := adapters.OSRunner{}
	sources := []monitor.Source{
		{Name: "gmail", ReportPath: cfg.Gmail.LatestReport, Sync: func(ctx context.Context) (monitor.Result, error) {
			report, err := collectGmail(ctx, cfg)
			if err != nil {
				return monitor.Result{Source: "gmail", Status: "failed", Detail: err.Error()}, err
			}
			attachments := 0
			for _, record := range report.Records {
				attachments += len(record.Attachments)
			}
			return monitor.Result{Source: "gmail", Status: report.Status, Detail: fmt.Sprintf("%d new messages; pending_items=%d failed_items=%d", report.Added, report.PendingItems, report.FailedItems), Added: report.Added, Attachments: attachments, RequiredEvidence: mailRequiredEvidence(report.Records), Report: report.Report}, nil
		}},
		{Name: "yandex_mail", ReportPath: cfg.YandexMail.LatestReport, Sync: func(ctx context.Context) (monitor.Result, error) {
			report, err := collectYandexMail(ctx, cfg)
			if err != nil {
				return monitor.Result{Source: "yandex_mail", Status: "failed", Detail: err.Error()}, err
			}
			attachments := 0
			for _, record := range report.Records {
				attachments += len(record.Attachments)
			}
			return monitor.Result{Source: "yandex_mail", Status: report.Status, Detail: fmt.Sprintf("%d new messages; pending_items=%d failed_items=%d", report.Added, report.PendingItems, report.FailedItems), Added: report.Added, Attachments: attachments, RequiredEvidence: mailRequiredEvidence(report.Records), Report: report.Report}, nil
		}},
		{Name: "schedule", ReportPath: cfg.Schedule.LatestReport, Sync: func(ctx context.Context) (monitor.Result, error) {
			return schedule.Sync(ctx, cfg.Schedule, schedule.Client{}, time.Now())
		}},
		{Name: "telegram", ReportPath: filepath.Join(cfg.Updates.StateDir, "telegram-latest.md"), Sync: func(ctx context.Context) (monitor.Result, error) {
			result, err := adapters.SyncTelegram(ctx, cfg, runner, false)
			return monitor.Result{Source: result.Source, Status: result.Status, Detail: result.Detail, Added: result.Added, Attachments: result.Attachments, RequiredEvidence: result.RequiredEvidence, NeedsAnalysis: result.NeedsAnalysis, AnalysisDecisionKnown: result.AnalysisDecisionKnown, Report: filepath.Join(cfg.Updates.StateDir, "telegram-latest.md")}, err
		}},
		{Name: "smart_lms", ReportPath: filepath.Join(cfg.Updates.StateDir, "smart_lms-latest.md"), Timeout: 12 * time.Minute, Sync: func(ctx context.Context) (monitor.Result, error) {
			result, err := adapters.SyncSmartLMS(ctx, cfg, runner, false)
			return monitor.Result{Source: result.Source, Status: result.Status, Detail: result.Detail, Attachments: result.Attachments, RequiredEvidence: result.RequiredEvidence, NeedsAnalysis: result.NeedsAnalysis, AnalysisDecisionKnown: result.AnalysisDecisionKnown, Report: filepath.Join(cfg.Updates.StateDir, "smart_lms-latest.md")}, err
		}},
		{Name: "netology", ReportPath: filepath.Join(cfg.Updates.StateDir, "netology-latest.md"), Sync: func(ctx context.Context) (monitor.Result, error) {
			result, err := adapters.SyncNetology(ctx, cfg, runner, false)
			return monitor.Result{Source: result.Source, Status: result.Status, Detail: result.Detail, Attachments: result.Attachments, RequiredEvidence: result.RequiredEvidence, NeedsAnalysis: result.NeedsAnalysis, AnalysisDecisionKnown: result.AnalysisDecisionKnown, Report: filepath.Join(cfg.Updates.StateDir, "netology-latest.md")}, err
		}},
	}
	if len(cfg.GitHub.Repositories) > 0 || len(cfg.GitHub.Projects) > 0 {
		sources = append(sources, monitor.Source{Name: "github", ReportPath: cfg.GitHub.LatestReport, Sync: func(ctx context.Context) (monitor.Result, error) {
			return githubstudy.Sync(ctx, cfg.GitHub, githubstudy.Client{}, time.Now())
		}})
	}
	if len(cfg.Club404.Subjects) > 0 {
		sources = append(sources, monitor.Source{Name: "club404", ReportPath: cfg.Club404.LatestReport, Timeout: 12 * time.Minute, Sync: func(ctx context.Context) (monitor.Result, error) {
			return club404.Sync(ctx, cfg.Club404, cfg.Paths.HSELMSHarvest, runner, time.Now())
		}})
	}
	sources = append(sources, manualSource(cfg))
	if *selected != "" {
		selectedSources := []monitor.Source{}
		for _, source := range sources {
			if source.Name == *selected {
				selectedSources = append(selectedSources, source)
			}
		}
		if len(selectedSources) == 0 {
			return fmt.Errorf("unknown or unconfigured source %q", *selected)
		}
		sources = selectedSources
	}
	started := time.Now()
	run := newCollectionRun(started, sources, *selected == "")
	runsDir := collectionDir(cfg.Paths.StateDir)
	if err := saveCollectionRun(runsDir, run, false); err != nil {
		return fmt.Errorf("record collection start: %w", err)
	}
	for i := range sources {
		sources[i].Progress = func(progress monitor.Progress) {
			printSyncProgress(progress)
			run = applyCollectionProgress(run, progress)
			if err := saveCollectionRun(runsDir, run, false); err != nil {
				fmt.Fprintf(os.Stderr, "record collection progress: %v\n", err)
			}
		}
	}
	report, runErr := monitor.Run(ctx, sources, cfg.Monitoring.LatestReport, cfg.Monitoring.QueueDir, started)
	run = finishCollectionRun(run, runErr, time.Now())
	if err := saveCollectionRun(runsDir, run, true); err != nil {
		runErr = errors.Join(runErr, fmt.Errorf("record collection result: %w", err))
	}
	for _, result := range report.Results {
		fmt.Printf("%-12s %-10s %s\n", result.Source, result.Status, result.Detail)
	}
	fmt.Printf("analysis_needed=%t pending_count=%d\ncombined report: %s\n", report.NeedsAnalysis, report.Pending.Pending, report.Path)
	return runErr
}

func printSyncProgress(progress monitor.Progress) {
	writeSyncProgress(os.Stderr, progress)
}

func writeSyncProgress(output io.Writer, progress monitor.Progress) {
	if progress.Phase == "started" {
		fmt.Fprintf(output, "source=%s phase=started timeout=%s\n", progress.Source, progress.Timeout)
		return
	}
	fmt.Fprintf(output, "source=%s phase=finished status=%s duration=%s detail=%s\n", progress.Source, progress.Status, progress.Duration.Round(time.Millisecond), progress.Detail)
}

func mailRequiredEvidence(records []maildata.Record) []string {
	paths := make([]string, 0)
	seen := map[string]bool{}
	for _, record := range records {
		for _, attachment := range record.Attachments {
			path := filepath.Clean(attachment.LocalPath)
			if attachment.LocalPath != "" && !seen[path] {
				seen[path] = true
				paths = append(paths, path)
			}
		}
	}
	return paths
}

func status(ok bool, label, detail string) doctorRow {
	value := "blocked"
	if ok {
		value = "ok"
	}
	return doctorRow{Status: value, Label: label, Detail: detail}
}

func defaultValue(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func usageError() error {
	return errors.New(usageText())
}

func printUsage() {
	fmt.Print(usageText())
}

func usageText() string {
	return `study-monitor — study source collector (Netology may start tasks for reading)

Usage:
  study-monitor brief [--limit 8] [--config <path>]
  study-monitor history --date YYYY-MM-DD [--config <path>]
  study-monitor history --from YYYY-MM-DD --to YYYY-MM-DD [--config <path>]
  study-monitor period --date YYYY-MM-DD [--limit 50] [--offset 0] [--config <path>]
  study-monitor period --from YYYY-MM-DD --to YYYY-MM-DD [--limit 50] [--offset 0] [--config <path>]
  study-monitor period inspect --source gmail|yandex_mail|telegram --id <record-id> [--config <path>]
  study-monitor task --query <text> [--config <path>]
  study-monitor board [--config <path>]
  study-monitor queue list|inspect <id> [--config <path>]
  study-monitor queue prepare --receipt <new-absolute-path.json> [--config <path>]
  study-monitor queue ack --receipt <path> [--config <path>]
  study-monitor retry-item --source gmail --message-id <id> [--config <path>]
  study-monitor retry-item --source yandex_mail --folder <name> --uid <uid> [--config <path>]
  study-monitor doctor [--config <path>] [--json]
  study-monitor auth gmail --client-json <downloaded.json> [--config <path>]
  study-monitor sync (--dry-run|--apply) [--config <path>]
  study-monitor sync gmail (--dry-run|--apply) [--config <path>]
  study-monitor sync yandex_mail (--dry-run|--apply) [--config <path>]
  study-monitor sync schedule|telegram|smart_lms|netology|club404|github|manual (--dry-run|--apply) [--config <path>]
`
}
