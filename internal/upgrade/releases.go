package upgrade

import (
	"sort"
	"time"

	"github.com/seanhamlin/tendarr/internal/clients/arr"
)

// ReleaseCheck summarises an interactive search for one candidate.
type ReleaseCheck struct {
	CheckedAt time.Time
	// Available means at least one release *arr would accept right now.
	Available    bool
	BestTitle    string
	BestScore    int
	BestQuality  string
	BestSize     int64
	Total        int
	Rejected     int
	TopRejection string
	Error        string
}

// EvaluateReleases picks the best acceptable release.
//
// *arr marks a release rejected when it would not grab it, including when it
// is not an upgrade over the file on disk, so "not rejected" is exactly
// "would be grabbed". Among those, the highest score wins, then the smaller
// file.
func EvaluateReleases(releases []arr.Release, now time.Time) ReleaseCheck {
	rc := ReleaseCheck{CheckedAt: now, Total: len(releases)}

	var best *arr.Release
	reasons := map[string]int{}
	for i := range releases {
		r := &releases[i]
		if r.Rejected {
			rc.Rejected++
			for _, reason := range r.Rejections {
				reasons[reason]++
			}
			continue
		}
		if best == nil || r.Score > best.Score || (r.Score == best.Score && r.Size < best.Size) {
			best = r
		}
	}

	if best != nil {
		rc.Available = true
		rc.BestTitle = best.Title
		rc.BestScore = best.Score
		rc.BestQuality = best.Quality
		rc.BestSize = best.Size
	}

	if len(reasons) > 0 {
		type kv struct {
			reason string
			n      int
		}
		var all []kv
		for r, n := range reasons {
			all = append(all, kv{r, n})
		}
		sort.Slice(all, func(i, j int) bool {
			if all[i].n != all[j].n {
				return all[i].n > all[j].n
			}
			return all[i].reason < all[j].reason
		})
		rc.TopRejection = all[0].reason
	}

	return rc
}
