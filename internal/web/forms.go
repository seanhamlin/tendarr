package web

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/seanhamlin/tendarr/internal/media"
	"github.com/seanhamlin/tendarr/internal/reap"
	"github.com/seanhamlin/tendarr/internal/upgrade"
)

const pageSize = 100

// maxSelection bounds how many keys one form may carry.
const maxSelection = 1000

// reapFilters is the filter form as the user typed it, kept as strings so an
// invalid value can be shown back alongside its error.
type reapFilters struct {
	UnwatchedFor string
	MinAge       string
	MinRating    string
	MinVotes     string
	MinSize      string
	Sort         string
	Fuzzy        bool
}

// DefaultMovieMinRating protects well-rated films out of the box. TV has no
// default floor: series ratings are thinner and less reliable.
const DefaultMovieMinRating = "7.5"

func defaultReapFilters(movies bool) reapFilters {
	f := reapFilters{UnwatchedFor: "2y", MinAge: "90d", MinVotes: "50", Sort: string(reap.SortSize)}
	if movies {
		f.MinRating = DefaultMovieMinRating
	}
	return f
}

// parseReapFilters validates the filter fields. Absent fields take defaults;
// present but invalid fields are errors, never silently replaced. A field
// present but empty is a deliberate "off" (min_rating, min_size), which is
// why Values always carries them.
func parseReapFilters(v url.Values, movies bool) (reap.Criteria, reapFilters, map[string]string) {
	f := defaultReapFilters(movies)
	errs := map[string]string{}
	cr := reap.DefaultCriteria()

	str := func(key string, dst *string) {
		if vals, ok := v[key]; ok {
			*dst = strings.TrimSpace(vals[0])
		}
	}
	str("unwatched", &f.UnwatchedFor)
	str("min_age", &f.MinAge)
	str("min_rating", &f.MinRating)
	str("min_votes", &f.MinVotes)
	str("min_size", &f.MinSize)
	str("sort", &f.Sort)
	f.Fuzzy = v.Get("fuzzy") == "1"

	for key, s := range map[string]string{"unwatched": f.UnwatchedFor, "min_age": f.MinAge, "min_rating": f.MinRating, "min_votes": f.MinVotes, "min_size": f.MinSize, "sort": f.Sort} {
		if len(s) > 32 {
			errs[key] = "too long"
		}
	}
	if len(errs) > 0 {
		return cr, f, errs
	}

	if p, err := media.ParsePeriod(f.UnwatchedFor); err != nil {
		errs["unwatched"] = err.Error()
	} else {
		cr.UnwatchedFor = p
	}
	if p, err := media.ParsePeriod(f.MinAge); err != nil {
		errs["min_age"] = err.Error()
	} else {
		cr.MinAge = p
	}
	if f.MinRating != "" {
		r, err := strconv.ParseFloat(f.MinRating, 64)
		if err != nil || r < 0 || r > 10 {
			errs["min_rating"] = "must be a number from 0 to 10"
		} else {
			cr.MinRating = &r
		}
	}
	if f.MinVotes == "" {
		f.MinVotes = "0"
	}
	if n, err := strconv.Atoi(f.MinVotes); err != nil || n < 0 || n > 10_000_000 {
		errs["min_votes"] = "must be a whole number of 0 or more"
	} else {
		cr.MinRatingVotes = n
	}
	if f.MinSize != "" {
		if n, err := media.ParseSize(f.MinSize); err != nil {
			errs["min_size"] = err.Error()
		} else {
			cr.MinSize = n
		}
	}
	if s, err := reap.ParseSortField(f.Sort); err != nil {
		errs["sort"] = "choose one of the listed sort orders"
	} else {
		cr.Sort = s
	}
	cr.AllowFuzzy = f.Fuzzy

	return cr, f, errs
}

