package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/seanhamlin/tendarr/internal/clients/arr"
	"github.com/seanhamlin/tendarr/internal/store"
	"github.com/seanhamlin/tendarr/internal/upgrade"
)

// MaxReleaseChecks caps one release-check job. Each check is a live search
// across every indexer, so an unbounded job is an indexer ban waiting to
// happen.
const MaxReleaseChecks = 50

// MaxSearch caps one search command, for the same reason.
const MaxSearch = 100

func upgradeKey(kind arr.Kind, req upgrade.Request) string {
	return fmt.Sprintf("%s|%s|%d", kind, req.Codecs, req.MinSize)
}

// UpgradePlan scans Radarr or Sonarr for upgrade candidates.
func (a *App) UpgradePlan(ctx context.Context, kind arr.Kind, req upgrade.Request, force bool) (upgrade.Plan, time.Time, error) {
	plan, at, err := a.upgrades.get(ctx, upgradeKey(kind, req), force, a.Now(), func(ctx context.Context) (upgrade.Plan, error) {
		client, err := a.ArrClient(ctx, kind)
		if err != nil {
			return upgrade.Plan{}, err
		}
		start := time.Now()
		var p upgrade.Plan
		if kind == arr.Radarr {
			p, err = upgrade.FindMovies(ctx, client, req)
		} else {
			p, err = upgrade.FindEpisodes(ctx, client, req, a.Logger)
		}
		if err != nil {
			return p, err
		}
		a.Logger.Info("upgrade scan", "service", kind, "eligible", len(p.Eligible), "ineligible", len(p.Ineligible), "took", time.Since(start).Round(time.Millisecond))
		return p, nil
	})
	if err != nil {
		return plan, at, err
	}
	// Sorting is cheap and not part of the cache key; re-sort a copy.
	plan = upgrade.Resort(plan, req.Sort)
	return plan, at, nil
}

// SelectUpgradeCandidates resolves keys against the eligible list. A key that
// is not eligible is an error rather than silently dropped.
func SelectUpgradeCandidates(plan upgrade.Plan, keys []int) ([]upgrade.Candidate, error) {
	byKey := make(map[int]upgrade.Candidate, len(plan.Eligible))
	for _, c := range plan.Eligible {
		byKey[c.Key] = c
	}
	seen := map[int]bool{}
	out := make([]upgrade.Candidate, 0, len(keys))
	for _, k := range keys {
		if seen[k] {
			continue
		}
		seen[k] = true
		c, ok := byKey[k]
		if !ok {
			return nil, fmt.Errorf("item %d is not eligible for an upgrade any more; refresh and try again", k)
		}
		out = append(out, c)
	}
	return out, nil
}

// Search asks Radarr or Sonarr to search for, and grab, upgrades.
func (a *App) Search(ctx context.Context, user string, kind arr.Kind, req upgrade.Request, keys []int) (int, int, error) {
	if len(keys) > MaxSearch {
		return 0, 0, fmt.Errorf("select at most %d items per search", MaxSearch)
	}
	plan, _, err := a.UpgradePlan(ctx, kind, req, false)
	if err != nil {
		return 0, 0, err
	}
	selected, err := SelectUpgradeCandidates(plan, keys)
	if err != nil {
		return 0, 0, err
	}
	if len(selected) == 0 {
		return 0, 0, fmt.Errorf("nothing selected")
	}

	client, err := a.ArrClient(ctx, kind)
	if err != nil {
		return 0, 0, err
	}

	var ids []int
	for _, c := range selected {
		ids = append(ids, c.SearchIDs...)
	}

	commandID, searchErr := client.Search(ctx, ids)
	outcome, msg := "QUEUED", ""
	if searchErr != nil {
		outcome, msg = "FAILED", searchErr.Error()
	}

	now := a.Now()
	for _, c := range selected {
		a.audit(ctx, store.AuditEntry{
			TS: now, Username: user, Action: "upgrade", Service: string(kind),
			ItemID: c.Key, Title: c.DisplayTitle(), Outcome: outcome, Message: msg,
			Details: map[string]any{
				"commandId":      commandID,
				"sizeBytes":      c.File.Size,
				"codec":          string(c.File.Codec),
				"quality":        c.File.Quality,
				"score":          c.File.Score,
				"cutoffScore":    c.CutoffScore(),
				"currentFormats": c.File.Formats,
				"file":           c.File.RelativePath,
			},
		})
	}
	a.Logger.Info("upgrade search", "user", user, "service", kind, "items", len(selected), "commandId", commandID, "outcome", outcome, "error", msg)

	if searchErr != nil {
		return 0, 0, searchErr
	}

	// Searched items are now queued as far as this tool is concerned; move
	// them out of the eligible list until the next scan.
	searched := map[int]bool{}
	for _, c := range selected {
		searched[c.Key] = true
	}
	a.upgrades.clearMatching(func(k string) bool { return strings.HasPrefix(k, string(kind)+"|") && k != upgradeKey(kind, req) })
	a.upgrades.update(upgradeKey(kind, req), func(p upgrade.Plan) upgrade.Plan {
		return upgrade.MarkQueued(p, searched)
	})

	return commandID, len(selected), nil
}

