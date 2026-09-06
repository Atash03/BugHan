// Package httpapi wires BugHan's HTTP surface: ingest, REST API, and web UI.
package httpapi

import (
	"log/slog"
	"net/http"

	"github.com/Atash03/BugHan/internal/config"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Server holds shared dependencies for all handlers.
type Server struct {
	cfg  *config.Config
	pool *pgxpool.Pool
	log  *slog.Logger
}

// New creates the server root.
func New(cfg *config.Config, pool *pgxpool.Pool, log *slog.Logger) *Server {
	return &Server{cfg: cfg, pool: pool, log: log}
}

// Handler builds the full route table.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Health (liveness + DB readiness).
	mux.HandleFunc("GET /api/health/", s.handleHealth)

	// API v0 (token-authenticated REST) and ingest are registered by later slices.
	s.registerIngest(mux)

	return s.logRequests(mux)
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
