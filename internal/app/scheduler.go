package app

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/seanhamlin/tendarr/internal/clients/arr"
	"github.com/seanhamlin/tendarr/internal/settings"
	"github.com/seanhamlin/tendarr/internal/upgrade"
)

// queueRefreshEvery bounds how often the scheduler re-reads Radarr/Sonarr to
// rebuild its work list. Between refreshes it works through the list it has.
const queueRefreshEvery = time.Hour

// autoItem is one pending scheduled check.
type autoItem struct {
	kind      arr.Kind
	candidate upgrade.Candidate
}

// SchedulerStatus is shown on the settings and upgrade pages.
type SchedulerStatus struct {
	Enabled bool
	// Radarr and Sonarr say which services are scheduled.
	Radarr, Sonarr bool
	// Scanning is set while the list of due items is being worked out, so
	// an empty list is not mistaken for "nothing to do".
	Scanning bool
	// Built is when the list was last worked out; zero until the first scan.
	Built       time.Time
	RecheckDays int
	PerHour     int
	Pending     int
	LastAt      time.Time
	LastTitle   string
	LastResult  string
	NextAt      time.Time
	Error       string
}

// Covers reports whether scheduled checks run for one service.
func (s SchedulerStatus) Covers(k arr.Kind) bool {
	switch k {
	case arr.Radarr:
		return s.Radarr
	case arr.Sonarr:
		return s.Sonarr
	}
	return false
}

type scheduler struct {
	mu      sync.Mutex
	queue   []autoItem
	builtAt time.Time
	status  SchedulerStatus
	wake    chan struct{}
}

// StartScheduler runs scheduled release checks until Shutdown.
func (a *App) StartScheduler() {
	// Load the saved settings into the status now: the first check is
	// deliberately delayed, and until it runs the pages would otherwise
	// report the schedule as off.
	if s, err := a.Settings(a.ctx); err == nil {
		a.sched.mu.Lock()
		a.sched.status.applyConfig(s.AutoCheck())
		a.sched.mu.Unlock()
	}
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.runScheduler()
	}()
}

// WakeScheduler makes the scheduler re-read its settings now, e.g. after
// they change, instead of at the end of its current wait.
func (a *App) WakeScheduler() {
	select {
	case a.sched.wake <- struct{}{}:
	default:
	}
	a.sched.mu.Lock()
	a.sched.builtAt = time.Time{} // filters may have changed
	a.sched.mu.Unlock()
}

// SchedulerStatus returns a copy of the scheduler's state.
func (a *App) SchedulerStatus() SchedulerStatus {
	a.sched.mu.Lock()
	defer a.sched.mu.Unlock()
	return a.sched.status
}

func (a *App) runScheduler() {
	// A short pause on startup lets the NAS bring Radarr/Sonarr up first.
	wait := 30 * time.Second
	for {
		a.sched.mu.Lock()
		if a.sched.status.Enabled {
			a.sched.status.NextAt = a.Now().Add(wait)
		} else {
			a.sched.status.NextAt = time.Time{}
		}
		a.sched.mu.Unlock()

		select {
		case <-a.ctx.Done():
			return
		case <-a.sched.wake:
		case <-time.After(wait):
		}

		wait = a.AutoCheckOnce(a.ctx)
	}
}

// AutoCheckOnce performs at most one scheduled check and returns how long to
// wait before the next. Exported for tests, which drive it without a clock.
func (a *App) AutoCheckOnce(ctx context.Context) time.Duration {
	s, err := a.Settings(ctx)
	if err != nil {
		a.Logger.Error("scheduler could not read settings", "error", err)
		return 5 * time.Minute
	}
	cfg := s.AutoCheck()

	a.sched.mu.Lock()
	a.sched.status.applyConfig(cfg)
	if !cfg.Enabled() {
		a.sched.queue, a.sched.status.Pending, a.sched.status.NextAt = nil, 0, time.Time{}
		a.sched.mu.Unlock()
		// Nothing to do; settings changes wake the loop immediately.
		return time.Hour
	}
	rebuild := len(a.sched.queue) == 0 || a.Now().Sub(a.sched.builtAt) > queueRefreshEvery
	a.sched.mu.Unlock()

	if rebuild {
		a.sched.mu.Lock()
		a.sched.status.Scanning = true
		a.sched.mu.Unlock()

		queue, errMsg := a.buildAutoQueue(ctx, s, cfg)
		a.sched.mu.Lock()
		a.sched.queue, a.sched.builtAt = queue, a.Now()
		a.sched.status.Scanning = false
		a.sched.status.Built = a.sched.builtAt
		a.sched.status.Error = errMsg
		a.sched.status.Pending = len(queue)
		a.sched.mu.Unlock()
		if len(queue) > 0 || errMsg != "" {
			a.Logger.Info("scheduled release checks queued", "pending", len(queue), "error", errMsg)
		}
	}

	item, ok := a.popAutoItem()
	if !ok {
		// Everything is checked recently enough. Look again after the
		// refresh window rather than spinning.
		return queueRefreshEvery
	}

	client, err := a.ArrClient(ctx, item.kind)
	if err != nil {
		a.setSchedulerError(err.Error())
		return cfg.Interval()
	}

	rc := a.checkReleases(client, item.kind, item.candidate)

	result := "no acceptable release"
	switch {
	case rc.Error != "":
		result = "failed: " + rc.Error
	case rc.Available:
		result = "upgrade available: " + rc.BestTitle
	}
	a.Logger.Info("scheduled release check", "service", item.kind, "title", item.candidate.DisplayTitle(), "result", result)

	a.sched.mu.Lock()
	a.sched.status.LastAt = a.Now()
	a.sched.status.LastTitle = item.candidate.DisplayTitle()
	a.sched.status.LastResult = result
	a.sched.status.Pending = len(a.sched.queue)
	a.sched.mu.Unlock()

	return cfg.Interval()
}

