package app

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/seanhamlin/tendarr/internal/clients/arr"
	"github.com/seanhamlin/tendarr/internal/media"
	"github.com/seanhamlin/tendarr/internal/settings"
	"github.com/seanhamlin/tendarr/internal/upgrade"
)

// keyUpgradeLastRun persists when the last scheduled upgrade was attempted,
// so a restart does not reset the clock and grab early.
const keyUpgradeLastRun = "autoupgrade.last_run"

// schedulerUser is the audit-log username for scheduled actions.
const schedulerUser = "scheduler"

// UpgraderStatus is shown on the settings and upgrade pages.
type UpgraderStatus struct {
	Enabled        bool
	Radarr, Sonarr bool
	EveryHours     int
	// Pending counts items with a fresh, acceptable release waiting.
	Pending    int
	LastAt     time.Time
	LastTitle  string
	LastResult string
	NextAt     time.Time
	Error      string
}

// Covers reports whether scheduled upgrades run for one service.
func (s UpgraderStatus) Covers(k arr.Kind) bool {
	switch k {
	case arr.Radarr:
		return s.Radarr
	case arr.Sonarr:
		return s.Sonarr
	}
	return false
}

func (s *UpgraderStatus) applyConfig(cfg settings.AutoUpgrade) {
	s.Enabled = cfg.Enabled()
	s.Radarr, s.Sonarr = cfg.Radarr, cfg.Sonarr
	s.EveryHours = int(cfg.Every / time.Hour)
	if !s.Enabled {
		s.NextAt, s.Pending = time.Time{}, 0
	}
}

type upgrader struct {
	mu     sync.Mutex
	status UpgraderStatus
	wake   chan struct{}
}

// pendingUpgrade is an item with an acceptable release, ranked by how much
// space replacing it would free.
type pendingUpgrade struct {
	kind      arr.Kind
	req       upgrade.Request
	candidate upgrade.Candidate
	saving    int64
}

// StartUpgrader runs scheduled upgrades until Shutdown.
func (a *App) StartUpgrader() {
	if s, err := a.Settings(a.ctx); err == nil {
		a.upg.mu.Lock()
		a.upg.status.applyConfig(s.AutoUpgrade())
		a.upg.status.LastAt = a.lastUpgradeRun(a.ctx)
		a.upg.mu.Unlock()
	}
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.runUpgrader()
	}()
}

// WakeUpgrader makes the upgrader re-read its settings now.
func (a *App) WakeUpgrader() {
	select {
	case a.upg.wake <- struct{}{}:
	default:
	}
}

// UpgraderStatus returns a copy of the upgrader's state.
func (a *App) UpgraderStatus() UpgraderStatus {
	a.upg.mu.Lock()
	defer a.upg.mu.Unlock()
	return a.upg.status
}

func (a *App) runUpgrader() {
	// Same startup pause as the release checker, so Radarr/Sonarr are up.
	wait := 30 * time.Second
	for {
		a.upg.mu.Lock()
		if a.upg.status.Enabled {
			a.upg.status.NextAt = a.Now().Add(wait)
		}
		a.upg.mu.Unlock()

		select {
		case <-a.ctx.Done():
			return
		case <-a.upg.wake:
		case <-time.After(wait):
		}

		wait = a.AutoUpgradeOnce(a.ctx)
	}
}

