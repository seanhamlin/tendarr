package app

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/seanhamlin/tendarr/internal/clients/arr"
	"github.com/seanhamlin/tendarr/internal/settings"
	"github.com/seanhamlin/tendarr/internal/store"
)

func (h *schedHarness) configureUpgrades(t *testing.T, radarr, sonarr bool, hours string) {
	t.Helper()
	f := settings.AutoUpgradeForm{Radarr: radarr, Sonarr: sonarr, EveryHours: hours}
	if err := h.app.SaveAutoUpgrade(context.Background(), "admin", f); err != nil {
		t.Fatal(err)
	}
}

// found records a release check that found an acceptable release of size.
func (h *schedHarness) found(t *testing.T, kind arr.Kind, key, fileID int, size int64, age time.Duration) {
	t.Helper()
	rc := store.ReleaseCheck{Kind: string(kind), ItemKey: key, FileID: fileID, CheckedAt: h.now.Add(-age), Available: true, BestTitle: "Better", BestSize: size}
	if err := h.app.Store.SaveReleaseCheck(context.Background(), rc); err != nil {
		t.Fatal(err)
	}
}

// grabbed lists search commands in order, as their request bodies.
func (h *schedHarness) grabbed() []string {
	var out []string
	for _, r := range h.radarr.Find(http.MethodPost, "/api/v3/command") {
		out = append(out, "radarr:"+r.Body)
	}
	for _, r := range h.sonarr.Find(http.MethodPost, "/api/v3/command") {
		out = append(out, "sonarr:"+r.Body)
	}
	return out
}

func TestUpgraderDisabledDoesNothing(t *testing.T) {
	h := newSchedHarness(t)
	h.found(t, arr.Radarr, 1, 11, 1<<30, time.Hour)
	if wait := h.app.AutoUpgradeOnce(context.Background()); wait != time.Hour {
		t.Errorf("disabled upgrader should idle, waited %s", wait)
	}
	if n := len(h.grabbed()); n != 0 {
		t.Fatalf("a disabled upgrader searched %d times", n)
	}
}

func TestUpgraderBiggestSavingFirstAndPaced(t *testing.T) {
	h := newSchedHarness(t)
	h.configureUpgrades(t, true, true, "2")
	ctx := context.Background()

	h.found(t, arr.Radarr, 1, 11, 1<<30, time.Hour)    // 5 GB file: saves 4 GB
	h.found(t, arr.Radarr, 2, 12, 8<<30, time.Hour)    // 9 GB file: saves 1 GB
	h.found(t, arr.Sonarr, 51, 51, 512<<20, time.Hour) // 2 GB file: saves 1.5 GB

	if wait := h.app.AutoUpgradeOnce(ctx); wait != 2*time.Hour {
		t.Fatalf("wait %s, want 2h", wait)
	}
	got := h.grabbed()
	if len(got) != 1 || !strings.HasPrefix(got[0], "radarr:") || !strings.Contains(got[0], "[1]") {
		t.Fatalf("movie 1 frees the most space and should go first: %v", got)
	}
	if checks, _ := h.app.Store.ReleaseChecks(ctx, string(arr.Radarr)); checks[1].ItemKey != 0 {
		t.Error("the upgraded item's release check should be cleared")
	}
	if st := h.app.UpgraderStatus(); st.Pending != 2 || st.LastTitle == "" {
		t.Errorf("status: %+v", st)
	}

	// Waking early never shortens the gap.
	if wait := h.app.AutoUpgradeOnce(ctx); wait <= 0 || wait > 2*time.Hour {
		t.Errorf("early wake waited %s", wait)
	}
	if n := len(h.grabbed()); n != 1 {
		t.Fatalf("searched again before the gap was up: %d", n)
	}

	h.now = h.now.Add(2 * time.Hour)
	h.app.AutoUpgradeOnce(ctx)
	if got := h.grabbed(); len(got) != 2 || !strings.HasPrefix(got[1], "sonarr:") {
		t.Fatalf("the episode saves more than movie 2 and should go next: %v", got)
	}

	entries, _ := h.app.Store.RecentAudit(ctx, 10)
	if len(entries) == 0 || entries[0].Username != schedulerUser || entries[0].Action != "upgrade" {
		t.Errorf("scheduled upgrades should be audited as the scheduler: %+v", entries)
	}
}

func TestUpgraderIgnoresStaleAndChangedResults(t *testing.T) {
	h := newSchedHarness(t)
	h.configure(t, false, false, "20", "7") // recheck window: 7 days
	h.configureUpgrades(t, true, false, "2")

	h.found(t, arr.Radarr, 1, 11, 1<<30, 8*24*time.Hour) // older than the window
	h.found(t, arr.Radarr, 2, 99, 1<<30, time.Hour)      // for a file since replaced

	if wait := h.app.AutoUpgradeOnce(context.Background()); wait != queueRefreshEvery {
		t.Errorf("with nothing to do, wait %s", wait)
	}
	if n := len(h.grabbed()); n != 0 {
		t.Fatalf("stale or mismatched results must not be acted on: %d searches", n)
	}
	if !h.app.lastUpgradeRun(context.Background()).IsZero() {
		t.Error("finding nothing should not start the clock")
	}
}

func TestUpgraderGapSurvivesRestart(t *testing.T) {
	h := newSchedHarness(t)
	h.configureUpgrades(t, true, false, "2")
	h.found(t, arr.Radarr, 1, 11, 1<<30, time.Hour)
	h.found(t, arr.Radarr, 2, 12, 1<<30, time.Hour)
	h.app.AutoUpgradeOnce(context.Background())

	restarted := New(h.app.Store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	restarted.Now = func() time.Time { return h.now.Add(30 * time.Minute) }
	restarted.NewArr = h.app.NewArr
	t.Cleanup(restarted.Shutdown)
	if wait := restarted.AutoUpgradeOnce(context.Background()); wait != 90*time.Minute {
		t.Errorf("after a restart the remaining gap should hold, waited %s", wait)
	}
	if n := len(h.grabbed()); n != 1 {
		t.Fatalf("restart triggered an early upgrade: %d searches", n)
	}
}
