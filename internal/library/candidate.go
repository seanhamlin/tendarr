package library

import (
	"strings"
	"time"

	"github.com/seanhamlin/tendarr/internal/clients/arr"
	"github.com/seanhamlin/tendarr/internal/clients/plex"
	"github.com/seanhamlin/tendarr/internal/media"
)

// ManagementState says whether an item can actually be deleted through *arr.
type ManagementState string

const (
	// Managed: matched in *arr and confirmed to be the same files.
	Managed ManagementState = "MANAGED"
	// Orphaned: an *arr record exists but reports no file. Deleting would
	// remove the record, free nothing, leave the file in Plex, and add a
	// permanent import exclusion: worse than doing nothing in every way.
	Orphaned ManagementState = "ORPHANED"
	// Mismatched: an *arr record exists but manages a different copy, e.g. a
	// separate 4K library matched by IMDb id to the 1080p record.
	Mismatched ManagementState = "MISMATCHED"
	// Unmanaged: no *arr record at all.
	Unmanaged ManagementState = "UNMANAGED"
)

// Reason explains a non-managed state.
func (s ManagementState) Reason() string {
	switch s {
	case Orphaned:
		return "matched in *arr but it reports no file on disk"
	case Mismatched:
		return "*arr manages a different copy of this title"
	case Unmanaged:
		return "not managed by Radarr/Sonarr"
	default:
		return ""
	}
}

// StalenessSource records which system supplied the last-played date.
type StalenessSource string

const (
	SourcePlex     StalenessSource = "Plex"
	SourceTautulli StalenessSource = "Tautulli"
	SourceNever    StalenessSource = "Never"
)

// ArrLink is the *arr record an item is matched to.
type ArrLink struct {
	Item            arr.Item
	MatchedBy       MatchStrategy
	StorageVerified bool
}

// IsContinuingSeries reports a series Sonarr still expects episodes for.
// Excluding one permanently blocks import lists from re-adding it, so next
// season would silently never arrive.
func (l ArrLink) IsContinuingSeries() bool {
	return l.Item.Kind == arr.Sonarr && strings.EqualFold(l.Item.Status, "continuing")
}

// State derives the management state.
func (l ArrLink) State() ManagementState {
	// Checked before hasFile: "*arr holds a different copy" is both more
	// specific and more dangerous than "*arr holds no file".
	if !l.StorageVerified {
		return Mismatched
	}
	if l.Item.HasFile && l.Item.SizeOnDisk != nil && *l.Item.SizeOnDisk > 0 {
		return Managed
	}
	return Orphaned
}

// Candidate is one library item with everything a guard, sort or report needs.
type Candidate struct {
	RatingKey   string
	SectionKey  string
	SectionName string
	Type        plex.MediaType
	Title       string
	Year        int
	IDs         media.ExternalIDs
	AddedAt     time.Time
	// LastPlayedAt is the later of Plex's and Tautulli's last play.
	LastPlayedAt *time.Time
	Source       StalenessSource
	PlayCount    int
	// Size is the best-known on-disk size, for display and sorting only.
	Size   *int64
	Rating *media.Rating
	Arr    *ArrLink
}

// DisplayTitle is "Title (Year)".
func (c Candidate) DisplayTitle() string { return media.DisplayTitle(c.Title, c.Year) }

// LastActivity is the date staleness is measured from. It falls back to the
// date added, so a never-watched item ages from the day it arrived.
func (c Candidate) LastActivity() time.Time {
	if c.LastPlayedAt != nil {
		return *c.LastPlayedAt
	}
	return c.AddedAt
}

// NeverPlayed reports no recorded play from either source.
func (c Candidate) NeverPlayed() bool { return c.LastPlayedAt == nil }

// State is the management state, Unmanaged when there is no *arr record.
func (c Candidate) State() ManagementState {
	if c.Arr == nil {
		return Unmanaged
	}
	return c.Arr.State()
}

// MatchedBy is how the *arr record was found.
func (c Candidate) MatchedBy() MatchStrategy {
	if c.Arr == nil {
		return MatchNone
	}
	return c.Arr.MatchedBy
}

// ArrID is the Radarr movie or Sonarr series id, or 0.
func (c Candidate) ArrID() int {
	if c.Arr == nil {
		return 0
	}
	return c.Arr.Item.ID
}

// ReclaimableBytes is what the delete call will actually free: only ever the
// *arr size, and only for managed items. Deliberately distinct from Size,
// which may come from Plex; totals must use this, or the confirmation page
// quotes a figure the delete cannot deliver.
func (c Candidate) ReclaimableBytes() *int64 {
	if c.State() != Managed {
		return nil
	}
	return c.Arr.Item.SizeOnDisk
}

// SizeBytes is Size or -1 when unknown, for sorting.
func (c Candidate) SizeBytes() int64 {
	if c.Size == nil {
		return -1
	}
	return *c.Size
}

// DaysSinceActivity is whole days since LastActivity.
func (c Candidate) DaysSinceActivity(now time.Time) int {
	return media.DaysBetween(c.LastActivity(), now)
}

// DaysSinceAdded is whole days since AddedAt.
func (c Candidate) DaysSinceAdded(now time.Time) int {
	return media.DaysBetween(c.AddedAt, now)
}
