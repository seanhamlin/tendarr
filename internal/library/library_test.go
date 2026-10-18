package library

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/seanhamlin/tendarr/internal/clients/arr"
	"github.com/seanhamlin/tendarr/internal/clients/plex"
	"github.com/seanhamlin/tendarr/internal/clients/tautulli"
	"github.com/seanhamlin/tendarr/internal/media"
)

func i64(v int64) *int64 { return &v }

func ts(y int, m time.Month, d int) *time.Time {
	t := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	return &t
}

func TestIndexPrefersStrongIdentifiers(t *testing.T) {
	idx := NewIndex([]arr.Item{
		{ID: 1, Title: "Dune", Year: 2021, IDs: media.ExternalIDs{TMDb: 438631, IMDb: "tt1160419"}},
		{ID: 2, Title: "Other", Year: 1999, IDs: media.ExternalIDs{IMDb: "tt0000002"}},
	})

	got, how := idx.Find(media.ExternalIDs{TMDb: 438631, IMDb: "tt0000002"}, "x", 0)
	if got == nil || got.ID != 1 || how != MatchTMDb {
		t.Errorf("TMDb should win: %v %v", got, how)
	}
	got, how = idx.Find(media.ExternalIDs{IMDb: "TT0000002"}, "x", 0)
	if got == nil || got.ID != 2 || how != MatchIMDb {
		t.Errorf("IMDb match should be case-insensitive: %v %v", got, how)
	}
	got, how = idx.Find(media.ExternalIDs{}, "The Dune!", 2021)
	if got == nil || got.ID != 1 || how != MatchTitleYear || !how.IsFuzzy() {
		t.Errorf("title+year fallback: %v %v", got, how)
	}
	if got, how := idx.Find(media.ExternalIDs{}, "Dune", 0); got != nil || how != MatchNone {
		t.Error("no year must mean no fuzzy match")
	}
}

func TestIndexPoisonsAmbiguousTitleYear(t *testing.T) {
	idx := NewIndex([]arr.Item{
		{ID: 1, Title: "Ghosts", Year: 2021},
		{ID: 2, Title: "Ghosts", Year: 2021},
	})
	if got, _ := idx.Find(media.ExternalIDs{}, "Ghosts", 2021); got != nil {
		t.Errorf("an ambiguous key must match nothing, matched %d", got.ID)
	}
}

func TestVerifier(t *testing.T) {
	movies := plex.Section{Key: "1", Title: "Movies", Type: plex.Movie, Locations: []string{"/mnt/media/movies"}}
	movies4k := plex.Section{Key: "2", Title: "UHD Movies", Type: plex.Movie, Locations: []string{"/mnt/media/movies-uhd"}}
	radarr := arr.Item{RootFolderPath: "/movies", Path: "/movies/Heat (1995)", FileRelativePath: "Heat.1995.1080p.mkv"}
	plexItem := plex.Item{FilePath: "/mnt/media/movies/Heat (1995)/Heat.1995.1080p.mkv"}
	plex4k := plex.Item{FilePath: "/mnt/media/movies-uhd/Heat (1995)/Heat.1995.2160p.mkv"}

	mapped := NewVerifier([]PathMapping{{Arr: "/movies", Plex: "/mnt/media/movies"}})
	if !mapped.Verify(movies, plexItem, radarr) {
		t.Error("same file under a mapped path should verify")
	}
	if mapped.Verify(movies4k, plex4k, radarr) {
		t.Error("a 4K copy must never verify against the 1080p record")
	}

	// Without a map, the last segment is compared: "movies" vs "Movies"
	// matches case-insensitively, "movies-uhd" does not.
	plain := NewVerifier(nil)
	if !plain.Verify(movies, plexItem, radarr) {
		t.Error("last-segment fallback should match /movies to /mnt/media/movies")
	}
	if plain.Verify(movies4k, plexItem, radarr) {
		t.Error("last-segment fallback must not match movies-uhd")
	}

	// A differing file name fails even when the folder matches.
	other := plex.Item{FilePath: "/mnt/media/movies/Heat (1995)/Heat.REMUX.mkv"}
	if mapped.Verify(movies, other, radarr) {
		t.Error("a different file name must not verify")
	}

	// Series have no single file: the section check decides.
	tv := plex.Section{Locations: []string{"/mnt/media/tv"}}
	series := arr.Item{Kind: arr.Sonarr, Path: "/tv/The Wire"}
	if !plain.Verify(tv, plex.Item{}, series) {
		t.Error("series should verify via the parent folder when no root folder is reported")
	}

	// Windows-style *arr paths.
	win := arr.Item{RootFolderPath: `D:\media\movies`, FileRelativePath: `Heat.1995.1080p.mkv`}
	if !plain.Verify(movies, plexItem, win) {
		t.Error("backslash paths should be normalised")
	}
}

