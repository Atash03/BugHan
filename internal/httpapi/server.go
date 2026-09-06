// Package httpapi wires BugHan's HTTP surface: ingest, REST API, and web UI.
package httpapi

import (
	"log/slog"
	"net/http"

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

// New creates the server root.
func New(cfg *config.Config, pool *pgxpool.Pool, log *slog.Logger) *Server {
	return &Server{
		cfg:      cfg,
		pool:     pool,
		log:      log,
		sessions: auth.NewSessions(pool),
		mailer:   mail.New(cfg, log),
	}
}

// Handler builds the full route table.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Health (liveness + DB readiness).
	mux.HandleFunc("GET /api/health/", s.handleHealth)

	// Embedded static assets.
	mux.Handle("GET /static/", web.Static())

	// Auth + tenancy pages/API.
	s.registerAuth(mux)
	s.registerLanding(mux)
	s.registerTenancy(mux)

	// SDK-facing ingest is registered by the ingest slice.
	s.registerIngest(mux)

	// Issues API: list, triage, activity, saved views, data wipe (T5).
	s.registerIssues(mux)

	return s.loadPrincipal(s.logRequests(mux))
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		if r.URL.Path != "/api/health/" {
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