// ReleaseChecks returns cached release-check results for kind keyed by item.
// A result for a different file than the current one is dropped: the file
// changed, so the result no longer describes it.
func (a *App) ReleaseChecks(ctx context.Context, kind arr.Kind, plan upgrade.Plan) (map[int]store.ReleaseCheck, error) {
	all, err := a.Store.ReleaseChecks(ctx, string(kind))
	if err != nil {
		return nil, err
	}
	out := map[int]store.ReleaseCheck{}
	for _, list := range [][]upgrade.Candidate{plan.Eligible, plan.Ineligible} {
		for _, c := range list {
			if rc, ok := all[c.Key]; ok && rc.FileID == c.File.ID {
				out[c.Key] = rc
			}
		}
	}
	return out, nil
}

// Job is a background release check.
type Job struct {
	ID        string
	Kind      arr.Kind
	User      string
	Total     int
	Done      int
	Available int
	Failed    int
	Current   string
	Started   time.Time
	Finished  time.Time
}

// Running reports whether the job is still in progress.
func (j Job) Running() bool { return j.Finished.IsZero() }

// JobStatus returns a copy of the latest job for kind.
func (a *App) JobStatus(kind arr.Kind) (Job, bool) {
	a.jobsMu.Lock()
	defer a.jobsMu.Unlock()
	j, ok := a.jobs[kind]
	if !ok {
		return Job{}, false
	}
	return *j, true
}

// StartReleaseCheck runs interactive searches for the selected candidates in
// the background, one at a time to be gentle on indexers.
func (a *App) StartReleaseCheck(ctx context.Context, user string, kind arr.Kind, req upgrade.Request, keys []int) (Job, error) {
	if len(keys) > MaxReleaseChecks {
		return Job{}, fmt.Errorf("check at most %d items at a time; each one searches every indexer", MaxReleaseChecks)
	}
	plan, _, err := a.UpgradePlan(ctx, kind, req, false)
	if err != nil {
		return Job{}, err
	}
	selected, err := SelectUpgradeCandidates(plan, keys)
	if err != nil {
		return Job{}, err
	}
	if len(selected) == 0 {
		return Job{}, fmt.Errorf("nothing selected")
	}
	client, err := a.ArrClient(ctx, kind)
	if err != nil {
		return Job{}, err
	}

	a.jobsMu.Lock()
	if j, ok := a.jobs[kind]; ok && j.Running() {
		a.jobsMu.Unlock()
		return Job{}, fmt.Errorf("a release check is already running (%d of %d done)", j.Done, j.Total)
	}
	job := &Job{ID: newID(), Kind: kind, User: user, Total: len(selected), Started: a.Now()}
	a.jobs[kind] = job
	snapshot := *job
	a.jobsMu.Unlock()

	a.Logger.Info("release check started", "user", user, "service", kind, "items", len(selected), "job", job.ID)

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.runReleaseCheck(job, client, selected)
	}()

	return snapshot, nil
}

func (a *App) runReleaseCheck(job *Job, client ArrClient, selected []upgrade.Candidate) {
	defer func() {
		a.jobsMu.Lock()
		job.Finished = a.Now()
		job.Current = ""
		a.jobsMu.Unlock()
		a.Logger.Info("release check finished", "service", job.Kind, "job", job.ID, "done", job.Done, "available", job.Available, "failed", job.Failed)
	}()

	for _, c := range selected {
		if a.ctx.Err() != nil {
			return
		}
		a.jobsMu.Lock()
		job.Current = c.DisplayTitle()
		a.jobsMu.Unlock()

		rc := a.checkReleases(client, job.Kind, c)

		a.jobsMu.Lock()
		job.Done++
		if rc.Error != "" {
			job.Failed++
		} else if rc.Available {
			job.Available++
		}
		a.jobsMu.Unlock()
	}
}

