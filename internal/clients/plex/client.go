// Package plex is a client for Plex Media Server.
//
// Plex is the primary source for external ids, watch state and movie file
// sizes: one paginated request per section returns all three. Its blind spot
// is that lastViewedAt is scoped to the token's own user, so plays by managed
// or home accounts are invisible here. Tautulli covers that.
package plex

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/seanhamlin/tendarr/internal/clients"
	"github.com/seanhamlin/tendarr/internal/media"
)

const pageSize = 500

// MediaType is a Plex library type this tool understands.
type MediaType string

const (
	Movie MediaType = "movie"
	Show  MediaType = "show"
)

// Client talks to one Plex server.
type Client struct {
	base *clients.Base
}

// New builds a client. The token is sent as a header, never in the URL, so it
// cannot leak into access logs.
func New(rawURL, token string) (*Client, error) {
	base, err := clients.NewBase("Plex", rawURL, 60*time.Second)
	if err != nil {
		return nil, err
	}
	base.Headers["X-Plex-Token"] = token
	base.Secrets = []string{token}
	return &Client{base: base}, nil
}

// Identity is the server name and version.
type Identity struct {
	Name    string
	Version string
}

// Identity confirms the token works. The root endpoint is used rather than
// /identity because /identity answers without authentication and so proves
// nothing about the token.
func (c *Client) Identity(ctx context.Context) (Identity, error) {
	var resp struct {
		MediaContainer *struct {
			FriendlyName string `json:"friendlyName"`
			Version      string `json:"version"`
		} `json:"MediaContainer"`
	}
	if err := c.base.Get(ctx, "/", nil, &resp); err != nil {
		return Identity{}, err
	}
	if resp.MediaContainer == nil {
		return Identity{}, &clients.Error{Service: "Plex", Kind: clients.KindResponse, Message: "response had no MediaContainer; is this a Plex server?"}
	}
	return Identity{Name: resp.MediaContainer.FriendlyName, Version: resp.MediaContainer.Version}, nil
}

// Section is a movie or show library.
type Section struct {
	Key       string
	Title     string
	Type      MediaType
	Locations []string
}

// Sections returns movie and show libraries; music and photos are skipped.
func (c *Client) Sections(ctx context.Context) ([]Section, error) {
	var resp struct {
		MediaContainer *struct {
			Directory []struct {
				Key      clients.FlexString `json:"key"`
				Title    string             `json:"title"`
				Type     string             `json:"type"`
				Location []struct {
					Path string `json:"path"`
				} `json:"Location"`
			} `json:"Directory"`
		} `json:"MediaContainer"`
	}
	if err := c.base.Get(ctx, "/library/sections", nil, &resp); err != nil {
		return nil, err
	}
	if resp.MediaContainer == nil {
		return nil, &clients.Error{Service: "Plex", Kind: clients.KindResponse, Message: "response had no MediaContainer"}
	}

	var out []Section
	for _, d := range resp.MediaContainer.Directory {
		t := MediaType(d.Type)
		if d.Key == "" || (t != Movie && t != Show) {
			continue
		}
		s := Section{Key: string(d.Key), Title: d.Title, Type: t}
		for _, l := range d.Location {
			if l.Path != "" {
				s.Locations = append(s.Locations, l.Path)
			}
		}
		out = append(out, s)
	}
	return out, nil
}

// Item is one movie or show in a section.
type Item struct {
	RatingKey    string
	Title        string
	Year         int
	IDs          media.ExternalIDs
	GUIDs        []string
	AddedAt      *time.Time
	LastViewedAt *time.Time
	ViewCount    int
	// FileSize sums every part of every media entry. Nil for shows, which
	// carry no Media at series level.
	FileSize *int64
	// FilePath is the first part's path as the Plex server sees it.
	FilePath string
}

type wireItem struct {
	RatingKey    clients.FlexString `json:"ratingKey"`
	Title        string             `json:"title"`
	Year         clients.FlexInt    `json:"year"`
	GUID         string             `json:"guid"`
	AddedAt      clients.FlexInt    `json:"addedAt"`
	LastViewedAt clients.FlexInt    `json:"lastViewedAt"`
	ViewCount    clients.FlexInt    `json:"viewCount"`
	Guids        []struct {
		ID string `json:"id"`
	} `json:"Guid"`
	Media []struct {
		Part []struct {
			File string          `json:"file"`
			Size clients.FlexInt `json:"size"`
		} `json:"Part"`
	} `json:"Media"`
}

// SectionItems returns every item in a section, paginated. includeGuids=1 is
// what makes this one call per page rather than one per item.
func (c *Client) SectionItems(ctx context.Context, s Section) ([]Item, error) {
	typeFilter := "1"
	if s.Type == Show {
		typeFilter = "2"
	}

	var out []Item
	for offset := 0; ; offset += pageSize {
		var resp struct {
			MediaContainer *struct {
				Size      int        `json:"size"`
				TotalSize int        `json:"totalSize"`
				Metadata  []wireItem `json:"Metadata"`
			} `json:"MediaContainer"`
		}
		q := url.Values{
			"type":                   {typeFilter},
			"includeGuids":           {"1"},
			"X-Plex-Container-Start": {strconv.Itoa(offset)},
			"X-Plex-Container-Size":  {strconv.Itoa(pageSize)},
		}
		if err := c.base.Get(ctx, fmt.Sprintf("/library/sections/%s/all", url.PathEscape(s.Key)), q, &resp); err != nil {
			return nil, err
		}
		if resp.MediaContainer == nil {
			return nil, &clients.Error{Service: "Plex", Kind: clients.KindResponse, Message: "section listing had no MediaContainer"}
		}

		for _, w := range resp.MediaContainer.Metadata {
			if w.RatingKey == "" || w.Title == "" {
				continue
			}
			out = append(out, w.toItem())
		}

		total := resp.MediaContainer.TotalSize
		if total == 0 {
			total = resp.MediaContainer.Size
		}
		if len(resp.MediaContainer.Metadata) < pageSize || offset+pageSize >= total {
			break
		}
	}
	return out, nil
}

func (w wireItem) toItem() Item {
	var guids []string
	seen := map[string]bool{}
	add := func(g string) {
		if g != "" && !seen[g] {
			seen[g] = true
			guids = append(guids, g)
		}
	}
	for _, g := range w.Guids {
		add(g.ID)
	}
	// On a legacy-agent item the primary guid is the only place the external
	// id appears; on a new-agent item it is plex:// and ignored.
	add(w.GUID)

	it := Item{
		RatingKey:    string(w.RatingKey),
		Title:        w.Title,
		Year:         int(w.Year.Value),
		IDs:          media.ExternalIDsFromGUIDs(guids),
		GUIDs:        guids,
		AddedAt:      w.AddedAt.Time(),
		LastViewedAt: w.LastViewedAt.Time(),
		ViewCount:    int(w.ViewCount.Value),
	}

	var total int64
	found := false
	for _, m := range w.Media {
		for _, p := range m.Part {
			if it.FilePath == "" && p.File != "" {
				it.FilePath = p.File
			}
			if p.Size.Valid && p.Size.Value > 0 {
				total += p.Size.Value
				found = true
			}
		}
	}
	if found {
		it.FileSize = &total
	}
	return it
}
