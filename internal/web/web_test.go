package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/seanhamlin/tendarr/internal/app"
	"github.com/seanhamlin/tendarr/internal/clients/arr"
	"github.com/seanhamlin/tendarr/internal/settings"
	"github.com/seanhamlin/tendarr/internal/store"
	"github.com/seanhamlin/tendarr/internal/testsvc"
)

const password = "correct-horse-battery"

func init() { bcryptCost = 4 }

type harness struct {
	t        *testing.T
	srv      *httptest.Server
	client   *http.Client
	app      *app.App
	store    *store.Store
	plex     *testsvc.Plex
	tautulli *testsvc.Tautulli
	radarr   *testsvc.Arr
	sonarr   *testsvc.Arr
}

func ago(years, months, days int) int64 {
	return time.Now().AddDate(-years, -months, -days).Unix()
}

// newHarness starts the app against fake services. When withUser is set an
// admin account exists and the client is signed in; when withServices is set
// every service is configured.
func newHarness(t *testing.T, withUser, withServices bool) *harness {
	t.Helper()
	h := &harness{t: t}

	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h.store = st

	h.plex = testsvc.NewPlex(t)
	h.tautulli = testsvc.NewTautulli(t)
	h.radarr = testsvc.NewArr(t, "Radarr")
	h.sonarr = testsvc.NewArr(t, "Sonarr")
	h.seedLibrary()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h.app = app.New(st, logger)
	t.Cleanup(h.app.Shutdown)

	srv, err := New(h.app, Config{CookieSecure: "false", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	h.srv = httptest.NewServer(srv)
	t.Cleanup(h.srv.Close)

	jar, _ := cookiejar.New(nil)
	h.client = &http.Client{
		Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	if withServices {
		for svc, u := range map[settings.Service]string{
			settings.Plex: h.plex.URL, settings.Tautulli: h.tautulli.URL,
			settings.Radarr: h.radarr.URL, settings.Sonarr: h.sonarr.URL,
		} {
			if err := st.SaveSettings(context.Background(), settings.ConnectionValues(svc, settings.Connection{URL: u, Key: testsvc.Key})); err != nil {
				t.Fatal(err)
			}
		}
	}

	if withUser {
		hash, _ := HashPassword(password)
		if _, err := st.CreateFirstUser(context.Background(), "admin", hash); err != nil {
			t.Fatal(err)
		}
		resp := h.post("/login", url.Values{"username": {"admin"}, "password": {password}})
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("login failed: %d", resp.StatusCode)
		}
	}
	return h
}

func (h *harness) seedLibrary() {
	h.plex.Sections = []map[string]any{
		{"key": "1", "title": "Movies", "type": "movie", "Location": []map[string]any{{"path": "/mnt/media/movies"}}},
		{"key": "2", "title": "TV", "type": "show", "Location": []map[string]any{{"path": "/mnt/media/tv"}}},
	}
	movie := func(key, title string, tmdb int, lastViewed int64, size int64) map[string]any {
		m := map[string]any{
			"ratingKey": key, "title": title, "year": 2010, "addedAt": ago(4, 0, 0),
			"Guid":  []map[string]any{{"id": "tmdb://" + strconv.Itoa(tmdb)}},
			"Media": []map[string]any{{"Part": []map[string]any{{"file": "/mnt/media/movies/" + title + "/" + title + ".mkv", "size": size}}}},
		}
		if lastViewed > 0 {
			m["lastViewedAt"] = lastViewed
		}
		return m
	}
	h.plex.Items["1"] = []map[string]any{
		movie("1001", "Old Big", 1, ago(3, 0, 0), 20<<30),
		movie("1002", "Old Small", 2, 0, 5<<30),
		movie("1003", "Recent", 3, ago(3, 0, 0), 8<<30),
		movie("1004", "Kept", 4, ago(3, 0, 0), 9<<30),
		movie("1005", "Orphan", 5, ago(3, 0, 0), 7<<30),
	}
	// Plex thinks Recent is stale, but another user watched it last week.
	h.tautulli.Records["1"] = []map[string]any{{"rating_key": "1003", "last_played": ago(0, 0, 7), "play_count": 1}}

	radarrMovie := func(id int, title string, tmdb int, hasFile bool, size int64, tags []int) map[string]any {
		return map[string]any{
			"id": id, "title": title, "year": 2010, "tmdbId": tmdb, "hasFile": hasFile, "sizeOnDisk": size,
			"monitored": true, "path": "/movies/" + title, "rootFolderPath": "/movies", "qualityProfileId": 1,
			"added": time.Now().AddDate(-4, 0, 0).Format(time.RFC3339), "tags": tags,
			"movieFile": map[string]any{"relativePath": title + ".mkv"},
		}
	}
	h.radarr.Items = []map[string]any{
		radarrMovie(101, "Old Big", 1, true, 20<<30, []int{}),
		radarrMovie(102, "Old Small", 2, true, 5<<30, []int{}),
		radarrMovie(103, "Recent", 3, true, 8<<30, []int{}),
		radarrMovie(104, "Kept", 4, true, 9<<30, []int{7}),
		radarrMovie(105, "Orphan", 5, false, 0, []int{}),
	}
	// Mediocre, well-voted ratings: below the default 7.5 floor, so the
	// fixtures are reapable unless a test says otherwise.
	for _, it := range h.radarr.Items {
		it["ratings"] = map[string]any{"imdb": map[string]any{"value": 6.0, "votes": 5000}}
	}
	h.radarr.Tags = []map[string]any{{"id": 7, "label": settings.DefaultIgnoreTag}}
	h.radarr.Profiles = []map[string]any{{
		"id": 1, "name": "HD", "upgradeAllowed": true, "cutoff": 3, "cutoffFormatScore": 6,
		"items":       []map[string]any{{"quality": map[string]any{"id": 3, "name": "WEBDL-1080p"}}},
		"formatItems": []map[string]any{{"name": "x265", "score": 4}},
	}}
	file := func(id, movieID int, size int64, codec string) map[string]any {
		return map[string]any{
			"id": id, "movieId": movieID, "size": size, "relativePath": "f.mkv", "customFormatScore": 2,
			"qualityCutoffNotMet": true, "quality": map[string]any{"quality": map[string]any{"name": "WEBDL-1080p"}},
			"mediaInfo": map[string]any{"videoCodec": codec},
		}
	}
	h.radarr.Files = []map[string]any{file(9101, 101, 20<<30, "x264"), file(9102, 102, 5<<30, "h264"), file(9103, 103, 8<<30, "x265")}
	h.radarr.Releases["101"] = []map[string]any{
		{"guid": "g1", "title": "Old.Big.2010.1080p.BluRay.x265", "size": 6 << 30, "customFormatScore": 6, "rejected": false, "quality": map[string]any{"quality": map[string]any{"name": "Bluray-1080p"}}},
	}

	h.plex.Items["2"] = []map[string]any{
		{"ratingKey": "2001", "title": "Ended Show", "year": 2005, "addedAt": ago(5, 0, 0), "lastViewedAt": ago(3, 0, 0), "Guid": []map[string]any{{"id": "tvdb://11"}}},
		{"ratingKey": "2002", "title": "Airing Show", "year": 2015, "addedAt": ago(5, 0, 0), "lastViewedAt": ago(3, 0, 0), "Guid": []map[string]any{{"id": "tvdb://12"}}},
	}
	series := func(id int, title string, tvdb int, status string) map[string]any {
		return map[string]any{
			"id": id, "title": title, "tvdbId": tvdb, "status": status, "monitored": true, "path": "/tv/" + title,
			"rootFolderPath": "/tv", "qualityProfileId": 1, "tags": []int{},
			"statistics": map[string]any{"sizeOnDisk": 30 << 30, "episodeFileCount": 10},
		}
	}
	h.sonarr.Items = []map[string]any{series(201, "Ended Show", 11, "ended"), series(202, "Airing Show", 12, "continuing")}
	h.sonarr.Profiles = h.radarr.Profiles
	h.sonarr.Files = []map[string]any{{"id": 7001, "seriesId": 201, "size": 3 << 30, "customFormatScore": 0, "qualityCutoffNotMet": true, "mediaInfo": map[string]any{"videoCodec": "x264"}}}
	h.sonarr.Episodes = []map[string]any{{"id": 8001, "seriesId": 201, "episodeFileId": 7001, "seasonNumber": 1, "episodeNumber": 1, "monitored": true}}
}

func (h *harness) get(path string) (*http.Response, string) {
	h.t.Helper()
	resp, err := h.client.Get(h.srv.URL + path)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

func (h *harness) post(path string, form url.Values) *http.Response {
	h.t.Helper()
	resp, err := h.client.PostForm(h.srv.URL+path, form)
	if err != nil {
		h.t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func (h *harness) postBody(path string, form url.Values) (*http.Response, string) {
	h.t.Helper()
	resp, err := h.client.PostForm(h.srv.URL+path, form)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

var csrfPattern = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func (h *harness) csrf() string {
	h.t.Helper()
	_, body := h.get("/settings")
	m := csrfPattern.FindStringSubmatch(body)
	if m == nil {
		h.t.Fatal("no csrf token on the settings page")
	}
	return m[1]
}

// flash follows a redirect and returns the page it lands on.
func (h *harness) follow(resp *http.Response) string {
	h.t.Helper()
	loc := resp.Header.Get("Location")
	if loc == "" {
		h.t.Fatalf("expected a redirect, got %d", resp.StatusCode)
	}
	u, _ := url.Parse(loc)
	_, body := h.get(u.RequestURI())
	return body
}

func TestSetupFlow(t *testing.T) {
	h := newHarness(t, false, false)

	resp, _ := h.get("/reap/movies")
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/setup" {
		t.Fatalf("before setup every page should go to /setup, got %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}

	resp, body := h.postBody("/setup", url.Values{"username": {"admin"}, "password": {password}, "confirm": {"different-password"}})
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "Passwords do not match") {
		t.Fatalf("mismatched passwords: %d", resp.StatusCode)
	}
	resp, _ = h.postBody("/setup", url.Values{"username": {"admin"}, "password": {"short"}, "confirm": {"short"}})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("short password accepted: %d", resp.StatusCode)
	}

	resp = h.post("/setup", url.Values{"username": {"admin"}, "password": {password}, "confirm": {password}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/settings" {
		t.Fatalf("setup: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp, _ := h.get("/settings"); resp.StatusCode != http.StatusOK {
		t.Fatalf("setup should sign the new admin in, got %d", resp.StatusCode)
	}

	// Setup is closed for good once an account exists.
	resp = h.post("/setup", url.Values{"username": {"intruder"}, "password": {password}, "confirm": {password}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("second setup: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if _, err := h.store.UserByUsername(context.Background(), "intruder"); err == nil {
		t.Fatal("a second account was created through setup")
	}
}

func TestLoginLogoutAndRateLimit(t *testing.T) {
	h := newHarness(t, true, false)

	resp := h.post("/logout", url.Values{"csrf": {h.csrf()}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("logout: %d", resp.StatusCode)
	}
	resp, _ = h.get("/settings")
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/login?next=") {
		t.Fatalf("after logout: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}

	resp, body := h.postBody("/login", url.Values{"username": {"admin"}, "password": {"wrong-password-here"}})
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(body, "Incorrect username or password") {
		t.Fatalf("wrong password: %d", resp.StatusCode)
	}
	resp, body = h.postBody("/login", url.Values{"username": {"nobody"}, "password": {password}})
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(body, "Incorrect username or password") {
		t.Fatalf("unknown user must look identical to a wrong password: %d", resp.StatusCode)
	}

	resp = h.post("/login", url.Values{"username": {"ADMIN"}, "password": {password}, "next": {"//evil.example/steal"}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("open redirect via next: %s", resp.Header.Get("Location"))
	}

	for i := 0; i < 10; i++ {
		h.post("/login", url.Values{"username": {"admin"}, "password": {"nope-nope-nope"}})
	}
	resp, _ = h.postBody("/login", url.Values{"username": {"admin"}, "password": {password}})
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected rate limiting after repeated failures, got %d", resp.StatusCode)
	}
}

func TestStateChangesNeedCSRFToken(t *testing.T) {
	h := newHarness(t, true, true)
	resp := h.post("/settings/library", url.Values{"ignore_tag": {"x"}})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("missing csrf: %d", resp.StatusCode)
	}
	resp = h.post("/settings/library", url.Values{"ignore_tag": {"x"}, "csrf": {"forged"}})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong csrf: %d", resp.StatusCode)
	}

	// A browser's cross-site POST is refused before it reaches a handler.
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/settings/library", strings.NewReader("csrf="+h.csrf()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, _ = h.client.Do(req)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site post: %d", resp.StatusCode)
	}
}

func TestSecurityHeaders(t *testing.T) {
	h := newHarness(t, true, false)
	resp, _ := h.get("/")
	for _, hdr := range []string{"Content-Security-Policy", "X-Frame-Options", "X-Content-Type-Options"} {
		if resp.Header.Get(hdr) == "" {
			t.Errorf("missing %s", hdr)
		}
	}
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie && !c.HttpOnly {
			t.Error("session cookie must be HttpOnly")
		}
	}
	if resp, body := h.get("/healthz"); resp.StatusCode != http.StatusOK || body != "ok\n" {
		t.Errorf("healthz: %d %q", resp.StatusCode, body)
	}
}

func TestSettingsValidateAndTestBeforeSaving(t *testing.T) {
	h := newHarness(t, true, false)
	csrf := h.csrf()

	resp, body := h.postBody("/settings/services/radarr", url.Values{"csrf": {csrf}, "action": {"save"}, "url": {"not a url"}, "key": {testsvc.Key}})
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "http://") {
		t.Fatalf("bad url: %d", resp.StatusCode)
	}

	resp, body = h.postBody("/settings/services/radarr", url.Values{"csrf": {csrf}, "action": {"save"}, "url": {h.radarr.URL}, "key": {"wrongwrongwrong1"}})
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "Connection failed") {
		t.Fatalf("wrong key must not save: %d", resp.StatusCode)
	}
	if s, _ := h.app.Settings(context.Background()); s.Conn(settings.Radarr).Configured() {
		t.Fatal("a failing connection was saved")
	}

	resp, body = h.postBody("/settings/services/radarr", url.Values{"csrf": {csrf}, "action": {"save"}, "url": {h.sonarr.URL}, "key": {testsvc.Key}})
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "Sonarr instance") {
		t.Fatalf("Sonarr in the Radarr slot: %d", resp.StatusCode)
	}

	resp, body = h.postBody("/settings/services/radarr", url.Values{"csrf": {csrf}, "action": {"test"}, "url": {h.radarr.URL}, "key": {testsvc.Key}})
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Not saved yet") {
		t.Fatalf("test only: %d", resp.StatusCode)
	}
	if s, _ := h.app.Settings(context.Background()); s.Conn(settings.Radarr).Configured() {
		t.Fatal("test must not save")
	}

	resp = h.post("/settings/services/radarr", url.Values{"csrf": {csrf}, "action": {"save"}, "url": {h.radarr.URL + "/"}, "key": {testsvc.Key}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save: %d", resp.StatusCode)
	}
	s, _ := h.app.Settings(context.Background())
	if s.Conn(settings.Radarr).URL != h.radarr.URL || s.Conn(settings.Radarr).Key != testsvc.Key {
		t.Fatalf("saved: %+v", s.Conn(settings.Radarr))
	}

	// The key is never rendered back, and a blank key keeps the saved one.
	_, page := h.get("/settings")
	if strings.Contains(page, testsvc.Key) {
		t.Fatal("API key leaked into the settings page")
	}
	resp = h.post("/settings/services/radarr", url.Values{"csrf": {csrf}, "action": {"save"}, "url": {h.radarr.URL}, "key": {""}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save with blank key: %d", resp.StatusCode)
	}

	if resp := h.post("/settings/services/bogus", url.Values{"csrf": {csrf}}); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown service: %d", resp.StatusCode)
	}

	resp, body = h.postBody("/settings/library", url.Values{"csrf": {csrf}, "ignore_tag": {"Bad Tag!"}, "path_map": {"nonsense"}})
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "lowercase letters") {
		t.Fatalf("library validation: %d", resp.StatusCode)
	}
	resp = h.post("/settings/library", url.Values{"csrf": {csrf}, "ignore_tag": {"keep"}, "path_map": {"/movies:/mnt/media/movies"}, "excluded_sections": {"UHD Movies"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("library save: %d", resp.StatusCode)
	}
}

func TestPasswordChange(t *testing.T) {
	h := newHarness(t, true, false)
	csrf := h.csrf()

	resp, _ := h.postBody("/settings/password", url.Values{"csrf": {csrf}, "current": {"wrong"}, "password": {"a-brand-new-password"}, "confirm": {"a-brand-new-password"}})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("wrong current password: %d", resp.StatusCode)
	}
	resp = h.post("/settings/password", url.Values{"csrf": {csrf}, "current": {password}, "password": {"a-brand-new-password"}, "confirm": {"a-brand-new-password"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("change: %d", resp.StatusCode)
	}
	if resp, _ := h.get("/settings"); resp.StatusCode != http.StatusOK {
		t.Fatal("the session that changed the password should stay signed in")
	}
}

func TestMovieReapListAndTabs(t *testing.T) {
	h := newHarness(t, true, true)

	resp, body := h.get("/reap/movies")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	for _, want := range []string{"Old Big (2010)", "Old Small (2010)"} {
		if !strings.Contains(body, want) {
			t.Errorf("eligible list missing %q", want)
		}
	}
	for _, unwanted := range []string{"Recent (2010)", "Kept (2010)", "Orphan (2010)"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("eligible list should not include %q", unwanted)
		}
	}
	if strings.Index(body, "Old Big") > strings.Index(body, "Old Small") {
		t.Error("largest should be listed first")
	}

	_, held := h.get("/reap/movies?tab=held")
	if !strings.Contains(held, "Orphan (2010)") || !strings.Contains(held, "no file on disk") {
		t.Error("orphaned item should be held back with its reason")
	}
	_, ignored := h.get("/reap/movies?tab=ignored")
	if !strings.Contains(ignored, "Kept (2010)") {
		t.Error("tagged item should be on the ignored tab")
	}

	resp, body = h.get("/reap/movies?unwatched=forever&min_rating=11")
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "not a valid duration") || !strings.Contains(body, "0 to 10") {
		t.Errorf("invalid filters: %d", resp.StatusCode)
	}

	// A tighter window protects everything.
	_, body = h.get("/reap/movies?unwatched=10y")
	if strings.Contains(body, "Old Big (2010)") {
		t.Error("a 10y window should make nothing eligible")
	}
}

func TestMovieReapConfirmAndDelete(t *testing.T) {
	h := newHarness(t, true, true)
	h.get("/reap/movies")
	csrf := h.csrf()

	// Confirm rescans and re-checks: Recent is refused, Old Big is ready.
	resp, body := h.postBody("/reap/movies/confirm", url.Values{"csrf": {csrf}, "key": {"1001", "1003"}, "unwatched": {"2y"}, "min_age": {"90d"}, "min_votes": {"50"}, "sort": {"size"}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("confirm: %d", resp.StatusCode)
	}
	if !strings.Contains(body, "Delete 1 movie?") || !strings.Contains(body, "no longer eligible") || !strings.Contains(body, "Recent (2010)") {
		t.Fatalf("confirm page did not separate ready and refused items:\n%s", body)
	}
	if len(h.radarr.Find(http.MethodDelete, "/api/v3/movie/101")) != 0 {
		t.Fatal("confirm must not delete anything")
	}

	// Without the confirm flag nothing happens.
	h.post("/reap/movies/delete", url.Values{"csrf": {csrf}, "key": {"1001"}})
	if len(h.radarr.Find(http.MethodDelete, "/api/v3/movie/101")) != 0 {
		t.Fatal("delete without confirmation")
	}

	// A key the server has never seen is rejected outright.
	resp = h.post("/reap/movies/delete", url.Values{"csrf": {csrf}, "confirm": {"yes"}, "key": {"1001", "999999"}})
	if !strings.Contains(h.follow(resp), "no longer in the library") {
		t.Error("unknown key should be reported")
	}
	if len(h.radarr.Find(http.MethodDelete, "/api/v3/movie/101")) != 0 {
		t.Fatal("a selection containing an unknown key must delete nothing")
	}

	// Forcing an ineligible item through the delete endpoint is refused by
	// the re-check even though it skipped the confirm page.
	resp, body = h.postBody("/reap/movies/delete", url.Values{"csrf": {csrf}, "confirm": {"yes"}, "key": {"1003"}})
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "REFUSED") {
		t.Fatalf("recently watched item should be refused: %d", resp.StatusCode)
	}
	if len(h.radarr.Find(http.MethodDelete, "/api/v3/movie/103")) != 0 {
		t.Fatal("a recently watched movie was deleted")
	}

	resp, body = h.postBody("/reap/movies/delete", url.Values{"csrf": {csrf}, "confirm": {"yes"}, "exclusions": {"1"}, "key": {"1001"}})
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "DELETED") || !strings.Contains(body, "20.0 GB") {
		t.Fatalf("delete result: %d\n%s", resp.StatusCode, body)
	}
	reqs := h.radarr.Find(http.MethodDelete, "/api/v3/movie/101")
	if len(reqs) != 1 || reqs[0].Query.Get("addImportExclusion") != "true" || reqs[0].Query.Get("deleteFiles") != "true" {
		t.Fatalf("radarr delete: %+v", reqs)
	}

	_, list := h.get("/reap/movies")
	if strings.Contains(list, "Old Big (2010)") {
		t.Error("deleted movie still listed")
	}
	_, activity := h.get("/activity")
	if !strings.Contains(activity, "Old Big (2010)") || !strings.Contains(activity, "DELETED") {
		t.Error("delete not in the activity log")
	}
}

func TestReapIgnoreAndUnignore(t *testing.T) {
	h := newHarness(t, true, true)
	h.get("/reap/movies")
	csrf := h.csrf()

	resp := h.post("/reap/movies/ignore", url.Values{"csrf": {csrf}, "action": {"ignore"}, "key": {"1002"}})
	page := h.follow(resp)
	if !strings.Contains(page, "hidden from reap") || strings.Contains(page, "Old Small (2010)") {
		t.Fatal("ignored item should leave the eligible list")
	}
	puts := h.radarr.Find(http.MethodPut, "/api/v3/movie/editor")
	if len(puts) != 1 || !strings.Contains(puts[0].Body, `"applyTags":"add"`) || !strings.Contains(puts[0].Body, `"tags":[7]`) {
		t.Fatalf("editor call: %+v", puts)
	}

	resp = h.post("/reap/movies/ignore", url.Values{"csrf": {csrf}, "action": {"unignore"}, "key": {"1002"}, "tab": {"ignored"}})
	page = h.follow(resp)
	if strings.Contains(page, "Old Small (2010)") {
		t.Error("un-ignored item should leave the ignored tab")
	}

	resp = h.post("/reap/movies/ignore", url.Values{"csrf": {csrf}, "action": {"explode"}, "key": {"1002"}})
	if !strings.Contains(h.follow(resp), "Unknown action") {
		t.Error("unknown action accepted")
	}
}

func TestShowReapSkipsExclusionForContinuingSeries(t *testing.T) {
	h := newHarness(t, true, true)
	_, body := h.get("/reap/shows")
	if !strings.Contains(body, "Ended Show") || !strings.Contains(body, "Airing Show") || !strings.Contains(body, "Continuing") {
		t.Fatalf("show list:\n%s", body)
	}
	csrf := h.csrf()

	_, confirm := h.postBody("/reap/shows/confirm", url.Values{"csrf": {csrf}, "key": {"2001", "2002"}})
	if !strings.Contains(confirm, "still-airing") {
		t.Error("confirm page should offer the continuing-series exclusion separately")
	}

	h.post("/reap/shows/delete", url.Values{"csrf": {csrf}, "confirm": {"yes"}, "exclusions": {"1"}, "key": {"2001", "2002"}})
	ended := h.sonarr.Find(http.MethodDelete, "/api/v3/series/201")
	airing := h.sonarr.Find(http.MethodDelete, "/api/v3/series/202")
	if len(ended) != 1 || ended[0].Query.Get("addImportListExclusion") != "true" {
		t.Errorf("ended series should be excluded: %+v", ended)
	}
	if len(airing) != 1 || airing[0].Query.Get("addImportListExclusion") != "false" {
		t.Errorf("continuing series must not be excluded by default: %+v", airing)
	}
}

func TestDeletesRefusedWhenScanIsIncomplete(t *testing.T) {
	h := newHarness(t, true, true)
	h.radarr.FailList = true
	_, body := h.get("/reap/movies?refresh=1")
	if !strings.Contains(body, "could not be read") {
		t.Fatal("expected an incomplete-scan warning")
	}
	resp := h.post("/reap/movies/confirm", url.Values{"csrf": {h.csrf()}, "key": {"1001"}})
	if !strings.Contains(h.follow(resp), "incomplete") {
		t.Error("confirm should refuse on an incomplete scan")
	}
}

func TestMovieUpgradeSearchAndReleaseCheck(t *testing.T) {
	h := newHarness(t, true, true)

	resp, body := h.get("/upgrade/movies")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if !strings.Contains(body, ">H.264<") || strings.Contains(body, ">AVC<") {
		t.Error("codecs should be shown by their H.26x names")
	}
	if !strings.Contains(body, "Old Big (2010)") || !strings.Contains(body, "Old Small (2010)") || strings.Contains(body, "Recent (2010)") {
		t.Fatal("upgrade list should hold the two x264 files and not the x265 one")
	}
	csrf := h.csrf()
	filters := url.Values{"csrf": {csrf}, "codecs": {"legacy"}, "min_size": {"4G"}, "sort": {"size"}}

	// Unknown keys are rejected and nothing is searched.
	bad := cloneValues(filters)
	bad["key"] = []string{"999"}
	if !strings.Contains(h.follow(h.post("/upgrade/movies/search", bad)), "not eligible") {
		t.Error("unknown key should be rejected")
	}
	if len(h.radarr.Find(http.MethodPost, "/api/v3/command")) != 0 {
		t.Fatal("search fired for an invalid selection")
	}

	// Release check runs in the background and is recorded.
	check := cloneValues(filters)
	check["key"] = []string{"101", "102"}
	if !strings.Contains(h.follow(h.post("/upgrade/movies/check", check)), "in the background") {
		t.Fatal("release check did not start")
	}
	waitForJob(t, h.app, arr.Radarr)
	_, body = h.get("/upgrade/movies")
	if !strings.Contains(body, "Available · score 6") || !strings.Contains(body, "None acceptable") {
		t.Fatalf("release results not shown:\n%s", body)
	}
	_, onlyAvailable := h.get("/upgrade/movies?only=available")
	if !strings.Contains(onlyAvailable, "Old Big (2010)") || strings.Contains(onlyAvailable, "Old Small (2010)") {
		t.Error("release-found filter wrong")
	}

	search := cloneValues(filters)
	search["key"] = []string{"101"}
	if !strings.Contains(h.follow(h.post("/upgrade/movies/search", search)), "command 4242") {
		t.Fatal("search not confirmed")
	}
	cmds := h.radarr.Find(http.MethodPost, "/api/v3/command")
	if len(cmds) != 1 || !strings.Contains(cmds[0].Body, `"MoviesSearch"`) || !strings.Contains(cmds[0].Body, `"movieIds":[101]`) {
		t.Fatalf("command: %+v", cmds)
	}
	_, skipped := h.get("/upgrade/movies?tab=skipped")
	if !strings.Contains(skipped, "Old Big (2010)") || !strings.Contains(skipped, "QUEUED") {
		t.Error("searched movie should move to skipped as queued")
	}

	// Too many for one check.
	many := cloneValues(filters)
	for i := 0; i < app.MaxReleaseChecks+1; i++ {
		many.Add("key", strconv.Itoa(i+1))
	}
	if !strings.Contains(h.follow(h.post("/upgrade/movies/check", many)), "at most") {
		t.Error("oversized release check accepted")
	}
}

func TestEpisodeUpgrade(t *testing.T) {
	h := newHarness(t, true, true)
	_, body := h.get("/upgrade/episodes")
	if !strings.Contains(body, "Ended Show") || !strings.Contains(body, "S01E01") {
		t.Fatalf("episode list:\n%s", body)
	}
	form := url.Values{"csrf": {h.csrf()}, "codecs": {"legacy"}, "min_size": {"1G"}, "sort": {"size"}, "key": {"7001"}}
	h.post("/upgrade/episodes/search", form)
	cmds := h.sonarr.Find(http.MethodPost, "/api/v3/command")
	if len(cmds) != 1 || !strings.Contains(cmds[0].Body, `"EpisodeSearch"`) || !strings.Contains(cmds[0].Body, `"episodeIds":[8001]`) {
		t.Fatalf("command: %+v", cmds)
	}

	resp, body := h.get("/upgrade/episodes?min_size=1K")
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "at least 100M") {
		t.Errorf("tiny min size should be rejected: %d", resp.StatusCode)
	}
}

func TestPagesRenderWhenNothingIsConfigured(t *testing.T) {
	h := newHarness(t, true, false)
	for _, p := range []string{"/", "/settings", "/activity", "/reap/movies", "/reap/shows", "/upgrade/movies", "/upgrade/episodes"} {
		resp, body := h.get(p)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: %d", p, resp.StatusCode)
		}
		if strings.HasPrefix(p, "/reap") || strings.HasPrefix(p, "/upgrade") {
			if !strings.Contains(body, "Not connected yet") {
				t.Errorf("%s should explain what is missing", p)
			}
		}
	}
	if resp, _ := h.get("/reap/music"); resp.StatusCode != http.StatusNotFound {
		t.Error("unknown reap type should 404")
	}
}

