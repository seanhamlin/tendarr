package reap

import (
	"context"
	"strings"
	"time"

	"github.com/seanhamlin/tendarr/internal/clients/arr"
	"github.com/seanhamlin/tendarr/internal/library"
)

// Deleter is the subset of the *arr client that removes items.
type Deleter interface {
	Delete(ctx context.Context, id int, addExclusion bool) (arr.DeleteResult, error)
}

// Status is the outcome of one delete.
type Status string

const (
	StatusDeleted     Status = "DELETED"
	StatusAlreadyGone Status = "ALREADY_GONE"
	StatusFailed      Status = "FAILED"
	// StatusRefused means the item failed the pre-delete guard re-check.
	StatusRefused Status = "REFUSED"
)

// Options control side effects of a delete.
type Options struct {
	// AddExclusions stops import lists re-adding deleted items.
	AddExclusions bool
	// ExcludeContinuing also excludes still-airing series. Off by default:
	// an exclusion on a continuing show silently blocks its next season.
	ExcludeContinuing bool
}

// Outcome records what happened to one candidate.
type Outcome struct {
	Candidate      library.Candidate
	Status         Status
	ExclusionAdded bool
	Message        string
}

// ReclaimedBytes is what was actually freed.
func (o Outcome) ReclaimedBytes() int64 {
	if o.Status != StatusDeleted {
		return 0
	}
	if r := o.Candidate.ReclaimableBytes(); r != nil {
		return *r
	}
	return 0
}

// ShouldExclude applies the continuing-series carve-out.
func ShouldExclude(c library.Candidate, opts Options) bool {
	if !opts.AddExclusions {
		return false
	}
	if c.Arr != nil && c.Arr.IsContinuingSeries() {
		return opts.ExcludeContinuing
	}
	return true
}

// Execute deletes each candidate in order. Failures are collected, never
// fatal: one timeout must not strand the rest of the selection.
func Execute(ctx context.Context, client Deleter, candidates []library.Candidate, cr Criteria, opts Options, now time.Time) []Outcome {
	out := make([]Outcome, 0, len(candidates))

	for _, c := range candidates {
		// Belt and braces: the planner already filtered on these guards.
		if vs := Evaluate(c, cr, now); len(vs) > 0 {
			reasons := make([]string, 0, len(vs))
			for _, v := range vs {
				reasons = append(reasons, v.Guard+": "+v.Reason)
			}
			out = append(out, Outcome{Candidate: c, Status: StatusRefused, Message: strings.Join(reasons, "; ")})
			continue
		}

		if err := ctx.Err(); err != nil {
			out = append(out, Outcome{Candidate: c, Status: StatusFailed, Message: "cancelled before this item was reached"})
			continue
		}

		exclude := ShouldExclude(c, opts)
		result, err := client.Delete(ctx, c.Arr.Item.ID, exclude)
		if err != nil {
			out = append(out, Outcome{Candidate: c, Status: StatusFailed, Message: err.Error()})
			continue
		}

		o := Outcome{Candidate: c, Status: StatusDeleted, ExclusionAdded: exclude}
		if result == arr.AlreadyGone {
			o.Status = StatusAlreadyGone
			o.Message = "record was already absent"
		}
		out = append(out, o)
	}

	return out
}
