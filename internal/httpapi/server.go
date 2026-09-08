// Package httpapi wires BugHan's HTTP surface: ingest, REST API, and web UI.
package httpapi

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/Atash03/BugHan/internal/auth"
	"github.com/Atash03/BugHan/internal/config"
	"github.com/Atash03/BugHan/internal/mail"
	"github.com/Atash03/BugHan/internal/web"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Server holds shared dependencies for all handlers.
type Server struct {
	cfg      *config.Config
	pool     *pgxpool.Pool
	log      *slog.Logger
	sessions *auth.Sessions
	mailer   *mail.Mailer
	version  string
}

// New creates the server root (binary version "dev"; use NewWithVersion
// when the caller knows the release tag).
func New(cfg *config.Config, pool *pgxpool.Pool, log *slog.Logger) *Server {
	return NewWithVersion(cfg, pool, log, "dev")
}

// NewWithVersion creates the server root, stamping health/whoami responses
// with the binary version (main.version, set via ldflags or BUGHAN_VERSION).
func NewWithVersion(cfg *config.Config, pool *pgxpool.Pool, log *slog.Logger, version string) *Server {
	return &Server{
		cfg:      cfg,
		pool:     pool,
		log:      log,
		sessions: auth.NewSessions(pool),
		mailer:   mail.New(cfg, log),
		version:  version,
	}
}

// Handler builds the full route table.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Health (liveness + DB readiness). Both spellings answer so probes
	// and humans never 404 on a missing trailing slash.
	mux.HandleFunc("GET /api/health/", s.handleHealth)
	mux.HandleFunc("GET /api/health", s.handleHealth)

	// Embedded static assets.
	mux.Handle("GET /static/", web.Static())

	// Auth + tenancy pages/API.
	s.registerAuth(mux)
	s.registerLanding(mux)
	s.registerTenancy(mux)

	// Classic release-files API (sentry-cli surface).
	s.registerReleases(mux)

	// Performance plane (summaries, transaction detail, traces, release health).
	s.registerPerformance(mux)

	// SDK-facing ingest is registered by the ingest slice.
	s.registerIngest(mux)

	// Issues API: list, triage, activity, saved views, data wipe (T5).
	s.registerIssues(mux)

	// Thin alerting: rule CRUD, send-test, delivery log (T10).
	s.registerAlerts(mux)

	// User settings pages (literal prefix: safe on the main mux).
	s.registerUserSettings(mux)

	// Server-rendered web UI (T8) lives on its own mux: leading-wildcard
	// /{org}/... patterns overlap the /static/ and /auth/ subtrees above,
	// which Go's ServeMux rejects at registration. uiOrAPI dispatches by
	// first path segment instead.
	ui := http.NewServeMux()
	s.registerWebUI(ui)

	return s.loadPrincipal(s.logRequests(uiOrAPI(ui, mux)))
}

// uiOrAPI sends extension-style app URLs (/{org}/..., /user/settings/) to
// the UI mux; ingest, REST, auth pages, and static assets stay on the main
// mux. See reservedTopSegments for the owned top-level segments.
func uiOrAPI(ui, api http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		first, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		if !reservedTopSegments(first) {
			ui.ServeHTTP(w, r)
			return
		}
		api.ServeHTTP(w, r)
	})
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		if r.URL.Path != "/api/health/" && r.URL.Path != "/api/health" {
			s.log.Info("http", "method", r.Method, "path", r.URL.Path, "status", rec.status)
		}
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}