func cloneValues(v url.Values) url.Values {
	out := url.Values{}
	for k, vs := range v {
		out[k] = append([]string(nil), vs...)
	}
	return out
}

func waitForJob(t *testing.T, a *app.App, kind arr.Kind) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if j, ok := a.JobStatus(kind); ok && !j.Running() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("release check did not finish")
}

func TestUnmonitorAndMonitorAgainMovies(t *testing.T) {
	h := newHarness(t, true, true)
	h.get("/upgrade/movies")
	form := url.Values{"csrf": {h.csrf()}, "codecs": {"legacy"}, "min_size": {"4G"}, "sort": {"size"}}

	unmonitor := cloneValues(form)
	unmonitor.Set("action", "unmonitor")
	unmonitor["key"] = []string{"101"}
	page := h.follow(h.post("/upgrade/movies/monitor", unmonitor))
	if !strings.Contains(page, "unmonitored in Radarr") || strings.Contains(page, "Old Big (2010)") {
		t.Fatal("unmonitored movie should leave the upgradeable list")
	}
	puts := h.radarr.Find(http.MethodPut, "/api/v3/movie/editor")
	if len(puts) != 1 || !strings.Contains(puts[0].Body, `"monitored":false`) || !strings.Contains(puts[0].Body, `"movieIds":[101]`) {
		t.Fatalf("editor call: %+v", puts)
	}

	_, skipped := h.get("/upgrade/movies?tab=skipped")
	if !strings.Contains(skipped, "Old Big (2010)") || !strings.Contains(skipped, "NOT_MONITORED") || !strings.Contains(skipped, `value="101"`) {
		t.Fatal("unmonitored movie should be on Skipped with a checkbox to monitor it again")
	}

	// Monitoring something that is not skipped as unmonitored is refused.
	bad := cloneValues(form)
	bad.Set("action", "monitor")
	bad["key"] = []string{"102"}
	if !strings.Contains(h.follow(h.post("/upgrade/movies/monitor", bad)), "not skipped as unmonitored") {
		t.Error("monitoring an eligible item should be refused")
	}

	monitor := cloneValues(form)
	monitor.Set("action", "monitor")
	monitor.Set("tab", "skipped")
	monitor["key"] = []string{"101"}
	resp := h.post("/upgrade/movies/monitor", monitor)
	if !strings.Contains(resp.Header.Get("Location"), "tab=skipped") {
		t.Errorf("should return to the skipped tab: %s", resp.Header.Get("Location"))
	}
	if !strings.Contains(h.follow(resp), "monitored again") {
		t.Fatal("monitor again not confirmed")
	}
	_, list := h.get("/upgrade/movies")
	if !strings.Contains(list, "Old Big (2010)") {
		t.Error("re-monitored movie should be upgradeable again after the rescan")
	}

	_, activity := h.get("/activity")
	if !strings.Contains(activity, "Unmonitor") || !strings.Contains(activity, ">Monitor<") {
		t.Error("monitoring changes should be in the activity log")
	}

	unknown := cloneValues(form)
	unknown.Set("action", "explode")
	unknown["key"] = []string{"101"}
	if !strings.Contains(h.follow(h.post("/upgrade/movies/monitor", unknown)), "unknown action") {
		t.Error("unknown action accepted")
	}
}

