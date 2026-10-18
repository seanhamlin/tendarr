// Package arr is a client for Radarr and Sonarr's v3 API.
//
// One type serves both because the APIs are near-identical, and the places
// they differ are exactly where bugs hide: the import-exclusion parameter is
// addImportExclusion on Radarr but addImportListExclusion on Sonarr, and
// ASP.NET silently ignores a parameter it cannot bind, so the wrong name
// returns 200 and quietly does nothing. Every difference lives in this file.
package arr

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/seanhamlin/tendarr/internal/clients"
	"github.com/seanhamlin/tendarr/internal/media"
)

// Kind selects Radarr or Sonarr.
type Kind string

const (
	Radarr Kind = "radarr"
	Sonarr Kind = "sonarr"
)

// Label is the display name.
func (k Kind) Label() string {
	if k == Radarr {
		return "Radarr"
	}
	return "Sonarr"
}

// Client talks to one Radarr or Sonarr instance.
type Client struct {
	kind Kind
	base *clients.Base
	// search is used for interactive release searches, which wait on every
	// indexer and routinely take far longer than a list call.
	search *clients.Base
}

// New builds a client. apiKey is sent as the X-Api-Key header.
func New(kind Kind, rawURL, apiKey string) (*Client, error) {
	base, err := clients.NewBase(kind.Label(), rawURL, 60*time.Second)
	if err != nil {
		return nil, err
	}
	base.Headers["X-Api-Key"] = apiKey
	base.Secrets = []string{apiKey}

	search := *base
	search.HTTP = clients.NewHTTPClient(3 * time.Minute)

	return &Client{kind: kind, base: base, search: &search}, nil
}

// Kind returns which application this client talks to.
func (c *Client) Kind() Kind { return c.kind }

func (c *Client) resource() string {
	if c.kind == Radarr {
		// Singular. /api/v3/movies does not exist.
		return "/api/v3/movie"
	}
	return "/api/v3/series"
}

func (c *Client) exclusionParam() string {
	if c.kind == Radarr {
		return "addImportExclusion"
	}
	// Note the extra "List". Not a typo.
	return "addImportListExclusion"
}

// SystemStatus confirms the key works and that the URL points at the expected
// application rather than, say, Sonarr's URL pasted into the Radarr field.
func (c *Client) SystemStatus(ctx context.Context) (Status, error) {
	var s Status
	if err := c.base.Get(ctx, "/api/v3/system/status", nil, &s); err != nil {
		return s, err
	}
	if s.AppName != "" && !strings.EqualFold(s.AppName, c.kind.Label()) {
		return s, &clients.Error{
			Service: c.kind.Label(),
			Kind:    clients.KindResponse,
			Message: fmt.Sprintf("this URL is a %s instance, not %s", s.AppName, c.kind.Label()),
		}
	}
	return s, nil
}

