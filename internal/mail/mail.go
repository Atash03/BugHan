// Package mail sends transactional email; without SMTP configured it logs
// the message (dev/self-host recovery path) and reports itself disabled.
package mail

import (
	"fmt"
	"log/slog"
	"net/smtp"
	"strings"

	"github.com/Atash03/BugHan/internal/config"
)

type Mailer struct {
	cfg *config.Config
	log *slog.Logger
}

func New(cfg *config.Config, log *slog.Logger) *Mailer {
	return &Mailer{cfg: cfg, log: log}
}

func (m *Mailer) Enabled() bool { return m.cfg.SMTPEnabled() }

// Send delivers a message, or logs it when SMTP is unconfigured.
func (m *Mailer) Send(to, subject, body string) error {
	if !m.Enabled() {
		m.log.Info("email (SMTP disabled; not sent)", "to", to, "subject", subject, "body", body)
		return nil
	}
	addr := fmt.Sprintf("%s:%d", m.cfg.SMTPHost, m.cfg.SMTPPort)
	from := m.cfg.SMTPFrom
	msg := strings.Join([]string{
		"From: " + from,
		"To: " + to,
		"Subject: " + subject,
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=utf-8",
		"",
		body,
	}, "\r\n")
	var auth smtp.Auth
	if m.cfg.SMTPUser != "" {
		auth = smtp.PlainAuth("", m.cfg.SMTPUser, m.cfg.SMTPPass, m.cfg.SMTPHost)
	}
	return smtp.SendMail(addr, auth, from, []string{to}, []byte(msg))
}
