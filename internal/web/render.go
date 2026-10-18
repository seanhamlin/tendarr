package web

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/seanhamlin/tendarr/internal/media"
)

type pageTemplate struct {
	t *template.Template
}

var funcs = template.FuncMap{
	"size": media.FormatBytes,
	// sizeAny formats a byte count decoded from JSON, which arrives as float64.
	"sizeAny": func(v any) string {
		switch n := v.(type) {
		case float64:
			return media.FormatBytes(int64(n))
		case int64:
			return media.FormatBytes(n)
		case int:
			return media.FormatBytes(int64(n))
		}
		return ""
	},
	"sizep": func(v *int64) string {
		if v == nil {
			return "—"
		}
		return media.FormatBytes(*v)
	},
	"date": func(t time.Time) string {
		if t.IsZero() {
			return "—"
		}
		return t.Local().Format("2 Jan 2006")
	},
	"datetime": func(t time.Time) string {
		if t.IsZero() {
			return "—"
		}
		return t.Local().Format("2 Jan 2006 15:04")
	},
	"iso": func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
	"ago": humanAgo,
	// hours estimates how long n checks take at perHour, e.g. "16 hours".
	"hours": func(n, perHour int) string {
		if perHour <= 0 {
			return "—"
		}
		mins := n * 60 / perHour
		switch {
		case mins < 60:
			return plural(max(mins, 1), "minute")
		case mins < 48*60:
			return plural((mins+30)/60, "hour")
		default:
			return plural((mins+12*60)/(24*60), "day")
		}
	},
	"until": func(t time.Time) string {
		d := time.Until(t)
		switch {
		case d <= time.Minute:
			return "within a minute"
		case d < time.Hour:
			return fmt.Sprintf("in %d min", int(d.Minutes()+0.5))
		default:
			return fmt.Sprintf("in %.1f hours", d.Hours())
		}
	},
	"join":  strings.Join,
	"add":   func(a, b int) int { return a + b },
	"lower": strings.ToLower,
	"pct": func(v *float64) string {
		if v == nil {
			return "—"
		}
		return fmt.Sprintf("%.0f%%", *v)
	},
	"pager": func(base string, q url.Values, page, pages int) map[string]any {
		return map[string]any{"Base": base, "Query": q, "Page": page, "Pages": pages}
	},
	"withTab": func(q url.Values, tab string) url.Values {
		cp := url.Values{}
		for k, v := range q {
			cp[k] = append([]string(nil), v...)
		}
		if tab != "" && tab != "eligible" {
			cp.Set("tab", tab)
		}
		return cp
	},
	"withQuery": func(base string, q url.Values, key, value string) string {
		cp := url.Values{}
		for k, v := range q {
			cp[k] = append([]string(nil), v...)
		}
		if value == "" {
			cp.Del(key)
		} else {
			cp.Set(key, value)
		}
		if key != "page" {
			cp.Del("page")
		}
		cp.Del("refresh")
		if enc := cp.Encode(); enc != "" {
			return base + "?" + enc
		}
		return base
	},
}

// humanAgo renders a coarse age such as "3 days ago" or "2.1 years ago".
func humanAgo(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute") + " ago"
	case d < 24*time.Hour:
		return plural(int(d.Hours()), "hour") + " ago"
	case d < 60*24*time.Hour:
		return plural(int(d.Hours()/24), "day") + " ago"
	case d < 730*24*time.Hour:
		return plural(int(d.Hours()/24/30.44), "month") + " ago"
	default:
		return fmt.Sprintf("%.1f years ago", d.Hours()/24/365.25)
	}
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

func parseTemplates() (map[string]pageTemplate, error) {
	entries, err := fs.ReadDir(templateFS, "templates")
	if err != nil {
		return nil, err
	}

	var partials []string
	var pages []string
	for _, e := range entries {
		name := e.Name()
		switch {
		case name == "layout.html" || strings.HasPrefix(name, "_"):
			partials = append(partials, "templates/"+name)
		default:
			pages = append(pages, name)
		}
	}

	out := map[string]pageTemplate{}
	for _, p := range pages {
		files := append(append([]string(nil), partials...), "templates/"+p)
		t, err := template.New(p).Funcs(funcs).ParseFS(templateFS, files...)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", p, err)
		}
		out[strings.TrimSuffix(p, path.Ext(p))] = pageTemplate{t: t}
	}
	return out, nil
}

// view is what every template receives.
type view struct {
	User    string
	CSRF    string
	Flash   *flashMessage
	Nav     string
	Version string
	Data    any
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, page string, data any) {
	pt, ok := s.pages[page]
	if !ok {
		s.serverError(w, r, fmt.Errorf("no template %q", page))
		return
	}

	v := view{Data: data, Nav: navFor(r.URL.Path), Version: s.cfg.Version}
	if sess, ok := currentSession(r); ok {
		v.User, v.CSRF = sess.Username, sess.CSRFToken
	}
	v.Flash = s.takeFlash(w, r)

	var buf bytes.Buffer
	if err := pt.t.ExecuteTemplate(&buf, "layout", v); err != nil {
		s.serverError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

func navFor(p string) string {
	switch {
	case strings.HasPrefix(p, "/upgrade/movies"):
		return "upgrade-movies"
	case strings.HasPrefix(p, "/upgrade/episodes"):
		return "upgrade-episodes"
	case strings.HasPrefix(p, "/reap/movies"):
		return "reap-movies"
	case strings.HasPrefix(p, "/reap/shows"):
		return "reap-shows"
	case strings.HasPrefix(p, "/settings"):
		return "settings"
	case strings.HasPrefix(p, "/activity"):
		return "activity"
	case p == "/":
		return "home"
	}
	return ""
}

const flashCookie = "tendarr_flash"

type flashMessage struct {
	Kind    string `json:"k"`
	Message string `json:"m"`
}

// flash stores a one-shot message shown on the next page.
func (s *Server) flash(w http.ResponseWriter, r *http.Request, kind, msg string) {
	b, _ := json.Marshal(flashMessage{Kind: kind, Message: msg})
	http.SetCookie(w, &http.Cookie{
		Name: flashCookie, Value: base64.RawURLEncoding.EncodeToString(b), Path: "/",
		HttpOnly: true, Secure: s.cookieSecure(r), SameSite: http.SameSiteLaxMode, MaxAge: 60,
	})
}

func (s *Server) takeFlash(w http.ResponseWriter, r *http.Request) *flashMessage {
	c, err := r.Cookie(flashCookie)
	if err != nil {
		return nil
	}
	http.SetCookie(w, &http.Cookie{Name: flashCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.cookieSecure(r), SameSite: http.SameSiteLaxMode})
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		return nil
	}
	var f flashMessage
	if json.Unmarshal(raw, &f) != nil || f.Message == "" {
		return nil
	}
	switch f.Kind {
	case "success", "error", "info":
	default:
		f.Kind = "info"
	}
	return &f
}
