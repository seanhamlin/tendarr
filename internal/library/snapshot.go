package library

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/seanhamlin/tendarr/internal/clients/arr"
	"github.com/seanhamlin/tendarr/internal/clients/plex"
	"github.com/seanhamlin/tendarr/internal/clients/tautulli"
)

// PlexSource is the subset of the Plex client the builder needs.
type PlexSource interface {
	Sections(ctx context.Context) ([]plex.Section, error)
	SectionItems(ctx context.Context, s plex.Section) ([]plex.Item, error)
}

// WatchSource is the subset of the Tautulli client the builder needs.
type WatchSource interface {
	WatchRecords(ctx context.Context, sectionID string) (map[string]tautulli.WatchRecord, error)
}

// ArrSource is the subset of the *arr client the builder needs.
type ArrSource interface {
	Items(ctx context.Context) ([]arr.Item, error)
	FindTag(ctx context.Context, label string) (int, error)
}

// Snapshot is the joined view of one media type at one moment.
type Snapshot struct {
	Type       plex.MediaType
	Candidates []Candidate
	TakenAt    time.Time
	Warnings   []string
	// Incomplete is set when the *arr library could not be read. Every item
	// then looks unmanaged; nothing would be wrongly deleted, but the report
	// would be fiction, so deletes are refused.
	Incomplete bool
	// SkippedSections were deliberately not read, so the omission is visible.
	SkippedSections []string
	// IgnoreTagID is the *arr id of the ignore tag, or 0 if it does not exist.
	IgnoreTagID int
}

// Builder fetches every source and produces a Snapshot.
type Builder struct {
	Plex     PlexSource
	Watch    WatchSource // optional; nil when Tautulli is not configured
	Arr      ArrSource
	Verifier *Verifier
	Logger   *slog.Logger
	Now      func() time.Time
}

// Build produces a snapshot for one media type.
//
// Failure handling is asymmetric on purpose. A failing *arr library marks the
// snapshot incomplete. A failing section or Tautulli call is only a warning:
// what remains is still worth reporting on.
func (b *Builder) Build(ctx context.Context, t plex.MediaType, excludedSections []string, ignoreTag string) (*Snapshot, error) {
	now := time.Now().UTC()
	if b.Now != nil {
		now = b.Now()
	}
	logger := b.Logger
	if logger == nil {
		logger = slog.Default()
	}

	snap := &Snapshot{Type: t, TakenAt: now}

	var index *Index
	items, err := b.Arr.Items(ctx)
	if err != nil {
		snap.Incomplete = true
		snap.Warnings = append(snap.Warnings, fmt.Sprintf("The *arr library could not be read (%v). Every item will show as UNMANAGED and nothing can be deleted until it is reachable.", err))
		logger.Error("failed to read *arr library", "type", t, "error", err)
	} else {
		index = NewIndex(items)
		if ignoreTag != "" {
			id, err := b.Arr.FindTag(ctx, ignoreTag)
			if err != nil {
				snap.Incomplete = true
				snap.Warnings = append(snap.Warnings, fmt.Sprintf("Could not read *arr tags (%v), so ignored items cannot be identified. Deletes are disabled until this succeeds.", err))
			}
			snap.IgnoreTagID = id
		}
	}

	sections, err := b.Plex.Sections(ctx)
	if err != nil {
		return nil, err
	}

	if b.Watch == nil {
		snap.Warnings = append(snap.Warnings, "Tautulli is not configured, so plays by other Plex users are invisible and their favourites may look unwatched.")
	}

	excluded := map[string]bool{}
	for _, s := range excludedSections {
		excluded[strings.ToLower(strings.TrimSpace(s))] = true
	}

	for _, section := range sections {
		if section.Type != t {
			continue
		}
		if excluded[strings.ToLower(section.Title)] {
			snap.SkippedSections = append(snap.SkippedSections, section.Title)
			continue
		}

		plexItems, err := b.Plex.SectionItems(ctx, section)
		if err != nil {
			snap.Warnings = append(snap.Warnings, fmt.Sprintf("Skipped section %q: %v", section.Title, err))
			logger.Error("failed to read Plex section", "section", section.Title, "error", err)
			continue
		}
		if len(plexItems) == 0 {
			// More often a scan or permission problem than a genuinely empty
			// library, so say so.
			snap.Warnings = append(snap.Warnings, fmt.Sprintf("Section %q returned no items. If that is unexpected, check the Plex library scan.", section.Title))
			continue
		}

		var watch map[string]tautulli.WatchRecord
		if b.Watch != nil {
			watch, err = b.Watch.WatchRecords(ctx, section.Key)
			if err != nil {
				snap.Warnings = append(snap.Warnings, fmt.Sprintf("Tautulli history unavailable for %q (%v). Falling back to Plex watch state only, which misses plays by other users.", section.Title, err))
				logger.Warn("tautulli unavailable for section", "section", section.Title, "error", err)
			}
		}

		snap.Candidates = append(snap.Candidates, Assemble(section, plexItems, watch, index, b.Verifier, now)...)
	}

	return snap, nil
}
