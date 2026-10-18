// Package demo runs Tendarr against fake services filled with public-domain
// films and TV, for README screenshots. It only runs when TENDARR_DEMO is set:
//
//	TENDARR_DEMO=1 go test ./internal/demo -run TestDemo -timeout 0
package demo

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seanhamlin/tendarr/internal/app"
	"github.com/seanhamlin/tendarr/internal/settings"
	"github.com/seanhamlin/tendarr/internal/store"
	"github.com/seanhamlin/tendarr/internal/testsvc"
	"github.com/seanhamlin/tendarr/internal/web"
)

const (
	addr     = "127.0.0.1:18490"
	password = "demo-password-123"
	gb       = int64(1) << 30
)

type film struct {
	title     string
	year      int
	sizeGB    float64
	codec     string
	quality   string
	score     int
	formats   []string
	rating    float64
	votes     int
	watched   time.Duration // 0 = never
	added     time.Duration
	best      float64 // acceptable release size in GB, 0 = none found
	bestQual  string
	bestScore int
}

var day = 24 * time.Hour

var films = []film{
	{"Metropolis", 1927, 24.6, "x264", "Remux-1080p", 1, []string{"Remaster"}, 8.3, 190000, 40 * day, 5 * 365 * day, 9.8, "Bluray-1080p", 7},
	{"Nosferatu", 1922, 18.2, "x264", "Bluray-1080p", 0, nil, 7.8, 110000, 400 * day, 6 * 365 * day, 6.1, "Bluray-1080p", 6},
	{"His Girl Friday", 1940, 15.4, "x264", "Bluray-1080p", 2, []string{"Remaster"}, 7.8, 65000, 90 * day, 4 * 365 * day, 0, "", 0},
	{"Charade", 1963, 14.9, "x264", "Bluray-1080p", 1, []string{"Criterion"}, 7.8, 85000, 200 * day, 4 * 365 * day, 7.2, "Bluray-1080p", 7},
	{"The General", 1926, 13.8, "x264", "Bluray-1080p", 0, nil, 8.1, 98000, 3 * 365 * day, 6 * 365 * day, 4.4, "Bluray-1080p", 6},
	{"Night of the Living Dead", 1968, 12.7, "x264", "Bluray-1080p", 0, nil, 7.8, 145000, 1100 * day, 5 * 365 * day, 5.3, "Bluray-1080p", 6},
	{"Plan 9 from Outer Space", 1957, 11.9, "x264", "Bluray-1080p", 0, nil, 3.9, 42000, 0, 4 * 365 * day, 3.8, "Bluray-1080p", 6},
	{"Carnival of Souls", 1962, 11.2, "x264", "Bluray-1080p", 0, nil, 7.0, 25000, 0, 5 * 365 * day, 0, "", 0},
	{"House on Haunted Hill", 1959, 10.6, "x264", "Bluray-1080p", 0, nil, 6.8, 40000, 1300 * day, 5 * 365 * day, 4.9, "Bluray-1080p", 6},
	{"The Last Man on Earth", 1964, 9.8, "x264", "Bluray-1080p", 2, []string{"Remaster"}, 6.8, 24000, 900 * day, 4 * 365 * day, 0, "", 0},
	{"Detour", 1945, 9.1, "x264", "Bluray-1080p", 0, nil, 7.2, 21000, 0, 4 * 365 * day, 4.2, "Bluray-1080p", 6},
	{"Reefer Madness", 1936, 8.4, "XviD", "DVD", 0, nil, 3.7, 15000, 0, 6 * 365 * day, 0, "", 0},
	{"The Little Shop of Horrors", 1960, 8.2, "x264", "Bluray-1080p", 0, nil, 6.2, 18000, 1500 * day, 5 * 365 * day, 3.6, "Bluray-1080p", 6},
	{"Dementia 13", 1963, 7.6, "x264", "WEBDL-1080p", 0, nil, 5.8, 16000, 0, 3 * 365 * day, 0, "", 0},
	{"Kansas City Confidential", 1952, 7.3, "x264", "Bluray-1080p", 0, nil, 7.1, 9000, 800 * day, 4 * 365 * day, 0, "", 0},
	{"Royal Wedding", 1951, 6.9, "x264", "WEBDL-1080p", 0, nil, 6.6, 12000, 1000 * day, 4 * 365 * day, 0, "", 0},
	{"The Hitch-Hiker", 1953, 6.4, "x264", "Bluray-1080p", 0, nil, 7.0, 11000, 0, 3 * 365 * day, 0, "", 0},
	{"Suddenly", 1954, 5.8, "x264", "WEBDL-1080p", 0, nil, 6.7, 8000, 1200 * day, 3 * 365 * day, 0, "", 0},
	{"McLintock!", 1963, 5.2, "x264", "WEBDL-1080p", 0, nil, 7.1, 18000, 950 * day, 4 * 365 * day, 0, "", 0},
	{"Sherlock Jr.", 1924, 4.6, "x265", "Bluray-1080p", 6, []string{"x265", "Remaster"}, 8.2, 60000, 30 * day, 3 * 365 * day, 0, "", 0},
}

