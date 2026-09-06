package httpapi

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Atash03/BugHan/internal/perf"
	"github.com/google/uuid"
)

// registerPerformance wires the performance plane REST endpoints
// (DESIGN.md §11): window summary, transaction detail, trace by id, and
// release health. Project-scoped; any member may read.
func (s *Server) registerPerformance(mux *http.ServeMux) {
	requireProject := func(h http.HandlerFunc) http.Handler {
		return s.requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := s.projectFromPath(r)
			if p == nil {
				writeErr(w, http.StatusNotFound, "project not found")
				return
			}
			if !roleAtLeast(s.orgRole(r, p.OrgSlug), "member") {
				writeErr(w, http.StatusForbidden, "insufficient role")
				return
			}
			h(w, r)
		}))
	}

	mux.Handle("GET /api/0/projects/{org}/{project}/performance/summary/", requireProject(s.handlePerformanceSummary))
	mux.Handle("GET /api/0/projects/{org}/{project}/performance/transaction/", requireProject(s.handlePerformanceTransaction))
	mux.Handle("GET /api/0/projects/{org}/{project}/traces/{traceID}/", requireProject(s.handleTraceByID))
	mux.Handle("GET /api/0/projects/{org}/{project}/releases-health/", requireProject(s.handleReleasesHealth))
}

var statsPeriodRe = regexp.MustCompile(`^(\d+)([hdw])$`)

// statsPeriod parses ?statsPeriod= (1h/24h/7d/30d…) into [since, until];
// default 24h, capped at 30d.
func statsPeriod(r *http.Request) (time.Time, time.Time) {
	until := time.Now().UTC()
	d := 24 * time.Hour
	if m := statsPeriodRe.FindStringSubmatch(r.URL.Query().Get("statsPeriod")); m != nil {
		n, _ := strconv.Atoi(m[1])
		switch m[2] {
		case "h":
			d = time.Duration(n) * time.Hour
		case "d":
			d = time.Duration(n) * 24 * time.Hour
		case "w":
			d = time.Duration(n) * 7 * 24 * time.Hour
		}
		if max := 30 * 24 * time.Hour; d > max {
			d = max
		}
		if d < time.Hour {
			d = time.Hour
		}
	}
	return until.Add(-d), until
}

func (s *Server) handlePerformanceSummary(w http.ResponseWriter, r *http.Request) {
	p := s.projectFromPath(r)
	since, until := statsPeriod(r)
	rows, err := perf.TransactionSummary(r.Context(), s.pool, p.ID, since, until)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, rows)
}

func (s *Server) handlePerformanceTransaction(w http.ResponseWriter, r *http.Request) {
	p := s.projectFromPath(r)
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		writeErr(w, http.StatusBadRequest, "name query parameter is required")
		return
	}
	since, until := statsPeriod(r)
	detail, err := perf.TransactionDetail(r.Context(), s.pool, p.ID, name, since, until)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, detail)
}

func (s *Server) handleTraceByID(w http.ResponseWriter, r *http.Request) {
	p := s.projectFromPath(r)
	traceID := r.PathValue("traceID")
	if _, err := uuid.Parse(traceID); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid trace id")
		return
	}
	trace, err := perf.TraceByID(r.Context(), s.pool, p.ID, traceID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, trace)
}

func (s *Server) handleReleasesHealth(w http.ResponseWriter, r *http.Request) {
	p := s.projectFromPath(r)
	since, until := statsPeriod(r)
	rows, err := perf.ReleaseHealth(r.Context(), s.pool, p.ID, since, until)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, rows)
}
