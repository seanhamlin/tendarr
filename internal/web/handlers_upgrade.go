package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/seanhamlin/tendarr/internal/app"
	"github.com/seanhamlin/tendarr/internal/clients/arr"
	"github.com/seanhamlin/tendarr/internal/store"
	"github.com/seanhamlin/tendarr/internal/upgrade"
)

type upgradeKind struct {
	Slug     string
	Label    string
	Service  string
	Kind     arr.Kind
	Episodes bool
}

func upgradeKindFor(slug string) (upgradeKind, bool) {
	switch slug {
	case "movies":
		return upgradeKind{Slug: "movies", Label: "Movies", Service: "Radarr", Kind: arr.Radarr}, true
	case "episodes":
		return upgradeKind{Slug: "episodes", Label: "TV episodes", Service: "Sonarr", Kind: arr.Sonarr, Episodes: true}, true
	}
	return upgradeKind{}, false
}

type upgradeRow struct {
	upgrade.Candidate
	Check *store.ReleaseCheck
}

type upgradePage struct {
	Kind          upgradeKind
	Filters       upgradeFilters
	FilterValues  url.Values
	PageValues    url.Values
	Errors        map[string]string
	Tab           string
	Only          string
	Plan          upgrade.Plan
	ScannedAt     time.Time
	Rows          []upgradeRow
	Total         int
	AvailableN    int
	UncheckedN    int
	Page, Pages   int
	Error         string
	NotConfigured bool
	Job           *app.Job
	Scheduler     app.SchedulerStatus
	Upgrader      app.UpgraderStatus
	MaxCheck      int
	MaxSearch     int
}