func TestUnmonitorEpisodes(t *testing.T) {
	h := newHarness(t, true, true)
	h.get("/upgrade/episodes")
	form := url.Values{"csrf": {h.csrf()}, "codecs": {"legacy"}, "min_size": {"1G"}, "sort": {"size"}, "action": {"unmonitor"}, "key": {"7001"}}
	h.post("/upgrade/episodes/monitor", form)
	puts := h.sonarr.Find(http.MethodPut, "/api/v3/episode/monitor")
	if len(puts) != 1 || !strings.Contains(puts[0].Body, `"episodeIds":[8001]`) || !strings.Contains(puts[0].Body, `"monitored":false`) {
		t.Fatalf("episode monitor call: %+v", puts)
	}
	if len(h.sonarr.Find(http.MethodPut, "/api/v3/series/editor")) != 0 {
		t.Fatal("unmonitoring an episode must never touch the series")
	}
}

func TestMonitorAgainRefusedWhenSeriesIsUnmonitored(t *testing.T) {
	h := newHarness(t, true, true)
	h.sonarr.Items[0]["monitored"] = false
	_, skipped := h.get("/upgrade/episodes?tab=skipped")
	if !strings.Contains(skipped, "the series is unmonitored") || strings.Contains(skipped, `value="7001"`) {
		t.Fatal("episodes of an unmonitored series should explain why and offer no checkbox")
	}
	form := url.Values{"csrf": {h.csrf()}, "codecs": {"legacy"}, "min_size": {"1G"}, "sort": {"size"}, "action": {"monitor"}, "key": {"7001"}}
	if !strings.Contains(h.follow(h.post("/upgrade/episodes/monitor", form)), "monitor the series there first") {
		t.Error("forcing it through should be refused")
	}
	if len(h.sonarr.Find(http.MethodPut, "/api/v3/episode/monitor")) != 0 {
		t.Fatal("nothing should have been sent to Sonarr")
	}
}

