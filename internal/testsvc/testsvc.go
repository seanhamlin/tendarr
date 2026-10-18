// Package testsvc provides in-process fakes of Plex, Tautulli, Radarr and
// Sonarr for tests. Each fake records the requests it receives so tests can
// assert on exactly what would have been sent to a real server.
package testsvc

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Key is the credential every fake accepts.
const Key = "0123456789abcdef0123456789abcdef"

// Request is one recorded call.
type Request struct {
	Method string
	Path   string
	Query  url.Values
	Body   string
}

// Server is a fake service.
type Server struct {
	*httptest.Server
	mu       sync.Mutex
	requests []Request
}

// Requests returns a copy of what has been received.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// Find returns recorded requests matching method and path.
func (s *Server) Find(method, path string) []Request {
	var out []Request
	for _, r := range s.Requests() {
		if r.Method == method && r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

func (s *Server) record(r *http.Request) Request {
	body, _ := io.ReadAll(r.Body)
	req := Request{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), Body: string(body)}
	s.mu.Lock()
	s.requests = append(s.requests, req)
	s.mu.Unlock()
	return req
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// Arr is a fake Radarr or Sonarr.
type Arr struct {
	*Server
	AppName string

	mu       sync.Mutex
	Items    []map[string]any
	Files    []map[string]any // moviefile or episodefile rows
	Episodes []map[string]any
	Profiles []map[string]any
	Queue    []map[string]any
	Tags     []map[string]any
	Releases map[string][]map[string]any // keyed by movieId/episodeId
	nextTag  int
	FailList bool
}

// NewArr starts a fake. appName is "Radarr" or "Sonarr".
func NewArr(t testing.TB, appName string) *Arr {
	a := &Arr{AppName: appName, Releases: map[string][]map[string]any{}, nextTag: 100}
	srv := &Server{}
	srv.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := srv.record(r)
		if r.Header.Get("X-Api-Key") != Key {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		a.serve(w, r, req)
	}))
	a.Server = srv
	t.Cleanup(srv.Close)
	return a
}

func (a *Arr) resource() string {
	if a.AppName == "Radarr" {
		return "/api/v3/movie"
	}
	return "/api/v3/series"
}

