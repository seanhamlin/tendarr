package clients

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestParseServiceURL(t *testing.T) {
	ok := map[string]string{
		"http://nas:7878":           "http://nas:7878",
		"https://nas.local/radarr/": "https://nas.local/radarr",
		" http://10.0.0.2:32400 ":   "http://10.0.0.2:32400",
	}
	for in, want := range ok {
		u, err := ParseServiceURL(in)
		if err != nil {
			t.Errorf("ParseServiceURL(%q): %v", in, err)
			continue
		}
		if u.String() != want {
			t.Errorf("ParseServiceURL(%q) = %q, want %q", in, u.String(), want)
		}
	}

	for _, bad := range []string{"", "nas:7878", "ftp://nas", "http://", "http://user:pw@nas", "http://nas?apikey=x", "http://nas#frag", "javascript:alert(1)"} {
		if _, err := ParseServiceURL(bad); err == nil {
			t.Errorf("ParseServiceURL(%q) succeeded, want error", bad)
		}
	}
}

func TestDoClassifiesErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth":
			w.WriteHeader(http.StatusUnauthorized)
		case "/forbidden":
			w.WriteHeader(http.StatusForbidden)
		case "/bad":
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode([]map[string]string{{"errorMessage": "Path is required"}})
		case "/gone":
			w.WriteHeader(http.StatusNotFound)
		case "/garbage":
			_, _ = w.Write([]byte("<html>not json"))
		default:
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer srv.Close()

	b, err := NewBase("Test", srv.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	var out map[string]any
	if err := b.Get(ctx, "/fine", nil, &out); err != nil || out["ok"] != true {
		t.Fatalf("fine: %v %v", err, out)
	}

	for path, kind := range map[string]ErrorKind{"/auth": KindAuth, "/forbidden": KindAuth, "/bad": KindResponse, "/garbage": KindResponse} {
		err := b.Get(ctx, path, nil, &out)
		if !IsKind(err, kind) {
			t.Errorf("%s: got %v, want kind %d", path, err, kind)
		}
	}

	if err := b.Get(ctx, "/bad", nil, nil); err == nil || !strings.Contains(err.Error(), "Path is required") {
		t.Errorf("validation detail not surfaced: %v", err)
	}

	status, err := b.Do(ctx, Request{Method: http.MethodDelete, Path: "/gone", AllowStatus: []int{404}}, nil)
	if err != nil || status != 404 {
		t.Errorf("allowed 404: status %d err %v", status, err)
	}
}

func TestDoRedactsSecretsFromTransportErrors(t *testing.T) {
	// Nothing listens on this port, so the request fails at the transport,
	// and Go's url.Error would otherwise embed the full URL with the key.
	b, err := NewBase("Tautulli", "http://127.0.0.1:1", 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	secret := "supersecretapikey123"
	b.Query = url.Values{"apikey": {secret}}
	b.Secrets = []string{secret}

	err = b.Get(context.Background(), "/api/v2", url.Values{"cmd": {"x"}}, nil)
	if !IsKind(err, KindUnavailable) {
		t.Fatalf("want unavailable, got %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("secret leaked into error: %v", err)
	}
}

func TestFlexInt(t *testing.T) {
	cases := []struct {
		in    string
		val   int64
		valid bool
	}{
		{`123`, 123, true},
		{`"456"`, 456, true},
		{`""`, 0, false},
		{`null`, 0, false},
		{`12.7`, 12, true},
		{`"abc"`, 0, false},
	}
	for _, c := range cases {
		var f FlexInt
		if err := json.Unmarshal([]byte(c.in), &f); err != nil {
			t.Errorf("%s: %v", c.in, err)
		}
		if f.Value != c.val || f.Valid != c.valid {
			t.Errorf("%s: got %+v", c.in, f)
		}
	}

	var zero FlexInt
	_ = json.Unmarshal([]byte(`0`), &zero)
	if zero.Time() != nil {
		t.Error("timestamp 0 must mean never, not 1970")
	}

	var s FlexString
	_ = json.Unmarshal([]byte(`12345`), &s)
	if s != "12345" {
		t.Errorf("FlexString from number = %q", s)
	}
}

// Go forwards custom headers such as X-Api-Key across a redirect, even to
// another host, so a redirect must never be followed.
func TestDoRefusesRedirects(t *testing.T) {
	var hits int
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write([]byte(`{}`))
	}))
	defer elsewhere.Close()
	target := strings.Replace(elsewhere.URL, "127.0.0.1", "localhost", 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target+"/login?rd=secret-key", http.StatusFound)
	}))
	defer srv.Close()

	b, err := NewBase("Test", srv.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	b.Headers["X-Api-Key"] = "secret-key"
	b.Secrets = []string{"secret-key"}

	err = b.Get(context.Background(), "/api", nil, &struct{}{})
	if hits != 0 {
		t.Fatalf("the redirect was followed %d time(s)", hits)
	}
	if !IsKind(err, KindUnavailable) {
		t.Fatalf("want KindUnavailable, got %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "redirected to "+target+"/login") {
		t.Errorf("the error should name where the service redirected: %q", msg)
	}
	if strings.Contains(msg, "secret-key") || strings.Contains(msg, "rd=") {
		t.Errorf("the redirect's query string must not reach the message: %q", msg)
	}
}
