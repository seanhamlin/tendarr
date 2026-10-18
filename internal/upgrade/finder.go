// Package upgrade finds large files below their quality profile's optimal
// custom format score and asks Radarr or Sonarr to replace them.
//
// The work worth doing here is deciding what not to search. *arr will
// happily accept a search for something it can never upgrade (unmonitored, a
// profile with upgrades disabled, a file already at the cutoff) and every one
// of those spends indexer quota to achieve nothing.
package upgrade

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"github.com/seanhamlin/tendarr/internal/clients/arr"
	"github.com/seanhamlin/tendarr/internal/media"
)

// Reason explains why a candidate will not be searched.
type Reason string

const (
	NotMonitored     Reason = "NOT_MONITORED"
	UpgradesDisabled Reason = "UPGRADES_DISABLED"
	AtCutoff         Reason = "AT_CUTOFF"
	AlreadyQueued    Reason = "QUEUED"
	UnknownProfile   Reason = "UNKNOWN_PROFILE"
)

// Describe is the human explanation.
func (r Reason) Describe() string {
	switch r {
	case NotMonitored:
		return "not monitored, so any result would be discarded"
	case UpgradesDisabled:
		return "quality profile has upgrades turned off"
	case AtCutoff:
		return "already at the profile cutoff, nothing to gain"
	case AlreadyQueued:
		return "already downloading"
	case UnknownProfile:
		return "quality profile could not be read"
	default:
		return string(r)
	}
}

// Request filters the scan.
type Request struct {
	Codecs  media.CodecPolicy
	MinSize int64
	Sort    SortField
}

// SortField orders candidates.
type SortField string

const (
	SortSize     SortField = "size"
	SortHeadroom SortField = "headroom"
	SortScore    SortField = "score"
	SortTitle    SortField = "title"
)

// SortFields lists options in display order.
var SortFields = []SortField{SortSize, SortHeadroom, SortScore, SortTitle}

// ParseSortField validates a sort name.
func ParseSortField(s string) (SortField, error) {
	for _, f := range SortFields {
		if string(f) == s {
			return f, nil
		}
	}
	return "", fmt.Errorf("unknown sort %q", s)
}

// Candidate is one file that might be replaced.
type Candidate struct {
	Kind arr.Kind
	// Key uniquely identifies the candidate in forms: the movie id for
	// Radarr, the episode file id for Sonarr.
	Key int
	// SearchIDs are passed to the search command: movie ids or episode ids.
	SearchIDs []int
	// ReleaseID is used for an interactive release check: a movie id or the
	// first episode id.
	ReleaseID    int
	Title        string
	Year         int
	SeriesID     int
	Episode      string
	EpisodeTitle string
	File         arr.File
	Profile      *arr.QualityProfile
	Ineligible   Reason
	// SeriesUnmonitored is set for Sonarr episodes whose whole series is
	// unmonitored; monitoring the episode alone would not make it searchable.
	SeriesUnmonitored bool
}

// DisplayTitle names the movie or episode.
func (c Candidate) DisplayTitle() string {
	if c.Kind == arr.Sonarr {
		return fmt.Sprintf("%s %s", c.Title, c.Episode)
	}
	return media.DisplayTitle(c.Title, c.Year)
}

// CutoffScore is the profile's cutoff format score, or 0 when unknown.
func (c Candidate) CutoffScore() int {
	if c.Profile == nil {
		return 0
	}
	return c.Profile.CutoffFormatScore
}

// Headroom is how much format score a perfect release would add, or -1 when
// the profile is unknown.
func (c Candidate) Headroom() int {
	if c.Profile == nil {
		return -1
	}
	return max(0, c.Profile.CutoffFormatScore-c.File.Score)
}

// MissingFormats lists the scored formats the file lacks.
func (c Candidate) MissingFormats() []arr.MissingFormat {
	if c.Profile == nil {
		return nil
	}
	return c.Profile.MissingFormats(c.File.Formats)
}

// SameQualityOnly reports that the file already meets the quality cutoff.
//
// Once that is true, *arr stops upgrading on quality and accepts a
// format-score-only upgrade solely from a release of the same quality. A
// higher-scoring x265 Bluray replacing a WEBDL is rejected outright, however
// good its score, so the practical window is far narrower than the headroom
// suggests.
func (c Candidate) SameQualityOnly() bool {
	return !c.File.QualityCutoffNotMet
}

// Plan is the result of a scan.
type Plan struct {
	Eligible   []Candidate
	Ineligible []Candidate
	Warnings   []string
	// Scanned counts files that matched the codec and size filters.
	Scanned       int
	EligibleBytes int64
}

// RadarrSource is the subset of the Radarr client used here.
type RadarrSource interface {
	Items(ctx context.Context) ([]arr.Item, error)
	QualityProfiles(ctx context.Context) (map[int]arr.QualityProfile, error)
	QueuedIDs(ctx context.Context) (map[int]bool, error)
	MovieFiles(ctx context.Context, movieIDs []int) (map[int]arr.File, error)
}