func TestScheduledCheckSettings(t *testing.T) {
	h := newHarness(t, true, false)
	csrf := h.csrf()

	resp, body := h.postBody("/settings/autocheck", url.Values{"csrf": {csrf}, "radarr": {"1"}, "per_hour": {"500"}, "recheck_days": {"7"}, "codecs": {"legacy"}, "movie_min_size": {"4G"}, "episode_min_size": {"1G"}})
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "from 1 to 60") || !strings.Contains(body, "connect Radarr first") {
		t.Fatalf("validation: %d", resp.StatusCode)
	}
	if strings.Contains(body, `value="500"`) == false {
		t.Error("the rejected value should be shown back")
	}

	// Configure Radarr, then enable.
	_ = h.store.SaveSettings(context.Background(), settings.ConnectionValues(settings.Radarr, settings.Connection{URL: h.radarr.URL, Key: testsvc.Key}))
	resp = h.post("/settings/autocheck", url.Values{"csrf": {csrf}, "radarr": {"1"}, "per_hour": {"20"}, "recheck_days": {"7"}, "codecs": {"legacy"}, "movie_min_size": {"4G"}, "episode_min_size": {"1G"}})
	page := h.follow(resp)
	if !strings.Contains(page, "one every 3 minutes") {
		t.Fatal("enabled confirmation missing")
	}
	if !strings.Contains(page, "Working out which items are due") || strings.Contains(page, "0 items due") {
		t.Error("right after saving, the status should say it is scanning, not that nothing is due")
	}
	s, _ := h.app.Settings(context.Background())
	if ac := s.AutoCheck(); !ac.Radarr || ac.Sonarr || ac.PerHour != 20 {
		t.Fatalf("saved: %+v", ac)
	}

	resp = h.post("/settings/autocheck", url.Values{"csrf": {csrf}, "per_hour": {"20"}, "recheck_days": {"7"}, "codecs": {"legacy"}, "movie_min_size": {"4G"}, "episode_min_size": {"1G"}})
	if !strings.Contains(h.follow(resp), "are off") {
		t.Fatal("disabling not confirmed")
	}
}