func (s *Server) handleUpgrade(w http.ResponseWriter, r *http.Request) {
	kind, ok := upgradeKindFor(r.PathValue("type"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	req, filters, errs := parseUpgradeFilters(q, kind.Episodes)

	page := upgradePage{
		Kind: kind, Filters: filters, FilterValues: filters.Values(), Errors: errs,
		Tab: q.Get("tab"), Only: q.Get("only"),
		MaxCheck: app.MaxReleaseChecks, MaxSearch: app.MaxSearch,
	}
	if page.Tab != "skipped" {
		page.Tab = "eligible"
	}
	// "only" narrows the candidate list by release-check result. Anything
	// unrecognised, or any value on the Skipped tab, means no narrowing.
	if (page.Only != "available" && page.Only != "unchecked") || page.Tab == "skipped" {
		page.Only = ""
	}
	page.PageValues = filters.Values()
	page.PageValues.Set("tab", page.Tab)
	if page.Only != "" {
		page.PageValues.Set("only", page.Only)
	}
	if j, ok := s.app.JobStatus(kind.Kind); ok {
		page.Job = &j
	}
	page.Scheduler = s.app.SchedulerStatus()
	page.Upgrader = s.app.UpgraderStatus()

	if len(errs) > 0 {
		s.render(w, r, http.StatusUnprocessableEntity, "upgrade", page)
		return
	}

	plan, at, err := s.app.UpgradePlan(r.Context(), kind.Kind, req, q.Get("refresh") == "1")
	if err != nil {
		page.Error, page.NotConfigured = err.Error(), app.IsNotConfigured(err)
		s.logger.Warn("upgrade scan failed", "service", kind.Kind, "error", err)
		s.render(w, r, http.StatusOK, "upgrade", page)
		return
	}
	page.Plan, page.ScannedAt = plan, at

	checks, err := s.app.ReleaseChecks(r.Context(), kind.Kind, plan)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	source := plan.Eligible
	if page.Tab == "skipped" {
		source = plan.Ineligible
	}
	var rows []upgradeRow
	for _, c := range source {
		row := upgradeRow{Candidate: c}
		if rc, ok := checks[c.Key]; ok {
			row.Check = &rc
			if rc.Available && rc.Error == "" && page.Tab == "eligible" {
				page.AvailableN++
			}
		} else if page.Tab == "eligible" {
			page.UncheckedN++
		}
		if page.Only == "available" && (row.Check == nil || !row.Check.Available || row.Check.Error != "") {
			continue
		}
		if page.Only == "unchecked" && row.Check != nil {
			continue
		}
		rows = append(rows, row)
	}
	page.Total = len(rows)
	start, end, pages, current := paginate(len(rows), parsePage(q))
	page.Rows, page.Pages, page.Page = rows[start:end], pages, current

	s.render(w, r, http.StatusOK, "upgrade", page)
}

func (s *Server) upgradeAction(w http.ResponseWriter, r *http.Request, act func(kind upgradeKind, req upgrade.Request, keys []int, user string) (string, error)) {
	kind, ok := upgradeKindFor(r.PathValue("type"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	sess, _ := currentSession(r)

	req, filters, errs := parseUpgradeFilters(r.PostForm, kind.Episodes)
	v := filters.Values()
	if only := r.PostFormValue("only"); only == "available" || only == "unchecked" {
		v.Set("only", only)
	}
	if r.PostFormValue("tab") == "skipped" {
		v.Set("tab", "skipped")
	}
	back := "/upgrade/" + kind.Slug + "?" + v.Encode()
	if len(errs) > 0 {
		s.flash(w, r, "error", "The filters were invalid; nothing was done.")
		http.Redirect(w, r, "/upgrade/"+kind.Slug, http.StatusSeeOther)
		return
	}

	keys, err := parseIntKeys(r.PostForm)
	if err != nil {
		s.flash(w, r, "error", err.Error())
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}

	msg, err := act(kind, req, keys, sess.Username)
	if err != nil {
		s.flash(w, r, "error", err.Error())
	} else {
		s.flash(w, r, "success", msg)
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

func (s *Server) handleUpgradeSearch(w http.ResponseWriter, r *http.Request) {
	s.upgradeAction(w, r, func(kind upgradeKind, req upgrade.Request, keys []int, user string) (string, error) {
		commandID, n, err := s.app.Search(r.Context(), user, kind.Kind, req, keys)
		if err != nil {
			return "", fmt.Errorf("search was not queued: %w", err)
		}
		return fmt.Sprintf("%s is searching for upgrades to %s (command %d). It will grab anything that beats the current file.", kind.Service, plural(n, "item"), commandID), nil
	})
}

func (s *Server) handleUpgradeCheck(w http.ResponseWriter, r *http.Request) {
	s.upgradeAction(w, r, func(kind upgradeKind, req upgrade.Request, keys []int, user string) (string, error) {
		job, err := s.app.StartReleaseCheck(r.Context(), user, kind.Kind, req, keys)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Checking releases for %s in the background. Results appear as each one finishes.", plural(job.Total, "item")), nil
	})
}

func (s *Server) handleUpgradeMonitor(w http.ResponseWriter, r *http.Request) {
	s.upgradeAction(w, r, func(kind upgradeKind, req upgrade.Request, keys []int, user string) (string, error) {
		var monitored bool
		switch r.PostFormValue("action") {
		case "monitor":
			monitored = true
		case "unmonitor":
		default:
			return "", fmt.Errorf("unknown action")
		}
		n, err := s.app.SetUpgradeMonitored(r.Context(), user, kind.Kind, req, keys, monitored)
		if err != nil {
			return "", fmt.Errorf("monitoring was not changed: %w", err)
		}
		if monitored {
			return fmt.Sprintf("%s monitored again in %s; they will be re-evaluated.", plural(n, "item"), kind.Service), nil
		}
		return fmt.Sprintf("%s unmonitored in %s and moved to Skipped. %s will not upgrade them.", plural(n, "item"), kind.Service, kind.Service), nil
	})
}

func (s *Server) handleUpgradeJob(w http.ResponseWriter, r *http.Request) {
	kind, ok := upgradeKindFor(r.PathValue("type"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	resp := map[string]any{"exists": false}
	if j, ok := s.app.JobStatus(kind.Kind); ok {
		resp = map[string]any{
			"exists":    true,
			"id":        j.ID,
			"running":   j.Running(),
			"total":     j.Total,
			"done":      j.Done,
			"available": j.Available,
			"failed":    j.Failed,
			"current":   j.Current,
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