// Items returns every movie or series.
func (c *Client) Items(ctx context.Context) ([]Item, error) {
	if c.kind == Radarr {
		var rows []wireMovie
		if err := c.base.Get(ctx, c.resource(), nil, &rows); err != nil {
			return nil, err
		}
		items := make([]Item, 0, len(rows))
		for _, r := range rows {
			if r.ID == 0 {
				continue
			}
			it := Item{
				Kind:  Radarr,
				ID:    r.ID,
				Title: r.Title,
				Year:  r.Year,
				// Radarr reports tmdbId 0 when unknown; that must never
				// become a lookup key or every unknown movie collides.
				IDs:              media.ExternalIDs{IMDb: r.IMDbID, TMDb: positive(r.TMDbID)},
				Path:             r.Path,
				RootFolderPath:   r.RootFolderPath,
				HasFile:          r.HasFile,
				SizeOnDisk:       r.SizeOnDisk,
				Monitored:        r.Monitored,
				Rating:           parseRatings(r.Ratings, media.RatingTMDb),
				Added:            parseTime(r.Added),
				QualityProfileID: r.QualityProfileID,
				Tags:             r.Tags,
			}
			if r.MovieFile != nil {
				it.FileRelativePath = r.MovieFile.RelativePath
			}
			items = append(items, it)
		}
		return items, nil
	}

	var rows []wireSeries
	if err := c.base.Get(ctx, c.resource(), nil, &rows); err != nil {
		return nil, err
	}
	items := make([]Item, 0, len(rows))
	for _, r := range rows {
		if r.ID == 0 {
			continue
		}
		it := Item{
			Kind:             Sonarr,
			ID:               r.ID,
			Title:            r.Title,
			Year:             r.Year,
			IDs:              media.ExternalIDs{IMDb: r.IMDbID, TVDb: positive(r.TVDbID)},
			Path:             r.Path,
			RootFolderPath:   r.RootFolderPath,
			Monitored:        r.Monitored,
			Rating:           parseRatings(r.Ratings, media.RatingTVDb),
			Added:            parseTime(r.Added),
			QualityProfileID: r.QualityProfileID,
			Tags:             r.Tags,
			Status:           r.Status,
		}
		if r.Statistics != nil {
			it.SizeOnDisk = r.Statistics.SizeOnDisk
			it.EpisodeFileCount = r.Statistics.EpisodeFileCount
			it.PercentOfEpisodes = r.Statistics.PercentOfEpisodes
			// Series carry no hasFile flag; at least one episode file on
			// disk is the equivalent signal.
			it.HasFile = r.Statistics.EpisodeFileCount > 0
		}
		items = append(items, it)
	}
	return items, nil
}

// QualityProfiles returns every profile keyed by id.
func (c *Client) QualityProfiles(ctx context.Context) (map[int]QualityProfile, error) {
	var rows []json.RawMessage
	if err := c.base.Get(ctx, "/api/v3/qualityprofile", nil, &rows); err != nil {
		return nil, err
	}
	out := make(map[int]QualityProfile, len(rows))
	for _, raw := range rows {
		if p, ok := parseProfile(raw); ok {
			out[p.ID] = p
		}
	}
	return out, nil
}

// MovieFiles returns Radarr file records keyed by movie id.
//
// Fetched separately from the movie list because customFormatScore appears
// only here. The endpoint accepts repeated movieId parameters, so it is
// chunked rather than issued per movie, keeping the query string well inside
// what a reverse proxy will accept.
func (c *Client) MovieFiles(ctx context.Context, movieIDs []int) (map[int]File, error) {
	if c.kind != Radarr {
		return nil, fmt.Errorf("MovieFiles is Radarr-only")
	}

	out := map[int]File{}
	const chunk = 100
	for start := 0; start < len(movieIDs); start += chunk {
		end := min(start+chunk, len(movieIDs))
		q := url.Values{}
		for _, id := range movieIDs[start:end] {
			q.Add("movieId", strconv.Itoa(id))
		}
		var rows []wireFile
		if err := c.base.Get(ctx, "/api/v3/moviefile", q, &rows); err != nil {
			return nil, err
		}
		for _, r := range rows {
			if r.ID != 0 && r.MovieID != 0 {
				out[r.MovieID] = r.toFile(Radarr)
			}
		}
	}
	return out, nil
}

// EpisodeFiles returns every episode file for one series.
func (c *Client) EpisodeFiles(ctx context.Context, seriesID int) ([]File, error) {
	if c.kind != Sonarr {
		return nil, fmt.Errorf("EpisodeFiles is Sonarr-only")
	}
	var rows []wireFile
	q := url.Values{"seriesId": {strconv.Itoa(seriesID)}}
	if err := c.base.Get(ctx, "/api/v3/episodefile", q, &rows); err != nil {
		return nil, err
	}
	out := make([]File, 0, len(rows))
	for _, r := range rows {
		if r.ID != 0 {
			out = append(out, r.toFile(Sonarr))
		}
	}
	return out, nil
}

