package settings

import (
	"context"
	"strings"
	"testing"

	"github.com/seanhamlin/tendarr/internal/testsvc"
)

func TestValidateConnection(t *testing.T) {
	existing := Connection{URL: "http://old:7878", Key: "existingkey123"}

	c, errs := ValidateConnection(Radarr, " http://nas:7878/ ", "", existing)
	if errs.Any() {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if c.URL != "http://nas:7878" || c.Key != "existingkey123" {
		t.Errorf("a blank key keeps the saved one, URL is normalised: %+v", c)
	}

	cases := []struct {
		url, key string
		field    string
	}{
		{"nas:7878", "abcdefgh12", "url"},
		{"http://nas", "", "key"},
		{"http://nas", "short", "key"},
		{"http://nas", "has spaces in it", "key"},
		{"http://nas", "quote\"injection1", "key"},
		{"http://nas", strings.Repeat("a", 201), "key"},
	}
	for _, c := range cases {
		_, errs := ValidateConnection(Radarr, c.url, c.key, Connection{})
		if _, ok := errs[c.field]; !ok {
			t.Errorf("url=%q key=%q: expected an error on %s, got %v", c.url, c.key, c.field, errs)
		}
	}
}

func TestValidateLibrary(t *testing.T) {
	pm, sections, tag, errs := ValidateLibrary(" /movies:/mnt/media/movies \n\n/tv:/mnt/media/tv", "UHD Movies\n\n  UHD TV ", " Keep-Me ")
	if errs.Any() {
		t.Fatalf("unexpected: %v", errs)
	}
	if pm != "/movies:/mnt/media/movies\n/tv:/mnt/media/tv" {
		t.Errorf("path map not normalised: %q", pm)
	}
	if len(sections) != 2 || sections[1] != "UHD TV" {
		t.Errorf("sections: %q", sections)
	}
	if tag != "keep-me" {
		t.Errorf("tag should be lower-cased: %q", tag)
	}

	for _, bad := range []string{"", "has space", "under_score", "émoji", strings.Repeat("a", 65)} {
		if _, _, _, errs := ValidateLibrary("", "", bad); errs["ignore_tag"] == "" {
			t.Errorf("tag %q should be rejected", bad)
		}
	}
	if _, _, _, errs := ValidateLibrary("relative:/x", "", "ok"); errs["path_map"] == "" {
		t.Error("relative path map should be rejected")
	}
}

func TestFromMapDefaults(t *testing.T) {
	s := FromMap(map[string]string{"radarr.url": "http://r", "radarr.key": "k", "library.excluded_sections": "A\nB"})
	if !s.Conn(Radarr).Configured() || s.Conn(Sonarr).Configured() {
		t.Error("configured flags wrong")
	}
	if s.IgnoreTag != DefaultIgnoreTag || len(s.ExcludedSections) != 2 {
		t.Errorf("%+v", s)
	}
}

func TestLiveConnectionTests(t *testing.T) {
	radarr := testsvc.NewArr(t, "Radarr")
	sonarr := testsvc.NewArr(t, "Sonarr")
	plex := testsvc.NewPlex(t)
	tautulli := testsvc.NewTautulli(t)
	ctx := context.Background()

	ok := map[Service]string{Radarr: radarr.URL, Sonarr: sonarr.URL, Plex: plex.URL, Tautulli: tautulli.URL}
	for svc, u := range ok {
		detail, err := Test(ctx, svc, Connection{URL: u, Key: testsvc.Key})
		if err != nil || !strings.HasPrefix(detail, "Connected to") {
			t.Errorf("%s: %q %v", svc, detail, err)
		}
		if _, err := Test(ctx, svc, Connection{URL: u, Key: "wrongwrongwrong"}); err == nil {
			t.Errorf("%s: a wrong key must fail", svc)
		}
	}

	if _, err := Test(ctx, Radarr, Connection{URL: sonarr.URL, Key: testsvc.Key}); err == nil {
		t.Error("Sonarr's URL in the Radarr field must fail")
	}
	if _, err := Test(ctx, Plex, Connection{URL: "http://127.0.0.1:1", Key: testsvc.Key}); err == nil {
		t.Error("an unreachable host must fail")
	}
}

func TestValidateAutoCheck(t *testing.T) {
	f := DefaultAutoCheckForm()
	f.Radarr = true
	ac, errs := ValidateAutoCheck(f)
	if errs.Any() {
		t.Fatalf("defaults should validate: %v", errs)
	}
	if !ac.Enabled() || ac.PerHour != 20 || ac.Interval().Minutes() != 3 || ac.RecheckAfter.Hours() != 7*24 {
		t.Errorf("parsed: %+v", ac)
	}

	bad := map[string]func(*AutoCheckForm){
		"per_hour":         func(f *AutoCheckForm) { f.PerHour = "0" },
		"recheck_days":     func(f *AutoCheckForm) { f.RecheckDays = "0" },
		"codecs":           func(f *AutoCheckForm) { f.Codecs = "everything" },
		"movie_min_size":   func(f *AutoCheckForm) { f.MovieMinSize = "1K" },
		"episode_min_size": func(f *AutoCheckForm) { f.EpisodeMinSize = "lots" },
	}
	for field, mutate := range bad {
		f := DefaultAutoCheckForm()
		mutate(&f)
		if _, errs := ValidateAutoCheck(f); errs[field] == "" {
			t.Errorf("%s: expected an error, got %v", field, errs)
		}
	}
	tooFast := DefaultAutoCheckForm()
	tooFast.PerHour = "1000"
	if _, errs := ValidateAutoCheck(tooFast); errs["per_hour"] == "" {
		t.Error("an indexer-hammering rate must be refused")
	}

	// Corrupt stored values disable the scheduler rather than guessing.
	s := FromMap(map[string]string{"autocheck.radarr": "1", "autocheck.per_hour": "banana"})
	if s.AutoCheck().Enabled() {
		t.Error("unreadable stored settings must leave the scheduler off")
	}
	if FromMap(nil).AutoCheck().Enabled() {
		t.Error("the scheduler must be off by default")
	}
}
