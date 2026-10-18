// Package settings holds the admin-configurable connection details and
// library options, with server-side validation and live connection tests.
package settings

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/seanhamlin/tendarr/internal/clients"
	"github.com/seanhamlin/tendarr/internal/clients/arr"
	"github.com/seanhamlin/tendarr/internal/clients/plex"
	"github.com/seanhamlin/tendarr/internal/clients/tautulli"
	"github.com/seanhamlin/tendarr/internal/library"
)

// Service names a connected application.
type Service string

const (
	Plex     Service = "plex"
	Tautulli Service = "tautulli"
	Radarr   Service = "radarr"
	Sonarr   Service = "sonarr"
)

// Services lists every service in display order.
var Services = []Service{Plex, Tautulli, Radarr, Sonarr}

// ParseService validates a service name from a URL or form.
func ParseService(s string) (Service, error) {
	for _, svc := range Services {
		if string(svc) == s {
			return svc, nil
		}
	}
	return "", fmt.Errorf("unknown service %q", s)
}

// Label is the display name.
func (s Service) Label() string {
	switch s {
	case Plex:
		return "Plex"
	case Tautulli:
		return "Tautulli"
	case Radarr:
		return "Radarr"
	case Sonarr:
		return "Sonarr"
	}
	return string(s)
}

// KeyLabel names the credential field.
func (s Service) KeyLabel() string {
	if s == Plex {
		return "Token"
	}
	return "API key"
}

// KeyHelp tells the admin where to find the credential.
func (s Service) KeyHelp() string {
	switch s {
	case Plex:
		return "Open any item in Plex Web, choose Get Info → View XML; the token is the X-Plex-Token value in the URL."
	case Tautulli:
		return "Tautulli → Settings → Web Interface → API → API Key."
	default:
		return s.Label() + " → Settings → General → Security → API Key."
	}
}

// URLPlaceholder suggests a typical address.
func (s Service) URLPlaceholder() string {
	switch s {
	case Plex:
		return "http://192.168.1.10:32400"
	case Tautulli:
		return "http://192.168.1.10:8181"
	case Radarr:
		return "http://192.168.1.10:7878"
	default:
		return "http://192.168.1.10:8989"
	}
}

// Required reports whether a feature cannot work without this service.
func (s Service) Required() bool { return s != Tautulli }

// Connection is one service's address and credential.
type Connection struct {
	URL string
	Key string
}

// Configured reports whether both fields are set.
func (c Connection) Configured() bool { return c.URL != "" && c.Key != "" }

// DefaultIgnoreTag is the *arr tag that hides an item from reap.
const DefaultIgnoreTag = "tendarr-ignore"

// Settings is everything an admin can configure.
type Settings struct {
	Connections map[Service]Connection
	// PathMap translates *arr paths to Plex paths, one per line.
	PathMap string
	// ExcludedSections are Plex libraries never read for reap.
	ExcludedSections []string
	// IgnoreTag hides tagged items from reap.
	IgnoreTag string
	// AutoCheckForm is the stored scheduler configuration as entered.
	AutoCheckForm AutoCheckForm
	// AutoUpgradeForm is the stored upgrade schedule as entered.
	AutoUpgradeForm AutoUpgradeForm
}

// AutoCheck returns the parsed scheduler configuration. Stored values were
// validated on save; anything unreadable disables the scheduler rather than
// running it with guessed limits.
func (s Settings) AutoCheck() AutoCheck {
	ac, errs := ValidateAutoCheck(s.AutoCheckForm)
	if errs.Any() {
		return AutoCheck{}
	}
	return ac
}

// AutoUpgrade returns the parsed upgrade schedule; like AutoCheck, anything
// unreadable disables it.
func (s Settings) AutoUpgrade() AutoUpgrade {
	au, errs := ValidateAutoUpgrade(s.AutoUpgradeForm)
	if errs.Any() {
		return AutoUpgrade{}
	}
	return au
}

// Conn returns one service's connection.
func (s Settings) Conn(svc Service) Connection { return s.Connections[svc] }

// PathMappings parses PathMap; it was validated on save.
func (s Settings) PathMappings() []library.PathMapping {
	m, _ := library.ParsePathMap(s.PathMap)
	return m
}

func urlKey(s Service) string    { return string(s) + ".url" }
func secretKey(s Service) string { return string(s) + ".key" }

const (
	keyPathMap   = "library.path_map"
	keyExcluded  = "library.excluded_sections"
	keyIgnoreTag = "library.ignore_tag"
)

