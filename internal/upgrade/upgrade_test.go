package upgrade

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/seanhamlin/tendarr/internal/clients/arr"
	"github.com/seanhamlin/tendarr/internal/media"
)

const gb = int64(1 << 30)

func i64(v int64) *int64 { return &v }

type fakeRadarr struct {
	movies   []arr.Item
	profiles map[int]arr.QualityProfile
	queue    map[int]bool
	queueErr error
	files    map[int]arr.File
	asked    []int
}

func (f *fakeRadarr) Items(context.Context) ([]arr.Item, error) { return f.movies, nil }
func (f *fakeRadarr) QualityProfiles(context.Context) (map[int]arr.QualityProfile, error) {
	return f.profiles, nil
}
func (f *fakeRadarr) QueuedIDs(context.Context) (map[int]bool, error) { return f.queue, f.queueErr }
func (f *fakeRadarr) MovieFiles(_ context.Context, ids []int) (map[int]arr.File, error) {
	f.asked = ids
	return f.files, nil
}

func profile() map[int]arr.QualityProfile {
	return map[int]arr.QualityProfile{
		1: {ID: 1, Name: "HD", UpgradeAllowed: true, CutoffFormatScore: 6, CutoffQuality: "WEB 1080p", FormatScores: map[string]int{"x265": 3, "Lama": 2, "BR-DISK": -1000}},
		2: {ID: 2, Name: "Frozen", UpgradeAllowed: false, CutoffFormatScore: 6},
	}
}

func movie(id int, size int64, monitored bool, profileID int) arr.Item {
	return arr.Item{Kind: arr.Radarr, ID: id, Title: "Movie", Year: 2000, HasFile: true, SizeOnDisk: i64(size), Monitored: monitored, QualityProfileID: profileID}
}

func file(movieID int, size int64, codec media.VideoCodec, score int, cutoffNotMet bool) arr.File {
	return arr.File{ID: movieID * 10, ParentID: movieID, Size: size, Codec: codec, Score: score, QualityCutoffNotMet: cutoffNotMet, Quality: "WEBDL-1080p"}
}

func TestFindMoviesAssessesEachCandidate(t *testing.T) {
	src := &fakeRadarr{
		movies: []arr.Item{
			movie(1, 10*gb, true, 1),  // eligible: format headroom
			movie(2, 10*gb, false, 1), // unmonitored
			movie(3, 10*gb, true, 2),  // upgrades disabled
			movie(4, 10*gb, true, 1),  // at cutoff
			movie(5, 10*gb, true, 1),  // queued
			movie(6, 10*gb, true, 9),  // unknown profile
			movie(7, 10*gb, true, 1),  // HEVC: filtered out by codec
			movie(8, 1*gb, true, 1),   // too small at movie level: never fetched
			movie(9, 10*gb, true, 1),  // movie big, file small (extras): filtered
			movie(10, 10*gb, true, 1), // maxed score but below quality cutoff: eligible
		},
		profiles: profile(),
		queue:    map[int]bool{5: true},
		files: map[int]arr.File{
			1:  file(1, 9*gb, media.CodecAVC, 2, false),
			2:  file(2, 9*gb, media.CodecAVC, 2, false),
			3:  file(3, 9*gb, media.CodecAVC, 2, false),
			4:  file(4, 9*gb, media.CodecAVC, 6, false),
			5:  file(5, 9*gb, media.CodecAVC, 2, false),
			6:  file(6, 9*gb, media.CodecAVC, 2, false),
			7:  file(7, 9*gb, media.CodecHEVC, 2, false),
			9:  file(9, 1*gb, media.CodecAVC, 2, false),
			10: file(10, 8*gb, media.CodecXvid, 6, true),
		},
	}

	plan, err := FindMovies(context.Background(), src, Request{Codecs: media.PolicyLegacy, MinSize: 4 * gb, Sort: SortSize})
	if err != nil {
		t.Fatal(err)
	}

	for _, id := range src.asked {
		if id == 8 {
			t.Error("movies below the size floor should never have their files fetched")
		}
	}

	if len(plan.Eligible) != 2 || plan.Eligible[0].Key != 1 || plan.Eligible[1].Key != 10 {
		t.Fatalf("eligible: %+v", plan.Eligible)
	}
	reasons := map[int]Reason{}
	for _, c := range plan.Ineligible {
		reasons[c.Key] = c.Ineligible
	}
	want := map[int]Reason{2: NotMonitored, 3: UpgradesDisabled, 4: AtCutoff, 5: AlreadyQueued, 6: UnknownProfile}
	for id, r := range want {
		if reasons[id] != r {
			t.Errorf("movie %d: reason %q, want %q", id, reasons[id], r)
		}
	}
	if len(plan.Ineligible) != len(want) {
		t.Errorf("unexpected ineligible: %v", reasons)
	}
	if plan.Scanned != 7 || plan.EligibleBytes != 17*gb {
		t.Errorf("scanned %d bytes %d", plan.Scanned, plan.EligibleBytes)
	}

	c := plan.Eligible[0]
	if c.Headroom() != 4 || c.CutoffScore() != 6 || !c.SameQualityOnly() {
		t.Errorf("candidate maths: headroom %d cutoff %d sameQuality %v", c.Headroom(), c.CutoffScore(), c.SameQualityOnly())
	}
	if m := c.MissingFormats(); len(m) != 2 || m[0].Name != "x265" {
		t.Errorf("missing formats: %+v", m)
	}
}