func TestScheduledUpgradeSettings(t *testing.T) {
	h := newHarness(t, true, false)
	csrf := h.csrf()

	resp, body := h.postBody("/settings/autoupgrade", url.Values{"csrf": {csrf}, "radarr": {"1"}, "every_hours": {"0"}})
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "from 1 to 168") || !strings.Contains(body, "connect Radarr first") {
		t.Fatalf("validation: %d", resp.StatusCode)
	}

	_ = h.store.SaveSettings(context.Background(), settings.ConnectionValues(settings.Radarr, settings.Connection{URL: h.radarr.URL, Key: testsvc.Key}))
	page := h.follow(h.post("/settings/autoupgrade", url.Values{"csrf": {csrf}, "radarr": {"1"}, "every_hours": {"2"}}))
	if !strings.Contains(page, "one every 2 hours") {
		t.Fatal("enabled confirmation missing")
	}
	s, _ := h.app.Settings(context.Background())
	if au := s.AutoUpgrade(); !au.Radarr || au.Sonarr || au.Every != 2*time.Hour {
		t.Fatalf("saved: %+v", au)
	}
	if _, movies := h.get("/upgrade/movies"); !strings.Contains(movies, "Scheduled upgrades are on") {
		t.Error("the movies page should show the upgrade schedule")
	}
	if _, episodes := h.get("/upgrade/episodes"); strings.Contains(episodes, "Scheduled upgrades are on") {
		t.Error("the episodes page should not claim Sonarr is scheduled")
	}

	if !strings.Contains(h.follow(h.post("/settings/autoupgrade", url.Values{"csrf": {csrf}, "every_hours": {"2"}})), "Scheduled upgrades are off") {
		t.Fatal("disabling not confirmed")
	}
}