// AutoUpgradeOnce performs at most one scheduled upgrade and returns how long
// to wait before trying again. Exported for tests, which drive it without a
// clock.
func (a *App) AutoUpgradeOnce(ctx context.Context) time.Duration {
	s, err := a.Settings(ctx)
	if err != nil {
		a.Logger.Error("upgrader could not read settings", "error", err)
		return 5 * time.Minute
	}
	cfg := s.AutoUpgrade()

	a.upg.mu.Lock()
	a.upg.status.applyConfig(cfg)
	a.upg.mu.Unlock()
	if !cfg.Enabled() {
		// Settings changes wake the loop immediately.
		return time.Hour
	}

	// Waking early (a settings save, a restart) never shortens the gap.
	if due := a.lastUpgradeRun(ctx).Add(cfg.Every); a.Now().Before(due) {
		return due.Sub(a.Now())
	}

	queue, errMsg := a.buildUpgradeQueue(ctx, s, cfg)
	a.upg.mu.Lock()
	a.upg.status.Pending = len(queue)
	a.upg.status.Error = errMsg
	a.upg.mu.Unlock()
	if len(queue) == 0 {
		// Nothing found yet. The clock is not reset, so the first upgrade a
		// release check turns up goes within the hour.
		return queueRefreshEvery
	}

	next := queue[0]
	title := next.candidate.DisplayTitle()
	_, _, err = a.Search(ctx, schedulerUser, next.kind, next.req, []int{next.candidate.Key})
	result := fmt.Sprintf("searched, about %s smaller", media.FormatBytes(next.saving))
	if next.saving <= 0 {
		result = "searched"
	}
	if err != nil {
		result = "failed: " + err.Error()
	} else if err := a.Store.DeleteReleaseChecks(ctx, string(next.kind), []int{next.candidate.Key}); err != nil {
		// The result described the old file; dropping it means the item is
		// only upgraded again once a fresh check finds something.
		a.Logger.Error("could not clear release check", "error", err)
	}
	a.Logger.Info("scheduled upgrade", "service", next.kind, "title", title, "savingBytes", next.saving, "result", result)

	// An attempt counts even when it fails, so a broken connection is
	// retried at the scheduled pace rather than hammered.
	now := a.Now()
	if err := a.Store.SaveSettings(ctx, map[string]string{keyUpgradeLastRun: now.Format(time.RFC3339Nano)}); err != nil {
		a.Logger.Error("could not record scheduled upgrade", "error", err)
	}
	a.upg.mu.Lock()
	a.upg.status.LastAt, a.upg.status.LastTitle, a.upg.status.LastResult = now, title, result
	a.upg.status.Pending = len(queue) - 1
	a.upg.mu.Unlock()

	return cfg.Every
}

// buildUpgradeQueue lists eligible items whose latest release check, for
// the current file and within the recheck window, found an acceptable
// release. The biggest space saving goes first, across both services.
//
// Scans use the scheduled release checks' codec and size filters, so the
// two schedules agree on what is a candidate.
func (a *App) buildUpgradeQueue(ctx context.Context, s settings.Settings, cfg settings.AutoUpgrade) ([]pendingUpgrade, string) {
	check := s.AutoCheck()
	if check.Codecs == "" {
		return nil, "the scheduled release check filters are invalid; fix them in Settings"
	}

	var out []pendingUpgrade
	var errMsg string
	now := a.Now()
	for _, kind := range []arr.Kind{arr.Radarr, arr.Sonarr} {
		if (kind == arr.Radarr && !cfg.Radarr) || (kind == arr.Sonarr && !cfg.Sonarr) {
			continue
		}
		if !s.Conn(serviceFor(kind)).Configured() {
			errMsg = kind.Label() + " is not configured"
			continue
		}
		req := upgrade.Request{Codecs: check.Codecs, MinSize: check.MovieMinSize, Sort: upgrade.SortSize}
		if kind == arr.Sonarr {
			req.MinSize = check.EpisodeMinSize
		}
		plan, _, err := a.UpgradePlan(ctx, kind, req, false)
		if err != nil {
			errMsg = kind.Label() + ": " + err.Error()
			continue
		}
		checks, err := a.ReleaseChecks(ctx, kind, plan)
		if err != nil {
			errMsg = err.Error()
			continue
		}
		for _, c := range plan.Eligible {
			rc, ok := checks[c.Key]
			if !ok || !rc.Available || rc.Error != "" {
				continue
			}
			if check.RecheckAfter > 0 && now.Sub(rc.CheckedAt) > check.RecheckAfter {
				continue // too old to trust the release is still there
			}
			out = append(out, pendingUpgrade{kind: kind, req: req, candidate: c, saving: c.File.Size - rc.BestSize})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].saving != out[j].saving {
			return out[i].saving > out[j].saving
		}
		return out[i].candidate.Key < out[j].candidate.Key
	})
	return out, errMsg
}

func (a *App) lastUpgradeRun(ctx context.Context) time.Time {
	m, err := a.Store.Settings(ctx)
	if err != nil {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339Nano, m[keyUpgradeLastRun])
	return t
}

// SaveAutoUpgrade stores the upgrade schedule and applies it immediately.
func (a *App) SaveAutoUpgrade(ctx context.Context, user string, f settings.AutoUpgradeForm) error {
	if err := a.Store.SaveSettings(ctx, settings.AutoUpgradeValues(f)); err != nil {
		return err
	}
	a.Logger.Info("scheduled upgrade settings saved", "user", user, "radarr", f.Radarr, "sonarr", f.Sonarr, "everyHours", f.EveryHours)

	cfg, _ := settings.ValidateAutoUpgrade(f)
	a.upg.mu.Lock()
	a.upg.status.applyConfig(cfg)
	a.upg.mu.Unlock()
	a.WakeUpgrader()
	return nil
}