type show struct {
	title    string
	year     int
	status   string
	sizeGB   float64
	files    int
	rating   float64
	watched  time.Duration
	episodes []episode
}

type episode struct {
	season, number int
	title          string
	sizeGB         float64
	best           float64
}

var shows = []show{
	{"Bonanza", 1959, "ended", 182.4, 96, 7.4, 1400 * day, []episode{
		{1, 1, "A Rose for Lotta", 2.9, 1.1}, {1, 2, "Death on Sun Mountain", 2.8, 0}, {1, 3, "The Newcomers", 2.8, 1.0},
	}},
	{"The Beverly Hillbillies", 1962, "ended", 96.1, 55, 7.1, 1700 * day, []episode{
		{1, 1, "The Clampetts Strike Oil", 1.9, 0.7}, {1, 2, "Getting Settled", 1.8, 0},
	}},
	{"One Step Beyond", 1959, "ended", 64.8, 40, 7.6, 0, []episode{
		{1, 1, "The Bride Possessed", 2.1, 0.8}, {1, 2, "Night of April 14th", 2.0, 0},
	}},
	{"The Lucy Show", 1962, "ended", 41.3, 30, 7.2, 2200 * day, []episode{
		{1, 1, "Lucy Waits Up for Chris", 1.6, 0},
	}},
}

func size(g float64) int64 { return int64(g * float64(gb)) }

