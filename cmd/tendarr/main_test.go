package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestAdoptLegacyDatabase(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := t.TempDir()
	legacy := filepath.Join(dir, "plex-automations.db")
	current := filepath.Join(dir, "tendarr.db")

	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.WriteFile(legacy+suffix, []byte("data"+suffix), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := adoptLegacyDatabase(dir, current, logger); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		got, err := os.ReadFile(current + suffix)
		if err != nil || string(got) != "data"+suffix {
			t.Errorf("%s not moved: %q %v", suffix, got, err)
		}
		if _, err := os.Stat(legacy + suffix); err == nil {
			t.Errorf("legacy %s still present", suffix)
		}
	}

	// An existing new database is never replaced.
	_ = os.WriteFile(legacy, []byte("old"), 0o600)
	if err := adoptLegacyDatabase(dir, current, logger); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(current); string(got) != "data" {
		t.Errorf("existing database was overwritten: %q", got)
	}

	// Nothing to adopt is not an error.
	if err := adoptLegacyDatabase(t.TempDir(), filepath.Join(t.TempDir(), "tendarr.db"), logger); err != nil {
		t.Errorf("empty data dir: %v", err)
	}
}