// FromMap decodes stored key/value pairs.
func FromMap(m map[string]string) Settings {
	s := Settings{Connections: map[Service]Connection{}}
	for _, svc := range Services {
		s.Connections[svc] = Connection{URL: m[urlKey(svc)], Key: m[secretKey(svc)]}
	}
	s.PathMap = m[keyPathMap]
	s.ExcludedSections = splitLines(m[keyExcluded])
	s.IgnoreTag = m[keyIgnoreTag]
	if s.IgnoreTag == "" {
		s.IgnoreTag = DefaultIgnoreTag
	}
	s.AutoCheckForm = autoCheckFormFromMap(m)
	s.AutoUpgradeForm = autoUpgradeFormFromMap(m)
	return s
}

// ConnectionValues encodes one connection for storage.
func ConnectionValues(svc Service, c Connection) map[string]string {
	return map[string]string{urlKey(svc): c.URL, secretKey(svc): c.Key}
}

// LibraryValues encodes the library options for storage.
func LibraryValues(pathMap string, excluded []string, ignoreTag string) map[string]string {
	return map[string]string{
		keyPathMap:   pathMap,
		keyExcluded:  strings.Join(excluded, "\n"),
		keyIgnoreTag: ignoreTag,
	}
}

func splitLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// Errors maps form field names to messages.
type Errors map[string]string

// Any reports whether there is at least one error.
func (e Errors) Any() bool { return len(e) > 0 }

var keyPattern = regexp.MustCompile(`^[A-Za-z0-9._~-]+$`)

// ValidateConnection checks the shape of a URL and credential before any
// network call. existing is the stored connection; an empty key in the form
// means "keep the saved one", so the secret never has to be sent back to the
// browser.
func ValidateConnection(svc Service, rawURL, key string, existing Connection) (Connection, Errors) {
	errs := Errors{}
	c := Connection{URL: strings.TrimSpace(rawURL), Key: strings.TrimSpace(key)}

	if u, err := clients.ParseServiceURL(c.URL); err != nil {
		errs["url"] = err.Error()
	} else {
		c.URL = u.String()
	}

	if c.Key == "" {
		c.Key = existing.Key
	}
	switch {
	case c.Key == "":
		errs["key"] = svc.KeyLabel() + " is required"
	case len(c.Key) < 8 || len(c.Key) > 200:
		errs["key"] = svc.KeyLabel() + " must be between 8 and 200 characters"
	case !keyPattern.MatchString(c.Key):
		errs["key"] = svc.KeyLabel() + " contains characters that never appear in a real key"
	}

	return c, errs
}

var tagPattern = regexp.MustCompile(`^[a-z0-9-]+$`)

// ValidateLibrary checks the library options.
func ValidateLibrary(pathMap, excluded, ignoreTag string) (string, []string, string, Errors) {
	errs := Errors{}

	pathMap = strings.TrimSpace(pathMap)
	mappings, err := library.ParsePathMap(pathMap)
	if err != nil {
		errs["path_map"] = err.Error()
	} else {
		lines := make([]string, 0, len(mappings))
		for _, m := range mappings {
			lines = append(lines, m.Arr+":"+m.Plex)
		}
		pathMap = strings.Join(lines, "\n")
	}

	sections := splitLines(excluded)
	for _, s := range sections {
		if len(s) > 200 {
			errs["excluded_sections"] = "section names must be under 200 characters"
		}
	}
	if len(sections) > 100 {
		errs["excluded_sections"] = "too many sections"
	}

	ignoreTag = strings.ToLower(strings.TrimSpace(ignoreTag))
	switch {
	case ignoreTag == "":
		errs["ignore_tag"] = "ignore tag is required"
	case len(ignoreTag) > 64:
		errs["ignore_tag"] = "ignore tag must be 64 characters or fewer"
	case !tagPattern.MatchString(ignoreTag):
		errs["ignore_tag"] = "Radarr and Sonarr only allow lowercase letters, digits and hyphens in tags"
	}

	return pathMap, sections, ignoreTag, errs
}

// Test makes a live, read-only call proving the address and credential work.
// It returns a short description of what answered.
func Test(ctx context.Context, svc Service, c Connection) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	switch svc {
	case Plex:
		cl, err := plex.New(c.URL, c.Key)
		if err != nil {
			return "", err
		}
		id, err := cl.Identity(ctx)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Connected to %s (Plex %s)", fallback(id.Name, "Plex Media Server"), id.Version), nil

	case Tautulli:
		cl, err := tautulli.New(c.URL, c.Key)
		if err != nil {
			return "", err
		}
		info, err := cl.Info(ctx)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Connected to Tautulli %s", info.Version), nil

	case Radarr, Sonarr:
		kind := arr.Radarr
		if svc == Sonarr {
			kind = arr.Sonarr
		}
		cl, err := arr.New(kind, c.URL, c.Key)
		if err != nil {
			return "", err
		}
		st, err := cl.SystemStatus(ctx)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Connected to %s %s", fallback(st.AppName, svc.Label()), st.Version), nil
	}

	return "", fmt.Errorf("unknown service %q", svc)
}

func fallback(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
