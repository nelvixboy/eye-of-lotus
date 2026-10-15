package web

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nelvixboy/eye-of-lotus/internal/config"
	"github.com/nelvixboy/eye-of-lotus/internal/model"
	"github.com/nelvixboy/eye-of-lotus/internal/store"
)

// Collector is the part of the sampling loop the web layer needs. The live
// collector and the demo machine both satisfy it.
type Collector interface {
	// Snapshot returns the most recent sample, blocking only if the loop
	// has not produced one yet.
	Snapshot(ctx context.Context) model.Snapshot
}

// History is the part of the store the web layer needs.
type History interface {
	History(ctx context.Context, from, to time.Time, maxPoints int) ([]model.HistoryPoint, store.HistoryInfo, error)
	Events(ctx context.Context, limit int) ([]store.Event, error)
	Stats(ctx context.Context) (store.Stats, error)
}

// Options configures the Server.
type Options struct {
	Config    config.Config
	Collector Collector
	Store     History
	Log       *slog.Logger
	// Started is when the process started, shown in the footer.
	Started time.Time
	// Version is the build version, shown in the footer.
	Version string
}

// Server is the HTTP handler for the whole panel.
type Server struct {
	cfg     config.Config
	col     Collector
	store   History
	log     *slog.Logger
	auth    *authState
	started time.Time
	version string
	base    string
	mux     *http.ServeMux
}

// New builds the server and its routing table.
func New(opt Options) *Server {
	log := opt.Log
	if log == nil {
		log = slog.Default()
	}
	started := opt.Started
	if started.IsZero() {
		started = time.Now()
	}
	s := &Server{
		cfg:     opt.Config,
		col:     opt.Collector,
		store:   opt.Store,
		log:     log,
		auth:    newAuth(opt.Config),
		started: started,
		version: opt.Version,
		base:    strings.TrimSuffix(opt.Config.BasePath, "/"),
		mux:     http.NewServeMux(),
	}
	s.routes()
	return s
}

// routes wires every endpoint. The paths already include the base path,
// which is stripped by the caller.
func (s *Server) routes() {
	s.mux.HandleFunc("/", s.handleIndex)
	s.mux.Handle("/static/", s.staticHandler())
	s.mux.HandleFunc("/favicon.svg", s.handleFavicon)
	s.mux.HandleFunc("/robots.txt", s.handleRobots)

	s.mux.HandleFunc("/api/health", s.handleHealth)
	s.mux.HandleFunc("/api/auth/login", s.handleLogin)
	s.mux.HandleFunc("/api/auth/logout", s.handleLogout)
	s.mux.HandleFunc("/api/auth/session", s.handleSession)

	s.mux.HandleFunc("/api/meta", s.protected(s.handleMeta))
	s.mux.HandleFunc("/api/snapshot", s.protected(s.handleSnapshot))
	s.mux.HandleFunc("/api/history", s.protected(s.handleHistory))
	s.mux.HandleFunc("/api/events", s.protected(s.handleEvents))
}

// Handler returns the HTTP handler with the base path and logging applied.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}

		s.setSecurityHeaders(rec)
		if !s.stripBase(rec, r) {
			logRequest(s.log, r, http.StatusNotFound, time.Since(start).Milliseconds())
			return
		}
		s.mux.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		logRequest(s.log, r, rec.status, time.Since(start).Milliseconds())
	})
}

// stripBase removes the configured base path from the request. A request
// outside the base path is redirected to it once, which is what a person
// typing the bare hostname expects.
func (s *Server) stripBase(w http.ResponseWriter, r *http.Request) bool {
	if s.base == "" {
		return true
	}
	if r.URL.Path == s.base {
		http.Redirect(w, r, s.base+"/", http.StatusFound)
		return false
	}
	rest, ok := strings.CutPrefix(r.URL.Path, s.base+"/")
	if !ok {
		http.NotFound(w, r)
		return false
	}
	r2 := r.Clone(r.Context())
	r2.URL.Path = "/" + rest
	*r = *r2
	return true
}

// setSecurityHeaders applies the headers that make a small panel hard to
// abuse: no framing, no inline script from elsewhere, no MIME sniffing.
func (s *Server) setSecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	// The panel is a single page with local styles and scripts and no
	// external requests at all, which this policy enforces.
	h.Set("Content-Security-Policy",
		"default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; "+
			"connect-src 'self'; font-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
}

