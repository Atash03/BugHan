package auth

import (
	"context"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	sessionCookie = "bughan_session"
	sessionTTL    = 14 * 24 * time.Hour
	slidingWindow = 24 * time.Hour // renew the cookie when less than this remains
)

// Sessions stores server-side cookie sessions.
type Sessions struct {
	pool *pgxpool.Pool
}

func NewSessions(pool *pgxpool.Pool) *Sessions { return &Sessions{pool: pool} }

// Create starts a session for a user and sets the cookie.
func (s *Sessions) Create(w http.ResponseWriter, r *http.Request, userID string, secure bool) error {
	raw, err := NewToken()
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(r.Context(), `
		INSERT INTO auth_sessions (id, user_id, token_hash, expires_at)
		VALUES (gen_random_uuid(), $1, $2, $3)`,
		userID, HashToken(raw), time.Now().Add(sessionTTL))
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    raw,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
	return nil
}

// Lookup resolves the session cookie to a user id, sliding the expiry when
// less than the sliding window remains. Returns "" when unauthenticated.
func (s *Sessions) Lookup(r *http.Request) (userID string) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return ""
	}
	var id string
	var expires time.Time
	err = s.pool.QueryRow(r.Context(), `
		SELECT user_id, expires_at FROM auth_sessions
		WHERE token_hash = $1 AND expires_at > now()`,
		HashToken(c.Value)).Scan(&id, &expires)
	if err != nil {
		return ""
	}
	if time.Until(expires) < slidingWindow {
		_, _ = s.pool.Exec(r.Context(), `
			UPDATE auth_sessions SET expires_at = $2, last_seen_at = now()
			WHERE token_hash = $1`,
			HashToken(c.Value), time.Now().Add(sessionTTL))
	}
	return id
}

// Destroy removes the session and clears the cookie.
func (s *Sessions) Destroy(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		_, _ = s.pool.Exec(r.Context(), `DELETE FROM auth_sessions WHERE token_hash = $1`, HashToken(c.Value))
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
}

type ctxKey int

const (
	ctxUser ctxKey = iota
	ctxCSRF
)

// User holds the authenticated principal attached to a request.
type User struct {
	ID    string
	Email string
	Name  string
	// Memberships cached at load: org id → role.
	Memberships map[string]string
}

// WithUser attaches the authenticated user (and CSRF token) to the context.
func WithUser(ctx context.Context, u *User, csrf string) context.Context {
	ctx = context.WithValue(ctx, ctxUser, u)
	return context.WithValue(ctx, ctxCSRF, csrf)
}

// FromContext returns the authenticated user, or nil.
func FromContext(ctx context.Context) *User {
	u, _ := ctx.Value(ctxUser).(*User)
	return u
}

// CSRF returns the CSRF token for the request ("" for token-authed API calls).
func CSRF(ctx context.Context) string {
	s, _ := ctx.Value(ctxCSRF).(string)
	return s
}
