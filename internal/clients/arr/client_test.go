package arr_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/seanhamlin/tendarr/internal/clients"
	"github.com/seanhamlin/tendarr/internal/clients/arr"
	"github.com/seanhamlin/tendarr/internal/media"
	"github.com/seanhamlin/tendarr/internal/testsvc"
)

func newClient(t *testing.T, kind arr.Kind, url string) *arr.Client {
	t.Helper()
	c, err := arr.New(kind, url, testsvc.Key)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// The exclusion parameter names differ between the two apps, and ASP.NET
// silently ignores one it cannot bind, so a wrong name returns 200 and does
// nothing. Both literal names are pinned here.
func TestDeleteUsesTheRightExclusionParameter(t *testing.T) {
	cases := []struct {
		kind  arr.Kind
		app   string
		path  string
		param string
	}{
		{arr.Radarr, "Radarr", "/api/v3/movie/7", "addImportExclusion"},
		{arr.Sonarr, "Sonarr", "/api/v3/series/7", "addImportListExclusion"},
	}
	for _, c := range cases {
		fake := testsvc.NewArr(t, c.app)
		fake.Items = []map[string]any{{"id": 7, "title": "X"}}
		client := newClient(t, c.kind, fake.URL)

		res, err := client.Delete(context.Background(), 7, true)
		if err != nil || res != arr.Deleted {
			t.Fatalf("%s delete: %v %v", c.app, res, err)
		}
		reqs := fake.Find(http.MethodDelete, c.path)
		if len(reqs) != 1 {
			t.Fatalf("%s: expected one DELETE %s, got %+v", c.app, c.path, fake.Requests())
		}
		q := reqs[0].Query
		if q.Get(c.param) != "true" || q.Get("deleteFiles") != "true" {
			t.Errorf("%s: query %v, want %s=true and deleteFiles=true", c.app, q, c.param)
		}

		// Already gone is a success, not an error.
		res, err = client.Delete(context.Background(), 7, false)
		if err != nil || res != arr.AlreadyGone {
			t.Errorf("%s second delete: %v %v", c.app, res, err)
		}
	}
}

func TestItemsRadarr(t *testing.T) {
	fake := testsvc.NewArr(t, "Radarr")
	fake.Items = []map[string]any{
		{
			"id": 1, "title": "Heat", "year": 1995, "imdbId": "tt0113277", "tmdbId": 949,
			"hasFile": true, "sizeOnDisk": 9000, "monitored": true, "path": "/movies/Heat (1995)",
			"rootFolderPath": "/movies", "added": "2020-01-02T03:04:05Z", "qualityProfileId": 4, "tags": []int{3},
			"ratings":   map[string]any{"imdb": map[string]any{"value": 8.3, "votes": 700000}, "tmdb": map[string]any{"value": 7.9, "votes": 6000}},
			"movieFile": map[string]any{"relativePath": "Heat.1995.mkv"},
		},
		{"id": 2, "title": "Unknown", "tmdbId": 0, "ratings": map[string]any{"rottenTomatoes": map[string]any{"value": 85}}},
		{"title": "no id"},
	}

	items, err := newClient(t, arr.Radarr, fake.URL).Items(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2 (row without id dropped)", len(items))
	}
	h := items[0]
	if h.IDs.TMDb != 949 || h.IDs.IMDb != "tt0113277" || h.FileRelativePath != "Heat.1995.mkv" || h.RootFolderPath != "/movies" {
		t.Errorf("unexpected item: %+v", h)
	}
	if h.Rating == nil || h.Rating.Source != media.RatingIMDb || h.Rating.Value != 8.3 {
		t.Errorf("IMDb should win the rating preference: %+v", h.Rating)
	}
	if !h.HasTag(3) || h.Added == nil || h.Added.Year() != 2020 {
		t.Errorf("tags or added not parsed: %+v", h)
	}
	if items[1].IDs.TMDb != 0 {
		t.Error("tmdbId 0 must be treated as unknown")
	}
	if items[1].Rating == nil || items[1].Rating.Value != 8.5 {
		t.Errorf("rotten tomatoes should be normalised to 0-10: %+v", items[1].Rating)
	}
}

func TestItemsSonarr(t *testing.T) {
	fake := testsvc.NewArr(t, "Sonarr")
	fake.Items = []map[string]any{{
		"id": 9, "title": "The Wire", "year": 2002, "tvdbId": 79126, "status": "ended", "monitored": true,
		"ratings":    map[string]any{"value": 9.3, "votes": 1200},
		"statistics": map[string]any{"sizeOnDisk": 50000, "episodeFileCount": 60, "percentOfEpisodes": 100.0},
	}}
	items, err := newClient(t, arr.Sonarr, fake.URL).Items(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s := items[0]
	if !s.HasFile || *s.SizeOnDisk != 50000 || s.IDs.TVDb != 79126 || s.Status != "ended" {
		t.Errorf("unexpected series: %+v", s)
	}
	if s.Rating == nil || s.Rating.Source != media.RatingTVDb || s.Rating.Value != 9.3 {
		t.Errorf("flat Sonarr rating not parsed: %+v", s.Rating)
	}
}

func TestSystemStatusRejectsTheWrongApp(t *testing.T) {
	fake := testsvc.NewArr(t, "Sonarr")
	_, err := newClient(t, arr.Radarr, fake.URL).SystemStatus(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Sonarr instance") {
		t.Fatalf("pasting Sonarr's URL into Radarr must fail clearly, got %v", err)
	}

	bad, _ := arr.New(arr.Radarr, fake.URL, "wrong-key-wrong-key")
	if _, err := bad.SystemStatus(context.Background()); !clients.IsKind(err, clients.KindAuth) {
		t.Fatalf("bad key: want auth error, got %v", err)
	}
}

func TestMovieFilesChunksAndParsesScores(t *testing.T) {
	fake := testsvc.NewArr(t, "Radarr")
	var ids []int
	for i := 1; i <= 230; i++ {
		ids = append(ids, i)
		fake.Files = append(fake.Files, map[string]any{
			"id": 1000 + i, "movieId": i, "relativePath": "f.mkv", "size": 5 << 30,
			"customFormatScore": 2, "qualityCutoffNotMet": false,
			"quality":       map[string]any{"quality": map[string]any{"name": "WEBDL-1080p"}},
			"customFormats": []map[string]any{{"name": "x264"}},
			"mediaInfo":     map[string]any{"videoCodec": "x264", "resolution": "1920x1080"},
		})
	}
	files, err := newClient(t, arr.Radarr, fake.URL).MovieFiles(context.Background(), ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 230 {
		t.Fatalf("got %d files", len(files))
	}
	if n := len(fake.Find(http.MethodGet, "/api/v3/moviefile")); n != 3 {
		t.Errorf("expected 3 chunked requests for 230 ids, got %d", n)
	}
	f := files[17]
	if f.Codec != media.CodecAVC || f.Score != 2 || f.Quality != "WEBDL-1080p" || len(f.Formats) != 1 {
		t.Errorf("file not parsed: %+v", f)
	}
}

func TestSearchCommandBodies(t *testing.T) {
	for _, c := range []struct {
		kind  arr.Kind
		app   string
		name  string
		field string
	}{
		{arr.Radarr, "Radarr", "MoviesSearch", "movieIds"},
		{arr.Sonarr, "Sonarr", "EpisodeSearch", "episodeIds"},
	} {
		fake := testsvc.NewArr(t, c.app)
		id, err := newClient(t, c.kind, fake.URL).Search(context.Background(), []int{4, 5})
		if err != nil || id != 4242 {
			t.Fatalf("%s: %d %v", c.app, id, err)
		}
		var body map[string]any
		_ = json.Unmarshal([]byte(fake.Find(http.MethodPost, "/api/v3/command")[0].Body), &body)
		if body["name"] != c.name {
			t.Errorf("%s: command %v", c.app, body["name"])
		}
		if ids, ok := body[c.field].([]any); !ok || len(ids) != 2 {
			t.Errorf("%s: %s = %v", c.app, c.field, body[c.field])
		}
	}
}

func TestSearchWithoutCommandIDIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	if _, err := newClient(t, arr.Radarr, srv.URL).Search(context.Background(), []int{1}); err == nil {
		t.Fatal("a search with no command id cannot be confirmed and must fail")
	}
}

func TestTagsEnsureAndApply(t *testing.T) {
	for _, c := range []struct {
		kind  arr.Kind
		app   string
		path  string
		field string
	}{
		{arr.Radarr, "Radarr", "/api/v3/movie/editor", "movieIds"},
		{arr.Sonarr, "Sonarr", "/api/v3/series/editor", "seriesIds"},
	} {
		fake := testsvc.NewArr(t, c.app)
		fake.Tags = []map[string]any{{"id": 1, "label": "keep"}}
		fake.Items = []map[string]any{{"id": 5, "title": "A", "tags": []int{}}}
		client := newClient(t, c.kind, fake.URL)
		ctx := context.Background()

		id, err := client.EnsureTag(ctx, "keep")
		if err != nil || id != 1 {
			t.Fatalf("existing tag: %d %v", id, err)
		}
		id, err = client.EnsureTag(ctx, "tendarr-ignore")
		if err != nil || id == 0 || id == 1 {
			t.Fatalf("new tag: %d %v", id, err)
		}
		if len(fake.Find(http.MethodPost, "/api/v3/tag")) != 1 {
			t.Error("expected exactly one tag creation")
		}

		if err := client.SetTag(ctx, []int{5}, id, true); err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		_ = json.Unmarshal([]byte(fake.Find(http.MethodPut, c.path)[0].Body), &body)
		if body["applyTags"] != "add" || body[c.field] == nil {
			t.Errorf("%s editor body: %v", c.app, body)
		}
	}
}

func TestQualityProfileParsing(t *testing.T) {
	fake := testsvc.NewArr(t, "Radarr")
	fake.Profiles = []map[string]any{{
		"id": 4, "name": "HD", "upgradeAllowed": true, "cutoff": 1001, "cutoffFormatScore": 6, "minFormatScore": 0,
		"items": []map[string]any{
			{"quality": map[string]any{"id": 3, "name": "WEBDL-1080p"}},
			{"id": 1001, "name": "WEB 1080p", "items": []map[string]any{}},
		},
		"formatItems": []map[string]any{{"name": "x265", "score": 3}, {"name": "Lama", "score": 2}, {"name": "BR-DISK", "score": -1000}},
	}}
	profiles, err := newClient(t, arr.Radarr, fake.URL).QualityProfiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p := profiles[4]
	if p.CutoffQuality != "WEB 1080p" {
		t.Errorf("cutoff group name not resolved: %q", p.CutoffQuality)
	}
	missing := p.MissingFormats([]string{"Lama"})
	if len(missing) != 1 || missing[0].Name != "x265" {
		t.Errorf("negative formats must never count as missing: %+v", missing)
	}
}

func TestReleasesParsesRejections(t *testing.T) {
	fake := testsvc.NewArr(t, "Radarr")
	fake.Releases["3"] = []map[string]any{
		{"guid": "a", "title": "Good", "size": 100, "customFormatScore": 6, "rejected": false, "quality": map[string]any{"quality": map[string]any{"name": "Bluray-1080p"}}},
		{"guid": "b", "title": "Bad", "rejected": true, "rejections": []any{"Not an upgrade"}},
		{"guid": "c", "title": "Bad2", "rejected": true, "rejections": []map[string]any{{"reason": "Cutoff met"}}},
	}
	rels, err := newClient(t, arr.Radarr, fake.URL).Releases(context.Background(), 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(rels) != 3 || rels[0].Quality != "Bluray-1080p" || rels[1].Rejections[0] != "Not an upgrade" || rels[2].Rejections[0] != "Cutoff met" {
		t.Errorf("releases not parsed: %+v", rels)
	}
}

func TestQueuedIDs(t *testing.T) {
	fake := testsvc.NewArr(t, "Sonarr")
	fake.Queue = []map[string]any{{"seriesId": 1, "episodeId": 11}, {"seriesId": 1, "episodeId": 12}}
	ids, err := newClient(t, arr.Sonarr, fake.URL).QueuedIDs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !ids[11] || !ids[12] || ids[1] {
		t.Errorf("Sonarr queue must be keyed by episode id: %v", ids)
	}
}

func TestSetMonitored(t *testing.T) {
	for _, c := range []struct {
		kind  arr.Kind
		app   string
		path  string
		field string
	}{
		{arr.Radarr, "Radarr", "/api/v3/movie/editor", "movieIds"},
		{arr.Sonarr, "Sonarr", "/api/v3/episode/monitor", "episodeIds"},
	} {
		fake := testsvc.NewArr(t, c.app)
		client := newClient(t, c.kind, fake.URL)
		if err := client.SetMonitored(context.Background(), []int{3, 4}, false); err != nil {
			t.Fatalf("%s: %v", c.app, err)
		}
		reqs := fake.Find(http.MethodPut, c.path)
		if len(reqs) != 1 {
			t.Fatalf("%s: expected PUT %s, got %+v", c.app, c.path, fake.Requests())
		}
		var body map[string]any
		_ = json.Unmarshal([]byte(reqs[0].Body), &body)
		if body["monitored"] != false || body[c.field] == nil {
			t.Errorf("%s body: %v", c.app, body)
		}
		if _, hasTags := body["applyTags"]; hasTags {
			t.Errorf("%s: a monitoring change must not touch tags: %v", c.app, body)
		}
	}
}

// Interactive searches use a separate, slower client; it must refuse
// redirects too, or the API key follows them to another host.
func TestBothClientsRefuseRedirects(t *testing.T) {
	var leaked []string
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = append(leaked, r.Header.Get("X-Api-Key"))
	}))
	defer elsewhere.Close()
	target := strings.Replace(elsewhere.URL, "127.0.0.1", "localhost", 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	c := newClient(t, arr.Radarr, srv.URL)
	if _, err := c.SystemStatus(context.Background()); !clients.IsKind(err, clients.KindUnavailable) {
		t.Errorf("SystemStatus: want KindUnavailable, got %v", err)
	}
	if _, err := c.Releases(context.Background(), 3); !clients.IsKind(err, clients.KindUnavailable) {
		t.Errorf("Releases: want KindUnavailable, got %v", err)
	}
	if len(leaked) > 0 {
		t.Fatalf("the API key was sent to the redirect target: %q", leaked)
	}
}
