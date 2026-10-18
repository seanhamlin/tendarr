package arr

import (
	"encoding/json"
	"sort"
)

// QualityProfile is reduced to what decides whether an upgrade is possible.
type QualityProfile struct {
	ID                int
	Name              string
	UpgradeAllowed    bool
	MinFormatScore    int
	CutoffFormatScore int
	// FormatScores maps custom format name to score.
	FormatScores map[string]int
	// CutoffQuality is the quality at which *arr stops upgrading on quality.
	CutoffQuality string
}

// MissingFormat is a positively scored format the current file lacks.
type MissingFormat struct {
	Name  string
	Score int
}

// MissingFormats lists positively scored formats absent from held, highest
// score first. This is where the headroom actually is.
func (p QualityProfile) MissingFormats(held []string) []MissingFormat {
	have := make(map[string]bool, len(held))
	for _, h := range held {
		have[h] = true
	}

	var missing []MissingFormat
	for name, score := range p.FormatScores {
		if score > 0 && !have[name] {
			missing = append(missing, MissingFormat{Name: name, Score: score})
		}
	}
	sort.Slice(missing, func(i, j int) bool {
		if missing[i].Score != missing[j].Score {
			return missing[i].Score > missing[j].Score
		}
		return missing[i].Name < missing[j].Name
	})
	return missing
}

type wireProfileItem struct {
	ID      *int              `json:"id"`
	Name    string            `json:"name"`
	Quality *wireQualityInfo  `json:"quality"`
	Items   []wireProfileItem `json:"items"`
}

type wireQualityInfo struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

type wireProfile struct {
	ID                int               `json:"id"`
	Name              string            `json:"name"`
	UpgradeAllowed    bool              `json:"upgradeAllowed"`
	Cutoff            int               `json:"cutoff"`
	MinFormatScore    int               `json:"minFormatScore"`
	CutoffFormatScore int               `json:"cutoffFormatScore"`
	Items             []wireProfileItem `json:"items"`
	FormatItems       []struct {
		Name  string `json:"name"`
		Score int    `json:"score"`
	} `json:"formatItems"`
}

func parseProfile(raw json.RawMessage) (QualityProfile, bool) {
	var w wireProfile
	if err := json.Unmarshal(raw, &w); err != nil || w.ID == 0 {
		return QualityProfile{}, false
	}

	p := QualityProfile{
		ID:                w.ID,
		Name:              w.Name,
		UpgradeAllowed:    w.UpgradeAllowed,
		MinFormatScore:    w.MinFormatScore,
		CutoffFormatScore: w.CutoffFormatScore,
		FormatScores:      map[string]int{},
	}
	for _, fi := range w.FormatItems {
		if fi.Name != "" {
			p.FormatScores[fi.Name] = fi.Score
		}
	}

	// The cutoff id refers to either a single quality or a quality group, so
	// both shapes are searched.
	for _, item := range w.Items {
		if item.ID != nil && *item.ID == w.Cutoff && item.Name != "" {
			p.CutoffQuality = item.Name
			break
		}
		if item.Quality != nil && item.Quality.ID == w.Cutoff {
			p.CutoffQuality = item.Quality.Name
			break
		}
	}

	return p, true
}
