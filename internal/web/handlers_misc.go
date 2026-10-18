package web

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/seanhamlin/tendarr/internal/app"
	"github.com/seanhamlin/tendarr/internal/settings"
	"github.com/seanhamlin/tendarr/internal/store"
)

type dashboardPage struct {
	Services []serviceState
	Recent   []store.AuditEntry
}

type serviceState struct {
	Service    settings.Service
	Configured bool
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	st, err := s.app.Settings(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	var page dashboardPage
	for _, svc := range settings.Services {
		page.Services = append(page.Services, serviceState{Service: svc, Configured: st.Conn(svc).Configured()})
	}
	if page.Recent, err = s.app.RecentActivity(r.Context(), 8); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "dashboard", page)
}

func (s *Server) handleActivity(w http.ResponseWriter, r *http.Request) {
	entries, err := s.app.RecentActivity(r.Context(), 500)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "activity", entries)
}

type serviceForm struct {
	Service settings.Service
	URL     string
	HasKey  bool
	Errors  map[string]string
	Result  string
	OK      bool
}

type settingsPage struct {
	Services       []serviceForm
	PathMap        string
	Excluded       string
	IgnoreTag      string
	LibraryErrors  map[string]string
	PasswordErrors map[string]string
	Statuses       []app.ServiceStatus
	AutoCheck      settings.AutoCheckForm
	AutoErrors     map[string]string
	Scheduler      app.SchedulerStatus
	MaxPerHour     int
	AutoUpgrade    settings.AutoUpgradeForm
	UpgradeErrors  map[string]string
	Upgrader       app.UpgraderStatus
	MaxUpgradeHrs  int

	// autoOverride keeps the submitted scheduler values on error.
	autoOverride *settings.AutoCheckForm
	// upgradeOverride keeps the submitted upgrade schedule on error.
	upgradeOverride *settings.AutoUpgradeForm

	// override replaces one service's form, e.g. to show validation errors.
	override *serviceForm
	// libraryOverride keeps the submitted library values on error.
	libraryOverride *settingsPage
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	s.renderSettings(w, r, http.StatusOK, settingsPage{})
}

