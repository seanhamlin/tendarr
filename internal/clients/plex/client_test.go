package plex_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/seanhamlin/tendarr/internal/clients"
	"github.com/seanhamlin/tendarr/internal/clients/plex"
	"github.com/seanhamlin/tendarr/internal/testsvc"
)

func TestIdentityRequiresTheToken(t *testing.T) {
	fake := testsvc.NewPlex(t)

	c, _ := plex.New(fake.URL, testsvc.Key)
	id, err := c.Identity(context.Background())
	if err != nil || id.Name != "Test Server" {
		t.Fatalf("identity: %+v %v", id, err)
	}

	bad, _ := plex.New(fake.URL, "not-the-token")
	if _, err := bad.Identity(context.Background()); !clients.IsKind(err, clients.KindAuth) {
		t.Fatalf("want auth error, got %v", err)
	}

	// The token travels as a header, never in the query string where it
	// would end up in access logs.
	for _, r := range fake.Requests() {
		if r.Query.Get("X-Plex-Token") != "" {
			t.Fatalf("token leaked into query: %+v", r)
		}
	}
}

func TestSectionsSkipsMusicAndPhotos(t *testing.T) {
	fake := testsvc.NewPlex(t)
	fake.Sections = []map[string]any{
		{"key": "1", "title": "Movies", "type": "movie", "Location": []map[string]any{{"path": "/mnt/media/movies"}}},
		{"key": 2, "title": "TV", "type": "show"},
		{"key": "3", "title": "Music", "type": "artist"},
		{"key": "4", "title": "Photos", "type": "photo"},
	}
	c, _ := plex.New(fake.URL, testsvc.Key)
	sections, err := c.Sections(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sections) != 2 || sections[0].Locations[0] != "/mnt/media/movies" || sections[1].Key != "2" {
		t.Errorf("unexpected sections: %+v", sections)
	}
}

func TestSectionItemsPaginatesAndParses(t *testing.T) {
	fake := testsvc.NewPlex(t)
	var items []map[string]any
	for i := 0; i < 1203; i++ {
		items = append(items, map[string]any{"ratingKey": fmt.Sprint(i), "title": fmt.Sprintf("Movie %d", i), "year": 2000})
	}
	items[0] = map[string]any{
		"ratingKey": "0", "title": "Heat", "year": 1995, "addedAt": 1600000000, "lastViewedAt": 1700000000, "viewCount": 2,
		"guid": "plex://movie/abc",
		"Guid": []map[string]any{{"id": "imdb://tt0113277"}, {"id": "tmdb://949"}},
		"Media": []map[string]any{
			{"Part": []map[string]any{{"file": "/mnt/media/movies/Heat/Heat.mkv", "size": 1000}, {"file": "/mnt/media/movies/Heat/Heat.cd2.mkv", "size": 500}}},
			{"Part": []map[string]any{{"file": "/mnt/media/movies/Heat/Heat.4k.mkv", "size": 3000}}},
		},
	}
	fake.Items["1"] = items

	c, _ := plex.New(fake.URL, testsvc.Key)
	got, err := c.SectionItems(context.Background(), plex.Section{Key: "1", Type: plex.Movie})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1203 {
		t.Fatalf("got %d items across pages, want 1203", len(got))
	}
	h := got[0]
	if h.IDs.IMDb != "tt0113277" || h.IDs.TMDb != 949 {
		t.Errorf("guids not parsed: %+v", h.IDs)
	}
	if h.FileSize == nil || *h.FileSize != 4500 {
		t.Errorf("file size must sum every part of every media: %v", h.FileSize)
	}
	if h.FilePath != "/mnt/media/movies/Heat/Heat.mkv" || h.LastViewedAt == nil || h.ViewCount != 2 {
		t.Errorf("unexpected item: %+v", h)
	}

	var pages int
	for _, r := range fake.Requests() {
		if strings.HasSuffix(r.Path, "/all") {
			pages++
			if r.Query.Get("includeGuids") != "1" {
				t.Error("includeGuids=1 is required to get external ids in one call")
			}
		}
	}
	if pages != 3 {
		t.Errorf("expected 3 pages of 500, got %d", pages)
	}
}
