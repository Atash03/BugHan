package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/Atash03/BugHan/internal/auth"
)

// loadPrincipal attaches the authenticated principal to the request context:
// a cookie session for browsers, or a Bearer API token for machines.
func (s *Server) loadPrincipal(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authz := r.Header.Get("Authorization")
		if strings.HasPrefix(authz, "Bearer ") {
			token := strings.TrimSpace(strings.TrimPrefix(authz, "Bearer "))
			if auth.ValidAPITokenShape(token) {
				if u, scope := s.userForAPIToken(r, token); u != nil {
					ctx := auth.WithUser(r.Context(), u, "")
					ctx = context.WithValue(ctx, principalScopeKey{}, scope)
					r = r.WithContext(ctx)
					next.ServeHTTP(w, r)
					return
				}
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="bughan"`)
			writeErr(w, http.StatusUnauthorized, "invalid or revoked API token")
			return
		}

		if uid := s.sessions.Lookup(r); uid != "" {
			if u, csrf := s.userForSession(r, uid); u != nil {
				r = r.WithContext(auth.WithUser(r.Context(), u, csrf))
			}
		}
		next.ServeHTTP(w, r)
	})
}

// principalScopeKey carries the credential kind: "read" or "write" for API
// tokens, "" for sessions (full user authority).
type principalScopeKey struct{}

func principalScope(ctx context.Context) string {
	v, _ := ctx.Value(principalScopeKey{}).(string)
	return v
}

// requireAuth 401s unauthenticated callers.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth.FromContext(r.Context()) == nil {
			writeErr(w, http.StatusUnauthorized, "authentication required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireCSRF enforces the CSRF token on state-changing session-authed posts.
// Bearer-token requests are exempt (no ambient credentials).
func (s *Server) requireCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth.CSRF(r.Context()) == "" {
			next.ServeHTTP(w, r)
			return
		}
		tok := r.Header.Get("X-CSRF-Token")
		if tok == "" {
			tok = r.PostFormValue("csrf_token")
		}
		if tok == "" || tok != auth.CSRF(r.Context()) {
			writeErr(w, http.StatusForbidden, "missing or invalid CSRF token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

type memberRow struct {
	id, email, name, orgID, role, orgSlug string
}

func scanMemberRows(rows interface {
	Next() bool
	Scan(dest ...any) error
}) (*auth.User, bool) {
	var u *auth.User
	for rows.Next() {
		var m memberRow
		if err := rows.Scan(&m.id, &m.email, &m.name, &m.orgID, &m.role, &m.orgSlug); err != nil {
			return nil, false
		}
		if u == nil {
			u = &auth.User{ID: m.id, Email: m.email, Name: m.name, Memberships: map[string]string{}}
		}
		if m.orgID != "" {
			u.Memberships[m.orgID] = m.role
			u.Memberships[m.orgSlug] = m.role
		}
	}
	return u, u != nil
}

// userForSession loads the session user, memberships, and CSRF token
// (the session row id doubles as the CSRF token).
func (s *Server) userForSession(r *http.Request, userID string) (*auth.User, string) {
	rows, err := s.pool.Query(r.Context(), `
		SELECT u.id, u.email, u.name,
		       COALESCE(m.org_id::text, ''), COALESCE(m.role, ''), COALESCE(o.slug, '')
		FROM users u
		LEFT JOIN memberships m ON m.user_id = u.id
		LEFT JOIN organizations o ON o.id = m.org_id
		WHERE u.id = $1`, userID)
	if err != nil {
		return nil, ""
	}
	defer rows.Close()
	u, ok := scanMemberRows(rows)
	if !ok {
		return nil, ""
	}
	var csrf string
	_ = s.pool.QueryRow(r.Context(),
		`SELECT id::text FROM auth_sessions WHERE token_hash = $1`,
		auth.HashToken(sessionTokenFrom(r))).Scan(&csrf)
	return u, csrf
}

// userForAPIToken resolves a Bearer token to its owner and scope.
func (s *Server) userForAPIToken(r *http.Request, token string) (*auth.User, string) {
	rows, err := s.pool.Query(r.Context(), `
		SELECT u.id, u.email, u.name, t.scope,
		       COALESCE(m.org_id::text, ''), COALESCE(m.role, ''), COALESCE(o.slug, '')
		FROM api_tokens t
		JOIN users u ON u.id = t.user_id
		LEFT JOIN memberships m ON m.user_id = u.id
		LEFT JOIN organizations o ON o.id = m.org_id
		WHERE t.token_hash = $1 AND t.revoked_at IS NULL`, auth.HashToken(token))
	if err != nil {
		return nil, ""
	}
	defer rows.Close()
	var scope string
	var u *auth.User
	for rows.Next() {
		var m memberRow
		if err := rows.Scan(&m.id, &m.email, &m.name, &scope, &m.orgID, &m.role, &m.orgSlug); err != nil {
			return nil, ""
		}
		if u == nil {
			u = &auth.User{ID: m.id, Email: m.email, Name: m.name, Memberships: map[string]string{}}
		}
		if m.orgID != "" {
			u.Memberships[m.orgID] = m.role
			u.Memberships[m.orgSlug] = m.role
		}
	}
	if u == nil {
		return nil, ""
	}
	_, _ = s.pool.Exec(r.Context(), `UPDATE api_tokens SET last_used_at = now()
		WHERE token_hash = $1 AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute')`,
		auth.HashToken(token))
	return u, scope
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func sessionTokenFrom(r *http.Request) string {
	c, err := r.Cookie("bughan_session")
	if err != nil {
		return ""
	}
	return c.Value
}

// orgRole returns the requester's role in the org (by slug or id); "" if none.
func (s *Server) orgRole(r *http.Request, orgIDOrSlug string) string {
	u := auth.FromContext(r.Context())
	if u == nil {
		return ""
	}
	return u.Memberships[orgIDOrSlug]
}

func roleAtLeast(role, min string) bool {
	rank := map[string]int{"member": 1, "admin": 2, "owner": 3}
	return rank[role] >= rank[min]
}
