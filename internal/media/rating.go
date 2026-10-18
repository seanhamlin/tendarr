package media

import (
	"fmt"
	"math"
)

// RatingSource identifies where a rating came from.
type RatingSource string

const (
	RatingIMDb           RatingSource = "imdb"
	RatingTMDb           RatingSource = "tmdb"
	RatingTrakt          RatingSource = "trakt"
	RatingRottenTomatoes RatingSource = "rottenTomatoes"
	RatingMetacritic     RatingSource = "metacritic"
	RatingTVDb           RatingSource = "tvdb"
)

// MaxValue is the top of the source's native scale.
func (s RatingSource) MaxValue() float64 {
	if s == RatingRottenTomatoes || s == RatingMetacritic {
		return 100
	}
	return 10
}

// Label is the display name.
func (s RatingSource) Label() string {
	switch s {
	case RatingIMDb:
		return "IMDb"
	case RatingTMDb:
		return "TMDb"
	case RatingTrakt:
		return "Trakt"
	case RatingRottenTomatoes:
		return "RT"
	case RatingMetacritic:
		return "Metacritic"
	case RatingTVDb:
		return "TVDb"
	default:
		return string(s)
	}
}

// Rating is normalised to a 0-10 scale, keeping the source and vote count.
//
// The vote count matters because an IMDb 8.4 from 11 votes is noise, and a
// rating floor that trusts it will protect junk while exposing well-rated
// obscure titles.
type Rating struct {
	Value  float64
	Source RatingSource
	Votes  *int
}

// NewRating normalises a raw value from the source's scale to 0-10.
func NewRating(raw float64, source RatingSource, votes *int) Rating {
	v := raw
	if max := source.MaxValue(); max != 10 {
		v = raw / max * 10
	}
	return Rating{Value: math.Round(v*10) / 10, Source: source, Votes: votes}
}

// IsTrustworthy reports whether the rating has enough votes to base a delete
// decision on. An unknown vote count is untrustworthy: the guard must never
// delete on absence of evidence.
func (r Rating) IsTrustworthy(minVotes int) bool {
	if minVotes <= 0 {
		return true
	}
	return r.Votes != nil && *r.Votes >= minVotes
}

// String formats the value only.
func (r Rating) String() string {
	return fmt.Sprintf("%.1f", r.Value)
}

// Describe includes source and votes.
func (r Rating) Describe() string {
	if r.Votes == nil {
		return fmt.Sprintf("%.1f %s", r.Value, r.Source.Label())
	}
	return fmt.Sprintf("%.1f %s, %d votes", r.Value, r.Source.Label(), *r.Votes)
}
