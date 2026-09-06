package ingest

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrTooLarge reports an oversized (decompressed) envelope → HTTP 413.
var ErrTooLarge = errors.New("envelope too large")

// ErrAuth reports an invalid key/project pair → HTTP 403.
var ErrAuth = errors.New("invalid or revoked ingest key")

// ProjectAuth is the resolved auth context for one ingest request.
type ProjectAuth struct {
	ProjectID string
	OrgID     string
	KeyID     string
}

// AuthCache resolves (projectID, sentry_key) pairs with a small TTL cache so
// flood traffic doesn't hammer Postgres (30s per the auth decision).
type AuthCache struct {
	pool *pgxpool.Pool
	ttl  time.Duration

	mu      sync.Mutex
	entries map[string]authEntry
	neg     map[string]time.Time // negative cache for invalid keys
}

type authEntry struct {
	auth      ProjectAuth
	expiresAt time.Time
}

func NewAuthCache(pool *pgxpool.Pool) *AuthCache {
	return &AuthCache{
		pool:    pool,
		ttl:     30 * time.Second,
		entries: map[string]authEntry{},
		neg:     map[string]time.Time{},
	}
}

// Resolve authenticates a request. Returns ErrAuth on failure.
func (c *AuthCache) Resolve(ctx context.Context, projectID, sentryKey string) (ProjectAuth, error) {
	if projectID == "" || sentryKey == "" {
		return ProjectAuth{}, ErrAuth
	}
	ck := projectID + "|" + sentryKey

	now := time.Now()
	c.mu.Lock()
	if e, ok := c.entries[ck]; ok && now.Before(e.expiresAt) {
		c.mu.Unlock()
		return e.auth, nil
	}
	if t, ok := c.neg[ck]; ok && now.Before(t) {
		c.mu.Unlock()
		return ProjectAuth{}, ErrAuth
	}
	c.mu.Unlock()

	var a ProjectAuth
	err := c.pool.QueryRow(ctx, `
		SELECT k.id::text, p.id::text, p.org_id::text
		FROM ingest_keys k
		JOIN projects p ON p.id = k.project_id
		WHERE k.id = $1 AND p.id::text = $2 AND k.revoked_at IS NULL`,
		sentryKey, projectID).Scan(&a.KeyID, &a.ProjectID, &a.OrgID)
	if err != nil {
		c.mu.Lock()
		if len(c.neg) > 10_000 {
			c.neg = map[string]time.Time{} // bound memory
		}
		c.neg[ck] = now.Add(c.ttl)
		c.mu.Unlock()
		return ProjectAuth{}, ErrAuth
	}

	c.mu.Lock()
	if len(c.entries) > 10_000 {
		c.entries = map[string]authEntry{}
	}
	c.entries[ck] = authEntry{auth: a, expiresAt: now.Add(c.ttl)}
	c.mu.Unlock()
	return a, nil
}

// ParseX SentryAuth extracts the sentry_key from an X-Sentry-Auth header:
// "Sentry sentry_version=7, sentry_key=…, sentry_client=…".
// ParseXSentryAuth extracts the sentry_key from an X-Sentry-Auth header:
// "Sentry sentry_version=7, sentry_key=…, sentry_client=…".
func ParseXSentryAuth(header string) (key string, version string) {
	v := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(header), "Sentry"))
	for _, pair := range strings.Split(v, ",") {
		k, val, found := strings.Cut(pair, "=")
		if !found {
			continue
		}
		k = strings.TrimSpace(k)
		val = strings.TrimSpace(val)
		switch strings.ToLower(k) {
		case "sentry_key":
			key = val
		case "sentry_version":
			version = val
		}
	}
	return key, version
}
