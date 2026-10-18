package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRunAs(t *testing.T) {
	for _, tc := range []struct {
		puid, pgid string
		uid, gid   int
		bad        bool
	}{
		{"", "", defaultID, defaultID, false},
		{"1026", "100", 1026, 100, false},
		{" 1026 ", "", 1026, defaultID, false},
		{"0", "0", 0, 0, false},
		{"abc", "100", 0, 0, true},
		{"1026", "-1", 0, 0, true},
		{"99999999999", "100", 0, 0, true},
	} {
		t.Setenv("PUID", tc.puid)
		t.Setenv("PGID", tc.pgid)
		uid, gid, err := runAs()
		if tc.bad {
			if err == nil {
				t.Errorf("PUID=%q PGID=%q should be rejected", tc.puid, tc.pgid)
			}
			continue
		}
		if err != nil || uid != tc.uid || gid != tc.gid {
			t.Errorf("PUID=%q PGID=%q: got %d:%d %v, want %d:%d", tc.puid, tc.pgid, uid, gid, err, tc.uid, tc.gid)
		}
	}
}

func TestChownTreeSkipsWhatAlreadyMatches(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "tendarr.db"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	// Everything is already ours; an unprivileged test can only chown to
	// itself, which is exactly the no-op path.
	changed, err := chownTree(dir, os.Getuid(), os.Getgid())
	if err != nil || changed != 0 {
		t.Errorf("changed %d entries, err %v; want 0 for a tree that already matches", changed, err)
	}
}