// protected wraps a handler so that it requires a valid session.
func (s *Server) protected(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.auth.authenticated(r) {
			writeError(w, http.StatusUnauthorized, "authentication required", "log in first; sessions expire after the configured lifetime")
			return
		}
		fn(w, r)
	}
}

// handleIndex serves the single page application.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	// Anything that is not the root is either an API path that was not
	// registered or a browser typo; both deserve a plain 404.
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	page, err := renderIndex(s.base)
	if err != nil {
		s.log.Error("cannot render the index page", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(page)
}

func (s *Server) handleFavicon(w http.ResponseWriter, r *http.Request) {
	b, err := staticFS.ReadFile("static/favicon.svg")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(b)
}

// handleRobots keeps the panel out of search engines, which is the least
// an operator should expect from an internal tool.
func (s *Server) handleRobots(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("User-agent: *\nDisallow: /\n"))
}

// handleHealth is deliberately unauthenticated: a supervisor needs to
// know whether the process is alive without knowing the password. It
// reveals nothing about the machine.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"version": s.version,
		"uptime":  int64(time.Since(s.started).Seconds()),
		"demo":    s.cfg.Demo,
	})
}

// sessionInfo is what the frontend learns before it draws anything.
type sessionInfo struct {
	Authenticated bool   `json:"authenticated"`
	Demo          bool   `json:"demo"`
	BasePath      string `json:"base_path"`
	Version       string `json:"version"`
	Hostname      string `json:"hostname,omitempty"`
	Theme         string `json:"theme"`
	// PollMS is how often the frontend should ask for a new snapshot.
	PollMS int `json:"poll_ms"`
	// PasswordIsPlain warns the operator that the configuration holds a
	// readable password rather than a hash.
	PasswordIsPlain bool `json:"password_is_plain,omitempty"`
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, sessionInfo{
		Authenticated:   s.auth.authenticated(r),
		Demo:            s.cfg.Demo,
		BasePath:        s.base,
		Version:         s.version,
		Theme:           "lotus-dark",
		PollMS:          2000,
		PasswordIsPlain: s.cfg.Password != "" && !config.IsHashed(s.cfg.Password),
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "use POST")
		return
	}
	if !csrfOK(r) {
		writeError(w, http.StatusForbidden, "cross site request rejected", "the Origin header does not match the requested host")
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request", err.Error())
		return
	}
	ip := s.auth.clientIP(r)
	// Demo mode exists to be looked at, and it holds no real data, so the
	// passphrase is not checked there. A session cookie is still issued,
	// which keeps the rest of the code on one path.
	ok, retry := true, time.Duration(0)
	if !s.cfg.Demo {
		ok, retry = s.auth.checkLogin(ip, body.Password)
	}
	if !ok {
		if retry > 0 {
			s.log.Warn("login throttled", "ip", ip, "retry_in", retry.String())
			w.Header().Set("Retry-After", retryIn(retry))
			writeJSON(w, http.StatusTooManyRequests, apiError{
				Error:   "too many attempts",
				Detail:  "wait before trying again",
				RetryIn: int(retry.Seconds()) + 1,
			})
			return
		}
		s.log.Warn("failed login", "ip", ip)
		writeError(w, http.StatusUnauthorized, "wrong password", "")
		return
	}
	s.auth.setSession(w, r, s.base)
	s.log.Info("login", "ip", ip)
	writeJSON(w, http.StatusOK, sessionInfo{
		Authenticated: true,
		Demo:          s.cfg.Demo,
		BasePath:      s.base,
		Version:       s.version,
		PollMS:        2000,
		Theme:         "lotus-dark",
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "use POST")
		return
	}
	if !csrfOK(r) {
		writeError(w, http.StatusForbidden, "cross site request rejected", "")
		return
	}
	s.auth.clearSession(w, r, s.base)
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": false})
}

// handleSnapshot returns the live picture of the machine.
func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	snap := s.col.Snapshot(ctx)
	writeJSON(w, http.StatusOK, snap)
}

