package tautulli_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/seanhamlin/tendarr/internal/clients"
	"github.com/seanhamlin/tendarr/internal/clients/tautulli"
	"github.com/seanhamlin/tendarr/internal/testsvc"
)

// Tautulli answers HTTP 200 with result:"error" for a bad key. Treating that
// as success would turn an auth failure into "nothing has been watched".
func TestBadKeyIsAnAuthErrorDespiteHTTP200(t *testing.T) {
	fake := testsvc.NewTautulli(t)
	c, _ := tautulli.New(fake.URL, "wrong-key-123456")
	_, err := c.Info(context.Background())
	if !clients.IsKind(err, clients.KindAuth) {
		t.Fatalf("want auth error, got %v", err)
	}
	if strings.Contains(err.Error(), "wrong-key-123456") {
		t.Fatalf("key leaked into error: %v", err)
	}

	_, err = c.WatchRecords(context.Background(), "1")
	if !clients.IsKind(err, clients.KindAuth) {
		t.Fatalf("watch records with a bad key must fail, not return empty: %v", err)
	}
}

func TestInfo(t *testing.T) {
	fake := testsvc.NewTautulli(t)
	c, _ := tautulli.New(fake.URL, testsvc.Key)
	info, err := c.Info(context.Background())
	if err != nil || info.Version != "v2.14.0" {
		t.Fatalf("%+v %v", info, err)
	}
}

func TestWatchRecordsCoercesMixedTypes(t *testing.T) {
	fake := testsvc.NewTautulli(t)
	fake.Records["1"] = []map[string]any{
		{"rating_key": "10", "added_at": "1600000000", "last_played": 1700000000, "play_count": 3, "file_size": 12345},
		{"rating_key": 11, "added_at": 1600000000, "last_played": nil, "play_count": nil, "file_size": ""},
		{"rating_key": "", "title": "no key"},
	}
	c, _ := tautulli.New(fake.URL, testsvc.Key)
	recs, err := c.WatchRecords(context.Background(), "1")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("got %d records: %+v", len(recs), recs)
	}
	a := recs["10"]
	if a.LastPlayedAt == nil || a.PlayCount != 3 || a.FileSize == nil || *a.FileSize != 12345 || a.AddedAt == nil {
		t.Errorf("record 10: %+v", a)
	}
	b := recs["11"]
	if b.LastPlayedAt != nil || b.FileSize != nil {
		t.Errorf("null last_played must mean never; empty file_size must mean unknown: %+v", b)
	}

	for _, r := range fake.Requests() {
		if r.Query.Get("refresh") != "" {
			t.Error("refresh must never be sent; it forces a full media_info rebuild")
		}
	}
}

func TestWatchRecordsPaginates(t *testing.T) {
	fake := testsvc.NewTautulli(t)
	// The fake returns every row on each call, so only the stop condition
	// is being exercised: recordsFiltered below one page means one request.
	for i := 0; i < 10; i++ {
		fake.Records["2"] = append(fake.Records["2"], map[string]any{"rating_key": fmt.Sprint(i)})
	}
	c, _ := tautulli.New(fake.URL, testsvc.Key)
	if _, err := c.WatchRecords(context.Background(), "2"); err != nil {
		t.Fatal(err)
	}
	if n := len(fake.Requests()); n != 1 {
		t.Errorf("expected a single page request, got %d", n)
	}
}
