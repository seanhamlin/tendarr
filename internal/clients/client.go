// Package clients holds the HTTP plumbing shared by the Plex, Tautulli,
// Radarr and Sonarr clients.
package clients

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrorKind classifies a failure so callers can react without string matching.
type ErrorKind int

const (
	// KindUnavailable means the service could not be reached at all.
	KindUnavailable ErrorKind = iota + 1
	// KindAuth means the service rejected the credentials.
	KindAuth
	// KindResponse means the service answered with an error or garbage.
	KindResponse
)

// Error is returned by every client.
type Error struct {
	Service string
	Kind    ErrorKind
	Message string
	Status  int
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Service, e.Message)
}

// IsKind reports whether err is a client Error of the given kind.
func IsKind(err error, kind ErrorKind) bool {
	var e *Error
	return errors.As(err, &e) && e.Kind == kind
}

// Base is a JSON-over-HTTP client for one service.
type Base struct {
	Service string
	BaseURL *url.URL
	HTTP    *http.Client
	// Headers are added to every request, typically the auth header.
	Headers map[string]string
	// Query is added to every request, for services that authenticate by
	// query parameter.
	Query url.Values
	// Secrets are scrubbed from every error message. The transport errors Go
	// returns embed the full URL, and Tautulli's key lives in the query string.
	Secrets []string
}

// NewBase parses rawURL and builds a client with the given timeout.
func NewBase(service, rawURL string, timeout time.Duration) (*Base, error) {
	u, err := ParseServiceURL(rawURL)
	if err != nil {
		return nil, err
	}
	return &Base{
		Service: service,
		BaseURL: u,
		HTTP:    NewHTTPClient(timeout),
		Headers: map[string]string{},
		Query:   url.Values{},
	}, nil
}

// ErrRedirect is returned when a service answers with a redirect.
var ErrRedirect = errors.New("redirect refused")

// NewHTTPClient returns a client that never follows redirects. Go forwards
// custom headers such as X-Api-Key and X-Plex-Token to wherever a redirect
// points, even another host, so following one could hand the key to a
// reverse proxy's login page or anything else the Location names. None of
// these APIs redirects in normal use; when one does, the configured URL is
// wrong and the admin should be told which address to use instead.
func NewHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			target := req.URL.Scheme + "://" + req.URL.Host + req.URL.Path
			return fmt.Errorf("%w: the service redirected to %s; set the URL to the address it should be reached at directly", ErrRedirect, target)
		},
	}
}

// ParseServiceURL validates a service base URL.
func ParseServiceURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("URL is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("URL must start with http:// or https://")
	}
	if u.Host == "" || u.Hostname() == "" {
		return nil, fmt.Errorf("URL must include a hostname")
	}
	if u.User != nil {
		return nil, fmt.Errorf("URL must not contain credentials")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("URL must not contain a query string or fragment")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return u, nil
}

// Request describes one call.
type Request struct {
	Method  string
	Path    string
	Query   url.Values
	Headers map[string]string
	Body    any
	// AllowStatus lists non-2xx statuses that should be returned rather than
	// raised, e.g. 404 on a DELETE that has already happened.
	AllowStatus []int
}

// Do executes req, decoding a JSON response into out when out is non-nil.
// It returns the HTTP status.
func (b *Base) Do(ctx context.Context, req Request, out any) (int, error) {
	u := *b.BaseURL
	u.Path = b.BaseURL.Path + req.Path

	q := url.Values{}
	for k, vs := range b.Query {
		q[k] = append([]string(nil), vs...)
	}
	for k, vs := range req.Query {
		q[k] = append(q[k], vs...)
	}
	u.RawQuery = q.Encode()

	var body io.Reader
	if req.Body != nil {
		buf, err := json.Marshal(req.Body)
		if err != nil {
			return 0, b.fail(KindResponse, 0, "could not encode request: %v", err)
		}
		body = bytes.NewReader(buf)
	}

	method := req.Method
	if method == "" {
		method = http.MethodGet
	}

	httpReq, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return 0, b.fail(KindResponse, 0, "could not build request: %v", err)
	}
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("User-Agent", "tendarr")
	if body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	for k, v := range b.Headers {
		httpReq.Header.Set(k, v)
	}
	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}

	resp, err := b.HTTP.Do(httpReq)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return 0, b.fail(KindUnavailable, 0, "could not reach %s: %v", b.Service, err)
	}
	defer resp.Body.Close()

	for _, s := range req.AllowStatus {
		if resp.StatusCode == s {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
			return resp.StatusCode, nil
		}
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return resp.StatusCode, b.fail(KindAuth, resp.StatusCode, "%s rejected the credentials (HTTP %d)", b.Service, resp.StatusCode)
	}

	if resp.StatusCode >= 400 {
		detail := readErrorDetail(resp.Body)
		if detail != "" {
			return resp.StatusCode, b.fail(KindResponse, resp.StatusCode, "HTTP %d from %s %s: %s", resp.StatusCode, method, req.Path, detail)
		}
		return resp.StatusCode, b.fail(KindResponse, resp.StatusCode, "HTTP %d from %s %s", resp.StatusCode, method, req.Path)
	}

	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return resp.StatusCode, nil
	}

	// 256 MiB is far beyond any real library listing but stops a misbehaving
	// proxy from exhausting memory.
	dec := json.NewDecoder(io.LimitReader(resp.Body, 256<<20))
	if err := dec.Decode(out); err != nil {
		return resp.StatusCode, b.fail(KindResponse, resp.StatusCode, "unreadable response from %s: %v", req.Path, err)
	}

	return resp.StatusCode, nil
}

// Get is shorthand for a GET decoded into out.
func (b *Base) Get(ctx context.Context, path string, query url.Values, out any) error {
	_, err := b.Do(ctx, Request{Path: path, Query: query}, out)
	return err
}

func (b *Base) fail(kind ErrorKind, status int, format string, args ...any) *Error {
	msg := fmt.Sprintf(format, args...)
	for _, s := range b.Secrets {
		if s != "" {
			msg = strings.ReplaceAll(msg, s, "[redacted]")
			msg = strings.ReplaceAll(msg, url.QueryEscape(s), "[redacted]")
		}
	}
	return &Error{Service: b.Service, Kind: kind, Message: msg, Status: status}
}

// readErrorDetail extracts a short message from an *arr validation error body,
// which is either {"message": "..."} or [{"errorMessage": "..."}].
func readErrorDetail(r io.Reader) string {
	raw, err := io.ReadAll(io.LimitReader(r, 64<<10))
	if err != nil || len(raw) == 0 {
		return ""
	}

	var single struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &single) == nil && single.Message != "" {
		return truncate(single.Message, 300)
	}

	var list []struct {
		ErrorMessage string `json:"errorMessage"`
	}
	if json.Unmarshal(raw, &list) == nil && len(list) > 0 && list[0].ErrorMessage != "" {
		return truncate(list[0].ErrorMessage, 300)
	}

	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