// checkReleases runs one interactive search and stores the summary. Shared
// by manual checks and the scheduler so both record results identically.
func (a *App) checkReleases(client ArrClient, kind arr.Kind, c upgrade.Candidate) store.ReleaseCheck {
	ctx, cancel := context.WithTimeout(a.ctx, 4*time.Minute)
	releases, err := client.Releases(ctx, c.ReleaseID)
	cancel()

	rc := store.ReleaseCheck{Kind: string(kind), ItemKey: c.Key, FileID: c.File.ID}
	if err != nil {
		rc.CheckedAt = a.Now()
		rc.Error = err.Error()
		a.Logger.Warn("release check failed", "service", kind, "title", c.DisplayTitle(), "error", err)
	} else {
		ev := upgrade.EvaluateReleases(releases, a.Now())
		rc.CheckedAt = ev.CheckedAt
		rc.Available = ev.Available
		rc.BestTitle = ev.BestTitle
		rc.BestScore = ev.BestScore
		rc.BestQuality = ev.BestQuality
		rc.BestSize = ev.BestSize
		rc.Total = ev.Total
		rc.Rejected = ev.Rejected
		rc.TopRejection = ev.TopRejection
	}
	if err := a.Store.SaveReleaseCheck(a.ctx, rc); err != nil {
		a.Logger.Error("could not save release check", "error", err)
	}
	return rc
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// SetUpgradeMonitored unmonitors candidate items, or re-monitors items
// skipped for being unmonitored. It returns how many items changed.
func (a *App) SetUpgradeMonitored(ctx context.Context, user string, kind arr.Kind, req upgrade.Request, keys []int, monitored bool) (int, error) {
	if len(keys) > MaxSearch {
		return 0, fmt.Errorf("select at most %d items at a time", MaxSearch)
	}
	plan, _, err := a.UpgradePlan(ctx, kind, req, false)
	if err != nil {
		return 0, err
	}

	var selected []upgrade.Candidate
	if monitored {
		selected, err = selectUnmonitored(plan, keys)
	} else {
		selected, err = SelectUpgradeCandidates(plan, keys)
	}
	if err != nil {
		return 0, err
	}
	if len(selected) == 0 {
		return 0, fmt.Errorf("nothing selected")
	}

	client, err := a.ArrClient(ctx, kind)
	if err != nil {
		return 0, err
	}

	var ids []int
	for _, c := range selected {
		ids = append(ids, c.SearchIDs...)
	}
	if err := client.SetMonitored(ctx, ids, monitored); err != nil {
		return 0, err
	}

	action := "unmonitor"
	if monitored {
		action = "monitor"
	}
	now := a.Now()
	for _, c := range selected {
		a.audit(ctx, store.AuditEntry{
			TS: now, Username: user, Action: action, Service: string(kind),
			ItemID: c.Key, Title: c.DisplayTitle(), Outcome: "OK",
			Details: map[string]any{"sizeBytes": c.File.Size, "ids": c.SearchIDs},
		})
	}
	a.Logger.Info("upgrade "+action, "user", user, "service", kind, "items", len(selected))

	if monitored {
		// Monitoring again may make an item eligible, queued or at cutoff;
		// only a rescan can say which.
		a.upgrades.clearMatching(func(k string) bool { return strings.HasPrefix(k, string(kind)+"|") })
	} else {
		changed := map[int]bool{}
		for _, c := range selected {
			changed[c.Key] = true
		}
		a.upgrades.clearMatching(func(k string) bool { return strings.HasPrefix(k, string(kind)+"|") && k != upgradeKey(kind, req) })
		a.upgrades.update(upgradeKey(kind, req), func(p upgrade.Plan) upgrade.Plan {
			return upgrade.MarkIneligible(p, changed, upgrade.NotMonitored)
		})
	}

	return len(selected), nil
}

// selectUnmonitored resolves keys against items skipped for being
// unmonitored. An episode in an unmonitored series is refused: monitoring the
// episode alone would not make it searchable, and monitoring the whole series
// would change what Sonarr downloads, which is not this tool's call.
func selectUnmonitored(plan upgrade.Plan, keys []int) ([]upgrade.Candidate, error) {
	byKey := map[int]upgrade.Candidate{}
	for _, c := range plan.Ineligible {
		if c.Ineligible == upgrade.NotMonitored {
			byKey[c.Key] = c
		}
	}
	seen := map[int]bool{}
	var out []upgrade.Candidate
	for _, k := range keys {
		if seen[k] {
			continue
		}
		seen[k] = true
		c, ok := byKey[k]
		if !ok {
			return nil, fmt.Errorf("item %d is not skipped as unmonitored any more; refresh and try again", k)
		}
		if c.SeriesUnmonitored {
			return nil, fmt.Errorf("%s: the whole series is unmonitored in Sonarr; monitor the series there first", c.Title)
		}
		out = append(out, c)
	}
	return out, nil
}