func TestQueueFailureIsAWarningNotAnError(t *testing.T) {
	src := &fakeRadarr{profiles: profile(), queueErr: errors.New("queue down")}
	plan, err := FindMovies(context.Background(), src, Request{Codecs: media.PolicyLegacy, MinSize: gb})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "queue") {
		t.Errorf("warnings: %v", plan.Warnings)
	}
}

type fakeSonarr struct {
	series   []arr.Item
	profiles map[int]arr.QualityProfile
	queue    map[int]bool
	files    map[int][]arr.File
	episodes map[int][]arr.Episode
	fileErr  map[int]bool
}

func (f *fakeSonarr) Items(context.Context) ([]arr.Item, error) { return f.series, nil }
func (f *fakeSonarr) QualityProfiles(context.Context) (map[int]arr.QualityProfile, error) {
	return f.profiles, nil
}
func (f *fakeSonarr) QueuedIDs(context.Context) (map[int]bool, error) { return f.queue, nil }
func (f *fakeSonarr) EpisodeFiles(_ context.Context, id int) ([]arr.File, error) {
	if f.fileErr[id] {
		return nil, errors.New("boom")
	}
	return f.files[id], nil
}
func (f *fakeSonarr) Episodes(_ context.Context, id int) ([]arr.Episode, error) {
	return f.episodes[id], nil
}