func TestDemo(t *testing.T) {
	if os.Getenv("TENDARR_DEMO") == "" {
		t.Skip("set TENDARR_DEMO=1 to run the screenshot demo")
	}
	ctx := context.Background()
	now := time.Now()

	plex := testsvc.NewPlex(t)
	tautulli := testsvc.NewTautulli(t)
	radarr := testsvc.NewArr(t, "Radarr")
	sonarr := testsvc.NewArr(t, "Sonarr")

	plex.Sections = []map[string]any{
		{"key": "1", "title": "Movies", "type": "movie", "Location": []map[string]any{{"path": "/mnt/media/movies"}}},
		{"key": "2", "title": "TV Shows", "type": "show", "Location": []map[string]any{{"path": "/mnt/media/tv"}}},
	}
	profile := map[string]any{
		"id": 1, "name": "HD-1080p", "upgradeAllowed": true, "cutoff": 7, "cutoffFormatScore": 6,
		"items": []map[string]any{{"quality": map[string]any{"id": 7, "name": "Bluray-1080p"}}},
		"formatItems": []map[string]any{
			{"name": "x265", "score": 4}, {"name": "Remaster", "score": 1}, {"name": "Criterion", "score": 1},
			{"name": "HDR", "score": 2}, {"name": "<6 GB", "score": 1},
		},
	}
	radarr.Profiles = []map[string]any{profile}
	sonarr.Profiles = []map[string]any{profile}

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "demo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	for i, f := range films {
		id := 100 + i
		f.added += time.Duration(i*47%311) * day // spread the added dates
		folder := fmt.Sprintf("%s (%d)", f.title, f.year)
		file := fmt.Sprintf("%s (%d) %s %s.mkv", f.title, f.year, f.quality, f.codec)
		pm := map[string]any{
			"ratingKey": fmt.Sprint(id), "title": f.title, "year": f.year, "addedAt": now.Add(-f.added).Unix(),
			"Guid":  []map[string]any{{"id": fmt.Sprintf("tmdb://%d", id)}},
			"Media": []map[string]any{{"Part": []map[string]any{{"file": "/mnt/media/movies/" + folder + "/" + file, "size": size(f.sizeGB)}}}},
		}
		if f.watched > 0 {
			pm["lastViewedAt"] = now.Add(-f.watched).Unix()
		}
		plex.Items["1"] = append(plex.Items["1"], pm)
		radarr.Items = append(radarr.Items, map[string]any{
			"id": id, "title": f.title, "year": f.year, "tmdbId": id, "hasFile": true, "sizeOnDisk": size(f.sizeGB),
			"monitored": true, "path": "/movies/" + folder, "rootFolderPath": "/movies", "qualityProfileId": 1,
			"added": now.Add(-f.added).Format(time.RFC3339), "tags": []int{},
			"movieFile": map[string]any{"relativePath": file},
			"ratings":   map[string]any{"imdb": map[string]any{"value": f.rating, "votes": f.votes}},
		})
		var cfs []map[string]any
		for _, n := range f.formats {
			cfs = append(cfs, map[string]any{"name": n})
		}
		radarr.Files = append(radarr.Files, map[string]any{
			"id": 9000 + i, "movieId": id, "size": size(f.sizeGB), "relativePath": file, "customFormatScore": f.score,
			"qualityCutoffNotMet": f.quality == "DVD" || f.quality == "WEBDL-1080p",
			"quality":             map[string]any{"quality": map[string]any{"name": f.quality}},
			"customFormats":       cfs,
			"mediaInfo":           map[string]any{"videoCodec": f.codec, "resolution": "1920x1080"},
		})
		if f.best > 0 {
			_ = st.SaveReleaseCheck(ctx, store.ReleaseCheck{
				Kind: "radarr", ItemKey: id, FileID: 9000 + i, CheckedAt: now.Add(-time.Duration(i+1) * 37 * time.Minute),
				Available: true, BestTitle: fmt.Sprintf("%s.%d.1080p.BluRay.x265-GROUP", strings.ReplaceAll(f.title, " ", "."), f.year),
				BestScore: f.bestScore, BestQuality: f.bestQual, BestSize: size(f.best), Total: 14, Rejected: 9,
			})
		} else {
			_ = st.SaveReleaseCheck(ctx, store.ReleaseCheck{
				Kind: "radarr", ItemKey: id, FileID: 9000 + i, CheckedAt: now.Add(-time.Duration(i+2) * 3 * time.Hour),
				Total: 8, Rejected: 8, TopRejection: "Not an upgrade for existing movie file",
			})
		}
	}

	epID, fileID := 50000, 70000
	for i, s := range shows {
		id := 300 + i
		sm := map[string]any{
			"ratingKey": fmt.Sprint(2000 + i), "title": s.title, "year": s.year, "addedAt": now.Add(-6 * 365 * day).Unix(),
			"Guid": []map[string]any{{"id": fmt.Sprintf("tvdb://%d", id)}},
		}
		if s.watched > 0 {
			sm["lastViewedAt"] = now.Add(-s.watched).Unix()
		}
		plex.Items["2"] = append(plex.Items["2"], sm)
		sonarr.Items = append(sonarr.Items, map[string]any{
			"id": id, "title": s.title, "year": s.year, "tvdbId": id, "status": s.status, "monitored": true,
			"path": "/tv/" + s.title, "rootFolderPath": "/tv", "qualityProfileId": 1, "tags": []int{},
			"added":      now.Add(-6 * 365 * day).Format(time.RFC3339),
			"ratings":    map[string]any{"value": s.rating, "votes": 3000},
			"statistics": map[string]any{"sizeOnDisk": size(s.sizeGB), "episodeFileCount": s.files},
		})
		for _, e := range s.episodes {
			epID++
			fileID++
			sonarr.Files = append(sonarr.Files, map[string]any{
				"id": fileID, "seriesId": id, "seasonNumber": e.season, "size": size(e.sizeGB), "customFormatScore": 0,
				"qualityCutoffNotMet": false, "quality": map[string]any{"quality": map[string]any{"name": "Bluray-1080p"}},
				"mediaInfo": map[string]any{"videoCodec": "x264", "resolution": "1440x1080"},
			})
			sonarr.Episodes = append(sonarr.Episodes, map[string]any{
				"id": epID, "seriesId": id, "episodeFileId": fileID, "seasonNumber": e.season, "episodeNumber": e.number,
				"title": e.title, "monitored": true,
			})
			rc := store.ReleaseCheck{Kind: "sonarr", ItemKey: fileID, FileID: fileID, CheckedAt: now.Add(-5 * time.Hour), Total: 6, Rejected: 6}
			if e.best > 0 {
				rc.Available, rc.BestQuality, rc.BestScore, rc.BestSize, rc.Rejected = true, "Bluray-1080p", 6, size(e.best), 3
				rc.BestTitle = strings.ReplaceAll(s.title, " ", ".") + ".S01.1080p.BluRay.x265-GROUP"
			}
			_ = st.SaveReleaseCheck(ctx, rc)
		}
	}

	for svc, u := range map[settings.Service]string{
		settings.Plex: plex.URL, settings.Tautulli: tautulli.URL, settings.Radarr: radarr.URL, settings.Sonarr: sonarr.URL,
	} {
		if err := st.SaveSettings(ctx, settings.ConnectionValues(svc, settings.Connection{URL: u, Key: testsvc.Key})); err != nil {
			t.Fatal(err)
		}
	}
	ac := settings.DefaultAutoCheckForm()
	ac.Radarr, ac.Sonarr = true, true
	must(t, st.SaveSettings(ctx, settings.LibraryValues("/movies:/mnt/media/movies\n/tv:/mnt/media/tv", nil, settings.DefaultIgnoreTag)))
	must(t, st.SaveSettings(ctx, settings.AutoCheckValues(ac)))
	must(t, st.SaveSettings(ctx, settings.AutoUpgradeValues(settings.AutoUpgradeForm{Radarr: true, Sonarr: true, EveryHours: "2"})))
	// Keep the upgrader from acting while screenshots are taken.
	must(t, st.SaveSettings(ctx, map[string]string{"autoupgrade.last_run": now.Add(-47 * time.Minute).Format(time.RFC3339Nano)}))

	hash, _ := web.HashPassword(password)
	if _, err := st.CreateFirstUser(ctx, "admin", hash); err != nil {
		t.Fatal(err)
	}

	for _, e := range []store.AuditEntry{
		{TS: now.Add(-47 * time.Minute), Username: "scheduler", Action: "upgrade", Service: "radarr", Title: "Peter Pan (1924)", Outcome: "QUEUED"},
		{TS: now.Add(-2*time.Hour - 47*time.Minute), Username: "scheduler", Action: "upgrade", Service: "radarr", Title: "The Phantom of the Opera (1925)", Outcome: "QUEUED"},
		{TS: now.Add(-5 * time.Hour), Username: "admin", Action: "delete", Service: "radarr", Title: "Robot Monster (1953)", Outcome: "DELETED"},
		{TS: now.Add(-5 * time.Hour), Username: "admin", Action: "delete", Service: "radarr", Title: "The Brain That Wouldn't Die (1962)", Outcome: "DELETED"},
		{TS: now.Add(-5*time.Hour - 3*time.Minute), Username: "admin", Action: "ignore", Service: "radarr", Title: "The Kid (1921)", Outcome: "OK"},
		{TS: now.Add(-26 * time.Hour), Username: "scheduler", Action: "upgrade", Service: "sonarr", Title: "Bonanza S01E04", Outcome: "QUEUED"},
	} {
		must(t, st.AddAudit(ctx, e))
	}

	a := app.New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer a.Shutdown()
	a.StartScheduler()
	a.StartUpgrader()
	srv, err := web.New(a, web.Config{CookieSecure: "false", Version: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = http.Serve(ln, srv) }()
	fmt.Println("demo ready on http://" + addr + " (admin / " + password + ")")

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	<-sig
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
