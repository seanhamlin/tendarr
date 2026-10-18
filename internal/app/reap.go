package app

import (
	"context"
	"fmt"
	"time"

	"github.com/seanhamlin/tendarr/internal/clients/arr"
	"github.com/seanhamlin/tendarr/internal/clients/plex"
	"github.com/seanhamlin/tendarr/internal/library"
	"github.com/seanhamlin/tendarr/internal/media"
	"github.com/seanhamlin/tendarr/internal/reap"
	"github.com/seanhamlin/tendarr/internal/settings"
	"github.com/seanhamlin/tendarr/internal/store"
)

// MaxSnapshotAgeForDelete bounds how stale the data behind a delete may be.
// The confirmation page always rescans, so this only bites when a
// confirmation page is left open and submitted much later, by which time
// somebody may have started watching.
const MaxSnapshotAgeForDelete = 10 * time.Minute

func arrKindFor(t plex.MediaType) arr.Kind {
	if t == plex.Movie {
		return arr.Radarr
	}
	return arr.Sonarr
}

// Snapshot returns the joined library view for one media type.
func (a *App) Snapshot(ctx context.Context, t plex.MediaType, force bool) (*library.Snapshot, error) {
	snap, _, err := a.snapshots.get(ctx, string(t), force, a.Now(), func(ctx context.Context) (*library.Snapshot, error) {
		s, err := a.Settings(ctx)
		if err != nil {
			return nil, err
		}

		pc := s.Conn(settings.Plex)
		if !pc.Configured() {
			return nil, &NotConfiguredError{Service: settings.Plex}
		}
		plexClient, err := a.NewPlex(pc.URL, pc.Key)
		if err != nil {
			return nil, err
		}

		arrClient, err := a.arrClient(s, arrKindFor(t))
		if err != nil {
			return nil, err
		}

		b := &library.Builder{
			Plex:     plexClient,
			Arr:      arrClient,
			Verifier: library.NewVerifier(s.PathMappings()),
			Logger:   a.Logger,
			Now:      a.Now,
		}
		if tc := s.Conn(settings.Tautulli); tc.Configured() {
			w, err := a.NewTautulli(tc.URL, tc.Key)
			if err != nil {
				return nil, err
			}
			b.Watch = w
		}

		start := time.Now()
		snap, err := b.Build(ctx, t, s.ExcludedSections, s.IgnoreTag)
		if err != nil {
			return nil, err
		}
		a.Logger.Info("library scanned", "type", t, "items", len(snap.Candidates), "warnings", len(snap.Warnings), "took", time.Since(start).Round(time.Millisecond))
		return snap, nil
	})
	return snap, err
}

// ReapPlan scans (or reuses a scan) and evaluates it against criteria.
func (a *App) ReapPlan(ctx context.Context, t plex.MediaType, cr reap.Criteria, force bool) (reap.Plan, *library.Snapshot, error) {
	snap, err := a.Snapshot(ctx, t, force)
	if err != nil {
		return reap.Plan{}, nil, err
	}
	cr.IgnoreTagID = snap.IgnoreTagID
	return reap.Build(snap, cr, a.Now()), snap, nil
}

// SelectReapCandidates resolves rating keys against the snapshot. Unknown
// keys are an error, never silently dropped: a form that names something the
// server cannot see is either stale or tampered with.
func SelectReapCandidates(snap *library.Snapshot, ratingKeys []string) ([]library.Candidate, error) {
	byKey := make(map[string]library.Candidate, len(snap.Candidates))
	for _, c := range snap.Candidates {
		byKey[c.RatingKey] = c
	}
	seen := map[string]bool{}
	out := make([]library.Candidate, 0, len(ratingKeys))
	for _, k := range ratingKeys {
		if seen[k] {
			continue
		}
		seen[k] = true
		c, ok := byKey[k]
		if !ok {
			return nil, fmt.Errorf("item %q is no longer in the library; refresh and try again", k)
		}
		out = append(out, c)
	}
	return out, nil
}

// DeleteResult summarises a delete run.
type DeleteResult struct {
	Outcomes  []reap.Outcome
	Reclaimed int64
	Failed    int
}