func (s *SchedulerStatus) applyConfig(cfg settings.AutoCheck) {
	s.Enabled = cfg.Enabled()
	s.Radarr, s.Sonarr = cfg.Radarr, cfg.Sonarr
	s.PerHour = cfg.PerHour
	s.RecheckDays = int(cfg.RecheckAfter.Hours() / 24)
}

func (a *App) setSchedulerError(msg string) {
	a.sched.mu.Lock()
	a.sched.status.Error = msg
	a.sched.mu.Unlock()
}

// popAutoItem takes the next item, skipping a service while a manual check
// is running against it, so the two never search the same indexers at once.
func (a *App) popAutoItem() (autoItem, bool) {
	busy := map[arr.Kind]bool{}
	for _, k := range []arr.Kind{arr.Radarr, arr.Sonarr} {
		if j, ok := a.JobStatus(k); ok && j.Running() {
			busy[k] = true
		}
	}

	a.sched.mu.Lock()
	defer a.sched.mu.Unlock()
	for i, it := range a.sched.queue {
		if busy[it.kind] {
			continue
		}
		a.sched.queue = append(a.sched.queue[:i:i], a.sched.queue[i+1:]...)
		a.sched.status.Pending = len(a.sched.queue)
		return it, true
	}
	return autoItem{}, false
}

// buildAutoQueue lists the eligible candidates that are due a check: never
// checked, checked against a file that has since changed, or checked longer
// ago than the recheck window. Never-checked items go first, then the
// stalest, then the largest. Services are interleaved so neither starves.
func (a *App) buildAutoQueue(ctx context.Context, s settings.Settings, cfg settings.AutoCheck) ([]autoItem, string) {
	type due struct {
		item    autoItem
		checked time.Time // zero when never checked
	}

	var perKind [][]due
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

		req := upgrade.Request{Codecs: cfg.Codecs, MinSize: cfg.MovieMinSize, Sort: upgrade.SortSize}
		if kind == arr.Sonarr {
			req.MinSize = cfg.EpisodeMinSize
		}
		plan, _, err := a.UpgradePlan(ctx, kind, req, false)
		if err != nil {
			errMsg = kind.Label() + ": " + err.Error()
			a.Logger.Warn("scheduler could not scan", "service", kind, "error", err)
			continue
		}
		checks, err := a.Store.ReleaseChecks(ctx, string(kind))
		if err != nil {
			errMsg = err.Error()
			continue
		}

		var list []due
		for _, c := range plan.Eligible {
			rc, seen := checks[c.Key]
			if seen && rc.FileID == c.File.ID && now.Sub(rc.CheckedAt) < cfg.RecheckAfter {
				continue
			}
			d := due{item: autoItem{kind: kind, candidate: c}}
			if seen && rc.FileID == c.File.ID {
				d.checked = rc.CheckedAt
			}
			list = append(list, d)
		}
		sort.SliceStable(list, func(i, j int) bool {
			a, b := list[i], list[j]
			if a.checked.IsZero() != b.checked.IsZero() {
				return a.checked.IsZero()
			}
			if !a.checked.Equal(b.checked) {
				return a.checked.Before(b.checked)
			}
			return a.item.candidate.File.Size > b.item.candidate.File.Size
		})
		perKind = append(perKind, list)
	}

	var out []autoItem
	for i := 0; ; i++ {
		added := false
		for _, list := range perKind {
			if i < len(list) {
				out = append(out, list[i].item)
				added = true
			}
		}
		if !added {
			break
		}
	}
	return out, errMsg
}

// SaveAutoCheck stores scheduler settings and applies them immediately.
func (a *App) SaveAutoCheck(ctx context.Context, user string, f settings.AutoCheckForm) error {
	if err := a.Store.SaveSettings(ctx, settings.AutoCheckValues(f)); err != nil {
		return err
	}
	a.Logger.Info("scheduled release check settings saved", "user", user, "radarr", f.Radarr, "sonarr", f.Sonarr, "perHour", f.PerHour, "recheckDays", f.RecheckDays)

	// Reflect the change straight away: the page reloads before the
	// scheduler has finished rescanning.
	cfg, _ := settings.ValidateAutoCheck(f)
	a.sched.mu.Lock()
	a.sched.status.applyConfig(cfg)
	a.sched.status.Scanning = cfg.Enabled()
	a.sched.status.Pending = 0
	a.sched.status.NextAt = time.Time{}
	a.sched.mu.Unlock()

	a.WakeScheduler()
	return nil
}
