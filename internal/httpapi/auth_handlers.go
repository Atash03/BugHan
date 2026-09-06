package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Atash03/BugHan/internal/auth"
	"github.com/Atash03/BugHan/internal/web"
	"github.com/google/uuid"
)

const inviteTTL = 7 * 24 * time.Hour

func (s *Server) registerAuth(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		if auth.FromContext(r.Context()) == nil {
			http.Redirect(w, r, "/auth/login/", http.StatusFound)
			return
		}
		http.Redirect(w, r, "/auth/landing/", http.StatusFound)
	})
	mux.HandleFunc("GET /auth/setup/", s.pageSetup)
	mux.HandleFunc("POST /auth/setup/", s.postSetup)
	mux.HandleFunc("GET /auth/login/", s.pageLogin)
	mux.HandleFunc("POST /auth/login/", s.postLogin)
	mux.HandleFunc("POST /auth/logout/", s.postLogout)
	mux.HandleFunc("GET /auth/signup/", s.pageSignup)
	mux.HandleFunc("POST /auth/signup/", s.postSignup)
	mux.HandleFunc("GET /auth/forgot/", s.pageForgot)
	mux.HandleFunc("POST /auth/forgot/", s.postForgot)
	mux.HandleFunc("GET /auth/reset/{token}", s.pageReset)
	mux.HandleFunc("POST /auth/reset/", s.postReset)
	mux.HandleFunc("GET /accept/{token}", s.pageAcceptInvite)
	mux.HandleFunc("POST /accept/{token}", s.postAcceptInvite)
}

func (s *Server) secureCookies() bool { return !s.cfg.DevNoSecureCookies }

func page(w http.ResponseWriter, status int, name, title string, data web.PageData) {
	data.Title = title
	web.Render(w, status, name, data)
}

// userCount guards the first-run setup page.
func (s *Server) userCount(r *http.Request) int {
	var n int
	_ = s.pool.QueryRow(r.Context(), `SELECT count(*) FROM users`).Scan(&n)
	return n
}

func (s *Server) pageSetup(w http.ResponseWriter, r *http.Request) {
	if auth.FromContext(r.Context()) != nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if s.userCount(r) > 0 {
		http.Redirect(w, r, "/auth/login/", http.StatusFound)
		return
	}
	page(w, 200, "setup.html", "Set up BugHan", web.PageData{})
}