// handleMeta returns the things that rarely change: identity, database
// statistics, the threshold table and the version.
func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	type meta struct {
		Version    string             `json:"version"`
		Demo       bool               `json:"demo"`
		Started    int64              `json:"started"`
		Now        int64              `json:"now"`
		BasePath   string             `json:"base_path"`
		Thresholds map[string]float64 `json:"thresholds"`
		History    *store.Stats       `json:"history,omitempty"`
	}
	out := meta{
		Version:  s.version,
		Demo:     s.cfg.Demo,
		Started:  s.started.UnixMilli(),
		Now:      time.Now().UnixMilli(),
		BasePath: s.base,
		// The thresholds are published so that the interface can show why
		// something is marked as a problem instead of leaving the reader
		// to guess.
		Thresholds: Thresholds(),
	}
	if s.store != nil {
		st, err := s.store.Stats(ctx)
		if err == nil {
			out.History = &st
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleEvents returns findings recorded in the past, so that a problem
// which has since been fixed is still visible.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSON(w, http.StatusOK, map[string]any{"events": []store.Event{}, "history_disabled": true})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	limit := queryInt(r, "limit", 50, 1, 500)
	events, err := s.store.Events(ctx, limit)
	if err != nil {
		s.log.Error("cannot read events", "error", err)
		writeError(w, http.StatusInternalServerError, "cannot read events", err.Error())
		return
	}
	if events == nil {
		events = []store.Event{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

// HistoryRange is one of the preset windows the interface offers.
type HistoryRange struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Span  time.Duration
}

// Ranges lists the selectable history windows, shortest first.
var Ranges = []HistoryRange{
	{Key: "15m", Label: "15 minutes", Span: 15 * time.Minute},
	{Key: "1h", Label: "1 hour", Span: time.Hour},
	{Key: "6h", Label: "6 hours", Span: 6 * time.Hour},
	{Key: "24h", Label: "24 hours", Span: 24 * time.Hour},
	{Key: "7d", Label: "7 days", Span: 7 * 24 * time.Hour},
	{Key: "30d", Label: "30 days", Span: 30 * 24 * time.Hour},
}

// handleHistory returns stored history for one of the preset windows.
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"points": []model.HistoryPoint{}, "range": "none", "history_disabled": true,
		})
		return
	}
	key := r.URL.Query().Get("range")
	if key == "" {
		key = "1h"
	}
	var span time.Duration
	for _, rg := range Ranges {
		if rg.Key == key {
			span = rg.Span
			break
		}
	}
	if span == 0 {
		writeError(w, http.StatusBadRequest, "unknown range", "valid ranges are 15m, 1h, 6h, 24h, 7d and 30d")
		return
	}
	points := queryInt(r, "points", 360, 30, 2000)

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	to := time.Now()
	from := to.Add(-span)
	hist, info, err := s.store.History(ctx, from, to, points)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			writeError(w, http.StatusGatewayTimeout, "history query timed out", "narrow the range or reduce the point count")
			return
		}
		s.log.Error("cannot read history", "error", err)
		writeError(w, http.StatusInternalServerError, "cannot read history", err.Error())
		return
	}
	if hist == nil {
		hist = []model.HistoryPoint{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"range": key,
		"from":  from.UnixMilli(),
		"to":    to.UnixMilli(),
		// The resolution tells the reader whether they are looking at
		// individual samples or at five minute averages.
		"resolution": info.Resolution,
		"points":     hist,
	})
}

func retryIn(d time.Duration) string {
	secs := int(d.Seconds()) + 1
	return strconv.Itoa(secs)
}

// Thresholds publishes the numbers the issue engine compares against, so
// that the interface can explain a verdict rather than assert it.
func Thresholds() map[string]float64 {
	return map[string]float64{
		"disk_warn":    88,
		"disk_crit":    95,
		"inode_warn":   88,
		"inode_crit":   95,
		"mem_warn":     90,
		"mem_crit":     96,
		"swap_warn":    40,
		"swap_crit":    80,
		"load_warn":    1.5,
		"load_crit":    3.0,
		"iowait_warn":  25,
		"iowait_crit":  50,
		"latency_warn": 400,
		"dns_warn":     500,
		"temp_warn":    80,
		"temp_crit":    95,
	}
}

// Static serving

func (s *Server) staticHandler() http.Handler {
	// The embedded tree keeps its "static/" prefix, so the handler serves
	// a sub-tree with that prefix removed.
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic("web: the embedded static directory is missing: " + err.Error())
	}
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Everything under /static is versioned by the binary itself, so
		// it can be cached; the HTML entry point deliberately is not.
		w.Header().Set("Cache-Control", "public, max-age=3600")
		http.StripPrefix("/static/", fileServer).ServeHTTP(w, r)
	})
}

func init() {
	// A quiet, structured logger by default; main replaces it once the
	// configured level is known.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
}
