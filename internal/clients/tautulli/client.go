// Package tautulli is a client for Tautulli's v2 API.
//
// Used only for watch state, specifically for plays Plex's own lastViewedAt
// cannot see: anything watched by a managed or home user.
//
// Tautulli answers HTTP 200 even when a request failed, signalling the real
// outcome in response.result, so the envelope is checked on every call. A bad
// API key otherwise becomes an empty library and, downstream, "nothing has
// ever been watched".
package tautulli

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/seanhamlin/tendarr/internal/clients"
)

const pageSize = 1000

// Client talks to one Tautulli instance.
type Client struct {
	base *clients.Base
}

// New builds a client. Tautulli only accepts the key as a query parameter, so
// it is registered as a secret and scrubbed from every error message.
func New(rawURL, apiKey string) (*Client, error) {
	base, err := clients.NewBase("Tautulli", rawURL, 90*time.Second)
	if err != nil {
		return nil, err
	}
	base.Query.Set("apikey", apiKey)
	base.Secrets = []string{apiKey}
	return &Client{base: base}, nil
}

// Info is Tautulli's own version, used to prove the key works.
type Info struct {
	Version string
}

// Info calls get_tautulli_info.
func (c *Client) Info(ctx context.Context) (Info, error) {
	var data struct {
		Version string `json:"tautulli_version"`
	}
	if err := c.command(ctx, "get_tautulli_info", nil, &data); err != nil {
		return Info{}, err
	}
	return Info{Version: data.Version}, nil
}

// WatchRecord is one row of get_library_media_info.
type WatchRecord struct {
	RatingKey    string
	AddedAt      *time.Time
	LastPlayedAt *time.Time
	PlayCount    int
	FileSize     *int64
}

// WatchRecords returns watch state for a library section keyed by rating key.
//
// Paginated rather than fetched in one request: Tautulli sorts and slices the
// whole library in memory, so an unbounded request on a big section times
// out. refresh=true is never sent; it forces a full media_info rebuild.
func (c *Client) WatchRecords(ctx context.Context, sectionID string) (map[string]WatchRecord, error) {
	out := map[string]WatchRecord{}
	for start := 0; ; start += pageSize {
		var data struct {
			RecordsFiltered clients.FlexInt `json:"recordsFiltered"`
			Data            []struct {
				RatingKey  clients.FlexString `json:"rating_key"`
				AddedAt    clients.FlexInt    `json:"added_at"`
				LastPlayed clients.FlexInt    `json:"last_played"`
				PlayCount  clients.FlexInt    `json:"play_count"`
				FileSize   clients.FlexInt    `json:"file_size"`
			} `json:"data"`
		}
		params := url.Values{
			"section_id":   {sectionID},
			"order_column": {"added_at"},
			"order_dir":    {"asc"},
			"start":        {strconv.Itoa(start)},
			"length":       {strconv.Itoa(pageSize)},
		}
		if err := c.command(ctx, "get_library_media_info", params, &data); err != nil {
			return nil, err
		}

		for _, row := range data.Data {
			if row.RatingKey == "" {
				continue
			}
			out[string(row.RatingKey)] = WatchRecord{
				RatingKey:    string(row.RatingKey),
				AddedAt:      row.AddedAt.Time(),
				LastPlayedAt: row.LastPlayed.Time(),
				PlayCount:    int(row.PlayCount.Value),
				FileSize:     row.FileSize.Ptr(),
			}
		}

		if len(data.Data) == 0 || int64(start+pageSize) >= data.RecordsFiltered.Value {
			break
		}
	}
	return out, nil
}

func (c *Client) command(ctx context.Context, cmd string, params url.Values, out any) error {
	q := url.Values{"cmd": {cmd}}
	for k, v := range params {
		q[k] = v
	}

	var envelope struct {
		Response *struct {
			Result  string          `json:"result"`
			Message string          `json:"message"`
			Data    json.RawMessage `json:"data"`
		} `json:"response"`
	}
	if err := c.base.Get(ctx, "/api/v2", q, &envelope); err != nil {
		return err
	}
	if envelope.Response == nil {
		return &clients.Error{Service: "Tautulli", Kind: clients.KindResponse, Message: "malformed response envelope; is this a Tautulli server?"}
	}

	r := envelope.Response
	if r.Result != "success" {
		msg := r.Message
		if msg == "" {
			msg = "no message given"
		}
		kind := clients.KindResponse
		if strings.Contains(strings.ToLower(msg), "apikey") || strings.Contains(strings.ToLower(msg), "api key") {
			kind = clients.KindAuth
		}
		return &clients.Error{Service: "Tautulli", Kind: kind, Message: "cmd=" + cmd + " failed: " + msg}
	}

	if out == nil || len(r.Data) == 0 || string(r.Data) == "null" {
		return nil
	}
	if err := json.Unmarshal(r.Data, out); err != nil {
		return &clients.Error{Service: "Tautulli", Kind: clients.KindResponse, Message: "unreadable data for cmd=" + cmd + ": " + err.Error()}
	}
	return nil
}