// Episodes returns every episode for one series.
func (c *Client) Episodes(ctx context.Context, seriesID int) ([]Episode, error) {
	if c.kind != Sonarr {
		return nil, fmt.Errorf("Episodes is Sonarr-only")
	}
	var rows []wireEpisode
	q := url.Values{"seriesId": {strconv.Itoa(seriesID)}}
	if err := c.base.Get(ctx, "/api/v3/episode", q, &rows); err != nil {
		return nil, err
	}
	out := make([]Episode, 0, len(rows))
	for _, r := range rows {
		out = append(out, Episode{
			ID:            r.ID,
			SeriesID:      r.SeriesID,
			EpisodeFileID: r.EpisodeFileID,
			Season:        r.SeasonNumber,
			Number:        r.EpisodeNumber,
			Title:         r.Title,
			Monitored:     r.Monitored,
		})
	}
	return out, nil
}

// QueuedIDs returns ids with something already downloading: movie ids for
// Radarr, episode ids for Sonarr. Searching those again would queue a second
// copy of work already under way.
func (c *Client) QueuedIDs(ctx context.Context) (map[int]bool, error) {
	out := map[int]bool{}
	const pageSize = 500
	for page := 1; page <= 50; page++ {
		var resp struct {
			TotalRecords int `json:"totalRecords"`
			Records      []struct {
				MovieID   int `json:"movieId"`
				EpisodeID int `json:"episodeId"`
			} `json:"records"`
		}
		q := url.Values{
			"page":     {strconv.Itoa(page)},
			"pageSize": {strconv.Itoa(pageSize)},
		}
		if err := c.base.Get(ctx, "/api/v3/queue", q, &resp); err != nil {
			return nil, err
		}
		for _, r := range resp.Records {
			id := r.MovieID
			if c.kind == Sonarr {
				id = r.EpisodeID
			}
			if id != 0 {
				out[id] = true
			}
		}
		if len(resp.Records) < pageSize || page*pageSize >= resp.TotalRecords {
			break
		}
	}
	return out, nil
}

// Releases runs an interactive search for one movie id (Radarr) or episode id
// (Sonarr). This hits every indexer live and is slow.
func (c *Client) Releases(ctx context.Context, id int) ([]Release, error) {
	param := "movieId"
	if c.kind == Sonarr {
		param = "episodeId"
	}
	var rows []wireRelease
	q := url.Values{param: {strconv.Itoa(id)}}
	if err := c.search.Get(ctx, "/api/v3/release", q, &rows); err != nil {
		return nil, err
	}
	out := make([]Release, 0, len(rows))
	for _, r := range rows {
		out = append(out, Release{
			GUID:       r.GUID,
			Title:      r.Title,
			Indexer:    r.Indexer,
			Size:       r.Size,
			Score:      r.CustomFormatScore,
			Quality:    r.Quality.Quality.Name,
			Rejected:   r.Rejected,
			Rejections: parseRejections(r.Rejections),
		})
	}
	return out, nil
}

// Search asks *arr to search for, and grab, better releases. ids are movie
// ids for Radarr and episode ids for Sonarr. One command carries every id,
// because *arr queues and paces the work itself.
//
// The command id is checked rather than assumed: a search that silently never
// ran looks exactly like a search that found nothing.
func (c *Client) Search(ctx context.Context, ids []int) (int, error) {
	if len(ids) == 0 {
		return 0, fmt.Errorf("nothing to search")
	}
	body := map[string]any{}
	if c.kind == Radarr {
		body["name"] = "MoviesSearch"
		body["movieIds"] = ids
	} else {
		body["name"] = "EpisodeSearch"
		body["episodeIds"] = ids
	}

	var resp struct {
		ID int `json:"id"`
	}
	if _, err := c.base.Do(ctx, clients.Request{Method: http.MethodPost, Path: "/api/v3/command", Body: body}, &resp); err != nil {
		return 0, err
	}
	if resp.ID == 0 {
		return 0, &clients.Error{
			Service: c.kind.Label(),
			Kind:    clients.KindResponse,
			Message: "search was accepted but no command id was returned, so it cannot be confirmed",
		}
	}
	return resp.ID, nil
}