func TestUpgradeShowFilter(t *testing.T) {
	h := newHarness(t, true, true)

	_, unchecked := h.get("/upgrade/movies?only=unchecked")
	if !strings.Contains(unchecked, "Old Big (2010)") || !strings.Contains(unchecked, "Old Small (2010)") {
		t.Fatal("before any check, both movies are unchecked")
	}
	_, available := h.get("/upgrade/movies?only=available")
	if strings.Contains(available, "Old Big (2010)") || !strings.Contains(available, "No checked item has an upgrade available") {
		t.Fatal("before any check, nothing has an upgrade available")
	}

	// Check only Old Big, which has an acceptable release in the fake.
	form := url.Values{"csrf": {h.csrf()}, "codecs": {"legacy"}, "min_size": {"4G"}, "sort": {"size"}, "only": {"unchecked"}, "key": {"101"}}
	resp := h.post("/upgrade/movies/check", form)
	if !strings.Contains(resp.Header.Get("Location"), "only=unchecked") {
		t.Errorf("the Show filter should survive an action: %s", resp.Header.Get("Location"))
	}
	waitForJob(t, h.app, arr.Radarr)

	_, available = h.get("/upgrade/movies?only=available")
	if !strings.Contains(available, "Old Big (2010)") || strings.Contains(available, "Old Small (2010)") {
		t.Error("Upgrade available should list only the movie with a release")
	}
	if !strings.Contains(available, `<option value="available" selected>`) {
		t.Error("the Show dropdown should reflect the filter")
	}
	_, unchecked = h.get("/upgrade/movies?only=unchecked")
	if strings.Contains(unchecked, "Old Big (2010)") || !strings.Contains(unchecked, "Old Small (2010)") {
		t.Error("Not checked yet should list only the unchecked movie")
	}

	// The filter does not apply to Skipped, and nonsense values are ignored.
	resp2, all := h.get("/upgrade/movies?only=bogus")
	if resp2.StatusCode != http.StatusOK || !strings.Contains(all, "Old Big (2010)") || !strings.Contains(all, "Old Small (2010)") {
		t.Error("an unknown Show value should list everything")
	}
	_, skipped := h.get("/upgrade/movies?tab=skipped&only=available")
	if strings.Contains(skipped, `name="only"`) {
		t.Error("the Show filter should not appear on the Skipped tab")
	}
}

