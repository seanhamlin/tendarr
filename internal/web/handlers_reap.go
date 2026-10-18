package web

import (
	"net/http"
	"net/url"
	"time"

	"github.com/seanhamlin/tendarr/internal/app"
	"github.com/seanhamlin/tendarr/internal/clients/plex"
	"github.com/seanhamlin/tendarr/internal/library"
	"github.com/seanhamlin/tendarr/internal/reap"
)

type reapKind struct {
	Slug    string
	Label   string
	Service string
	Type    plex.MediaType
}

func reapKindFor(slug string) (reapKind, bool) {
	switch slug {
	case "movies":
		return reapKind{Slug: "movies", Label: "Movies", Service: "Radarr", Type: plex.Movie}, true
	case "shows":
		return reapKind{Slug: "shows", Label: "TV shows", Service: "Sonarr", Type: plex.Show}, true
	}
	return reapKind{}, false
}

type reapPage struct {
	Kind          reapKind
	Filters       reapFilters
	FilterValues  url.Values
	Errors        map[string]string
	Tab           string
	Plan          reap.Plan
	Snapshot      *library.Snapshot
	Rows          []reap.Evaluated
	Page, Pages   int
	Error         string
	NotConfigured bool
	IgnoreTag     string
	Now           time.Time
}

func (s *Server) handleReap(w http.ResponseWriter, r *http.Request) {
	kind, ok := reapKindFor(r.PathValue("type"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	cr, filters, errs := parseReapFilters(q, kind.Type == plex.Movie)

	page := reapPage{Kind: kind, Filters: filters, FilterValues: filters.Values(), Errors: errs, Tab: q.Get("tab"), Now: time.Now()}
	switch page.Tab {
	case "held", "ignored":
	default:
		page.Tab = "eligible"
	}

	if st, err := s.app.Settings(r.Context()); err == nil {
		page.IgnoreTag = st.IgnoreTag
	}

	if len(errs) > 0 {
		s.render(w, r, http.StatusUnprocessableEntity, "reap", page)
		return
	}

	plan, snap, err := s.app.ReapPlan(r.Context(), kind.Type, cr, q.Get("refresh") == "1")
	if err != nil {
		page.Error, page.NotConfigured = err.Error(), app.IsNotConfigured(err)
		s.logger.Warn("reap scan failed", "type", kind.Type, "error", err)
		s.render(w, r, http.StatusOK, "reap", page)
		return
	}
	page.Plan, page.Snapshot = plan, snap

	rows := plan.Eligible
	switch page.Tab {
	case "held":
		rows = plan.HeldBack
	case "ignored":
		rows = plan.Ignored
	}
	start, end, pages, current := paginate(len(rows), parsePage(q))
	page.Rows, page.Pages, page.Page = rows[start:end], pages, current

	s.render(w, r, http.StatusOK, "reap", page)
}

type reapConfirmPage struct {
	Kind             reapKind
	FilterValues     url.Values
	Ready            []reap.Evaluated
	Refused          []reap.Evaluated
	ReclaimableBytes int64
	Keys             []string
	HasContinuing    bool
	ContinuingCount  int
	Snapshot         *library.Snapshot
}

// handleReapConfirm rescans and re-evaluates the selection before anything is
// deleted, so the confirmation reflects the library as it is now rather than
// as it was when the list page was loaded.
func (s *Server) handleReapConfirm(w http.ResponseWriter, r *http.Request) {
	kind, ok := reapKindFor(r.PathValue("type"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	back := "/reap/" + kind.Slug

	cr, filters, errs := parseReapFilters(r.PostForm, kind.Type == plex.Movie)
	if len(errs) > 0 {
		s.flash(w, r, "error", "The filters were invalid; review them and try again.")
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	backWithFilters := back + "?" + filters.Values().Encode()

	keys, err := parseKeys(r.PostForm)
	if err != nil {
		s.flash(w, r, "error", err.Error())
		http.Redirect(w, r, backWithFilters, http.StatusSeeOther)
		return
	}

	_, snap, err := s.app.ReapPlan(r.Context(), kind.Type, cr, true)
	if err != nil {
		s.flash(w, r, "error", "Could not rescan the library: "+err.Error())
		http.Redirect(w, r, backWithFilters, http.StatusSeeOther)
		return
	}
	if snap.Incomplete {
		s.flash(w, r, "error", "The library scan was incomplete, so deletes are disabled. See the warnings on the page.")
		http.Redirect(w, r, backWithFilters, http.StatusSeeOther)
		return
	}

	selected, err := app.SelectReapCandidates(snap, keys)
	if err != nil {
		s.flash(w, r, "error", err.Error())
		http.Redirect(w, r, backWithFilters, http.StatusSeeOther)
		return
	}

	cr.IgnoreTagID = snap.IgnoreTagID
	page := reapConfirmPage{Kind: kind, FilterValues: filters.Values(), Snapshot: snap}
	now := time.Now().UTC()
	for _, c := range selected {
		e := reap.Evaluated{Candidate: c, Violations: reap.Evaluate(c, cr, now)}
		if !e.Deletable() {
			page.Refused = append(page.Refused, e)
			continue
		}
		page.Ready = append(page.Ready, e)
		page.Keys = append(page.Keys, c.RatingKey)
		if b := c.ReclaimableBytes(); b != nil {
			page.ReclaimableBytes += *b
		}
		if c.Arr != nil && c.Arr.IsContinuingSeries() {
			page.ContinuingCount++
		}
	}
	page.HasContinuing = page.ContinuingCount > 0

	s.render(w, r, http.StatusOK, "reap_confirm", page)
}

type reapResultPage struct {
	Kind         reapKind
	FilterValues url.Values
	Result       app.DeleteResult
}

func (s *Server) handleReapDelete(w http.ResponseWriter, r *http.Request) {
	kind, ok := reapKindFor(r.PathValue("type"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	back := "/reap/" + kind.Slug
	sess, _ := currentSession(r)

	cr, filters, errs := parseReapFilters(r.PostForm, kind.Type == plex.Movie)
	if len(errs) > 0 {
		s.flash(w, r, "error", "The filters were invalid; nothing was deleted.")
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	backWithFilters := back + "?" + filters.Values().Encode()

	if r.PostFormValue("confirm") != "yes" {
		s.flash(w, r, "error", "Deletion was not confirmed; nothing was deleted.")
		http.Redirect(w, r, backWithFilters, http.StatusSeeOther)
		return
	}
	keys, err := parseKeys(r.PostForm)
	if err != nil {
		s.flash(w, r, "error", err.Error())
		http.Redirect(w, r, backWithFilters, http.StatusSeeOther)
		return
	}

	opts := reap.Options{
		AddExclusions:     r.PostFormValue("exclusions") == "1",
		ExcludeContinuing: r.PostFormValue("exclude_continuing") == "1",
	}

	result, err := s.app.Delete(r.Context(), sess.Username, kind.Type, keys, cr, opts)
	if err != nil {
		s.flash(w, r, "error", "Nothing was deleted: "+err.Error())
		http.Redirect(w, r, backWithFilters, http.StatusSeeOther)
		return
	}

	s.render(w, r, http.StatusOK, "reap_result", reapResultPage{Kind: kind, FilterValues: filters.Values(), Result: result})
}

func (s *Server) handleReapIgnore(w http.ResponseWriter, r *http.Request) {
	kind, ok := reapKindFor(r.PathValue("type"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	sess, _ := currentSession(r)

	_, filters, _ := parseReapFilters(r.PostForm, kind.Type == plex.Movie)
	v := filters.Values()
	if tab := r.PostFormValue("tab"); tab == "held" || tab == "ignored" {
		v.Set("tab", tab)
	}
	back := "/reap/" + kind.Slug + "?" + v.Encode()

	var ignore bool
	switch r.PostFormValue("action") {
	case "ignore":
		ignore = true
	case "unignore":
		ignore = false
	default:
		s.flash(w, r, "error", "Unknown action.")
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}

	keys, err := parseKeys(r.PostForm)
	if err != nil {
		s.flash(w, r, "error", err.Error())
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}

	n, err := s.app.SetIgnored(r.Context(), sess.Username, kind.Type, keys, ignore)
	if err != nil {
		s.flash(w, r, "error", "Could not update tags: "+err.Error())
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}

	if ignore {
		s.flash(w, r, "success", plural(n, "item")+" tagged in "+kind.Service+" and hidden from reap.")
	} else {
		s.flash(w, r, "success", plural(n, "item")+" un-ignored; they will be evaluated again.")
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}
