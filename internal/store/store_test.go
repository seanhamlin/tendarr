package store

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestMigrationsAreIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	for i := 0; i < 2; i++ {
		s, err := Open(context.Background(), path)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		s.Close()
	}
}

func TestCreateFirstUserOnlyOnce(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	if n, _ := s.CountUsers(ctx); n != 0 {
		t.Fatal("fresh database should have no users")
	}

	// Race several setups: exactly one may win.
	var wg sync.WaitGroup
	results := make(chan error, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.CreateFirstUser(ctx, "admin"+string(rune('a'+i)), "hash")
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)

	wins := 0
	for err := range results {
		if err == nil {
			wins++
		} else if !errors.Is(err, ErrSetupComplete) {
			t.Errorf("unexpected error: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d setups succeeded, want exactly 1", wins)
	}
}

func TestUsernamesAreCaseInsensitive(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if _, err := s.CreateFirstUser(ctx, "Admin", "h"); err != nil {
		t.Fatal(err)
	}
	u, err := s.UserByUsername(ctx, "ADMIN")
	if err != nil || u.Username != "Admin" {
		t.Fatalf("%+v %v", u, err)
	}
	if _, err := s.UserByUsername(ctx, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestSessions(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	u, _ := s.CreateFirstUser(ctx, "admin", "h")

	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return clock })

	if err := s.CreateSession(ctx, "hash1", u.ID, "csrf1", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession(ctx, "hash2", u.ID, "csrf2", time.Hour); err != nil {
		t.Fatal(err)
	}
	sess, err := s.SessionByHash(ctx, "hash1")
	if err != nil || sess.Username != "admin" || sess.CSRFToken != "csrf1" {
		t.Fatalf("%+v %v", sess, err)
	}

	clock = clock.Add(2 * time.Hour)
	if _, err := s.SessionByHash(ctx, "hash1"); !errors.Is(err, ErrNotFound) {
		t.Fatal("an expired session must not be returned")
	}
	if n, _ := s.PurgeExpiredSessions(ctx); n != 2 {
		t.Errorf("purged %d, want 2", n)
	}

	// A password change keeps the current session and drops the rest.
	_ = s.CreateSession(ctx, "keep", u.ID, "c", time.Hour)
	_ = s.CreateSession(ctx, "drop", u.ID, "c", time.Hour)
	if err := s.UpdatePassword(ctx, u.ID, "newhash", "keep"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionByHash(ctx, "keep"); err != nil {
		t.Error("current session should survive a password change")
	}
	if _, err := s.SessionByHash(ctx, "drop"); !errors.Is(err, ErrNotFound) {
		t.Error("other sessions must be revoked on password change")
	}
	got, _ := s.UserByID(ctx, u.ID)
	if got.PasswordHash != "newhash" {
		t.Error("password hash not updated")
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if err := s.SaveSettings(ctx, map[string]string{"a": "1", "b": "2"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSettings(ctx, map[string]string{"a": "3"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Settings(ctx)
	if err != nil || got["a"] != "3" || got["b"] != "2" {
		t.Fatalf("%v %v", got, err)
	}
}

func TestReleaseChecks(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	at := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	rc := ReleaseCheck{Kind: "radarr", ItemKey: 5, FileID: 50, CheckedAt: at, Available: true, BestTitle: "x", BestScore: 6, BestSize: 99}
	if err := s.SaveReleaseCheck(ctx, rc); err != nil {
		t.Fatal(err)
	}
	rc.BestScore = 7
	if err := s.SaveReleaseCheck(ctx, rc); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReleaseChecks(ctx, "radarr")
	if err != nil || len(got) != 1 || got[5].BestScore != 7 || !got[5].CheckedAt.Equal(at) || !got[5].Available {
		t.Fatalf("%+v %v", got, err)
	}
	if other, _ := s.ReleaseChecks(ctx, "sonarr"); len(other) != 0 {
		t.Error("kinds must be separate")
	}
	if err := s.DeleteReleaseChecks(ctx, "radarr", []int{5}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ReleaseChecks(ctx, "radarr"); len(got) != 0 {
		t.Error("delete did not remove the check")
	}
}

func TestAudit(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := s.AddAudit(ctx, AuditEntry{Username: "admin", Action: "delete", Service: "radarr", ItemID: i, Title: "T", Outcome: "DELETED", Details: map[string]any{"sizeBytes": 10}}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.RecentAudit(ctx, 2)
	if err != nil || len(got) != 2 || got[0].ItemID != 2 {
		t.Fatalf("newest first, limited: %+v %v", got, err)
	}
	if got[0].Details["sizeBytes"].(float64) != 10 {
		t.Errorf("details not round-tripped: %v", got[0].Details)
	}
}

func TestLegacyIgnoreTagIsPinnedForExistingInstalls(t *testing.T) {
	ctx := context.Background()

	// Fresh install: no account when migrations run, so no pinned tag.
	fresh := open(t)
	if got, _ := fresh.Settings(ctx); got["library.ignore_tag"] != "" {
		t.Fatalf("a fresh install must use the new default, got %q", got["library.ignore_tag"])
	}

	// Existing install: an account exists when the migration first runs.
	path := filepath.Join(t.TempDir(), "old.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateFirstUser(ctx, "admin", "h"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version = 2`); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, _ := s.Settings(ctx)
	if got["library.ignore_tag"] != "plex-automations-ignore" {
		t.Fatalf("existing install should keep the tag its items carry, got %q", got["library.ignore_tag"])
	}

	// A tag the admin chose is never overwritten.
	_ = s.SaveSettings(ctx, map[string]string{"library.ignore_tag": "keep"})
	_, _ = s.db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version = 2`)
	s.Close()
	s, _ = Open(ctx, path)
	defer s.Close()
	if got, _ := s.Settings(ctx); got["library.ignore_tag"] != "keep" {
		t.Errorf("a chosen tag was overwritten: %q", got["library.ignore_tag"])
	}
}