// Delete removes the selected items through Radarr or Sonarr.
func (a *App) Delete(ctx context.Context, user string, t plex.MediaType, ratingKeys []string, cr reap.Criteria, opts reap.Options) (DeleteResult, error) {
	snap, err := a.Snapshot(ctx, t, false)
	if err != nil {
		return DeleteResult{}, err
	}
	if snap.Incomplete {
		return DeleteResult{}, fmt.Errorf("the last scan was incomplete, so deletes are disabled; fix the warnings and refresh")
	}
	if age := a.Now().Sub(snap.TakenAt); age > MaxSnapshotAgeForDelete {
		return DeleteResult{}, fmt.Errorf("the library data is %s old; review the selection again so it is checked against fresh data", age.Round(time.Minute))
	}

	selected, err := SelectReapCandidates(snap, ratingKeys)
	if err != nil {
		return DeleteResult{}, err
	}

	client, err := a.ArrClient(ctx, arrKindFor(t))
	if err != nil {
		return DeleteResult{}, err
	}

	cr.IgnoreTagID = snap.IgnoreTagID
	now := a.Now()
	outcomes := reap.Execute(ctx, client, selected, cr, opts, now)

	var res DeleteResult
	res.Outcomes = outcomes
	removed := map[string]bool{}
	for _, o := range outcomes {
		c := o.Candidate
		res.Reclaimed += o.ReclaimedBytes()
		if o.Status == reap.StatusDeleted || o.Status == reap.StatusAlreadyGone {
			removed[c.RatingKey] = true
		} else {
			res.Failed++
		}

		details := map[string]any{
			"ratingKey":      c.RatingKey,
			"section":        c.SectionName,
			"matchedBy":      string(c.MatchedBy()),
			"stalenessFrom":  string(c.Source),
			"daysInactive":   c.DaysSinceActivity(now),
			"playCount":      c.PlayCount,
			"exclusionAdded": o.ExclusionAdded,
			"unwatchedFor":   cr.UnwatchedFor.String(),
			"minAge":         cr.MinAge.String(),
		}
		if r := c.ReclaimableBytes(); r != nil {
			details["sizeBytes"] = *r
		}
		if c.LastPlayedAt != nil {
			details["lastPlayedAt"] = c.LastPlayedAt.Format(time.RFC3339)
		}
		if c.Rating != nil {
			details["rating"] = c.Rating.Describe()
		}
		if cr.MinRating != nil {
			details["minRating"] = *cr.MinRating
		}

		a.audit(ctx, store.AuditEntry{
			TS:       now,
			Username: user,
			Action:   "delete",
			Service:  string(arrKindFor(t)),
			ItemID:   c.ArrID(),
			Title:    c.DisplayTitle(),
			Outcome:  string(o.Status),
			Message:  o.Message,
			Details:  details,
		})
		a.Logger.Info("reap delete", "user", user, "title", c.DisplayTitle(), "service", arrKindFor(t), "arrId", c.ArrID(), "outcome", o.Status, "exclusion", o.ExclusionAdded, "message", o.Message)
	}

	// Drop what is gone so the page stays honest without another full scan.
	a.snapshots.update(string(t), func(s *library.Snapshot) *library.Snapshot {
		cp := *s
		cp.Candidates = make([]library.Candidate, 0, len(s.Candidates))
		for _, c := range s.Candidates {
			if !removed[c.RatingKey] {
				cp.Candidates = append(cp.Candidates, c)
			}
		}
		return &cp
	})
	if len(removed) > 0 {
		// Upgrade lists for this service may include what was just deleted.
		kind := string(arrKindFor(t))
		a.upgrades.clearMatching(func(k string) bool { return len(k) >= len(kind) && k[:len(kind)] == kind })
	}

	a.Logger.Info("reap run finished", "user", user, "type", t, "selected", len(selected), "failed", res.Failed, "reclaimed", media.FormatBytes(res.Reclaimed))
	return res, nil
}

// SetIgnored adds or removes the ignore tag on the selected items.
func (a *App) SetIgnored(ctx context.Context, user string, t plex.MediaType, ratingKeys []string, ignore bool) (int, error) {
	snap, err := a.Snapshot(ctx, t, false)
	if err != nil {
		return 0, err
	}
	selected, err := SelectReapCandidates(snap, ratingKeys)
	if err != nil {
		return 0, err
	}

	var ids []int
	seen := map[int]bool{}
	for _, c := range selected {
		if c.Arr == nil {
			return 0, fmt.Errorf("%s is not managed by %s, so it cannot be tagged", c.DisplayTitle(), arrKindFor(t).Label())
		}
		if !seen[c.Arr.Item.ID] {
			seen[c.Arr.Item.ID] = true
			ids = append(ids, c.Arr.Item.ID)
		}
	}
	if len(ids) == 0 {
		return 0, fmt.Errorf("nothing selected")
	}

	s, err := a.Settings(ctx)
	if err != nil {
		return 0, err
	}
	client, err := a.arrClient(s, arrKindFor(t))
	if err != nil {
		return 0, err
	}

	var tagID int
	if ignore {
		tagID, err = client.EnsureTag(ctx, s.IgnoreTag)
	} else {
		tagID, err = client.FindTag(ctx, s.IgnoreTag)
		if err == nil && tagID == 0 {
			return 0, fmt.Errorf("the %q tag does not exist in %s", s.IgnoreTag, arrKindFor(t).Label())
		}
	}
	if err != nil {
		return 0, err
	}

	if err := client.SetTag(ctx, ids, tagID, ignore); err != nil {
		return 0, err
	}

	action := "ignore"
	if !ignore {
		action = "unignore"
	}
	now := a.Now()
	for _, c := range selected {
		a.audit(ctx, store.AuditEntry{
			TS: now, Username: user, Action: action, Service: string(arrKindFor(t)),
			ItemID: c.Arr.Item.ID, Title: c.DisplayTitle(), Outcome: "OK",
			Details: map[string]any{"tag": s.IgnoreTag},
		})
	}
	a.Logger.Info("reap "+action, "user", user, "service", arrKindFor(t), "items", len(ids), "tag", s.IgnoreTag)

	// Reflect the new tag state in the cached scan.
	a.snapshots.update(string(t), func(sn *library.Snapshot) *library.Snapshot {
		cp := *sn
		cp.IgnoreTagID = tagID
		cp.Candidates = make([]library.Candidate, len(sn.Candidates))
		for i, c := range sn.Candidates {
			if c.Arr != nil && seen[c.Arr.Item.ID] {
				link := *c.Arr
				link.Item.Tags = withTag(link.Item.Tags, tagID, ignore)
				c.Arr = &link
			}
			cp.Candidates[i] = c
		}
		return &cp
	})

	return len(ids), nil
}

func withTag(tags []int, id int, present bool) []int {
	out := make([]int, 0, len(tags)+1)
	for _, t := range tags {
		if t != id {
			out = append(out, t)
		}
	}
	if present {
		out = append(out, id)
	}
	return out
}
