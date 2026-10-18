// Package library joins Plex, Tautulli and Radarr/Sonarr into one view of
// each item: what it is, who manages it, when it was last watched, and
// whether *arr can actually delete it.
package library

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/seanhamlin/tendarr/internal/clients/arr"
	"github.com/seanhamlin/tendarr/internal/media"
)

// MatchStrategy records how a Plex item was matched to an *arr record.
type MatchStrategy string

const (
	MatchTMDb      MatchStrategy = "TMDB"
	MatchTVDb      MatchStrategy = "TVDB"
	MatchIMDb      MatchStrategy = "IMDB"
	MatchTitleYear MatchStrategy = "FUZZY"
	MatchNone      MatchStrategy = "NONE"
)

// IsFuzzy reports a title+year match, which is not trusted for deletes.
func (m MatchStrategy) IsFuzzy() bool { return m == MatchTitleYear }

// Index is a set of lookup maps over one *arr library, built once per
// snapshot from a single list call.
type Index struct {
	items       []arr.Item
	byTMDb      map[int]arr.Item
	byTVDb      map[int]arr.Item
	byIMDb      map[string]arr.Item
	byTitleYear map[string]*arr.Item // nil marks an ambiguous key
}

// NewIndex builds the lookup maps.
func NewIndex(items []arr.Item) *Index {
	idx := &Index{
		items:       items,
		byTMDb:      map[int]arr.Item{},
		byTVDb:      map[int]arr.Item{},
		byIMDb:      map[string]arr.Item{},
		byTitleYear: map[string]*arr.Item{},
	}
	for i := range items {
		it := items[i]
		if it.IDs.TMDb > 0 {
			idx.byTMDb[it.IDs.TMDb] = it
		}
		if it.IDs.TVDb > 0 {
			idx.byTVDb[it.IDs.TVDb] = it
		}
		if it.IDs.IMDb != "" {
			idx.byIMDb[strings.ToLower(it.IDs.IMDb)] = it
		}
		if key := TitleYearKey(it.Title, it.Year); key != "" {
			// A collision means the key cannot identify one record, so it is
			// poisoned rather than overwritten. Silently keeping the last
			// writer is how "Dune" resolves to the wrong film.
			if _, exists := idx.byTitleYear[key]; exists {
				idx.byTitleYear[key] = nil
			} else {
				idx.byTitleYear[key] = &items[i]
			}
		}
	}
	return idx
}

// Items returns every record in the index.
func (idx *Index) Items() []arr.Item { return idx.items }

// Find resolves a record, strongest identifier first.
func (idx *Index) Find(ids media.ExternalIDs, title string, year int) (*arr.Item, MatchStrategy) {
	if ids.TMDb > 0 {
		if it, ok := idx.byTMDb[ids.TMDb]; ok {
			return &it, MatchTMDb
		}
	}
	if ids.TVDb > 0 {
		if it, ok := idx.byTVDb[ids.TVDb]; ok {
			return &it, MatchTVDb
		}
	}
	if ids.IMDb != "" {
		if it, ok := idx.byIMDb[strings.ToLower(ids.IMDb)]; ok {
			return &it, MatchIMDb
		}
	}
	// Last resort, and flagged as such.
	if key := TitleYearKey(title, year); key != "" {
		if it := idx.byTitleYear[key]; it != nil {
			found := *it
			return &found, MatchTitleYear
		}
	}
	return nil, MatchNone
}

var (
	leadingArticle = regexp.MustCompile(`^(the|a|an)\s+`)
	nonAlnum       = regexp.MustCompile(`[^a-z0-9]+`)
)

// TitleYearKey normalises a title and year into a fuzzy key, or "" when the
// year is unknown. The year is mandatory because titles alone collide
// constantly (The Office, Shameless, Ghosts).
func TitleYearKey(title string, year int) string {
	if year <= 0 {
		return ""
	}
	n := strings.ToLower(strings.TrimSpace(title))
	n = leadingArticle.ReplaceAllString(n, "")
	n = nonAlnum.ReplaceAllString(n, "")
	if n == "" {
		return ""
	}
	return fmt.Sprintf("%s:%d", n, year)
}
