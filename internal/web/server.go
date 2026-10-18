// Package web serves the HTML interface.
package web

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/seanhamlin/tendarr/internal/app"
	"github.com/seanhamlin/tendarr/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// Config controls server behaviour.
type Config struct {
	// CookieSecure: "auto" sets Secure when the request arrived over HTTPS
	// (directly or via X-Forwarded-Proto), "true" always, "false" never.
	CookieSecure string
	SessionTTL   time.Duration
	// TrustedOrigins are extra origins allowed to submit forms, for reverse
	// proxies that rewrite the Host header.
	TrustedOrigins []string
	Version        string
}

// Server is the HTTP handler.
type Server struct {
	app     *app.App
	store   *store.Store
	logger  *slog.Logger
	cfg     Config
	pages   map[string]pageTemplate
	limiter *loginLimiter
	handler http.Handler

	setupMu sync.Mutex
}

// New builds the server.
func New(a *app.App, cfg Config) (*Server, error) {
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = 7 * 24 * time.Hour
	}
	if cfg.CookieSecure == "" {
		cfg.CookieSecure = "auto"
	}

	pages, err := parseTemplates()
	if err != nil {
		return nil, err
	}

	s := &Server{
		app:     a,
		store:   a.Store,
		logger:  a.Logger,
		cfg:     cfg,
		pages:   pages,
		limiter: newLoginLimiter(10, 15*time.Minute),
	}

	cop := http.NewCrossOriginProtection()
	for _, o := range cfg.TrustedOrigins {
		if err := cop.AddTrustedOrigin(o); err != nil {
			return nil, fmt.Errorf("trusted origin %q: %w", o, err)
		}
	}
	cop.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.logger.Warn("cross-origin request rejected", "path", r.URL.Path, "origin", r.Header.Get("Origin"))
		http.Error(w, "Cross-origin request rejected", http.StatusForbidden)
	}))

	s.handler = s.recoverer(s.logRequests(securityHeaders(cop.Handler(s.withSession(s.routes())))))
	return s, nil
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	static, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheStatic(http.FileServerFS(static))))
	mux.HandleFunc("GET /healthz", s.handleHealth)

	mux.HandleFunc("GET /setup", s.handleSetupForm)
	mux.HandleFunc("POST /setup", s.handleSetup)
	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.handleLogin)

	auth := func(h http.HandlerFunc) http.Handler { return s.requireAuth(h) }

	mux.Handle("POST /logout", auth(s.handleLogout))
	mux.Handle("GET /{$}", auth(s.handleDashboard))
	mux.Handle("GET /activity", auth(s.handleActivity))

	mux.Handle("GET /settings", auth(s.handleSettings))
	mux.Handle("POST /settings/services/{service}", auth(s.handleSaveService))
	mux.Handle("POST /settings/library", auth(s.handleSaveLibrary))
	mux.Handle("POST /settings/password", auth(s.handleChangePassword))
	mux.Handle("POST /settings/test", auth(s.handleTestAll))
	mux.Handle("POST /settings/autocheck", auth(s.handleSaveAutoCheck))
	mux.Handle("POST /settings/autoupgrade", auth(s.handleSaveAutoUpgrade))

	mux.Handle("GET /reap/{type}", auth(s.handleReap))
	mux.Handle("POST /reap/{type}/confirm", auth(s.handleReapConfirm))
	mux.Handle("POST /reap/{type}/delete", auth(s.handleReapDelete))
	mux.Handle("POST /reap/{type}/ignore", auth(s.handleReapIgnore))

	mux.Handle("GET /upgrade/{type}", auth(s.handleUpgrade))
	mux.Handle("POST /upgrade/{type}/search", auth(s.handleUpgradeSearch))
	mux.Handle("POST /upgrade/{type}/check", auth(s.handleUpgradeCheck))
	mux.Handle("POST /upgrade/{type}/monitor", auth(s.handleUpgradeMonitor))
	mux.Handle("GET /upgrade/{type}/job", auth(s.handleUpgradeJob))

	return s.setupGate(mux)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}

// setupGate sends every page to /setup until an account exists, and keeps
// /setup closed afterwards.
func (s *Server) setupGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if strings.HasPrefix(p, "/static/") || p == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		n, err := s.store.CountUsers(r.Context())
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		switch {
		case n == 0 && p != "/setup":
			http.Redirect(w, r, "/setup", http.StatusSeeOther)
		case n > 0 && p == "/setup":
			http.Redirect(w, r, "/login", http.StatusSeeOther)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'; object-src 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		if !strings.HasPrefix(r.URL.Path, "/static/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

func cacheStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		next.ServeHTTP(w, r)
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

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if strings.HasPrefix(r.URL.Path, "/static/") || r.URL.Path == "/healthz" {
			return
		}
		level := slog.LevelInfo
		if rec.status >= 500 {
			level = slog.LevelError
		}
		// Path only: query strings can carry filter values but never secrets,
		// and keeping the line short keeps the container log readable.
		s.logger.Log(r.Context(), level, "request", "method", r.Method, "path", r.URL.Path, "status", rec.status, "took", time.Since(start).Round(time.Millisecond))
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(v)
				}
				s.logger.Error("panic", "error", v, "path", r.URL.Path, "stack", string(debug.Stack()))
				http.Error(w, "Internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) serverError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	s.logger.Error("request failed", "path", r.URL.Path, "error", err)
	http.Error(w, "Internal server error", http.StatusInternalServerError)
}