// SonarrSource is the subset of the Sonarr client used here.
type SonarrSource interface {
	Items(ctx context.Context) ([]arr.Item, error)
	QualityProfiles(ctx context.Context) (map[int]arr.QualityProfile, error)
	QueuedIDs(ctx context.Context) (map[int]bool, error)
	EpisodeFiles(ctx context.Context, seriesID int) ([]arr.File, error)
	Episodes(ctx context.Context, seriesID int) ([]arr.Episode, error)
}

// FindMovies scans Radarr.
func FindMovies(ctx context.Context, src RadarrSource, req Request) (Plan, error) {
	var plan Plan

	movies, err := src.Items(ctx)
	if err != nil {
		return plan, err
	}
	profiles, err := src.QualityProfiles(ctx)
	if err != nil {
		return plan, err
	}
	queued, err := src.QueuedIDs(ctx)
	if err != nil {
		// Not fatal: the cost of missing this is a duplicate search.
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("Could not read the Radarr queue (%v); movies already downloading may be searched again.", err))
		queued = map[int]bool{}
	}

	// Narrow on the movie list first so the file lookup covers only what
	// could matter.
	byID := map[int]arr.Item{}
	var ids []int
	for _, m := range movies {
		if !m.HasFile || m.SizeOnDisk == nil || *m.SizeOnDisk < req.MinSize {
			continue
		}
		byID[m.ID] = m
		ids = append(ids, m.ID)
	}
	sort.Ints(ids)

	files := map[int]arr.File{}
	if len(ids) > 0 {
		files, err = src.MovieFiles(ctx, ids)
		if err != nil {
			return plan, err
		}
	}

	for _, id := range ids {
		f, ok := files[id]
		if !ok || !req.Codecs.Includes(f.Codec) {
			continue
		}
		// Re-checked against the file itself: sizeOnDisk can include extras.
		if f.Size < req.MinSize {
			continue
		}
		plan.Scanned++

		m := byID[id]
		c := Candidate{
			Kind:      arr.Radarr,
			Key:       m.ID,
			SearchIDs: []int{m.ID},
			ReleaseID: m.ID,
			Title:     m.Title,
			Year:      m.Year,
			File:      f,
		}
		if p, ok := profiles[m.QualityProfileID]; ok {
			c.Profile = &p
		}
		c.Ineligible = assess(m.Monitored, c.Profile, f, queued[m.ID])
		plan.add(c)
	}

	plan.finish(req.Sort)
	return plan, nil
}

// FindEpisodes scans Sonarr. Episode files can only be listed per series, so
// series are shortlisted on total size first and fetched with bounded
// concurrency.
func FindEpisodes(ctx context.Context, src SonarrSource, req Request, logger *slog.Logger) (Plan, error) {
	var plan Plan
	if logger == nil {
		logger = slog.Default()
	}

	series, err := src.Items(ctx)
	if err != nil {
		return plan, err
	}
	profiles, err := src.QualityProfiles(ctx)
	if err != nil {
		return plan, err
	}
	queued, err := src.QueuedIDs(ctx)
	if err != nil {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("Could not read the Sonarr queue (%v); episodes already downloading may be searched again.", err))
		queued = map[int]bool{}
	}

	var shortlist []arr.Item
	for _, s := range series {
		// No single file can exceed the series total.
		if s.EpisodeFileCount == 0 || s.SizeOnDisk == nil || *s.SizeOnDisk < req.MinSize {
			continue
		}
		shortlist = append(shortlist, s)
	}

	type result struct {
		series   arr.Item
		files    []arr.File
		episodes []arr.Episode
		err      error
	}

	results := make([]result, len(shortlist))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, s := range shortlist {
		wg.Add(1)
		go func(i int, s arr.Item) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			r := result{series: s}
			r.files, r.err = src.EpisodeFiles(ctx, s.ID)
			if r.err == nil {
				// Only fetch episodes when a file passed the filters.
				needed := false
				for _, f := range r.files {
					if f.Size >= req.MinSize && req.Codecs.Includes(f.Codec) {
						needed = true
						break
					}
				}
				if needed {
					r.episodes, r.err = src.Episodes(ctx, s.ID)
				}
			}
			results[i] = r
		}(i, s)
	}
	wg.Wait()

	failed := 0
	for _, r := range results {
		if r.err != nil {
			failed++
			logger.Warn("could not read series files", "series", r.series.Title, "error", r.err)
			continue
		}

		episodesByFile := map[int][]arr.Episode{}
		for _, e := range r.episodes {
			if e.EpisodeFileID != 0 {
				episodesByFile[e.EpisodeFileID] = append(episodesByFile[e.EpisodeFileID], e)
			}
		}

		for _, f := range r.files {
			if f.Size < req.MinSize || !req.Codecs.Includes(f.Codec) {
				continue
			}
			eps := episodesByFile[f.ID]
			if len(eps) == 0 {
				continue
			}
			sort.Slice(eps, func(i, j int) bool {
				if eps[i].Season != eps[j].Season {
					return eps[i].Season < eps[j].Season
				}
				return eps[i].Number < eps[j].Number
			})
			plan.Scanned++

			c := Candidate{
				Kind:         arr.Sonarr,
				Key:          f.ID,
				ReleaseID:    eps[0].ID,
				Title:        r.series.Title,
				Year:         r.series.Year,
				SeriesID:     r.series.ID,
				Episode:      episodeLabel(eps),
				EpisodeTitle: eps[0].Title,
				File:         f,
			}
			monitored := r.series.Monitored
			isQueued := false
			for _, e := range eps {
				c.SearchIDs = append(c.SearchIDs, e.ID)
				monitored = monitored && e.Monitored
				isQueued = isQueued || queued[e.ID]
			}
			if p, ok := profiles[r.series.QualityProfileID]; ok {
				c.Profile = &p
			}
			c.Ineligible = assess(monitored, c.Profile, f, isQueued)
			c.SeriesUnmonitored = !r.series.Monitored
			plan.add(c)
		}
	}

	if failed > 0 {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("%d series could not be read from Sonarr and were skipped; see the container log for details.", failed))
	}

	plan.finish(req.Sort)
	return plan, nil
}

