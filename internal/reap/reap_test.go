package reap

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/seanhamlin/tendarr/internal/clients/arr"
	"github.com/seanhamlin/tendarr/internal/clients/plex"
	"github.com/seanhamlin/tendarr/internal/library"
	"github.com/seanhamlin/tendarr/internal/media"
)

var now = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

func i64(v int64) *int64     { return &v }
func f64(v float64) *float64 { return &v }
func ip(v int) *int          { return &v }

// stale returns a managed, verified movie last watched three years ago.
func stale(key string, size int64) library.Candidate {
	played := now.AddDate(-3, 0, 0)
	return library.Candidate{
		RatingKey:    key,
		Type:         plex.Movie,
		Title:        "Movie " + key,
		Year:         2010,
		AddedAt:      now.AddDate(-5, 0, 0),
		LastPlayedAt: &played,
		Source:       library.SourcePlex,
		Size:         i64(size),
		Rating:       &media.Rating{Value: 5.0, Source: media.RatingIMDb, Votes: ip(1000)},
		Arr: &library.ArrLink{
			Item:            arr.Item{Kind: arr.Radarr, ID: len(key) + 100, Title: "Movie " + key, HasFile: true, SizeOnDisk: i64(size)},
			MatchedBy:       library.MatchTMDb,
			StorageVerified: true,
		},
	}
}

func guardsOf(vs []Violation) string {
	var names []string
	for _, v := range vs {
		names = append(names, v.Guard)
	}
	return strings.Join(names, ",")
}

