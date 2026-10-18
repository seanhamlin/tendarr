package library

import (
	"time"

	"github.com/seanhamlin/tendarr/internal/clients/arr"
	"github.com/seanhamlin/tendarr/internal/clients/plex"
	"github.com/seanhamlin/tendarr/internal/clients/tautulli"
)

// Assemble joins one Plex section with Tautulli and an *arr index.
func Assemble(
	section plex.Section,
	items []plex.Item,
	watch map[string]tautulli.WatchRecord,
	index *Index,
	verifier *Verifier,
	now time.Time,
) []Candidate {
	out := make([]Candidate, 0, len(items))

	for _, pi := range items {
		var tr *tautulli.WatchRecord
		if r, ok := watch[pi.RatingKey]; ok {
			tr = &r
		}

		lastPlayed, source := lastActivity(pi, tr)

		var link *ArrLink
		var arrItem *arr.Item
		if index != nil {
			if found, strategy := index.Find(pi.IDs, pi.Title, pi.Year); found != nil {
				arrItem = found
				// Same title is not the same file. Verified here so that a
				// 4K copy never inherits the deletability of the 1080p
				// record it shares an id with.
				link = &ArrLink{
					Item:            *found,
					MatchedBy:       strategy,
					StorageVerified: verifier.Verify(section, pi, *found),
				}
			}
		}

		playCount := pi.ViewCount
		if tr != nil && tr.PlayCount > playCount {
			playCount = tr.PlayCount
		}

		c := Candidate{
			RatingKey:    pi.RatingKey,
			SectionKey:   section.Key,
			SectionName:  section.Title,
			Type:         section.Type,
			Title:        pi.Title,
			Year:         pi.Year,
			IDs:          pi.IDs,
			AddedAt:      addedAt(pi, tr, arrItem, now),
			LastPlayedAt: lastPlayed,
			Source:       source,
			PlayCount:    playCount,
			Size:         bestSize(section.Type, pi, tr, arrItem),
			Arr:          link,
		}
		if arrItem != nil {
			c.Rating = arrItem.Rating
		}
		out = append(out, c)
	}

	return out
}

// lastActivity takes the later of Plex's and Tautulli's last play.
//
// Neither source is complete on its own. Tautulli only knows about playback
// since it was installed. Plex's lastViewedAt covers the full history but only
// for the token's own user. Taking the later means any evidence of a play from
// either system protects the item, which is the fail-safe direction.
func lastActivity(pi plex.Item, tr *tautulli.WatchRecord) (*time.Time, StalenessSource) {
	var tPlayed *time.Time
	if tr != nil {
		tPlayed = tr.LastPlayedAt
	}
	switch {
	case pi.LastViewedAt == nil && tPlayed == nil:
		return nil, SourceNever
	case tPlayed == nil:
		return pi.LastViewedAt, SourcePlex
	case pi.LastViewedAt == nil:
		return tPlayed, SourceTautulli
	case !pi.LastViewedAt.Before(*tPlayed):
		return pi.LastViewedAt, SourcePlex
	default:
		return tPlayed, SourceTautulli
	}
}

// addedAt picks the earliest known date added. The earliest rather than the
// latest, because a never-watched item is aged by this and a later date would
// understate how long it has sat there. Falls back to now only if every source
// is silent, which makes the item look new and therefore protected.
func addedAt(pi plex.Item, tr *tautulli.WatchRecord, ai *arr.Item, now time.Time) time.Time {
	var earliest *time.Time
	consider := func(t *time.Time) {
		if t != nil && (earliest == nil || t.Before(*earliest)) {
			earliest = t
		}
	}
	consider(pi.AddedAt)
	if tr != nil {
		consider(tr.AddedAt)
	}
	if ai != nil {
		consider(ai.Added)
	}
	if earliest == nil {
		return now
	}
	return *earliest
}

// bestSize is for display and sorting only. Shows have no usable Plex or
// Tautulli size at series level, so Sonarr is the only source there.
func bestSize(t plex.MediaType, pi plex.Item, tr *tautulli.WatchRecord, ai *arr.Item) *int64 {
	positive := func(v *int64) bool { return v != nil && *v > 0 }

	if t == plex.Show {
		if ai != nil && positive(ai.SizeOnDisk) {
			return ai.SizeOnDisk
		}
		return nil
	}
	if positive(pi.FileSize) {
		return pi.FileSize
	}
	if tr != nil && positive(tr.FileSize) {
		return tr.FileSize
	}
	if ai != nil && positive(ai.SizeOnDisk) {
		return ai.SizeOnDisk
	}
	return nil
}
