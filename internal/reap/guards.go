// Package reap decides which unwatched items may be deleted, and deletes them.
package reap

import (
	"fmt"
	"time"

	"github.com/seanhamlin/tendarr/internal/library"
	"github.com/seanhamlin/tendarr/internal/media"
)

// Criteria are the thresholds a reap is evaluated against.
type Criteria struct {
	UnwatchedFor   media.Period
	MinAge         media.Period
	MinRating      *float64
	MinRatingVotes int
	MinSize        int64
	AllowFuzzy     bool
	Sort           SortField
	// IgnoreTagID marks items the user has excluded; 0 means no tag exists.
	IgnoreTagID int
}

// DefaultCriteria mirrors the CLI defaults.
func DefaultCriteria() Criteria {
	return Criteria{
		UnwatchedFor:   media.MustPeriod("2y"),
		MinAge:         media.MustPeriod("90d"),
		MinRatingVotes: 50,
		Sort:           SortSize,
	}
}

// Guard names, stable for the UI and audit log.
const (
	GuardIgnored      = "IGNORED"
	GuardManagement   = "MANAGEMENT"
	GuardUnwatchedFor = "UNWATCHED_FOR"
	GuardMinAge       = "MIN_AGE"
	GuardMinRating    = "MIN_RATING"
	GuardMinSize      = "MIN_SIZE"
)

// Violation is one guard that held an item back, with the reason.
type Violation struct {
	Guard  string
	Reason string
}

type guard struct {
	name  string
	check func(c library.Candidate, cr Criteria, now time.Time) string
}

// guards is the single definition of what may be deleted. It runs twice on
// purpose: once while planning, and again immediately before each delete, so
// a bug in sorting or selection can never turn into data loss.
var guards = []guard{
	{GuardIgnored, checkIgnored},
	{GuardManagement, checkManagement},
	{GuardUnwatchedFor, checkUnwatchedFor},
	{GuardMinAge, checkMinAge},
	{GuardMinRating, checkMinRating},
	{GuardMinSize, checkMinSize},
}

// Evaluate runs every guard and returns the violations; empty means deletable.
func Evaluate(c library.Candidate, cr Criteria, now time.Time) []Violation {
	var out []Violation
	for _, g := range guards {
		if reason := g.check(c, cr, now); reason != "" {
			out = append(out, Violation{Guard: g.name, Reason: reason})
		}
	}
	return out
}

func checkIgnored(c library.Candidate, cr Criteria, _ time.Time) string {
	if cr.IgnoreTagID != 0 && c.Arr != nil && c.Arr.Item.HasTag(cr.IgnoreTagID) {
		return "tagged to be ignored"
	}
	return ""
}

// checkManagement allows only items *arr can actually delete. A title+year
// match is not trusted unless explicitly allowed: remakes and same-named
// shows make it a false-positive machine.
func checkManagement(c library.Candidate, cr Criteria, _ time.Time) string {
	if s := c.State(); s != library.Managed {
		return s.Reason()
	}
	if c.MatchedBy().IsFuzzy() && !cr.AllowFuzzy {
		return fmt.Sprintf("matched to %q on title and year only", c.Arr.Item.DisplayTitle())
	}
	return ""
}

// checkUnwatchedFor is the main window, measured from the last play or, for
// never-played items, from the date added.
func checkUnwatchedFor(c library.Candidate, cr Criteria, now time.Time) string {
	if c.LastActivity().After(cr.UnwatchedFor.Before(now)) {
		return fmt.Sprintf("last activity %dd ago, needs %s", c.DaysSinceActivity(now), cr.UnwatchedFor.Describe())
	}
	return ""
}

// checkMinAge is a grace period measured from the date added, never the last
// play, so something grabbed last week is not culled because nobody has got
// to it yet.
func checkMinAge(c library.Candidate, cr Criteria, now time.Time) string {
	if c.AddedAt.After(cr.MinAge.Before(now)) {
		return fmt.Sprintf("added %dd ago, grace period is %s", c.DaysSinceAdded(now), cr.MinAge.Describe())
	}
	return ""
}

// checkMinRating protects anything rated at or above the floor. A missing or
// thinly voted rating is protected, never assumed low: otherwise the guard
// would systematically delete the indie and foreign titles whose metadata
// failed to populate.
func checkMinRating(c library.Candidate, cr Criteria, _ time.Time) string {
	if cr.MinRating == nil {
		return ""
	}
	if c.Rating == nil {
		return "no rating available, protected rather than assumed low"
	}
	if !c.Rating.IsTrustworthy(cr.MinRatingVotes) {
		votes := "no vote count"
		if c.Rating.Votes != nil {
			votes = fmt.Sprintf("%d votes", *c.Rating.Votes)
		}
		return fmt.Sprintf("rating %.1f has %s, below the %d-vote threshold", c.Rating.Value, votes, cr.MinRatingVotes)
	}
	if c.Rating.Value >= *cr.MinRating {
		return fmt.Sprintf("rated %.1f, at or above the %.1f floor", c.Rating.Value, *cr.MinRating)
	}
	return ""
}

func checkMinSize(c library.Candidate, cr Criteria, _ time.Time) string {
	if cr.MinSize <= 0 {
		return ""
	}
	r := c.ReclaimableBytes()
	if r == nil {
		return "reclaimable size unknown"
	}
	if *r < cr.MinSize {
		return fmt.Sprintf("only %s reclaimable, below the %s floor", media.FormatBytes(*r), media.FormatBytes(cr.MinSize))
	}
	return ""
}
