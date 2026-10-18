package arr

import (
	"encoding/json"
	"time"

	"github.com/seanhamlin/tendarr/internal/media"
)

// Item is a Radarr movie or Sonarr series, flattened to what this tool needs.
type Item struct {
	Kind  Kind
	ID    int
	Title string
	Year  int
	IDs   media.ExternalIDs
	Path  string
	// RootFolderPath is where *arr stores this item; used to prove it manages
	// the same folder a Plex section reads.
	RootFolderPath   string
	HasFile          bool
	SizeOnDisk       *int64
	Monitored        bool
	Rating           *media.Rating
	Added            *time.Time
	QualityProfileID int
	Tags             []int

	// Radarr only: the file name Radarr believes it manages.
	FileRelativePath string

	// Sonarr only.
	Status            string
	PercentOfEpisodes *float64
	EpisodeFileCount  int
}

// HasTag reports whether the item carries tag id.
func (i Item) HasTag(id int) bool {
	for _, t := range i.Tags {
		if t == id {
			return true
		}
	}
	return false
}

// DisplayTitle is "Title (Year)".
func (i Item) DisplayTitle() string {
	return media.DisplayTitle(i.Title, i.Year)
}

// File is a Radarr movie file or Sonarr episode file, with the custom format
// scoring the list endpoints omit.
type File struct {
	ID                  int
	ParentID            int // movieId or seriesId
	SeasonNumber        int
	RelativePath        string
	Size                int64
	Codec               media.VideoCodec
	RawCodec            string
	Score               int
	Formats             []string
	Quality             string
	QualityCutoffNotMet bool
	Resolution          string
}

// Episode is a Sonarr episode, used to map episode files to searchable ids.
type Episode struct {
	ID            int
	SeriesID      int
	EpisodeFileID int
	Season        int
	Number        int
	Title         string
	Monitored     bool
}

// Tag is an *arr tag.
type Tag struct {
	ID    int    `json:"id"`
	Label string `json:"label"`
}

// Status is the response from /api/v3/system/status.
type Status struct {
	AppName string `json:"appName"`
	Version string `json:"version"`
}

// Release is one result from an interactive search.
type Release struct {
	GUID       string
	Title      string
	Indexer    string
	Size       int64
	Score      int
	Quality    string
	Rejected   bool
	Rejections []string
}

// DeleteResult distinguishes a real delete from one that had already happened.
type DeleteResult int

const (
	Deleted DeleteResult = iota + 1
	AlreadyGone
)

// --- wire formats ---

type wireRatingEntry struct {
	Value float64 `json:"value"`
	Votes *int    `json:"votes"`
}

type wireMovie struct {
	ID               int             `json:"id"`
	Title            string          `json:"title"`
	Year             int             `json:"year"`
	IMDbID           string          `json:"imdbId"`
	TMDbID           int             `json:"tmdbId"`
	HasFile          bool            `json:"hasFile"`
	SizeOnDisk       *int64          `json:"sizeOnDisk"`
	Monitored        bool            `json:"monitored"`
	Path             string          `json:"path"`
	RootFolderPath   string          `json:"rootFolderPath"`
	Added            string          `json:"added"`
	QualityProfileID int             `json:"qualityProfileId"`
	Tags             []int           `json:"tags"`
	Ratings          json.RawMessage `json:"ratings"`
	MovieFile        *struct {
		RelativePath string `json:"relativePath"`
	} `json:"movieFile"`
}

type wireSeries struct {
	ID               int             `json:"id"`
	Title            string          `json:"title"`
	Year             int             `json:"year"`
	IMDbID           string          `json:"imdbId"`
	TVDbID           int             `json:"tvdbId"`
	Monitored        bool            `json:"monitored"`
	Path             string          `json:"path"`
	RootFolderPath   string          `json:"rootFolderPath"`
	Added            string          `json:"added"`
	QualityProfileID int             `json:"qualityProfileId"`
	Tags             []int           `json:"tags"`
	Status           string          `json:"status"`
	Ratings          json.RawMessage `json:"ratings"`
	Statistics       *struct {
		SizeOnDisk        *int64   `json:"sizeOnDisk"`
		EpisodeFileCount  int      `json:"episodeFileCount"`
		PercentOfEpisodes *float64 `json:"percentOfEpisodes"`
	} `json:"statistics"`
}

type wireQuality struct {
	Quality struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	} `json:"quality"`
}

