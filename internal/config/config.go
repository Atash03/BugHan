// Package config loads BugHan's environment-based configuration.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config is the full runtime configuration, loaded once at startup.
type Config struct {
	// Core
	SecretKey      string // HMAC/cookie signing secret (required)
	DatabaseURL    string // Postgres connection string (required)
	PublicURL      string // Public base URL, e.g. https://bugs.example.com (used in DSNs, links)
	BindAddr       string // HTTP listen address
	WorkerEmbedded bool   // run the background worker in this process
	SingleOrg      bool   // lock the install to the first organization

	// Ingest limits
	MaxEventBytes int64 // decompressed envelope cap

	// Retention (days)
	RetentionEventDays       int
	RetentionTransactionDays int
	RetentionSessionDays     int
	RetentionFeedbackDays    int
	RetentionReleaseDays     int

	// SMTP (optional; gates verification/invites/alert emails)
	SMTPHost string
	SMTPPort int
	SMTPUser string
	SMTPPass string
	SMTPFrom string

	// Dev/test
	DevNoSecureCookies bool
}

// Load reads configuration from the environment, applying defaults.
func Load() (*Config, error) {
	c := &Config{
		SecretKey:                os.Getenv("BUGHAN_SECRET_KEY"),
		DatabaseURL:              os.Getenv("DATABASE_URL"),
		PublicURL:                strings.TrimRight(getEnv("BUGHAN_URL", "http://localhost:8000"), "/"),
		BindAddr:                 getEnv("BUGHAN_BIND", ":8000"),
		WorkerEmbedded:           getEnvBool("BUGHAN_WORKER_EMBEDDED", true),
		SingleOrg:                getEnvBool("BUGHAN_SINGLE_ORG", false),
		MaxEventBytes:            int64(getEnvInt("BUGHAN_MAX_EVENT_MB", 20)) << 20,
		RetentionEventDays:       getEnvInt("BUGHAN_RETENTION_EVENT_DAYS", 90),
		RetentionTransactionDays: getEnvInt("BUGHAN_RETENTION_TRANSACTION_DAYS", 30),
		RetentionSessionDays:     getEnvInt("BUGHAN_RETENTION_SESSION_DAYS", 30),
		RetentionFeedbackDays:    getEnvInt("BUGHAN_RETENTION_FEEDBACK_DAYS", 90),
		RetentionReleaseDays:     getEnvInt("BUGHAN_RETENTION_RELEASE_DAYS", 365),
		SMTPHost:                 os.Getenv("BUGHAN_SMTP_HOST"),
		SMTPPort:                 getEnvInt("BUGHAN_SMTP_PORT", 587),
		SMTPUser:                 os.Getenv("BUGHAN_SMTP_USER"),
		SMTPPass:                 os.Getenv("BUGHAN_SMTP_PASS"),
		SMTPFrom:                 os.Getenv("BUGHAN_SMTP_FROM"),
		DevNoSecureCookies:       getEnvBool("BUGHAN_DEV_INSECURE_COOKIES", false),
	}
	if c.SecretKey == "" {
		// Dev convenience: stable per-install default so sessions survive restarts in dev.
		c.SecretKey = "dev-insecure-secret-key"
		if !c.DevNoSecureCookies {
			fmt.Fprintln(os.Stderr, "warning: BUGHAN_SECRET_KEY not set; using an insecure development key")
		}
	}
	if c.DatabaseURL == "" {
		c.DatabaseURL = "postgres://bughan:bughan@localhost:5432/bughan"
	}
	return c, nil
}

// SMTPEnabled reports whether outgoing email can be sent.
func (c *Config) SMTPEnabled() bool { return c.SMTPHost != "" && c.SMTPFrom != "" }

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getEnvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func getEnvBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		switch strings.ToLower(v) {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
	}
	return def
}
