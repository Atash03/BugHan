package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/Atash03/BugHan/internal/ingest"
)

// registerIngest mounts the SDK-facing endpoints. Cross-origin browser SDKs
// POST here, so responses carry permissive CORS headers (GlitchTip posture).
func (s *Server) registerIngest(mux *http.ServeMux) {
	authCache := ingest.NewAuthCache(s.pool)
	limiter := ingest.NewRateLimiter(s.pool, s.cfg.IngestRateLimitPerMinute)
	proc := ingest.NewProcessor(s.pool)

	envelopeHandler := func(w http.ResponseWriter, r *http.Request) {
		s.cors(w)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		s.handleEnvelope(w, r, authCache, limiter, proc)
	}
	mux.HandleFunc("POST /api/{projectID}/envelope/", envelopeHandler)
	mux.HandleFunc("OPTIONS /api/{projectID}/envelope/", envelopeHandler)

	// Legacy JSON store endpoint (deprecated by the spec; accepted for older SDKs).
	mux.HandleFunc("POST /api/{projectID}/store/", func(w http.ResponseWriter, r *http.Request) {
		s.cors(w)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		s.handleStore(w, r, authCache, limiter, proc)
	})
}

func (s *Server) cors(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "X-Sentry-Auth, Content-Type, Content-Encoding")
	h.Set("Access-Control-Max-Age", "86400")
}

func (s *Server) handleEnvelope(w http.ResponseWriter, r *http.Request, authCache *ingest.AuthCache, limiter *ingest.RateLimiter, proc *ingest.Processor) {
	projectID := r.PathValue("projectID")

	body, ok := s.readIngestBody(w, r)
	if !ok {
		return
	}
	env, err := ingest.ParseEnvelope(body)
	if err != nil {
		writeIngestErr(w, http.StatusBadRequest, "malformed envelope: "+err.Error())
		return
	}

	key := r.URL.Query().Get("sentry_key")
	if key == "" {
		key = r.URL.Query().Get("glitchtip_key") // GlitchTip compat for OTLP-style callers
	}
	if key == "" {
		key, _ = ingest.ParseXSentryAuth(r.Header.Get("X-Sentry-Auth"))
	}
	if key == "" {
		key = env.Header.DSNPublicKey() // envelope `dsn` header (tunnel mode)
	}

	auth, err := authCache.Resolve(r.Context(), projectID, key)
	if err != nil {
		writeIngestErr(w, http.StatusForbidden, "invalid ingest key for project")
		return
	}

	if allowed, retryAfter, remaining := limiter.Check(r.Context(), auth.ProjectID); !allowed {
		w.Header().Set("Content-Type", "application/json")
		for _, kv := range ingest.QuotaHeaders(retryAfter, remaining) {
			w.Header().Set(kv[0], kv[1])
		}
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "rate limited"})
		return
	}

	res, err := proc.Process(r.Context(), env, auth.ProjectID)
	if err != nil {
		// A single malformed item fails the request per spec (4xx = drop).
		writeIngestErr(w, http.StatusBadRequest, "envelope rejected: "+err.Error())
		return
	}
	s.log.Debug("envelope accepted", "project", auth.ProjectID,
		"accepted", res.Accepted, "ignored", res.Ignored, "unknown", res.Unknown, "dupes", res.Duplicates)
	s.enqueueAlertEvaluations(r.Context(), res.Signals, auth.ProjectID)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"id": env.Header.EventID, "accepted": res.Accepted})
}

// enqueueAlertEvaluations schedules async alert-rule evaluation (DESIGN.md
// §12) for each stored error event. Failures to enqueue never fail ingest —
// the worker also self-heals nothing here; the event is already stored.
func (s *Server) enqueueAlertEvaluations(ctx context.Context, signals []ingest.AlertSignal, projectID string) {
	for _, sig := range signals {
		payload, _ := json.Marshal(map[string]any{
			"project_id": projectID, "issue_id": sig.IssueID, "event_id": sig.EventID,
			"is_new": sig.IsNew, "is_regression": sig.IsRegression,
			"environment": sig.Environment, "release": sig.Release, "level": sig.Level,
		})
		if _, err := s.pool.Exec(ctx,
			`INSERT INTO jobs (kind, payload, run_at) VALUES ('alert_evaluate', $1, now())`,
			payload); err != nil {
			s.log.Warn("enqueue alert evaluation", "err", err)
		}
	}
}

// handleStore accepts the legacy single-event JSON store endpoint by wrapping
// the payload into a synthetic envelope.
func (s *Server) handleStore(w http.ResponseWriter, r *http.Request, authCache *ingest.AuthCache, limiter *ingest.RateLimiter, proc *ingest.Processor) {
	projectID := r.PathValue("projectID")

	body, ok := s.readIngestBody(w, r)
	if !ok {
		return
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		writeIngestErr(w, http.StatusBadRequest, "malformed event JSON: "+err.Error())
		return
	}
	key := r.URL.Query().Get("sentry_key")
	if key == "" {
		key, _ = ingest.ParseXSentryAuth(r.Header.Get("X-Sentry-Auth"))
	}
	auth, err := authCache.Resolve(r.Context(), projectID, key)
	if err != nil {
		writeIngestErr(w, http.StatusForbidden, "invalid ingest key for project")
		return
	}
	if allowed, retryAfter, remaining := limiter.Check(r.Context(), auth.ProjectID); !allowed {
		for _, kv := range ingest.QuotaHeaders(retryAfter, remaining) {
			w.Header().Set(kv[0], kv[1])
		}
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}

	// Synthetic envelope: {"event_id": ...}\n{"type":"event"}\n<payload>
	eventID, _ := payload["event_id"]
	header, _ := json.Marshal(map[string]json.RawMessage{"event_id": eventID})
	envBody := append(append(append(header, '\n'), []byte(`{"type":"event"}`)...), '\n')
	envBody = append(envBody, body...)
	env, err := ingest.ParseEnvelope(envBody)
	if err != nil {
		writeIngestErr(w, http.StatusBadRequest, "malformed event: "+err.Error())
		return
	}
	res, err := proc.Process(r.Context(), env, auth.ProjectID)
	if err != nil {
		writeIngestErr(w, http.StatusBadRequest, "event rejected: "+err.Error())
		return
	}
	s.enqueueAlertEvaluations(r.Context(), res.Signals, auth.ProjectID)
	// eventID is raw JSON — decode to a plain string so the response id
	// isn't double-quoted.
	var idStr string
	if eventID != nil {
		_ = json.Unmarshal(eventID, &idStr)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"id": idStr, "accepted": res.Accepted})
}

// readIngestBody enforces the raw body cap and decompresses per
// content-encoding; writes the proper error response on failure.
func (s *Server) readIngestBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	maxBytes := s.cfg.MaxEventBytes
	if maxBytes <= 0 {
		maxBytes = 20 << 20 // defensive default matching the config default
	}
	if r.ContentLength > maxBytes+64<<10 {
		writeIngestErr(w, http.StatusRequestEntityTooLarge, "request body too large")
		return nil, false
	}
	body, err := ingest.Decompress(r.Body, r.Header.Get("Content-Encoding"), maxBytes)
	if err != nil {
		if errors.Is(err, ingest.ErrTooLarge) {
			writeIngestErr(w, http.StatusRequestEntityTooLarge, "decompressed envelope too large")
			return nil, false
		}
		writeIngestErr(w, http.StatusBadRequest, "could not read body: "+err.Error())
		return nil, false
	}
	return body, true
}

func writeIngestErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
