// Command tendarr serves the Tendarr web UI.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // the distroless image has no zoneinfo; TZ still works

	"github.com/seanhamlin/tendarr/internal/app"
	"github.com/seanhamlin/tendarr/internal/store"
	"github.com/seanhamlin/tendarr/internal/web"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}
	if len(os.Args) > 1 && (os.Args[1] == "version" || os.Args[1] == "--version") {
		fmt.Println(version)
		return
	}

	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

type config struct {
	listen         string
	dataDir        string
	cookieSecure   string
	sessionTTL     time.Duration
	trustedOrigins []string
	logLevel       slog.Level
	logFormat      string
}

func loadConfig() (config, error) {
	c := config{
		listen:       env("TENDARR_LISTEN_ADDR", ":8484"),
		dataDir:      env("TENDARR_DATA_DIR", "/data"),
		cookieSecure: strings.ToLower(env("TENDARR_COOKIE_SECURE", "auto")),
		logFormat:    strings.ToLower(env("TENDARR_LOG_FORMAT", "text")),
	}

	switch c.cookieSecure {
	case "auto", "true", "false":
	default:
		return c, fmt.Errorf("TENDARR_COOKIE_SECURE must be auto, true or false")
	}
	switch c.logFormat {
	case "text", "json":
	default:
		return c, fmt.Errorf("TENDARR_LOG_FORMAT must be text or json")
	}

	ttl, err := time.ParseDuration(env("TENDARR_SESSION_TTL", "168h"))
	if err != nil || ttl < 5*time.Minute {
		return c, fmt.Errorf("TENDARR_SESSION_TTL must be a Go duration of at least 5m, e.g. 168h")
	}
	c.sessionTTL = ttl

	if err := c.logLevel.UnmarshalText([]byte(env("TENDARR_LOG_LEVEL", "info"))); err != nil {
		return c, fmt.Errorf("TENDARR_LOG_LEVEL must be debug, info, warn or error")
	}

	for _, o := range strings.Split(os.Getenv("TENDARR_TRUSTED_ORIGINS"), ",") {
		if o = strings.TrimSpace(o); o != "" {
			c.trustedOrigins = append(c.trustedOrigins, o)
		}
	}
	return c, nil
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	opts := &slog.HandlerOptions{Level: cfg.logLevel}
	var handler slog.Handler = slog.NewTextHandler(os.Stdout, opts)
	if cfg.logFormat == "json" {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}
	logger := slog.New(handler)
	slog.SetDefault(logger)

	// Before anything touches the data directory, so every file it creates
	// belongs to PUID:PGID.
	if err := dropPrivileges(cfg.dataDir, logger); err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.dataDir, 0o750); err != nil {
		return fmt.Errorf("create data directory %s: %w (%s)", cfg.dataDir, err, writableHint())
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dbPath := filepath.Join(cfg.dataDir, "tendarr.db")
	if err := adoptLegacyDatabase(cfg.dataDir, dbPath, logger); err != nil {
		return err
	}
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		return fmt.Errorf("%w (%s)", err, writableHint())
	}
	defer st.Close()

	a := app.New(st, logger)
	defer a.Shutdown()
	a.StartScheduler()
	a.StartUpgrader()

	srv, err := web.New(a, web.Config{
		CookieSecure:   cfg.cookieSecure,
		SessionTTL:     cfg.sessionTTL,
		TrustedOrigins: cfg.trustedOrigins,
		Version:        version,
	})
	if err != nil {
		return err
	}

	httpServer := &http.Server{
		Addr:              cfg.listen,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// Generous: the first scan of a large library reads every section
		// from Plex and Tautulli before the page can render.
		WriteTimeout: 5 * time.Minute,
		IdleTimeout:  2 * time.Minute,
		ErrorLog:     slog.NewLogLogger(handler, slog.LevelWarn),
	}

	go purgeSessions(ctx, st, logger)

	n, _ := st.CountUsers(ctx)
	logger.Info("starting", "version", version, "listen", cfg.listen, "database", dbPath, "accounts", n)
	if n == 0 {
		logger.Warn("no account exists yet; open the web UI to create the administrator account")
	}

	errCh := make(chan error, 1)
	go func() {
		ln, err := net.Listen("tcp", cfg.listen)
		if err != nil {
			errCh <- err
			return
		}
		errCh <- httpServer.Serve(ln)
	}()

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		logger.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return httpServer.Shutdown(shutdownCtx)
}

// writableHint explains a data directory the process cannot write to.
func writableHint() string {
	return fmt.Sprintf("is the volume writable by uid %d gid %d? Set PUID and PGID to the owner of the folder on the host", os.Getuid(), os.Getgid())
}

func purgeSessions(ctx context.Context, st *store.Store, logger *slog.Logger) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if n, err := st.PurgeExpiredSessions(ctx); err != nil && ctx.Err() == nil {
			logger.Warn("could not purge sessions", "error", err)
		} else if n > 0 {
			logger.Info("purged expired sessions", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// healthcheck is used by the container HEALTHCHECK; the distroless image has
// no curl or wget.
func healthcheck() int {
	addr := env("TENDARR_LISTEN_ADDR", ":8484")
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + addr + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "unhealthy:", resp.Status)
		return 1
	}
	return 0
}

// adoptLegacyDatabase renames the database from before the project was called
// Tendarr, so an existing install keeps its account, settings and history.
// It does nothing when the new file already exists or the old one does not.
func adoptLegacyDatabase(dataDir, dbPath string, logger *slog.Logger) error {
	legacy := filepath.Join(dataDir, "plex-automations.db")
	if _, err := os.Stat(dbPath); err == nil {
		return nil
	}
	if _, err := os.Stat(legacy); err != nil {
		return nil
	}
	// The WAL and shared-memory files belong to the database and must move
	// with it, or recent writes would be lost.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		from, to := legacy+suffix, dbPath+suffix
		if _, err := os.Stat(from); err != nil {
			continue
		}
		if err := os.Rename(from, to); err != nil {
			return fmt.Errorf("rename %s to %s: %w", from, to, err)
		}
	}
	logger.Info("renamed database from the previous project name", "from", legacy, "to", dbPath)
	return nil
}
