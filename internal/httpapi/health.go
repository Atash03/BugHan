package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// handleHealth implements GET /api/health[/] — 200 when live, 503 when the
// DB is unreachable (compose healthcheck + CI smoke use this). The body
// always carries status, binary version, and server time; Cache-Control is
// no-store so probes never serve a cached verdict.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	resp := map[string]any{
		"status":  "ok",
		"version": s.version,
		"time":    time.Now().UTC().Format(time.RFC3339),
	}
	if err := s.pool.Ping(ctx); err != nil {
		resp["status"] = "degraded"
		resp["database"] = "unreachable"
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(resp)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(resp)
}