func TestGuards(t *testing.T) {
	base := DefaultCriteria()

	cases := []struct {
		name   string
		mutate func(*library.Candidate, *Criteria)
		want   string
	}{
		{"eligible", func(*library.Candidate, *Criteria) {}, ""},
		{"recently watched", func(c *library.Candidate, _ *Criteria) {
			p := now.AddDate(0, -2, 0)
			c.LastPlayedAt = &p
		}, GuardUnwatchedFor},
		{"never watched but old", func(c *library.Candidate, _ *Criteria) {
			c.LastPlayedAt = nil
		}, ""},
		{"never watched and new", func(c *library.Candidate, _ *Criteria) {
			c.LastPlayedAt = nil
			c.AddedAt = now.AddDate(0, 0, -30)
		}, GuardUnwatchedFor + "," + GuardMinAge},
		{"old watch, new add", func(c *library.Candidate, _ *Criteria) {
			c.AddedAt = now.AddDate(0, 0, -10)
		}, GuardMinAge},
		{"unmanaged", func(c *library.Candidate, _ *Criteria) { c.Arr = nil }, GuardManagement},
		{"orphaned", func(c *library.Candidate, _ *Criteria) { c.Arr.Item.HasFile = false }, GuardManagement},
		{"mismatched", func(c *library.Candidate, _ *Criteria) { c.Arr.StorageVerified = false }, GuardManagement},
		{"fuzzy not allowed", func(c *library.Candidate, _ *Criteria) { c.Arr.MatchedBy = library.MatchTitleYear }, GuardManagement},
		{"fuzzy allowed", func(c *library.Candidate, cr *Criteria) {
			c.Arr.MatchedBy = library.MatchTitleYear
			cr.AllowFuzzy = true
		}, ""},
		{"rated above floor", func(_ *library.Candidate, cr *Criteria) { cr.MinRating = f64(5.0) }, GuardMinRating},
		{"rated below floor", func(_ *library.Candidate, cr *Criteria) { cr.MinRating = f64(6.0) }, ""},
		{"unrated is protected", func(c *library.Candidate, cr *Criteria) {
			c.Rating = nil
			cr.MinRating = f64(6.0)
		}, GuardMinRating},
		{"thin votes are protected", func(c *library.Candidate, cr *Criteria) {
			c.Rating.Votes = ip(3)
			cr.MinRating = f64(6.0)
		}, GuardMinRating},
		{"unrated without a floor", func(c *library.Candidate, _ *Criteria) { c.Rating = nil }, ""},
		{"too small", func(_ *library.Candidate, cr *Criteria) { cr.MinSize = 2 << 30 }, GuardMinSize},
		{"ignored", func(c *library.Candidate, cr *Criteria) {
			cr.IgnoreTagID = 9
			c.Arr.Item.Tags = []int{9}
		}, GuardIgnored},
		{"other tag", func(c *library.Candidate, cr *Criteria) {
			cr.IgnoreTagID = 9
			c.Arr.Item.Tags = []int{3}
		}, ""},
	}

	for _, tc := range cases {
		c := stale("x", 1<<30)
		cr := base
		tc.mutate(&c, &cr)
		if got := guardsOf(Evaluate(c, cr, now)); got != tc.want {
			t.Errorf("%s: violations %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestPlanPartitionsAndSorts(t *testing.T) {
	ignored := stale("ign", 50)
	ignored.Arr.Item.Tags = []int{9}

	fresh := stale("fresh", 999)
	p := now.AddDate(0, 0, -5)
	fresh.LastPlayedAt = &p

	held := stale("held", 70)
	held.Arr = nil

	snap := &library.Snapshot{Candidates: []library.Candidate{stale("small", 10), stale("big", 100), ignored, fresh, held}}
	cr := DefaultCriteria()
	cr.IgnoreTagID = 9

	plan := Build(snap, cr, now)
	if len(plan.Eligible) != 2 || plan.Eligible[0].RatingKey != "big" || plan.Eligible[1].RatingKey != "small" {
		t.Errorf("eligible should be sorted largest first: %+v", plan.Eligible)
	}
	if plan.EligibleBytes != 110 {
		t.Errorf("eligible bytes %d", plan.EligibleBytes)
	}
	if len(plan.Ignored) != 1 || plan.Ignored[0].RatingKey != "ign" {
		t.Errorf("ignored: %+v", plan.Ignored)
	}
	if len(plan.HeldBack) != 1 || plan.HeldBack[0].RatingKey != "held" {
		t.Errorf("only stale-but-protected items are held back, not recently watched ones: %+v", plan.HeldBack)
	}
	if plan.Total != 5 {
		t.Errorf("total %d", plan.Total)
	}
}

func TestSortFields(t *testing.T) {
	a, b := stale("a", 10), stale("b", 20)
	a.Rating.Value, b.Rating.Value = 3, 7
	older := now.AddDate(-6, 0, 0)
	b.LastPlayedAt = &older
	a.Title, b.Title = "zulu", "alpha"

	if !SortRating.less(a, b) {
		t.Error("lowest rated first")
	}
	if !SortStaleness.less(b, a) {
		t.Error("longest unwatched first")
	}
	if !SortTitle.less(b, a) {
		t.Error("alphabetical")
	}
	if !SortSize.less(b, a) {
		t.Error("largest first")
	}
	if _, err := ParseSortField("random"); err == nil {
		t.Error("unknown sort accepted")
	}
}

type fakeDeleter struct {
	calls []int
	excl  []bool
	fail  map[int]bool
	gone  map[int]bool
}

func (f *fakeDeleter) Delete(_ context.Context, id int, addExclusion bool) (arr.DeleteResult, error) {
	f.calls = append(f.calls, id)
	f.excl = append(f.excl, addExclusion)
	if f.fail[id] {
		return 0, errors.New("timeout")
	}
	if f.gone[id] {
		return arr.AlreadyGone, nil
	}
	return arr.Deleted, nil
}

func TestExecute(t *testing.T) {
	ok := stale("ok", 100)
	failing := stale("failing", 200)
	failing.Arr.Item.ID = 555
	gone := stale("gone", 300)
	gone.Arr.Item.ID = 666
	recent := stale("recent", 400)
	p := now.AddDate(0, 0, -1)
	recent.LastPlayedAt = &p

	d := &fakeDeleter{fail: map[int]bool{555: true}, gone: map[int]bool{666: true}}
	out := Execute(context.Background(), d, []library.Candidate{ok, failing, gone, recent}, DefaultCriteria(), Options{AddExclusions: true}, now)

	want := []Status{StatusDeleted, StatusFailed, StatusAlreadyGone, StatusRefused}
	for i, o := range out {
		if o.Status != want[i] {
			t.Errorf("%s: %s, want %s (%s)", o.Candidate.RatingKey, o.Status, want[i], o.Message)
		}
	}
	if len(d.calls) != 3 {
		t.Errorf("the re-check must stop the recently watched item before any HTTP call: %v", d.calls)
	}
	if out[0].ReclaimedBytes() != 100 || out[1].ReclaimedBytes() != 0 || out[2].ReclaimedBytes() != 0 {
		t.Error("only a real delete reclaims space")
	}
}

func TestExclusionCarveOutForContinuingSeries(t *testing.T) {
	movie := stale("m", 1)
	series := stale("s", 1)
	series.Arr.Item.Kind = arr.Sonarr
	series.Arr.Item.Status = "continuing"
	ended := stale("e", 1)
	ended.Arr.Item.Kind = arr.Sonarr
	ended.Arr.Item.Status = "ended"

	cases := []struct {
		opts Options
		c    library.Candidate
		want bool
	}{
		{Options{AddExclusions: true}, movie, true},
		{Options{AddExclusions: true}, ended, true},
		{Options{AddExclusions: true}, series, false},
		{Options{AddExclusions: true, ExcludeContinuing: true}, series, true},
		{Options{AddExclusions: false, ExcludeContinuing: true}, series, false},
		{Options{}, movie, false},
	}
	for i, c := range cases {
		if got := ShouldExclude(c.c, c.opts); got != c.want {
			t.Errorf("case %d: got %v, want %v", i, got, c.want)
		}
	}
}