func (s *Server) postSetup(w http.ResponseWriter, r *http.Request) {
	if s.userCount(r) > 0 {
		http.Redirect(w, r, "/auth/login/", http.StatusFound)
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	email := strings.ToLower(strings.TrimSpace(r.PostFormValue("email")))
	password := r.PostFormValue("password")
	orgName := strings.TrimSpace(r.PostFormValue("org"))

	if name == "" || email == "" || password == "" || orgName == "" {
		page(w, 400, "setup.html", "Set up BugHan", web.PageData{Error: "All fields are required."})
		return
	}
	if len(password) < 8 {
		page(w, 400, "setup.html", "Set up BugHan", web.PageData{Error: "Password must be at least 8 characters."})
		return
	}

	userID := uuid.NewString()
	orgID := uuid.NewString()
	hash, err := auth.HashPassword(password)
	if err != nil {
		writeErr(w, 500, "hash password: "+err.Error())
		return
	}
	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer tx.Rollback(r.Context())

	orgSlug := auth.Slugify(orgName)
	var finalSlug string
	if err := tx.QueryRow(r.Context(), `
		INSERT INTO organizations (id, name, slug) VALUES ($1, $2, $3)
		ON CONFLICT (slug) DO UPDATE SET name = EXCLUDED.name
		RETURNING slug`, orgID, orgName, orgSlug).Scan(&finalSlug); err != nil {
		page(w, 400, "setup.html", "Set up BugHan", web.PageData{Error: "Could not create organization: " + err.Error()})
		return
	}
	if _, err := tx.Exec(r.Context(),
		`INSERT INTO users (id, email, name, password_hash, verified) VALUES ($1, $2, $3, $4, true)`,
		userID, email, name, hash); err != nil {
		page(w, 400, "setup.html", "Set up BugHan", web.PageData{Error: "Could not create user: " + err.Error()})
		return
	}
	if _, err := tx.Exec(r.Context(),
		`INSERT INTO memberships (id, org_id, user_id, role) VALUES ($1, $2, $3, 'owner')`,
		uuid.NewString(), orgID, userID); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	_ = s.sessions.Create(w, r, userID, s.secureCookies())
	http.Redirect(w, r, "/", http.StatusFound)
}

func (s *Server) pageLogin(w http.ResponseWriter, r *http.Request) {
	if auth.FromContext(r.Context()) != nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if s.userCount(r) == 0 {
		http.Redirect(w, r, "/auth/setup/", http.StatusFound)
		return
	}
	page(w, 200, "login.html", "Log in", web.PageData{})
}

func (s *Server) postLogin(w http.ResponseWriter, r *http.Request) {
	email := strings.ToLower(strings.TrimSpace(r.PostFormValue("email")))
	password := r.PostFormValue("password")

	var userID, hash string
	err := s.pool.QueryRow(r.Context(), `SELECT id, password_hash FROM users WHERE email = $1`, email).
		Scan(&userID, &hash)
	if err != nil {
		page(w, 401, "login.html", "Log in", web.PageData{Error: "Invalid email or password."})
		return
	}
	ok, err := auth.VerifyPassword(password, hash)
	if err != nil || !ok {
		page(w, 401, "login.html", "Log in", web.PageData{Error: "Invalid email or password."})
		return
	}
	_ = s.sessions.Create(w, r, userID, s.secureCookies())
	http.Redirect(w, r, "/", http.StatusFound)
}

func (s *Server) postLogout(w http.ResponseWriter, r *http.Request) {
	s.sessions.Destroy(w, r)
	http.Redirect(w, r, "/auth/login/", http.StatusFound)
}

func (s *Server) pageSignup(w http.ResponseWriter, r *http.Request) {
	if auth.FromContext(r.Context()) != nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	page(w, 200, "signup.html", "Sign up", web.PageData{})
}

func (s *Server) postSignup(w http.ResponseWriter, r *http.Request) {
	if s.cfg.SingleOrg {
		page(w, 403, "login.html", "Log in", web.PageData{Error: "This install does not accept open signups."})
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	email := strings.ToLower(strings.TrimSpace(r.PostFormValue("email")))
	password := r.PostFormValue("password")
	if name == "" || email == "" || len(password) < 8 {
		page(w, 400, "signup.html", "Sign up", web.PageData{Error: "Name, email, and a password of at least 8 characters are required."})
		return
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	userID := uuid.NewString()
	if _, err := s.pool.Exec(r.Context(),
		`INSERT INTO users (id, email, name, password_hash) VALUES ($1, $2, $3, $4)`,
		userID, email, name, hash); err != nil {
		page(w, 400, "signup.html", "Sign up", web.PageData{Error: "Could not create account (email may already exist)."})
		return
	}
	_ = s.sessions.Create(w, r, userID, s.secureCookies())
	http.Redirect(w, r, "/", http.StatusFound)
}

func (s *Server) pageForgot(w http.ResponseWriter, r *http.Request) {
	page(w, 200, "forgot.html", "Forgot password", web.PageData{})
}

func (s *Server) postForgot(w http.ResponseWriter, r *http.Request) {
	email := strings.ToLower(strings.TrimSpace(r.PostFormValue("email")))
	var userID string
	err := s.pool.QueryRow(r.Context(), `SELECT id FROM users WHERE email = $1`, email).Scan(&userID)
	notice := "If that account exists, a reset link has been sent."
	if err == nil {
		raw, _ := auth.NewToken()
		if _, err := s.pool.Exec(r.Context(),
			`UPDATE users SET reset_token = $2, reset_expires = $3 WHERE id = $1`,
			userID, auth.HashToken(raw), time.Now().Add(time.Hour)); err == nil {
			link := s.cfg.PublicURL + "/auth/reset/" + raw
			if err := s.mailer.Send(email, "BugHan password reset", "Reset your password:\n"+link+"\n\nThe link expires in one hour."); err != nil {
				s.log.Error("send reset email", "err", err)
			}
			if !s.mailer.Enabled() {
				notice = "SMTP is not configured — use this link to reset the password: " + link
			}
		}
	}
	page(w, 200, "forgot.html", "Forgot password", web.PageData{Notice: notice})
}

func (s *Server) pageReset(w http.ResponseWriter, r *http.Request) {
	page(w, 200, "reset.html", "Reset password", web.PageData{Data: map[string]any{"token": r.PathValue("token")}})
}

func (s *Server) postReset(w http.ResponseWriter, r *http.Request) {
	token := r.PostFormValue("token")
	password := r.PostFormValue("password")
	if len(password) < 8 {
		page(w, 400, "reset.html", "Reset password", web.PageData{Error: "Password must be at least 8 characters.", Data: map[string]any{"token": token}})
		return
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	ct, err := s.pool.Exec(r.Context(),
		`UPDATE users SET password_hash = $2, reset_token = NULL, reset_expires = NULL
		 WHERE reset_token = $1 AND reset_expires > now()`,
		auth.HashToken(token), hash)
	if err != nil || ct.RowsAffected() == 0 {
		page(w, 400, "forgot.html", "Forgot password", web.PageData{Error: "That reset link is invalid or expired."})
		return
	}
	http.Redirect(w, r, "/auth/login/", http.StatusFound)
}

func (s *Server) pageAcceptInvite(w http.ResponseWriter, r *http.Request) {
	var orgName, role string
	err := s.pool.QueryRow(r.Context(), `
		SELECT o.name, i.role FROM invitations i
		JOIN organizations o ON o.id = i.org_id
		WHERE i.token_hash = $1 AND i.accepted_at IS NULL AND i.expires_at > now()`,
		auth.HashToken(r.PathValue("token"))).Scan(&orgName, &role)
	if err != nil {
		page(w, 404, "login.html", "Log in", web.PageData{Error: "That invite link is invalid, used, or expired."})
		return
	}
	page(w, 200, "accept.html", "Accept invite", web.PageData{
		Data: map[string]any{"org_name": orgName, "role": role, "token": r.PathValue("token")},
	})
}

func (s *Server) postAcceptInvite(w http.ResponseWriter, r *http.Request) {
	u := auth.FromContext(r.Context())
	if u == nil {
		writeErr(w, 401, "log in first")
		return
	}
	token := r.PathValue("token")
	var invID, orgID, role string
	err := s.pool.QueryRow(r.Context(), `
		SELECT i.id, i.org_id, i.role FROM invitations i
		WHERE i.token_hash = $1 AND i.accepted_at IS NULL AND i.expires_at > now()
		  AND lower(i.email) = $2`,
		auth.HashToken(token), strings.ToLower(u.Email)).Scan(&invID, &orgID, &role)
	if err != nil {
		page(w, 404, "login.html", "Log in", web.PageData{Error: "That invite link is invalid, used, or expired (or was sent to a different email)."})
		return
	}
	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer tx.Rollback(r.Context())
	if _, err := tx.Exec(r.Context(),
		`INSERT INTO memberships (id, org_id, user_id, role) VALUES ($1, $2, $3, $4)
		 ON CONFLICT (org_id, user_id) DO UPDATE SET role = EXCLUDED.role`,
		uuid.NewString(), orgID, u.ID, role); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if _, err := tx.Exec(r.Context(),
		`UPDATE invitations SET accepted_at = now() WHERE id = $1`, invID); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

// Minimal landing until the full UI slice lands: lists the user's orgs and
// offers org creation.
func (s *Server) registerLanding(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/landing/", func(w http.ResponseWriter, r *http.Request) {
		u := auth.FromContext(r.Context())
		if u == nil {
			http.Redirect(w, r, "/auth/login/", http.StatusFound)
			return
		}
		rows, err := s.pool.Query(r.Context(), `
			SELECT o.name, o.slug, m.role FROM organizations o
			JOIN memberships m ON m.org_id = o.id AND m.user_id = $1
			ORDER BY o.name`, u.ID)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		defer rows.Close()
		type orgRow struct{ Name, Slug, Role string }
		orgs := []orgRow{}
		for rows.Next() {
			var o orgRow
			_ = rows.Scan(&o.Name, &o.Slug, &o.Role)
			orgs = append(orgs, o)
		}
		var hasOrgs string
		if len(orgs) == 0 {
			hasOrgs = "none"
		}
		page(w, 200, "landing.html", "Your organizations", web.PageData{
			Data: map[string]any{"user": u.Name, "orgs": orgs, "has_orgs": hasOrgs,
				"csrf": auth.CSRF(r.Context())},
		})
	})
	mux.Handle("POST /orgs/new/", s.requireCSRF(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimSpace(r.PostFormValue("name"))
		if name == "" {
			http.Redirect(w, r, "/auth/landing/", http.StatusFound)
			return
		}
		r.Header.Set("Content-Type", "application/json")
		body, _ := json.Marshal(map[string]string{"name": name})
		r.Body = io.NopCloser(bytes.NewReader(body))
		s.handleCreateOrg(w, r)
	})))
}