type wireFile struct {
	ID                  int         `json:"id"`
	MovieID             int         `json:"movieId"`
	SeriesID            int         `json:"seriesId"`
	SeasonNumber        int         `json:"seasonNumber"`
	RelativePath        string      `json:"relativePath"`
	Size                int64       `json:"size"`
	CustomFormatScore   int         `json:"customFormatScore"`
	QualityCutoffNotMet bool        `json:"qualityCutoffNotMet"`
	Quality             wireQuality `json:"quality"`
	CustomFormats       []struct {
		Name string `json:"name"`
	} `json:"customFormats"`
	MediaInfo *struct {
		VideoCodec string `json:"videoCodec"`
		Resolution string `json:"resolution"`
	} `json:"mediaInfo"`
}

type wireEpisode struct {
	ID            int    `json:"id"`
	SeriesID      int    `json:"seriesId"`
	EpisodeFileID int    `json:"episodeFileId"`
	SeasonNumber  int    `json:"seasonNumber"`
	EpisodeNumber int    `json:"episodeNumber"`
	Title         string `json:"title"`
	Monitored     bool   `json:"monitored"`
}

type wireRelease struct {
	GUID              string          `json:"guid"`
	Title             string          `json:"title"`
	Indexer           string          `json:"indexer"`
	Size              int64           `json:"size"`
	CustomFormatScore int             `json:"customFormatScore"`
	Rejected          bool            `json:"rejected"`
	Rejections        json.RawMessage `json:"rejections"`
	Quality           wireQuality     `json:"quality"`
}

func (w wireFile) toFile(kind Kind) File {
	f := File{
		ID:                  w.ID,
		SeasonNumber:        w.SeasonNumber,
		RelativePath:        w.RelativePath,
		Size:                w.Size,
		Score:               w.CustomFormatScore,
		Quality:             w.Quality.Quality.Name,
		QualityCutoffNotMet: w.QualityCutoffNotMet,
		Codec:               media.CodecUnknown,
	}
	if kind == Radarr {
		f.ParentID = w.MovieID
	} else {
		f.ParentID = w.SeriesID
	}
	if w.MediaInfo != nil {
		f.RawCodec = w.MediaInfo.VideoCodec
		f.Codec = media.NormaliseCodec(w.MediaInfo.VideoCodec)
		f.Resolution = w.MediaInfo.Resolution
	}
	for _, cf := range w.CustomFormats {
		if cf.Name != "" {
			f.Formats = append(f.Formats, cf.Name)
		}
	}
	return f
}

func parseTime(s string) *time.Time {
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil || t.Year() < 1971 {
		return nil
	}
	t = t.UTC()
	return &t
}

func positive(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

// ratingPreference is the order sources are tried in Radarr's nested shape.
var ratingPreference = []media.RatingSource{
	media.RatingIMDb,
	media.RatingTMDb,
	media.RatingTrakt,
	media.RatingRottenTomatoes,
	media.RatingMetacritic,
}

// parseRatings handles both *arr shapes.
//
// Radarr nests by source ({"imdb": {"value": 8.4, "votes": 981}, ...}) with
// some sources on 0-100. Sonarr is flat ({"value": 8.0, "votes": 42}). An
// unexpected shape yields nil rather than an error, because a nil rating is
// treated as protected downstream.
func parseRatings(raw json.RawMessage, flatSource media.RatingSource) *media.Rating {
	if len(raw) == 0 {
		return nil
	}

	var nested map[string]json.RawMessage
	if err := json.Unmarshal(raw, &nested); err != nil {
		return nil
	}

	for _, source := range ratingPreference {
		entryRaw, ok := nested[string(source)]
		if !ok {
			continue
		}
		var entry wireRatingEntry
		if json.Unmarshal(entryRaw, &entry) != nil || entry.Value <= 0 {
			continue
		}
		r := media.NewRating(entry.Value, source, entry.Votes)
		return &r
	}

	var flat wireRatingEntry
	if json.Unmarshal(raw, &flat) == nil && flat.Value > 0 {
		r := media.NewRating(flat.Value, flatSource, flat.Votes)
		return &r
	}

	return nil
}

func parseRejections(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var asStrings []string
	if json.Unmarshal(raw, &asStrings) == nil {
		return asStrings
	}
	var asObjects []struct {
		Reason string `json:"reason"`
	}
	if json.Unmarshal(raw, &asObjects) == nil {
		out := make([]string, 0, len(asObjects))
		for _, o := range asObjects {
			if o.Reason != "" {
				out = append(out, o.Reason)
			}
		}
		return out
	}
	return nil
}
