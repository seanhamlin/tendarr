package settings

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/seanhamlin/tendarr/internal/media"
)

// AutoCheck configures scheduled release checks.
//
// One budget covers Radarr and Sonarr together: they almost always search
// the same indexers (often through Prowlarr), so what trackers see is the
// combined rate.
type AutoCheck struct {
	Radarr bool
	Sonarr bool
	// PerHour is the combined number of interactive searches per hour.
	PerHour int
	// RecheckAfter is how long a result is trusted before checking again.
	RecheckAfter   time.Duration
	Codecs         media.CodecPolicy
	MovieMinSize   int64
	EpisodeMinSize int64
}

// Enabled reports whether any service is scheduled.
func (a AutoCheck) Enabled() bool { return a.Radarr || a.Sonarr }

// Interval is the gap between checks, spreading the budget evenly rather
// than spending it in a burst at the top of the hour.
func (a AutoCheck) Interval() time.Duration {
	if a.PerHour <= 0 {
		return time.Hour
	}
	return time.Hour / time.Duration(a.PerHour)
}

// Limits for the form.
const (
	MaxChecksPerHour = 60
	MinRecheckDays   = 1
	MaxRecheckDays   = 365
)

const (
	keyAutoRadarr      = "autocheck.radarr"
	keyAutoSonarr      = "autocheck.sonarr"
	keyAutoPerHour     = "autocheck.per_hour"
	keyAutoRecheckDays = "autocheck.recheck_days"
	keyAutoCodecs      = "autocheck.codecs"
	keyAutoMovieMin    = "autocheck.movie_min_size"
	keyAutoEpisodeMin  = "autocheck.episode_min_size"
)

// AutoCheckForm is the form as typed, so invalid input can be shown back.
type AutoCheckForm struct {
	Radarr         bool
	Sonarr         bool
	PerHour        string
	RecheckDays    string
	Codecs         string
	MovieMinSize   string
	EpisodeMinSize string
}

// DefaultAutoCheckForm is off, at 20 checks an hour, rechecking weekly.
func DefaultAutoCheckForm() AutoCheckForm {
	return AutoCheckForm{
		PerHour:        "20",
		RecheckDays:    "7",
		Codecs:         string(media.PolicyLegacy),
		MovieMinSize:   "4G",
		EpisodeMinSize: "1G",
	}
}

func autoCheckFormFromMap(m map[string]string) AutoCheckForm {
	f := DefaultAutoCheckForm()
	f.Radarr = m[keyAutoRadarr] == "1"
	f.Sonarr = m[keyAutoSonarr] == "1"
	for key, dst := range map[string]*string{
		keyAutoPerHour: &f.PerHour, keyAutoRecheckDays: &f.RecheckDays, keyAutoCodecs: &f.Codecs,
		keyAutoMovieMin: &f.MovieMinSize, keyAutoEpisodeMin: &f.EpisodeMinSize,
	} {
		if v := m[key]; v != "" {
			*dst = v
		}
	}
	return f
}

// ValidateAutoCheck parses and checks the form.
func ValidateAutoCheck(f AutoCheckForm) (AutoCheck, Errors) {
	errs := Errors{}
	ac := AutoCheck{Radarr: f.Radarr, Sonarr: f.Sonarr}

	f.PerHour = strings.TrimSpace(f.PerHour)
	if n, err := strconv.Atoi(f.PerHour); err != nil || n < 1 || n > MaxChecksPerHour {
		errs["per_hour"] = fmt.Sprintf("must be a whole number from 1 to %d", MaxChecksPerHour)
	} else {
		ac.PerHour = n
	}

	f.RecheckDays = strings.TrimSpace(f.RecheckDays)
	if n, err := strconv.Atoi(f.RecheckDays); err != nil || n < MinRecheckDays || n > MaxRecheckDays {
		errs["recheck_days"] = fmt.Sprintf("must be a whole number of days from %d to %d", MinRecheckDays, MaxRecheckDays)
	} else {
		ac.RecheckAfter = time.Duration(n) * 24 * time.Hour
	}

	if p, err := media.ParseCodecPolicy(strings.TrimSpace(f.Codecs)); err != nil {
		errs["codecs"] = "choose one of the listed codec groups"
	} else {
		ac.Codecs = p
	}

	parseMin := func(field, raw string) int64 {
		n, err := media.ParseSize(strings.TrimSpace(raw))
		switch {
		case err != nil:
			errs[field] = err.Error()
		case n < 100<<20:
			errs[field] = "must be at least 100M"
		}
		return n
	}
	ac.MovieMinSize = parseMin("movie_min_size", f.MovieMinSize)
	ac.EpisodeMinSize = parseMin("episode_min_size", f.EpisodeMinSize)

	return ac, errs
}

// AutoCheckValues encodes validated settings for storage.
func AutoCheckValues(f AutoCheckForm) map[string]string {
	b := func(v bool) string {
		if v {
			return "1"
		}
		return "0"
	}
	return map[string]string{
		keyAutoRadarr:      b(f.Radarr),
		keyAutoSonarr:      b(f.Sonarr),
		keyAutoPerHour:     strings.TrimSpace(f.PerHour),
		keyAutoRecheckDays: strings.TrimSpace(f.RecheckDays),
		keyAutoCodecs:      strings.TrimSpace(f.Codecs),
		keyAutoMovieMin:    strings.TrimSpace(f.MovieMinSize),
		keyAutoEpisodeMin:  strings.TrimSpace(f.EpisodeMinSize),
	}
}