func (s *Server) renderSettings(w http.ResponseWriter, r *http.Request, status int, page settingsPage) {
	st, err := s.app.Settings(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	for _, svc := range settings.Services {
		c := st.Conn(svc)
		f := serviceForm{Service: svc, URL: c.URL, HasKey: c.Key != "", Errors: map[string]string{}}
		if page.override != nil && page.override.Service == svc {
			f = *page.override
			f.HasKey = c.Key != ""
		}
		page.Services = append(page.Services, f)
	}

	if page.libraryOverride != nil {
		page.PathMap, page.Excluded, page.IgnoreTag = page.libraryOverride.PathMap, page.libraryOverride.Excluded, page.libraryOverride.IgnoreTag
	} else {
		page.PathMap, page.Excluded, page.IgnoreTag = st.PathMap, strings.Join(st.ExcludedSections, "\n"), st.IgnoreTag
	}
	page.AutoCheck = st.AutoCheckForm
	if page.autoOverride != nil {
		page.AutoCheck = *page.autoOverride
	}
	if page.AutoErrors == nil {
		page.AutoErrors = map[string]string{}
	}
	page.Scheduler = s.app.SchedulerStatus()
	page.MaxPerHour = settings.MaxChecksPerHour
	page.AutoUpgrade = st.AutoUpgradeForm
	if page.upgradeOverride != nil {
		page.AutoUpgrade = *page.upgradeOverride
	}
	if page.UpgradeErrors == nil {
		page.UpgradeErrors = map[string]string{}
	}
	page.Upgrader = s.app.UpgraderStatus()
	page.MaxUpgradeHrs = settings.MaxUpgradeHours

	if page.LibraryErrors == nil {
		page.LibraryErrors = map[string]string{}
	}
	if page.PasswordErrors == nil {
		page.PasswordErrors = map[string]string{}
	}

	s.render(w, r, status, "settings", page)
}

func (s *Server) handleSaveService(w http.ResponseWriter, r *http.Request) {
	svc, err := settings.ParseService(r.PathValue("service"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	sess, _ := currentSession(r)

	st, err := s.app.Settings(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	rawURL := r.PostFormValue("url")
	conn, errs := settings.ValidateConnection(svc, rawURL, r.PostFormValue("key"), st.Conn(svc))
	form := serviceForm{Service: svc, URL: strings.TrimSpace(rawURL), Errors: errs}
	if errs.Any() {
		s.renderSettings(w, r, http.StatusUnprocessableEntity, settingsPage{override: &form})
		return
	}
	form.URL = conn.URL

	// Always prove the connection works before saving. A saved-but-broken
	// connection only surfaces later as an empty or failing scan.
	detail, err := settings.Test(r.Context(), svc, conn)
	if err != nil {
		s.logger.Warn("connection test failed", "service", svc, "error", err)
		form.Errors["test"] = err.Error()
		s.renderSettings(w, r, http.StatusUnprocessableEntity, settingsPage{override: &form})
		return
	}

	if r.PostFormValue("action") == "test" {
		form.Result, form.OK = detail+". Not saved yet.", true
		s.renderSettings(w, r, http.StatusOK, settingsPage{override: &form})
		return
	}

	if err := s.app.SaveConnection(r.Context(), sess.Username, svc, conn); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.flash(w, r, "success", svc.Label()+" saved. "+detail+".")
	http.Redirect(w, r, "/settings#"+string(svc), http.StatusSeeOther)
}

func (s *Server) handleSaveLibrary(w http.ResponseWriter, r *http.Request) {
	sess, _ := currentSession(r)
	pathMap, excluded, tag, errs := settings.ValidateLibrary(r.PostFormValue("path_map"), r.PostFormValue("excluded_sections"), r.PostFormValue("ignore_tag"))
	if errs.Any() {
		s.renderSettings(w, r, http.StatusUnprocessableEntity, settingsPage{
			LibraryErrors: errs,
			libraryOverride: &settingsPage{
				PathMap: r.PostFormValue("path_map"), Excluded: r.PostFormValue("excluded_sections"), IgnoreTag: r.PostFormValue("ignore_tag"),
			},
		})
		return
	}
	if err := s.app.SaveLibrary(r.Context(), sess.Username, pathMap, excluded, tag); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.flash(w, r, "success", "Library settings saved.")
	http.Redirect(w, r, "/settings#library", http.StatusSeeOther)
}

func (s *Server) handleTestAll(w http.ResponseWriter, r *http.Request) {
	statuses, err := s.app.TestAll(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.renderSettings(w, r, http.StatusOK, settingsPage{Statuses: statuses})
}

func (s *Server) handleSaveAutoCheck(w http.ResponseWriter, r *http.Request) {
	sess, _ := currentSession(r)
	form := settings.AutoCheckForm{
		Radarr:         r.PostFormValue("radarr") == "1",
		Sonarr:         r.PostFormValue("sonarr") == "1",
		PerHour:        r.PostFormValue("per_hour"),
		RecheckDays:    r.PostFormValue("recheck_days"),
		Codecs:         r.PostFormValue("codecs"),
		MovieMinSize:   r.PostFormValue("movie_min_size"),
		EpisodeMinSize: r.PostFormValue("episode_min_size"),
	}
	cfg, errs := settings.ValidateAutoCheck(form)

	st, err := s.app.Settings(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if cfg.Radarr && !st.Conn(settings.Radarr).Configured() {
		errs["radarr"] = "connect Radarr first"
	}
	if cfg.Sonarr && !st.Conn(settings.Sonarr).Configured() {
		errs["sonarr"] = "connect Sonarr first"
	}
	if errs.Any() {
		s.renderSettings(w, r, http.StatusUnprocessableEntity, settingsPage{AutoErrors: errs, autoOverride: &form})
		return
	}

	if err := s.app.SaveAutoCheck(r.Context(), sess.Username, form); err != nil {
		s.serverError(w, r, err)
		return
	}
	msg := "Scheduled release checks are off."
	if cfg.Enabled() {
		msg = fmt.Sprintf("Scheduled release checks are on: %d an hour, one every %s.", cfg.PerHour, describeInterval(cfg.Interval()))
	}
	s.flash(w, r, "success", msg)
	http.Redirect(w, r, "/settings#autocheck", http.StatusSeeOther)
}

func (s *Server) handleSaveAutoUpgrade(w http.ResponseWriter, r *http.Request) {
	sess, _ := currentSession(r)
	form := settings.AutoUpgradeForm{
		Radarr:     r.PostFormValue("radarr") == "1",
		Sonarr:     r.PostFormValue("sonarr") == "1",
		EveryHours: r.PostFormValue("every_hours"),
	}
	cfg, errs := settings.ValidateAutoUpgrade(form)

	st, err := s.app.Settings(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if cfg.Radarr && !st.Conn(settings.Radarr).Configured() {
		errs["radarr"] = "connect Radarr first"
	}
	if cfg.Sonarr && !st.Conn(settings.Sonarr).Configured() {
		errs["sonarr"] = "connect Sonarr first"
	}
	if errs.Any() {
		s.renderSettings(w, r, http.StatusUnprocessableEntity, settingsPage{UpgradeErrors: errs, upgradeOverride: &form})
		return
	}

	if err := s.app.SaveAutoUpgrade(r.Context(), sess.Username, form); err != nil {
		s.serverError(w, r, err)
		return
	}
	msg := "Scheduled upgrades are off."
	if cfg.Enabled() {
		msg = fmt.Sprintf("Scheduled upgrades are on: one every %s.", plural(int(cfg.Every/time.Hour), "hour"))
	}
	s.flash(w, r, "success", msg)
	http.Redirect(w, r, "/settings#autoupgrade", http.StatusSeeOther)
}

// describeInterval renders a check interval such as "3 minutes" or "90 seconds".
func describeInterval(d time.Duration) string {
	if d%time.Minute == 0 {
		return plural(int(d/time.Minute), "minute")
	}
	return plural(int(d.Round(time.Second)/time.Second), "second")
}