func TestParsePathMap(t *testing.T) {
	m, err := ParsePathMap("/movies:/mnt/media/movies\n/tv:/mnt/media/tv, /movies/4k:/mnt/media/movies-uhd")
	if err != nil || len(m) != 3 {
		t.Fatalf("%v %v", m, err)
	}
	v := NewVerifier(m)
	if got, _ := v.translate("/movies/4k/Film"); got != "/mnt/media/movies-uhd/Film" {
		t.Errorf("longest prefix should win: %q", got)
	}
	if got, ok := v.translate("/moviesextra/Film"); ok {
		t.Errorf("prefixes match whole segments only, got %q", got)
	}

	for _, bad := range []string{"/movies", "movies:/x", "/movies:", ":/x"} {
		if _, err := ParsePathMap(bad); err == nil {
			t.Errorf("ParsePathMap(%q) succeeded", bad)
		}
	}
}

func TestAssemble(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	section := plex.Section{Key: "1", Title: "Movies", Type: plex.Movie, Locations: []string{"/mnt/media/movies"}}
	idx := NewIndex([]arr.Item{
		{Kind: arr.Radarr, ID: 10, Title: "A", Year: 2000, IDs: media.ExternalIDs{TMDb: 1}, HasFile: true, SizeOnDisk: i64(900), RootFolderPath: "/movies", Added: ts(2018, 1, 1)},
		{Kind: arr.Radarr, ID: 11, Title: "B", Year: 2001, IDs: media.ExternalIDs{TMDb: 2}, HasFile: false, RootFolderPath: "/movies"},
		{Kind: arr.Radarr, ID: 12, Title: "C", Year: 2002, IDs: media.ExternalIDs{TMDb: 3}, HasFile: true, SizeOnDisk: i64(100), RootFolderPath: "/elsewhere"},
	})
	items := []plex.Item{
		{RatingKey: "a", Title: "A", Year: 2000, IDs: media.ExternalIDs{TMDb: 1}, AddedAt: ts(2019, 1, 1), LastViewedAt: ts(2022, 1, 1), FileSize: i64(1000), ViewCount: 1},
		{RatingKey: "b", Title: "B", Year: 2001, IDs: media.ExternalIDs{TMDb: 2}, AddedAt: ts(2019, 1, 1)},
		{RatingKey: "c", Title: "C", Year: 2002, IDs: media.ExternalIDs{TMDb: 3}, AddedAt: ts(2019, 1, 1)},
		{RatingKey: "d", Title: "D", Year: 2003, AddedAt: ts(2019, 1, 1)},
	}
	watch := map[string]tautulli.WatchRecord{
		"a": {RatingKey: "a", LastPlayedAt: ts(2024, 6, 1), PlayCount: 5},
		"b": {RatingKey: "b", LastPlayedAt: ts(2020, 1, 1)},
	}

	got := Assemble(section, items, watch, idx, NewVerifier(nil), now)
	byKey := map[string]Candidate{}
	for _, c := range got {
		byKey[c.RatingKey] = c
	}

	a := byKey["a"]
	if a.Source != SourceTautulli || !a.LastPlayedAt.Equal(*ts(2024, 6, 1)) {
		t.Errorf("later Tautulli play should win: %v %v", a.Source, a.LastPlayedAt)
	}
	if !a.AddedAt.Equal(*ts(2018, 1, 1)) {
		t.Errorf("earliest added date should win: %v", a.AddedAt)
	}
	if a.PlayCount != 5 || *a.Size != 1000 || a.State() != Managed || *a.ReclaimableBytes() != 900 {
		t.Errorf("a: %+v", a)
	}
	if byKey["b"].State() != Orphaned || byKey["b"].ReclaimableBytes() != nil {
		t.Errorf("b should be orphaned: %v", byKey["b"].State())
	}
	if byKey["c"].State() != Mismatched {
		t.Errorf("c is stored somewhere else and must be mismatched: %v", byKey["c"].State())
	}
	if byKey["d"].State() != Unmanaged || !byKey["d"].NeverPlayed() || byKey["d"].Source != SourceNever {
		t.Errorf("d: %+v", byKey["d"])
	}
}