func TestFindEpisodes(t *testing.T) {
	series := func(id int, size int64, monitored bool) arr.Item {
		return arr.Item{Kind: arr.Sonarr, ID: id, Title: "Show", SizeOnDisk: i64(size), EpisodeFileCount: 3, Monitored: monitored, QualityProfileID: 1}
	}
	src := &fakeSonarr{
		series:   []arr.Item{series(1, 50*gb, true), series(2, 50*gb, false), series(3, 500<<20, true), series(4, 50*gb, true)},
		profiles: profile(),
		queue:    map[int]bool{102: true},
		files: map[int][]arr.File{
			1: {
				{ID: 11, Size: 3 * gb, Codec: media.CodecAVC, Score: 0, Quality: "HDTV-1080p"},
				{ID: 12, Size: 3 * gb, Codec: media.CodecAVC, Score: 0},
				{ID: 13, Size: 3 * gb, Codec: media.CodecHEVC, Score: 0},
				{ID: 14, Size: 3 * gb, Codec: media.CodecAVC, Score: 0},
			},
			2: {{ID: 21, Size: 3 * gb, Codec: media.CodecAVC}},
		},
		episodes: map[int][]arr.Episode{
			1: {
				{ID: 101, EpisodeFileID: 11, Season: 1, Number: 1, Monitored: true, Title: "Pilot"},
				{ID: 103, EpisodeFileID: 11, Season: 1, Number: 2, Monitored: true},
				{ID: 102, EpisodeFileID: 12, Season: 1, Number: 3, Monitored: true},
				{ID: 104, EpisodeFileID: 14, Season: 2, Number: 1, Monitored: false},
			},
			2: {{ID: 201, EpisodeFileID: 21, Season: 1, Number: 1, Monitored: true}},
		},
		fileErr: map[int]bool{4: true},
	}

	plan, err := FindEpisodes(context.Background(), src, Request{Codecs: media.PolicyLegacy, MinSize: gb, Sort: SortTitle}, nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(plan.Eligible) != 1 {
		t.Fatalf("eligible: %+v", plan.Eligible)
	}
	c := plan.Eligible[0]
	if c.Key != 11 || c.Episode != "S01E01-E02" || len(c.SearchIDs) != 2 || c.ReleaseID != 101 || c.EpisodeTitle != "Pilot" {
		t.Errorf("multi-episode file: %+v", c)
	}

	reasons := map[int]Reason{}
	for _, c := range plan.Ineligible {
		reasons[c.Key] = c.Ineligible
	}
	if reasons[12] != AlreadyQueued || reasons[14] != NotMonitored || reasons[21] != NotMonitored {
		t.Errorf("reasons: %v", reasons)
	}
	if len(plan.Warnings) == 0 || !strings.Contains(strings.Join(plan.Warnings, " "), "1 series could not be read") {
		t.Errorf("a failing series should be a warning: %v", plan.Warnings)
	}
}

func TestEvaluateReleases(t *testing.T) {
	now := time.Now()
	rc := EvaluateReleases([]arr.Release{
		{Title: "rejected high", Score: 99, Rejected: true, Rejections: []string{"Not an upgrade"}},
		{Title: "rejected again", Rejected: true, Rejections: []string{"Not an upgrade", "Size"}},
		{Title: "ok low", Score: 3, Size: 5},
		{Title: "ok best big", Score: 6, Size: 9},
		{Title: "ok best small", Score: 6, Size: 7, Quality: "Bluray-1080p"},
	}, now)

	if !rc.Available || rc.BestTitle != "ok best small" || rc.BestScore != 6 || rc.BestQuality != "Bluray-1080p" {
		t.Errorf("best release: %+v", rc)
	}
	if rc.Total != 5 || rc.Rejected != 2 || rc.TopRejection != "Not an upgrade" {
		t.Errorf("counts: %+v", rc)
	}

	none := EvaluateReleases([]arr.Release{{Rejected: true}}, now)
	if none.Available {
		t.Error("only rejected releases means nothing is available")
	}
}

func TestResortAndMarkQueuedDoNotMutateTheOriginal(t *testing.T) {
	p := Plan{Eligible: []Candidate{
		{Key: 1, Title: "b", File: arr.File{Size: 1}},
		{Key: 2, Title: "a", File: arr.File{Size: 2}},
	}, EligibleBytes: 3}

	sorted := Resort(p, SortTitle)
	if sorted.Eligible[0].Key != 2 || p.Eligible[0].Key != 1 {
		t.Error("Resort must sort a copy")
	}

	q := MarkQueued(p, map[int]bool{1: true})
	if len(q.Eligible) != 1 || q.Eligible[0].Key != 2 || q.EligibleBytes != 2 {
		t.Errorf("eligible after queueing: %+v", q)
	}
	if len(q.Ineligible) != 1 || q.Ineligible[0].Ineligible != AlreadyQueued {
		t.Errorf("ineligible after queueing: %+v", q.Ineligible)
	}
	if len(p.Eligible) != 2 {
		t.Error("MarkQueued must not mutate the cached plan")
	}
}
