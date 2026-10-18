// Package app is the service layer between the web handlers and everything
// else: it builds clients from the stored settings, caches library scans,
// performs actions, and records each one in the audit log.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/seanhamlin/tendarr/internal/clients/arr"
	"github.com/seanhamlin/tendarr/internal/clients/plex"
	"github.com/seanhamlin/tendarr/internal/clients/tautulli"
	"github.com/seanhamlin/tendarr/internal/library"
	"github.com/seanhamlin/tendarr/internal/settings"
	"github.com/seanhamlin/tendarr/internal/store"
	"github.com/seanhamlin/tendarr/internal/upgrade"
)

// NotConfiguredError means a feature needs a service the admin has not set up.
type NotConfiguredError struct {
	Service settings.Service
}

func (e *NotConfiguredError) Error() string {
	return fmt.Sprintf("%s is not configured yet", e.Service.Label())
}

// IsNotConfigured reports a NotConfiguredError.
func IsNotConfigured(err error) bool {
	var e *NotConfiguredError
	return errors.As(err, &e)
}

// App is the service layer.
type App struct {
	Store  *store.Store
	Logger *slog.Logger
	Now    func() time.Time

	snapshots *cache[*library.Snapshot]
	upgrades  *cache[upgrade.Plan]

	jobsMu sync.Mutex
	jobs   map[arr.Kind]*Job

	sched *scheduler
	upg   *upgrader

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// Factories are swappable so tests can inject fakes.
	NewArr      func(kind arr.Kind, url, key string) (ArrClient, error)
	NewPlex     func(url, token string) (library.PlexSource, error)
	NewTautulli func(url, key string) (library.WatchSource, error)
}

// ArrClient is everything the app needs from Radarr or Sonarr.
type ArrClient interface {
	library.ArrSource
	upgrade.RadarrSource
	EpisodeFiles(ctx context.Context, seriesID int) ([]arr.File, error)
	Episodes(ctx context.Context, seriesID int) ([]arr.Episode, error)
	Delete(ctx context.Context, id int, addExclusion bool) (arr.DeleteResult, error)
	EnsureTag(ctx context.Context, label string) (int, error)
	SetTag(ctx context.Context, itemIDs []int, tagID int, add bool) error
	Search(ctx context.Context, ids []int) (int, error)
	SetMonitored(ctx context.Context, ids []int, monitored bool) error
	Releases(ctx context.Context, id int) ([]arr.Release, error)
}

// New builds the service layer.
func New(st *store.Store, logger *slog.Logger) *App {
	ctx, cancel := context.WithCancel(context.Background())
	return &App{
		Store:     st,
		Logger:    logger,
		Now:       func() time.Time { return time.Now().UTC() },
		snapshots: newCache[*library.Snapshot](15 * time.Minute),
		upgrades:  newCache[upgrade.Plan](15 * time.Minute),
		jobs:      map[arr.Kind]*Job{},
		sched:     &scheduler{wake: make(chan struct{}, 1)},
		upg:       &upgrader{wake: make(chan struct{}, 1)},
		ctx:       ctx,
		cancel:    cancel,
		NewArr: func(kind arr.Kind, url, key string) (ArrClient, error) {
			return arr.New(kind, url, key)
		},
		NewPlex: func(url, token string) (library.PlexSource, error) {
			return plex.New(url, token)
		},
		NewTautulli: func(url, key string) (library.WatchSource, error) {
			return tautulli.New(url, key)
		},
	}
}

// Shutdown stops background jobs and waits for them.
func (a *App) Shutdown() {
	a.cancel()
	a.wg.Wait()
}

// Settings loads the current settings.
func (a *App) Settings(ctx context.Context) (settings.Settings, error) {
	m, err := a.Store.Settings(ctx)
	if err != nil {
		return settings.Settings{}, err
	}
	return settings.FromMap(m), nil
}

// SaveConnection stores one service's connection and drops cached scans,
// which were built from the old one.
func (a *App) SaveConnection(ctx context.Context, user string, svc settings.Service, c settings.Connection) error {
	if err := a.Store.SaveSettings(ctx, settings.ConnectionValues(svc, c)); err != nil {
		return err
	}
	a.invalidate()
	a.Logger.Info("connection settings saved", "service", svc, "user", user, "url", c.URL)
	return nil
}

// SaveLibrary stores the library options.
func (a *App) SaveLibrary(ctx context.Context, user, pathMap string, excluded []string, ignoreTag string) error {
	if err := a.Store.SaveSettings(ctx, settings.LibraryValues(pathMap, excluded, ignoreTag)); err != nil {
		return err
	}
	a.invalidate()
	a.Logger.Info("library settings saved", "user", user)
	return nil
}

func (a *App) invalidate() {
	a.snapshots.clear()
	a.upgrades.clear()
}

func serviceFor(kind arr.Kind) settings.Service {
	if kind == arr.Radarr {
		return settings.Radarr
	}
	return settings.Sonarr
}

func (a *App) arrClient(s settings.Settings, kind arr.Kind) (ArrClient, error) {
	c := s.Conn(serviceFor(kind))
	if !c.Configured() {
		return nil, &NotConfiguredError{Service: serviceFor(kind)}
	}
	return a.NewArr(kind, c.URL, c.Key)
}

// ArrClient returns a client for Radarr or Sonarr built from current settings.
func (a *App) ArrClient(ctx context.Context, kind arr.Kind) (ArrClient, error) {
	s, err := a.Settings(ctx)
	if err != nil {
		return nil, err
	}
	return a.arrClient(s, kind)
}

func (a *App) audit(ctx context.Context, e store.AuditEntry) {
	if err := a.Store.AddAudit(ctx, e); err != nil {
		// Never abort an otherwise successful action over a missing ledger
		// line; the container log still has it.
		a.Logger.Error("could not write audit entry", "error", err, "action", e.Action, "title", e.Title)
	}
}

// RecentActivity returns the audit log, newest first.
func (a *App) RecentActivity(ctx context.Context, limit int) ([]store.AuditEntry, error) {
	return a.Store.RecentAudit(ctx, limit)
}

// ServiceStatus is the result of a live connection test.
type ServiceStatus struct {
	Service    settings.Service
	Configured bool
	OK         bool
	Detail     string
}

// TestAll checks every configured service concurrently.
func (a *App) TestAll(ctx context.Context) ([]ServiceStatus, error) {
	s, err := a.Settings(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ServiceStatus, len(settings.Services))
	var wg sync.WaitGroup
	for i, svc := range settings.Services {
		out[i] = ServiceStatus{Service: svc, Configured: s.Conn(svc).Configured()}
		if !out[i].Configured {
			out[i].Detail = "Not configured"
			continue
		}
		wg.Add(1)
		go func(i int, svc settings.Service) {
			defer wg.Done()
			detail, err := settings.Test(ctx, svc, s.Conn(svc))
			if err != nil {
				out[i].Detail = err.Error()
				return
			}
			out[i].OK, out[i].Detail = true, detail
		}(i, svc)
	}
	wg.Wait()
	return out, nil
}