// Delete removes a movie or series and its files. A 404 means it is already
// gone, which is the outcome wanted, so it is reported rather than raised.
func (c *Client) Delete(ctx context.Context, id int, addExclusion bool) (DeleteResult, error) {
	q := url.Values{
		"deleteFiles":      {"true"},
		c.exclusionParam(): {strconv.FormatBool(addExclusion)},
	}
	status, err := c.base.Do(ctx, clients.Request{
		Method:      http.MethodDelete,
		Path:        fmt.Sprintf("%s/%d", c.resource(), id),
		Query:       q,
		AllowStatus: []int{http.StatusNotFound},
	}, nil)
	if err != nil {
		return 0, err
	}
	if status == http.StatusNotFound {
		return AlreadyGone, nil
	}
	return Deleted, nil
}

// Tags returns every tag.
func (c *Client) Tags(ctx context.Context) ([]Tag, error) {
	var tags []Tag
	err := c.base.Get(ctx, "/api/v3/tag", nil, &tags)
	return tags, err
}

// FindTag returns the id of the tag with this label, or 0 if none exists.
func (c *Client) FindTag(ctx context.Context, label string) (int, error) {
	tags, err := c.Tags(ctx)
	if err != nil {
		return 0, err
	}
	for _, t := range tags {
		if strings.EqualFold(t.Label, label) {
			return t.ID, nil
		}
	}
	return 0, nil
}

// EnsureTag returns the id of the tag with this label, creating it if needed.
func (c *Client) EnsureTag(ctx context.Context, label string) (int, error) {
	id, err := c.FindTag(ctx, label)
	if err != nil || id != 0 {
		return id, err
	}
	var created Tag
	if _, err := c.base.Do(ctx, clients.Request{
		Method: http.MethodPost,
		Path:   "/api/v3/tag",
		Body:   map[string]string{"label": label},
	}, &created); err != nil {
		return 0, err
	}
	if created.ID == 0 {
		return 0, &clients.Error{Service: c.kind.Label(), Kind: clients.KindResponse, Message: "tag was created but no id was returned"}
	}
	return created.ID, nil
}

// SetTag adds or removes a tag on several items in one editor call.
func (c *Client) SetTag(ctx context.Context, itemIDs []int, tagID int, add bool) error {
	if len(itemIDs) == 0 {
		return nil
	}
	mode := "remove"
	if add {
		mode = "add"
	}
	body := map[string]any{
		"tags":      []int{tagID},
		"applyTags": mode,
	}
	if c.kind == Radarr {
		body["movieIds"] = itemIDs
	} else {
		body["seriesIds"] = itemIDs
	}
	_, err := c.base.Do(ctx, clients.Request{
		Method: http.MethodPut,
		Path:   c.resource() + "/editor",
		Body:   body,
	}, nil)
	return err
}

// SetMonitored monitors or unmonitors items: movie ids for Radarr, episode ids
// for Sonarr. Unmonitored items are never upgraded, by this tool or by *arr's
// own scheduled searches, so it is the native way to say "leave this file be".
func (c *Client) SetMonitored(ctx context.Context, ids []int, monitored bool) error {
	if len(ids) == 0 {
		return nil
	}
	req := clients.Request{Method: http.MethodPut}
	if c.kind == Radarr {
		req.Path = "/api/v3/movie/editor"
		req.Body = map[string]any{"movieIds": ids, "monitored": monitored}
	} else {
		req.Path = "/api/v3/episode/monitor"
		req.Body = map[string]any{"episodeIds": ids, "monitored": monitored}
	}
	_, err := c.base.Do(ctx, req, nil)
	return err
}
