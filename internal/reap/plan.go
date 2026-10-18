package reap

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/seanhamlin/tendarr/internal/library"
)

// SortField orders the eligible list.
type SortField string

const (
	SortSize      SortField = "size"
	SortRating    SortField = "rating"
	SortStaleness SortField = "staleness"
	SortTitle     SortField = "title"
)

// SortFields lists the options in display order.
var SortFields = []SortField{SortSize, SortStaleness, SortRating, SortTitle}

// ParseSortField validates a sort name.
func ParseSortField(s string) (SortField, error) {
	for _, f := range SortFields {
		if string(f) == s {
			return f, nil
		}
	}
	return "", fmt.Errorf("unknown sort %q", s)
}

// less orders two candidates in each field's natural "reap first" order:
// biggest first, lowest rated first, longest unwatched first.
func (f SortField) less(a, b library.Candidate) bool {
	switch f {
	case SortRating:
		ra, rb := 1e9, 1e9
		if a.Rating != nil {
			ra = a.Rating.Value
		}
		if b.Rating != nil {
			rb = b.Rating.Value
		}
		if ra != rb {
			return ra < rb
		}
	case SortStaleness:
		if !a.LastActivity().Equal(b.LastActivity()) {
			return a.LastActivity().Before(b.LastActivity())
		}
	case SortTitle:
		ta, tb := strings.ToLower(a.Title), strings.ToLower(b.Title)
		if ta != tb {
			return ta < tb
		}
	}
	// Size is both the default and the tie-breaker. Unknown sizes sort last;
	// there is nothing to reclaim from them.
	if a.SizeBytes() != b.SizeBytes() {
		return a.SizeBytes() > b.SizeBytes()
	}
	return a.RatingKey < b.RatingKey
}

// Evaluated pairs a candidate with its guard verdict.
type Evaluated struct {
	library.Candidate
	Violations []Violation
}

// Deletable reports that every guard passed.
func (e Evaluated) Deletable() bool { return len(e.Violations) == 0 }

// Plan is a partitioned, sorted snapshot.
type Plan struct {
	// Eligible passed every guard.
	Eligible []Evaluated
	// HeldBack is past the watch window but held by another guard. Items
	// that simply are not stale are omitted; they would drown the report.
	HeldBack []Evaluated
	// Ignored carry the ignore tag, whatever their other state.
	Ignored []Evaluated
	// Total is the number of items considered.
	Total         int
	EligibleBytes int64
}

// Build evaluates and partitions every candidate in the snapshot.
func Build(snap *library.Snapshot, cr Criteria, now time.Time) Plan {
	var p Plan
	p.Total = len(snap.Candidates)

	for _, c := range snap.Candidates {
		e := Evaluated{Candidate: c, Violations: Evaluate(c, cr, now)}
		switch {
		case e.Deletable():
			p.Eligible = append(p.Eligible, e)
			if r := c.ReclaimableBytes(); r != nil {
				p.EligibleBytes += *r
			}
		case hasGuard(e.Violations, GuardIgnored):
			p.Ignored = append(p.Ignored, e)
		case !hasGuard(e.Violations, GuardUnwatchedFor):
			p.HeldBack = append(p.HeldBack, e)
		}
	}

	for _, list := range [][]Evaluated{p.Eligible, p.HeldBack, p.Ignored} {
		sort.SliceStable(list, func(i, j int) bool { return cr.Sort.less(list[i].Candidate, list[j].Candidate) })
	}

	return p
}

func hasGuard(vs []Violation, name string) bool {
	for _, v := range vs {
		if v.Guard == name {
			return true
		}
	}
	return false
}