func TestMovieReapDefaultRatingFloor(t *testing.T) {
	h := newHarness(t, true, true)
	// Old Small is a classic: rated above the default floor.
	h.radarr.Items[1]["ratings"] = map[string]any{"imdb": map[string]any{"value": 8.1, "votes": 90000}}

	_, page := h.get("/reap/movies")
	if !strings.Contains(page, `name="min_rating" value="7.5"`) {
		t.Fatal("movies should default to protecting anything rated 7.5 or more")
	}
	if strings.Contains(page, "Old Small (2010)") || !strings.Contains(page, "Old Big (2010)") {
		t.Fatal("the 8.1-rated movie should be protected by default, the 6.0 one eligible")
	}
	_, held := h.get("/reap/movies?tab=held")
	if !strings.Contains(held, "Old Small (2010)") || !strings.Contains(held, "at or above the 7.5 floor") {
		t.Error("the protected movie should be held back with the reason")
	}

	// TV has no default floor.
	_, tv := h.get("/reap/shows")
	if !strings.Contains(tv, `name="min_rating" value=""`) {
		t.Error("TV reap should not default to a rating floor")
	}

	// Clearing the box turns the floor off, and that survives to the
	// confirm and delete steps instead of falling back to 7.5.
	_, off := h.get("/reap/movies?min_rating=&unwatched=2y&min_age=90d&min_votes=50&sort=size")
	if !strings.Contains(off, "Old Small (2010)") {
		t.Fatal("with the floor off, the highly rated movie should be eligible")
	}
	if !strings.Contains(off, `name="min_rating" value=""`) {
		t.Error("the hidden form fields must carry the empty floor")
	}
	form := url.Values{"csrf": {h.csrf()}, "key": {"1002"}, "unwatched": {"2y"}, "min_age": {"90d"}, "min_votes": {"50"}, "sort": {"size"}, "min_rating": {""}, "min_size": {""}}
	_, confirm := h.postBody("/reap/movies/confirm", form)
	if !strings.Contains(confirm, "Delete 1 movie?") {
		t.Fatal("confirm must honour the floor being off")
	}
	if !strings.Contains(confirm, `href="/reap/movies?min_age=90d&amp;min_rating=&amp;`) {
		t.Error("the Cancel link should be a working URL that keeps the filters")
	}
	form.Set("confirm", "yes")
	h.post("/reap/movies/delete", form)
	if len(h.radarr.Find(http.MethodDelete, "/api/v3/movie/102")) != 1 {
		t.Fatal("delete must honour the floor being off")
	}
}

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"":                 "/",
		"/upgrade/movies":  "/upgrade/movies",
		"/reap/movies?x=1": "/reap/movies?x=1",
		"https://evil.com": "/",
		"//evil.com":       "/",
		"/\\evil.com":      "/",
		"/\t/evil.com":     "/",
		"/\n/evil.com":     "/",
		"/\r\n/evil.com":   "/",
		"/a\\b":            "/",
		"/\x7f/evil.com":   "/",
		"evil.com":         "/",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}