func episodeLabel(eps []arr.Episode) string {
	first := eps[0]
	label := fmt.Sprintf("S%02dE%02d", first.Season, first.Number)
	if len(eps) > 1 {
		last := eps[len(eps)-1]
		if last.Season == first.Season {
			label += fmt.Sprintf("-E%02d", last.Number)
		} else {
			label += fmt.Sprintf("-S%02dE%02d", last.Season, last.Number)
		}
	}
	return label
}

// assess decides whether *arr could act on a search.
func assess(monitored bool, profile *arr.QualityProfile, f arr.File, queued bool) Reason {
	switch {
	case queued:
		return AlreadyQueued
	case !monitored:
		return NotMonitored
	case profile == nil:
		return UnknownProfile
	case !profile.UpgradeAllowed:
		return UpgradesDisabled
	}
	// Two independent routes to an upgrade: format headroom, or a file below
	// the quality cutoff even if its format score is maxed.
	if f.Score >= profile.CutoffFormatScore && !f.QualityCutoffNotMet {
		return AtCutoff
	}
	return ""
}

func (p *Plan) add(c Candidate) {
	if c.Ineligible == "" {
		p.Eligible = append(p.Eligible, c)
		p.EligibleBytes += c.File.Size
	} else {
		p.Ineligible = append(p.Ineligible, c)
	}
}

func (p *Plan) finish(field SortField) {
	for _, list := range [][]Candidate{p.Eligible, p.Ineligible} {
		sort.SliceStable(list, func(i, j int) bool { return field.less(list[i], list[j]) })
	}
}

func (f SortField) less(a, b Candidate) bool {
	switch f {
	case SortHeadroom:
		if a.Headroom() != b.Headroom() {
			return a.Headroom() > b.Headroom()
		}
	case SortScore:
		if a.File.Score != b.File.Score {
			return a.File.Score < b.File.Score
		}
	case SortTitle:
		ta, tb := strings.ToLower(a.DisplayTitle()), strings.ToLower(b.DisplayTitle())
		if ta != tb {
			return ta < tb
		}
	}
	if a.File.Size != b.File.Size {
		return a.File.Size > b.File.Size
	}
	return a.Key < b.Key
}

// Resort returns a copy of p sorted by field. The cached plan is shared
// between requests, so it is never sorted in place.
func Resort(p Plan, field SortField) Plan {
	out := p
	out.Eligible = append([]Candidate(nil), p.Eligible...)
	out.Ineligible = append([]Candidate(nil), p.Ineligible...)
	for _, list := range [][]Candidate{out.Eligible, out.Ineligible} {
		sort.SliceStable(list, func(i, j int) bool { return field.less(list[i], list[j]) })
	}
	return out
}

// MarkQueued returns a copy of p with the given keys moved from eligible to
// ineligible as already queued.
func MarkQueued(p Plan, keys map[int]bool) Plan {
	return MarkIneligible(p, keys, AlreadyQueued)
}

// MarkIneligible returns a copy of p with the given keys moved from eligible
// to ineligible for reason. The cached plan is shared, so it is never mutated.
func MarkIneligible(p Plan, keys map[int]bool, reason Reason) Plan {
	out := p
	out.Eligible = nil
	out.EligibleBytes = 0
	out.Ineligible = append([]Candidate(nil), p.Ineligible...)
	for _, c := range p.Eligible {
		if keys[c.Key] {
			c.Ineligible = reason
			out.Ineligible = append(out.Ineligible, c)
			continue
		}
		out.Eligible = append(out.Eligible, c)
		out.EligibleBytes += c.File.Size
	}
	return out
}
