package media

import (
	"regexp"
	"strconv"
)

// ExternalIDs are the identifiers used to join Plex items to *arr records.
type ExternalIDs struct {
	IMDb string
	TMDb int
	TVDb int
}

// IsEmpty reports whether no identifier is known.
func (e ExternalIDs) IsEmpty() bool {
	return e.IMDb == "" && e.TMDb == 0 && e.TVDb == 0
}

var (
	guidIMDb       = regexp.MustCompile(`(?i)^imdb://(tt\d+)`)
	guidTMDb       = regexp.MustCompile(`(?i)^tmdb://(\d+)`)
	guidTVDb       = regexp.MustCompile(`(?i)^tvdb://(\d+)`)
	legacyIMDb     = regexp.MustCompile(`(?i)agents\.imdb://(tt\d+)`)
	legacyTMDb     = regexp.MustCompile(`(?i)agents\.(?:themoviedb|tmdb)://(\d+)`)
	legacyTVDb     = regexp.MustCompile(`(?i)agents\.(?:thetvdb|tvdb)://(\d+)`)
	bareIMDbInGUID = regexp.MustCompile(`\b(tt\d{7,})\b`)
)

// ExternalIDsFromGUIDs parses Plex's Guid children.
//
// New-agent items carry scheme-prefixed URIs (imdb://tt0111161, tmdb://278,
// tvdb://121361). Legacy-agent items instead have a single guid string like
// "com.plexapp.agents.imdb://tt0111161?lang=en", so both forms are handled.
// plex:// GUIDs are Plex-internal and useless for matching.
func ExternalIDsFromGUIDs(guids []string) ExternalIDs {
	var ids ExternalIDs

	atoi := func(s string) int {
		n, _ := strconv.Atoi(s)
		return n
	}

	for _, g := range guids {
		if g == "" {
			continue
		}

		if m := guidIMDb.FindStringSubmatch(g); m != nil {
			if ids.IMDb == "" {
				ids.IMDb = m[1]
			}
			continue
		}
		if m := guidTMDb.FindStringSubmatch(g); m != nil {
			if ids.TMDb == 0 {
				ids.TMDb = atoi(m[1])
			}
			continue
		}
		if m := guidTVDb.FindStringSubmatch(g); m != nil {
			if ids.TVDb == 0 {
				ids.TVDb = atoi(m[1])
			}
			continue
		}
		if m := legacyIMDb.FindStringSubmatch(g); m != nil {
			if ids.IMDb == "" {
				ids.IMDb = m[1]
			}
			continue
		}
		if m := legacyTMDb.FindStringSubmatch(g); m != nil {
			if ids.TMDb == 0 {
				ids.TMDb = atoi(m[1])
			}
			continue
		}
		if m := legacyTVDb.FindStringSubmatch(g); m != nil {
			if ids.TVDb == 0 {
				ids.TVDb = atoi(m[1])
			}
			continue
		}
		if ids.IMDb == "" {
			if m := bareIMDbInGUID.FindStringSubmatch(g); m != nil {
				ids.IMDb = m[1]
			}
		}
	}

	return ids
}