// Values encodes the filters for links and hidden fields.
func (f reapFilters) Values() url.Values {
	v := url.Values{}
	v.Set("unwatched", f.UnwatchedFor)
	v.Set("min_age", f.MinAge)
	v.Set("min_votes", f.MinVotes)
	v.Set("sort", f.Sort)
	// Always present, even when empty: an absent field means "use the
	// default", so dropping an empty one would silently turn a floor the
	// user switched off back on at the confirm and delete steps.
	v.Set("min_rating", f.MinRating)
	v.Set("min_size", f.MinSize)
	if f.Fuzzy {
		v.Set("fuzzy", "1")
	}
	return v
}

type upgradeFilters struct {
	Codecs  string
	MinSize string
	Sort    string
}

func defaultUpgradeFilters(episodes bool) upgradeFilters {
	f := upgradeFilters{Codecs: string(media.PolicyLegacy), MinSize: "4G", Sort: string(upgrade.SortSize)}
	if episodes {
		f.MinSize = "1G"
	}
	return f
}

func parseUpgradeFilters(v url.Values, episodes bool) (upgrade.Request, upgradeFilters, map[string]string) {
	f := defaultUpgradeFilters(episodes)
	errs := map[string]string{}
	req := upgrade.Request{Codecs: media.PolicyLegacy, Sort: upgrade.SortSize}

	if vals, ok := v["codecs"]; ok {
		f.Codecs = strings.TrimSpace(vals[0])
	}
	if vals, ok := v["min_size"]; ok {
		f.MinSize = strings.TrimSpace(vals[0])
	}
	if vals, ok := v["sort"]; ok {
		f.Sort = strings.TrimSpace(vals[0])
	}

	if p, err := media.ParseCodecPolicy(f.Codecs); err != nil {
		errs["codecs"] = "choose one of the listed codec groups"
	} else {
		req.Codecs = p
	}
	if len(f.MinSize) > 32 {
		errs["min_size"] = "too long"
	} else if n, err := media.ParseSize(f.MinSize); err != nil {
		errs["min_size"] = err.Error()
	} else if n < 100<<20 {
		// Below this the scan would touch nearly every file for no benefit.
		errs["min_size"] = "must be at least 100M"
	} else {
		req.MinSize = n
	}
	if s, err := upgrade.ParseSortField(f.Sort); err != nil {
		errs["sort"] = "choose one of the listed sort orders"
	} else {
		req.Sort = s
	}

	return req, f, errs
}

// Values encodes the filters for links and hidden fields.
func (f upgradeFilters) Values() url.Values {
	return url.Values{"codecs": {f.Codecs}, "min_size": {f.MinSize}, "sort": {f.Sort}}
}

// parseKeys reads selected string keys, rejecting anything that is not a
// plain identifier.
func parseKeys(v url.Values) ([]string, error) {
	keys := v["key"]
	if len(keys) == 0 {
		return nil, fmt.Errorf("select at least one item")
	}
	if len(keys) > maxSelection {
		return nil, fmt.Errorf("select at most %d items at a time", maxSelection)
	}
	for _, k := range keys {
		if k == "" || len(k) > 32 || strings.Trim(k, "0123456789") != "" {
			return nil, fmt.Errorf("invalid selection")
		}
	}
	return keys, nil
}

// parseIntKeys reads selected numeric keys.
func parseIntKeys(v url.Values) ([]int, error) {
	raw, err := parseKeys(v)
	if err != nil {
		return nil, err
	}
	out := make([]int, 0, len(raw))
	for _, k := range raw {
		n, err := strconv.Atoi(k)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("invalid selection")
		}
		out = append(out, n)
	}
	return out, nil
}

func parsePage(v url.Values) int {
	p, err := strconv.Atoi(v.Get("page"))
	if err != nil || p < 1 {
		return 1
	}
	return p
}

// paginate returns the slice bounds for page, clamped.
func paginate(total, page int) (start, end, pages, current int) {
	pages = (total + pageSize - 1) / pageSize
	if pages == 0 {
		pages = 1
	}
	current = min(max(page, 1), pages)
	start = (current - 1) * pageSize
	end = min(start+pageSize, total)
	return start, end, pages, current
}