func TestShowSizeComesOnlyFromSonarr(t *testing.T) {
	section := plex.Section{Type: plex.Show, Locations: []string{"/mnt/media/tv"}}
	idx := NewIndex([]arr.Item{{Kind: arr.Sonarr, ID: 1, IDs: media.ExternalIDs{TVDb: 5}, HasFile: true, SizeOnDisk: i64(5000), Path: "/tv/X"}})
	got := Assemble(section, []plex.Item{{RatingKey: "x", IDs: media.ExternalIDs{TVDb: 5}, FileSize: i64(1)}}, nil, idx, NewVerifier(nil), time.Now())
	if got[0].Size == nil || *got[0].Size != 5000 {
		t.Errorf("show size should come from Sonarr: %v", got[0].Size)
	}
}

func TestContinuingSeries(t *testing.T) {
	l := ArrLink{Item: arr.Item{Kind: arr.Sonarr, Status: "Continuing"}}
	if !l.IsContinuingSeries() {
		t.Error("status comparison should be case-insensitive")
	}
	if (ArrLink{Item: arr.Item{Kind: arr.Radarr, Status: "continuing"}}).IsContinuingSeries() {
		t.Error("movies are never continuing series")
	}
}

type fakePlex struct {
	sections []plex.Section
	items    map[string][]plex.Item
	failKey  string
}

func (f fakePlex) Sections(context.Context) ([]plex.Section, error) { return f.sections, nil }
func (f fakePlex) SectionItems(_ context.Context, s plex.Section) ([]plex.Item, error) {
	if s.Key == f.failKey {
		return nil, errors.New("boom")
	}
	return f.items[s.Key], nil
}

type fakeWatch struct{ err error }

func (f fakeWatch) WatchRecords(context.Context, string) (map[string]tautulli.WatchRecord, error) {
	return nil, f.err
}

type fakeArr struct {
	items  []arr.Item
	err    error
	tag    int
	tagErr error
}

func (f fakeArr) Items(context.Context) ([]arr.Item, error)    { return f.items, f.err }
func (f fakeArr) FindTag(context.Context, string) (int, error) { return f.tag, f.tagErr }

func TestBuilder(t *testing.T) {
	p := fakePlex{
		sections: []plex.Section{
			{Key: "1", Title: "Movies", Type: plex.Movie},
			{Key: "2", Title: "UHD Movies", Type: plex.Movie},
			{Key: "3", Title: "TV", Type: plex.Show},
			{Key: "4", Title: "Broken", Type: plex.Movie},
			{Key: "5", Title: "Empty", Type: plex.Movie},
		},
		items: map[string][]plex.Item{
			"1": {{RatingKey: "a", Title: "A"}},
			"2": {{RatingKey: "b", Title: "B"}},
			"3": {{RatingKey: "c", Title: "C"}},
		},
		failKey: "4",
	}
	b := &Builder{Plex: p, Watch: fakeWatch{err: errors.New("down")}, Arr: fakeArr{tag: 7}, Verifier: NewVerifier(nil)}

	snap, err := b.Build(context.Background(), plex.Movie, []string{"uhd movies"}, "ignore")
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Candidates) != 1 || snap.Candidates[0].RatingKey != "a" {
		t.Errorf("only the Movies section should be read: %+v", snap.Candidates)
	}
	if len(snap.SkippedSections) != 1 || snap.SkippedSections[0] != "UHD Movies" {
		t.Errorf("excluded section should be reported: %v", snap.SkippedSections)
	}
	if snap.Incomplete || snap.IgnoreTagID != 7 {
		t.Errorf("snapshot should be complete with tag 7: %+v", snap)
	}
	joined := strings.Join(snap.Warnings, "\n")
	for _, want := range []string{"Broken", "Empty", "Tautulli history unavailable"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing warning about %q in:\n%s", want, joined)
		}
	}

	b.Arr = fakeArr{err: errors.New("radarr down")}
	snap, err = b.Build(context.Background(), plex.Movie, nil, "ignore")
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Incomplete {
		t.Error("an unreadable *arr library must mark the snapshot incomplete")
	}

	b.Arr = fakeArr{tagErr: errors.New("tags down")}
	snap, _ = b.Build(context.Background(), plex.Movie, nil, "ignore")
	if !snap.Incomplete {
		t.Error("unreadable tags mean ignored items cannot be identified; deletes must be disabled")
	}

	b.Watch = nil
	b.Arr = fakeArr{}
	snap, _ = b.Build(context.Background(), plex.Movie, nil, "")
	if !strings.Contains(strings.Join(snap.Warnings, " "), "Tautulli is not configured") {
		t.Error("missing Tautulli should be called out")
	}
}
