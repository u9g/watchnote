// Watchnote watches GitHub issues and PRs and emails you when they change,
// with the note you saved about why you care.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // users pick IANA timezones; don't depend on the image having them
)

type Config struct {
	BaseURL            string
	ListenAddr         string
	DatabasePath       string
	SecretKey          string
	GoogleClientID     string
	GoogleClientSecret string
	GitHubToken        string
	GitHubAPIURL       string
	MailMode           string // smtp | log
	SMTPHost           string
	SMTPPort           int
	SMTPUsername       string
	SMTPPassword       string
	MailFrom           string
	DevLogin           bool
}

func env(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func loadConfig() (Config, error) {
	port, _ := strconv.Atoi(env("SMTP_PORT", "587"))
	c := Config{
		BaseURL:            strings.TrimRight(env("BASE_URL", "http://localhost:8080"), "/"),
		ListenAddr:         env("LISTEN_ADDR", ":8080"),
		DatabasePath:       env("DATABASE_PATH", "watchnote.db"),
		SecretKey:          env("SECRET_KEY", ""),
		GoogleClientID:     env("GOOGLE_CLIENT_ID", ""),
		GoogleClientSecret: env("GOOGLE_CLIENT_SECRET", ""),
		GitHubToken:        env("GITHUB_TOKEN", ""),
		GitHubAPIURL:       strings.TrimRight(env("GITHUB_API_URL", "https://api.github.com"), "/"),
		MailMode:           env("MAIL_MODE", "smtp"),
		SMTPHost:           env("SMTP_HOST", ""),
		SMTPPort:           port,
		SMTPUsername:       env("SMTP_USERNAME", ""),
		SMTPPassword:       env("SMTP_PASSWORD", ""),
		MailFrom:           env("MAIL_FROM", ""),
		DevLogin:           env("DEV_LOGIN", "") == "true",
	}
	var problems []string
	if len(c.SecretKey) < 32 {
		problems = append(problems, "SECRET_KEY must be at least 32 characters")
	}
	if _, err := url.Parse(c.BaseURL); err != nil {
		problems = append(problems, "BASE_URL is not a URL")
	}
	if !c.DevLogin && (c.GoogleClientID == "" || c.GoogleClientSecret == "") {
		problems = append(problems, "GOOGLE_CLIENT_ID and GOOGLE_CLIENT_SECRET are required")
	}
	switch c.MailMode {
	case "log":
	case "smtp":
		if c.SMTPHost == "" || c.MailFrom == "" {
			problems = append(problems, "SMTP_HOST and MAIL_FROM are required when MAIL_MODE=smtp")
		}
	default:
		problems = append(problems, "MAIL_MODE must be smtp or log")
	}
	if len(problems) > 0 {
		return c, errors.New(strings.Join(problems, "; "))
	}
	if c.MailFrom == "" {
		c.MailFrom = "Watchnote <watchnote@localhost>"
	}
	return c, nil
}

type App struct {
	cfg   Config
	db    *DB
	gh    *GitHub
	mail  Sender
	keys  *Keys
	pages map[string]*pageTemplate
	now   func() time.Time
	log   *slog.Logger
}

func newApp(cfg Config, db *DB, mail Sender) (*App, error) {
	pages, err := loadPages()
	if err != nil {
		return nil, err
	}
	return &App{
		cfg:   cfg,
		db:    db,
		gh:    &GitHub{BaseURL: cfg.GitHubAPIURL, AppToken: cfg.GitHubToken, HTTP: &http.Client{Timeout: 20 * time.Second}},
		mail:  mail,
		keys:  newKeys(cfg.SecretKey),
		pages: pages,
		now:   time.Now,
		log:   slog.Default(),
	}, nil
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if cfg.DevLogin {
		slog.Warn("DEV_LOGIN is enabled: anyone can sign in as any email. Never use this in production.")
	}
	if cfg.GitHubToken == "" {
		slog.Warn("GITHUB_TOKEN is not set; unauthenticated GitHub API calls are limited to 60/hour")
	}
	db, err := openDB(cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer db.Close()

	var mail Sender = &logSender{}
	if cfg.MailMode == "smtp" {
		mail = &smtpSender{Host: cfg.SMTPHost, Port: cfg.SMTPPort, Username: cfg.SMTPUsername, Password: cfg.SMTPPassword}
	}
	app, err := newApp(cfg, db, mail)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go app.every(ctx, 10*time.Second, "poller", app.pollDue)
	go app.every(ctx, 20*time.Second, "mailer", app.runMailer)

	srv := &http.Server{Addr: cfg.ListenAddr, Handler: app.routes(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()
	slog.Info("listening", "addr", cfg.ListenAddr, "base_url", cfg.BaseURL)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// every runs fn on an interval until ctx is done, logging (not dying on) errors.
func (a *App) every(ctx context.Context, d time.Duration, name string, fn func(context.Context) error) {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		if err := fn(ctx); err != nil && ctx.Err() == nil {
			a.log.Error(name+" failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