func (a *Arr) serve(w http.ResponseWriter, r *http.Request, req Request) {
	a.mu.Lock()
	defer a.mu.Unlock()

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/v3/system/status":
		writeJSON(w, map[string]any{"appName": a.AppName, "version": "5.0.0"})

	case r.Method == http.MethodGet && r.URL.Path == a.resource():
		if a.FailList {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		writeJSON(w, a.Items)

	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, a.resource()+"/"):
		id, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, a.resource()+"/"))
		for i, it := range a.Items {
			if toInt(it["id"]) == id {
				a.Items = append(a.Items[:i], a.Items[i+1:]...)
				w.WriteHeader(http.StatusOK)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)

	case r.Method == http.MethodGet && r.URL.Path == "/api/v3/qualityprofile":
		writeJSON(w, a.Profiles)

	case r.Method == http.MethodGet && r.URL.Path == "/api/v3/moviefile":
		want := map[int]bool{}
		for _, v := range r.URL.Query()["movieId"] {
			n, _ := strconv.Atoi(v)
			want[n] = true
		}
		var out []map[string]any
		for _, f := range a.Files {
			if want[toInt(f["movieId"])] {
				out = append(out, f)
			}
		}
		writeJSON(w, out)

	case r.Method == http.MethodGet && r.URL.Path == "/api/v3/episodefile":
		sid, _ := strconv.Atoi(r.URL.Query().Get("seriesId"))
		out := []map[string]any{}
		for _, f := range a.Files {
			if toInt(f["seriesId"]) == sid {
				out = append(out, f)
			}
		}
		writeJSON(w, out)

	case r.Method == http.MethodGet && r.URL.Path == "/api/v3/episode":
		sid, _ := strconv.Atoi(r.URL.Query().Get("seriesId"))
		out := []map[string]any{}
		for _, e := range a.Episodes {
			if toInt(e["seriesId"]) == sid {
				out = append(out, e)
			}
		}
		writeJSON(w, out)

	case r.Method == http.MethodPut && r.URL.Path == "/api/v3/episode/monitor":
		var body struct {
			EpisodeIDs []int `json:"episodeIds"`
			Monitored  bool  `json:"monitored"`
		}
		_ = json.Unmarshal([]byte(req.Body), &body)
		ids := map[int]bool{}
		for _, id := range body.EpisodeIDs {
			ids[id] = true
		}
		for _, e := range a.Episodes {
			if ids[toInt(e["id"])] {
				e["monitored"] = body.Monitored
			}
		}
		writeJSON(w, a.Episodes)

	case r.Method == http.MethodGet && r.URL.Path == "/api/v3/queue":
		writeJSON(w, map[string]any{"page": 1, "pageSize": 500, "totalRecords": len(a.Queue), "records": a.Queue})

	case r.Method == http.MethodGet && r.URL.Path == "/api/v3/release":
		key := r.URL.Query().Get("movieId")
		if key == "" {
			key = r.URL.Query().Get("episodeId")
		}
		rels := a.Releases[key]
		if rels == nil {
			rels = []map[string]any{}
		}
		writeJSON(w, rels)

	case r.Method == http.MethodPost && r.URL.Path == "/api/v3/command":
		writeJSON(w, map[string]any{"id": 4242, "name": "search", "status": "queued"})

	case r.Method == http.MethodGet && r.URL.Path == "/api/v3/tag":
		writeJSON(w, a.Tags)

	case r.Method == http.MethodPost && r.URL.Path == "/api/v3/tag":
		var body map[string]any
		_ = json.Unmarshal([]byte(req.Body), &body)
		a.nextTag++
		tag := map[string]any{"id": a.nextTag, "label": body["label"]}
		a.Tags = append(a.Tags, tag)
		writeJSON(w, tag)

	case r.Method == http.MethodPut && r.URL.Path == a.resource()+"/editor":
		var body struct {
			MovieIDs  []int  `json:"movieIds"`
			SeriesIDs []int  `json:"seriesIds"`
			Tags      []int  `json:"tags"`
			ApplyTags string `json:"applyTags"`
			Monitored *bool  `json:"monitored"`
		}
		_ = json.Unmarshal([]byte(req.Body), &body)
		ids := map[int]bool{}
		for _, id := range append(body.MovieIDs, body.SeriesIDs...) {
			ids[id] = true
		}
		for _, it := range a.Items {
			if !ids[toInt(it["id"])] {
				continue
			}
			if body.Monitored != nil {
				it["monitored"] = *body.Monitored
			}
			tags := toInts(it["tags"])
			for _, t := range body.Tags {
				tags = removeInt(tags, t)
				if body.ApplyTags == "add" {
					tags = append(tags, t)
				}
			}
			it["tags"] = tags
		}
		writeJSON(w, a.Items)

	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// Plex is a fake Plex Media Server.
type Plex struct {
	*Server
	mu       sync.Mutex
	Sections []map[string]any
	Items    map[string][]map[string]any // by section key
}

// NewPlex starts a fake.
func NewPlex(t testing.TB) *Plex {
	p := &Plex{Items: map[string][]map[string]any{}}
	srv := &Server{}
	srv.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv.record(r)
		if r.Header.Get("X-Plex-Token") != Key {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		switch {
		case r.URL.Path == "/":
			writeJSON(w, map[string]any{"MediaContainer": map[string]any{"friendlyName": "Test Server", "version": "1.40.0"}})
		case r.URL.Path == "/library/sections":
			writeJSON(w, map[string]any{"MediaContainer": map[string]any{"Directory": p.Sections}})
		case strings.HasPrefix(r.URL.Path, "/library/sections/") && strings.HasSuffix(r.URL.Path, "/all"):
			key := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/library/sections/"), "/all")
			items := p.Items[key]
			start, _ := strconv.Atoi(r.URL.Query().Get("X-Plex-Container-Start"))
			size, _ := strconv.Atoi(r.URL.Query().Get("X-Plex-Container-Size"))
			if size == 0 {
				size = len(items)
			}
			end := min(start+size, len(items))
			if start > len(items) {
				start = len(items)
			}
			writeJSON(w, map[string]any{"MediaContainer": map[string]any{
				"size": end - start, "totalSize": len(items), "Metadata": items[start:end],
			}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	p.Server = srv
	t.Cleanup(srv.Close)
	return p
}

// Tautulli is a fake Tautulli.
type Tautulli struct {
	*Server
	mu      sync.Mutex
	Records map[string][]map[string]any // by section id
}

// NewTautulli starts a fake.
func NewTautulli(t testing.TB) *Tautulli {
	ta := &Tautulli{Records: map[string][]map[string]any{}}
	srv := &Server{}
	srv.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv.record(r)
		q := r.URL.Query()
		if q.Get("apikey") != Key {
			// Tautulli really does answer 200 with an error envelope.
			writeJSON(w, map[string]any{"response": map[string]any{"result": "error", "message": "Invalid apikey", "data": map[string]any{}}})
			return
		}
		ta.mu.Lock()
		defer ta.mu.Unlock()
		switch q.Get("cmd") {
		case "get_tautulli_info":
			writeJSON(w, map[string]any{"response": map[string]any{"result": "success", "data": map[string]any{"tautulli_version": "v2.14.0"}}})
		case "get_library_media_info":
			rows := ta.Records[q.Get("section_id")]
			if rows == nil {
				rows = []map[string]any{}
			}
			writeJSON(w, map[string]any{"response": map[string]any{"result": "success", "data": map[string]any{
				"recordsFiltered": len(rows), "data": rows,
			}}})
		default:
			writeJSON(w, map[string]any{"response": map[string]any{"result": "error", "message": "unknown cmd"}})
		}
	}))
	ta.Server = srv
	t.Cleanup(srv.Close)
	return ta
}

func toInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	}
	return 0
}

func toInts(v any) []int {
	switch t := v.(type) {
	case []int:
		return append([]int(nil), t...)
	case []any:
		out := make([]int, 0, len(t))
		for _, x := range t {
			out = append(out, toInt(x))
		}
		return out
	}
	return nil
}

func removeInt(s []int, v int) []int {
	out := s[:0]
	for _, x := range s {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}
