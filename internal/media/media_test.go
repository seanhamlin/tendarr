package media

import (
	"testing"
	"time"
)

func TestParseSize(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"0", 0},
		{"1024", 1024},
		{"500M", 500 << 20},
		{"500MB", 500 << 20},
		{"500mib", 500 << 20},
		{"4G", 4 << 30},
		{"4 GB", 4 << 30},
		{"1.5T", 1536 << 30},
		{"10k", 10 << 10},
	}
	for _, c := range cases {
		got, err := ParseSize(c.in)
		if err != nil {
			t.Errorf("ParseSize(%q) error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseSize(%q) = %d, want %d", c.in, got, c.want)
		}
	}

	for _, bad := range []string{"", "G", "-1G", "4X", "four", "1e9G", "99999999999T"} {
		if _, err := ParseSize(bad); err == nil {
			t.Errorf("ParseSize(%q) succeeded, want error", bad)
		}
	}
}

func TestFormatBytes(t *testing.T) {
	cases := map[int64]string{
		0:           "0 B",
		512:         "512 B",
		1536:        "1.5 KB",
		4 << 30:     "4.0 GB",
		-1:          "-",
		11811160064: "11.0 GB",
	}
	for in, want := range cases {
		if got := FormatBytes(in); got != want {
			t.Errorf("FormatBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestParsePeriod(t *testing.T) {
	now := time.Date(2026, 3, 31, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		in       string
		want     time.Time
		describe string
	}{
		{"2y", time.Date(2024, 3, 31, 12, 0, 0, 0, time.UTC), "2 years"},
		{"18m", time.Date(2024, 10, 1, 12, 0, 0, 0, time.UTC), "18 months"}, // Go normalises 31 Sep to 1 Oct
		{"6w", now.AddDate(0, 0, -42), "6 weeks"},
		{"90d", now.AddDate(0, 0, -90), "90 days"},
		{"1 year", now.AddDate(-1, 0, 0), "1 year"},
		{"  30 DAYS ", now.AddDate(0, 0, -30), "30 days"},
	}
	for _, c := range cases {
		p, err := ParsePeriod(c.in)
		if err != nil {
			t.Errorf("ParsePeriod(%q) error: %v", c.in, err)
			continue
		}
		if got := p.Before(now); !got.Equal(c.want) {
			t.Errorf("ParsePeriod(%q).Before = %v, want %v", c.in, got, c.want)
		}
		if p.Describe() != c.describe {
			t.Errorf("ParsePeriod(%q).Describe = %q, want %q", c.in, p.Describe(), c.describe)
		}
	}

	for _, bad := range []string{"", "0d", "-2y", "2x", "y", "P2Y", "2.5y", "999999d"} {
		if _, err := ParsePeriod(bad); err == nil {
			t.Errorf("ParsePeriod(%q) succeeded, want error", bad)
		}
	}
}

func TestNormaliseCodec(t *testing.T) {
	cases := map[string]VideoCodec{
		"x264":       CodecAVC,
		"h264":       CodecAVC,
		"AVC":        CodecAVC,
		"x265":       CodecHEVC,
		"HEVC":       CodecHEVC,
		"h.265":      CodecHEVC,
		"AV1":        CodecAV1,
		"XviD":       CodecXvid,
		"DivX":       CodecXvid,
		"MPEG-4":     CodecXvid,
		"MPEG2VIDEO": CodecMPEG2,
		"VC1":        CodecVC1,
		"":           CodecUnknown,
		"ProRes":     CodecUnknown,
	}
	for in, want := range cases {
		if got := NormaliseCodec(in); got != want {
			t.Errorf("NormaliseCodec(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCodecLabel(t *testing.T) {
	cases := map[VideoCodec]string{CodecAVC: "H.264", CodecHEVC: "H.265", CodecAV1: "AV1", CodecXvid: "Xvid/DivX", CodecMPEG2: "MPEG-2", CodecUnknown: "Unknown"}
	for c, want := range cases {
		if got := c.Label(); got != want {
			t.Errorf("%s.Label() = %q, want %q", c, got, want)
		}
	}
}

func TestCodecPolicy(t *testing.T) {
	cases := []struct {
		policy CodecPolicy
		codec  VideoCodec
		want   bool
	}{
		{PolicyLegacy, CodecAVC, true},
		{PolicyLegacy, CodecXvid, true},
		{PolicyLegacy, CodecHEVC, false},
		{PolicyLegacy, CodecUnknown, false},
		{PolicyAVC, CodecAVC, true},
		{PolicyAVC, CodecXvid, false},
		{PolicyNonModern, CodecUnknown, true}, // the only policy that includes unknown
		{PolicyNonModern, CodecMPEG2, true},
		{PolicyNonModern, CodecAV1, false},
		{PolicyAny, CodecHEVC, true},
	}
	for _, c := range cases {
		if got := c.policy.Includes(c.codec); got != c.want {
			t.Errorf("%s.Includes(%s) = %v, want %v", c.policy, c.codec, got, c.want)
		}
	}
	if _, err := ParseCodecPolicy("bogus"); err == nil {
		t.Error("ParseCodecPolicy accepted an unknown policy")
	}
}

func TestExternalIDsFromGUIDs(t *testing.T) {
	cases := []struct {
		name  string
		guids []string
		want  ExternalIDs
	}{
		{"modern", []string{"imdb://tt0111161", "tmdb://278", "tvdb://121361", "plex://movie/5d776"}, ExternalIDs{IMDb: "tt0111161", TMDb: 278, TVDb: 121361}},
		{"legacy imdb", []string{"com.plexapp.agents.imdb://tt0111161?lang=en"}, ExternalIDs{IMDb: "tt0111161"}},
		{"legacy tmdb", []string{"com.plexapp.agents.themoviedb://278?lang=en"}, ExternalIDs{TMDb: 278}},
		{"legacy tvdb", []string{"com.plexapp.agents.thetvdb://121361?lang=en"}, ExternalIDs{TVDb: 121361}},
		{"first wins", []string{"imdb://tt0000001", "imdb://tt0000002"}, ExternalIDs{IMDb: "tt0000001"}},
		{"bare imdb fallback", []string{"local://something/tt1234567/x"}, ExternalIDs{IMDb: "tt1234567"}},
		{"plex only", []string{"plex://movie/abc"}, ExternalIDs{}},
	}
	for _, c := range cases {
		if got := ExternalIDsFromGUIDs(c.guids); got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestRating(t *testing.T) {
	votes := 120
	r := NewRating(91, RatingRottenTomatoes, &votes)
	if r.Value != 9.1 {
		t.Errorf("rotten tomatoes 91 normalised to %v, want 9.1", r.Value)
	}
	if !r.IsTrustworthy(100) || r.IsTrustworthy(121) {
		t.Error("vote threshold not applied correctly")
	}

	unknown := NewRating(8.4, RatingIMDb, nil)
	if unknown.IsTrustworthy(1) {
		t.Error("a rating with no vote count must not be trusted when a threshold is set")
	}
	if !unknown.IsTrustworthy(0) {
		t.Error("a zero threshold trusts everything")
	}
}

func TestDisplayTitle(t *testing.T) {
	if got := DisplayTitle("Dune", 2021); got != "Dune (2021)" {
		t.Errorf("got %q", got)
	}
	if got := DisplayTitle("Dune", 0); got != "Dune" {
		t.Errorf("got %q", got)
	}
}
