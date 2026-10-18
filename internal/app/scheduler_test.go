package app

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/seanhamlin/tendarr/internal/clients/arr"
	"github.com/seanhamlin/tendarr/internal/settings"
	"github.com/seanhamlin/tendarr/internal/store"
	"github.com/seanhamlin/tendarr/internal/testsvc"
)

type schedHarness struct {
	app    *App
	radarr *testsvc.Arr
	sonarr *testsvc.Arr
	now    time.Time
}

func newSchedHarness(t *testing.T) *schedHarness {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	h := &schedHarness{radarr: testsvc.NewArr(t, "Radarr"), sonarr: testsvc.NewArr(t, "Sonarr"), now: time.Now().UTC()}
	h.app = New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.app.Now = func() time.Time { return h.now }
	t.Cleanup(h.app.Shutdown)
	st.SetClock(func() time.Time { return h.now })

	profile := map[string]any{"id": 1, "name": "HD", "upgradeAllowed": true, "cutoff": 3, "cutoffFormatScore": 6,
		"items": []map[string]any{{"quality": map[string]any{"id": 3, "name": "WEBDL-1080p"}}}}
	movie := func(id int, size int64) map[string]any {
		return map[string]any{"id": id, "title": "M", "year": 2000, "hasFile": true, "sizeOnDisk": size, "monitored": true, "qualityProfileId": 1}
	}
	file := func(id, movieID int, size int64) map[string]any {
		return map[string]any{"id": id, "movieId": movieID, "size": size, "customFormatScore": 2, "mediaInfo": map[string]any{"videoCodec": "x264"}}
	}
	h.radarr.Profiles = []map[string]any{profile}
	h.radarr.Items = []map[string]any{movie(1, 5<<30), movie(2, 9<<30)}
	h.radarr.Files = []map[string]any{file(11, 1, 5<<30), file(12, 2, 9<<30)}

	h.sonarr.Profiles = []map[string]any{profile}
	h.sonarr.Items = []map[string]any{{"id": 5, "title": "S", "monitored": true, "qualityProfileId": 1, "statistics": map[string]any{"sizeOnDisk": 20 << 30, "episodeFileCount": 1}}}
	h.sonarr.Files = []map[string]any{{"id": 51, "seriesId": 5, "size": 2 << 30, "customFormatScore": 0, "mediaInfo": map[string]any{"videoCodec": "x264"}}}
	h.sonarr.Episodes = []map[string]any{{"id": 501, "seriesId": 5, "episodeFileId": 51, "seasonNumber": 1, "episodeNumber": 1, "monitored": true}}

	for svc, u := range map[settings.Service]string{settings.Radarr: h.radarr.URL, settings.Sonarr: h.sonarr.URL} {
		if err := st.SaveSettings(context.Background(), settings.ConnectionValues(svc, settings.Connection{URL: u, Key: testsvc.Key})); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

func (h *schedHarness) configure(t *testing.T, radarr, sonarr bool, perHour, recheckDays string) {
	t.Helper()
	f := settings.DefaultAutoCheckForm()
	f.Radarr, f.Sonarr, f.PerHour, f.RecheckDays = radarr, sonarr, perHour, recheckDays
	if err := h.app.SaveAutoCheck(context.Background(), "admin", f); err != nil {
		t.Fatal(err)
	}
}

// searched lists release lookups in order, as "radarr:2" or "sonarr:501".
func (h *schedHarness) searched() []string {
	var out []string
	for _, r := range h.radarr.Find(http.MethodGet, "/api/v3/release") {
		out = append(out, "radarr:"+r.Query.Get("movieId"))
	}
	for _, r := range h.sonarr.Find(http.MethodGet, "/api/v3/release") {
		out = append(out, "sonarr:"+r.Query.Get("episodeId"))
	}
	return out
}

func TestSchedulerDisabledDoesNothing(t *testing.T) {
	h := newSchedHarness(t)
	if wait := h.app.AutoCheckOnce(context.Background()); wait != time.Hour {
		t.Errorf("disabled scheduler should idle, waited %s", wait)
	}
	if n := len(h.searched()); n != 0 {
		t.Fatalf("a disabled scheduler searched %d times", n)
	}
	if h.app.SchedulerStatus().Enabled {
		t.Error("status should say disabled")
	}
}

func TestSchedulerPacesAndOrdersChecks(t *testing.T) {
	h := newSchedHarness(t)
	h.configure(t, true, false, "20", "7")
	ctx := context.Background()

	// 20 an hour is one every three minutes, never a burst.
	if wait := h.app.AutoCheckOnce(ctx); wait != 3*time.Minute {
		t.Fatalf("interval %s, want 3m", wait)
	}
	if got := h.searched(); len(got) != 1 || got[0] != "radarr:2" {
		t.Fatalf("largest never-checked movie should go first: %v", got)
	}
	if st := h.app.SchedulerStatus(); st.Pending != 1 || st.LastTitle == "" {
		t.Errorf("status: %+v", st)
	}

	h.app.AutoCheckOnce(ctx)
	if got := h.searched(); len(got) != 2 || got[1] != "radarr:1" {
		t.Fatalf("second check: %v", got)
	}

	// Everything is now fresh: nothing more is searched, and the scheduler
	// backs off instead of spinning.
	if wait := h.app.AutoCheckOnce(ctx); wait != queueRefreshEvery {
		t.Errorf("idle wait %s", wait)
	}
	if n := len(h.searched()); n != 2 {
		t.Fatalf("searched %d times, want 2", n)
	}

	// Once results age past the recheck window they become due again.
	h.now = h.now.Add(8 * 24 * time.Hour)
	h.app.upgrades.clear()
	h.app.AutoCheckOnce(ctx)
	if n := len(h.searched()); n != 3 {
		t.Fatalf("stale result should be rechecked, searched %d times", n)
	}

	checks, _ := h.app.Store.ReleaseChecks(ctx, string(arr.Radarr))
	if len(checks) != 2 {
		t.Errorf("results should be stored like manual checks: %+v", checks)
	}
}

func TestSchedulerRechecksWhenTheFileChanges(t *testing.T) {
	h := newSchedHarness(t)
	h.configure(t, true, false, "60", "30")
	ctx := context.Background()
	h.app.AutoCheckOnce(ctx)
	h.app.AutoCheckOnce(ctx)

	// Movie 2 got a new file: its old result no longer describes it.
	h.radarr.Files[1]["id"] = 99
	h.app.upgrades.clear()
	h.app.WakeScheduler()
	h.app.AutoCheckOnce(ctx)
	if got := h.searched(); len(got) != 3 || got[2] != "radarr:2" {
		t.Fatalf("changed file should be rechecked: %v", got)
	}
}

func TestSchedulerInterleavesServices(t *testing.T) {
	h := newSchedHarness(t)
	h.configure(t, true, true, "60", "7")
	ctx := context.Background()
	h.app.AutoCheckOnce(ctx)
	h.app.AutoCheckOnce(ctx)
	radarr := len(h.radarr.Find(http.MethodGet, "/api/v3/release"))
	sonarr := len(h.sonarr.Find(http.MethodGet, "/api/v3/release"))
	if radarr != 1 || sonarr != 1 {
		t.Errorf("the first two checks should cover both services, got radarr=%d sonarr=%d", radarr, sonarr)
	}
}

func TestSchedulerYieldsToManualChecks(t *testing.T) {
	h := newSchedHarness(t)
	h.configure(t, true, false, "20", "7")
	h.app.jobsMu.Lock()
	h.app.jobs[arr.Radarr] = &Job{ID: "manual", Kind: arr.Radarr, Total: 5}
	h.app.jobsMu.Unlock()

	h.app.AutoCheckOnce(context.Background())
	if n := len(h.searched()); n != 0 {
		t.Fatalf("scheduler searched %d times while a manual check was running", n)
	}
}

func TestSchedulerStatusDistinguishesStartingFromUpToDate(t *testing.T) {
	h := newSchedHarness(t)
	h.configure(t, true, false, "20", "7")
	if st := h.app.SchedulerStatus(); !st.Enabled || !st.Scanning {
		t.Fatalf("straight after saving, status should say it is working out what is due: %+v", st)
	}
	ctx := context.Background()
	h.app.AutoCheckOnce(ctx)
	if st := h.app.SchedulerStatus(); st.Scanning || st.Built.IsZero() || st.Pending != 1 {
		t.Fatalf("after the first scan: %+v", st)
	}
	h.app.AutoCheckOnce(ctx)
	h.app.AutoCheckOnce(ctx)
	if st := h.app.SchedulerStatus(); st.Scanning || st.Pending != 0 || st.Built.IsZero() || st.RecheckDays != 7 {
		t.Fatalf("when everything is fresh, status should be up to date: %+v", st)
	}
}

func TestSchedulerStatusKnownBeforeFirstCheck(t *testing.T) {
	h := newSchedHarness(t)
	f := settings.DefaultAutoCheckForm()
	f.Radarr, f.PerHour = true, "20"
	// Saved before startup, as on a restart: SaveAutoCheck never runs.
	if err := h.app.Store.SaveSettings(context.Background(), settings.AutoCheckValues(f)); err != nil {
		t.Fatal(err)
	}
	h.app.StartScheduler()
	st := h.app.SchedulerStatus()
	if !st.Enabled || st.PerHour != 20 {
		t.Fatalf("status should reflect saved settings before the first check: %+v", st)
	}
	if !st.Covers(arr.Radarr) || st.Covers(arr.Sonarr) {
		t.Errorf("only Radarr is scheduled: %+v", st)
	}
}
