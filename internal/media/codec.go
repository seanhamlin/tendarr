package media

import (
	"fmt"
	"strings"
)

// VideoCodec is a normalised codec.
//
// The *arr apps report mediaInfo.videoCodec as whatever the encoder wrote, so
// the same format arrives under several spellings: x264/h264/AVC are one
// thing, x265/h265/HEVC another. Comparing those strings directly is how half
// a library gets missed.
type VideoCodec string

const (
	CodecAVC     VideoCodec = "AVC"
	CodecHEVC    VideoCodec = "HEVC"
	CodecAV1     VideoCodec = "AV1"
	CodecXvid    VideoCodec = "Xvid"
	CodecMPEG2   VideoCodec = "MPEG-2"
	CodecVC1     VideoCodec = "VC-1"
	CodecUnknown VideoCodec = "unknown"
)

// NormaliseCodec maps a raw codec string to a VideoCodec.
func NormaliseCodec(raw string) VideoCodec {
	v := strings.ToLower(strings.TrimSpace(raw))
	if v == "" {
		return CodecUnknown
	}

	has := func(needles ...string) bool {
		for _, n := range needles {
			if strings.Contains(v, n) {
				return true
			}
		}
		return false
	}

	switch {
	case has("x265", "h265", "hevc", "h.265"):
		return CodecHEVC
	case has("av1"):
		return CodecAV1
	case has("x264", "h264", "h.264", "avc"):
		return CodecAVC
	case has("xvid", "divx", "mpeg4", "mpeg-4"):
		return CodecXvid
	case has("mpeg2", "mpeg-2"):
		return CodecMPEG2
	case has("vc1", "vc-1"):
		return CodecVC1
	default:
		return CodecUnknown
	}
}

// Label is the display name, using the H.26x names people know from release
// titles rather than the formal AVC/HEVC.
func (c VideoCodec) Label() string {
	switch c {
	case CodecAVC:
		return "H.264"
	case CodecHEVC:
		return "H.265"
	case CodecXvid:
		return "Xvid/DivX"
	case CodecUnknown:
		return "Unknown"
	default:
		return string(c)
	}
}

// IsModern reports whether the codec is already space-efficient, so there is
// nothing to gain by re-grabbing on codec grounds.
func (c VideoCodec) IsModern() bool {
	return c == CodecHEVC || c == CodecAV1
}

// CodecPolicy selects which codecs are worth re-grabbing.
type CodecPolicy string

const (
	// PolicyLegacy is x264/h264 plus the older Xvid/DivX encodes. The default.
	PolicyLegacy CodecPolicy = "legacy"
	// PolicyAVC is x264/h264 only.
	PolicyAVC CodecPolicy = "avc"
	// PolicyNonModern is anything not already HEVC or AV1, including unknown.
	PolicyNonModern CodecPolicy = "non-modern"
	// PolicyAny ignores the codec and relies purely on custom format score.
	PolicyAny CodecPolicy = "any"
)

// CodecPolicies lists every policy in display order.
var CodecPolicies = []CodecPolicy{PolicyLegacy, PolicyAVC, PolicyNonModern, PolicyAny}

// ParseCodecPolicy validates a policy name.
func ParseCodecPolicy(s string) (CodecPolicy, error) {
	for _, p := range CodecPolicies {
		if string(p) == s {
			return p, nil
		}
	}
	return "", fmt.Errorf("unknown codec policy %q", s)
}

// Includes reports whether a file in codec c falls under this policy.
func (p CodecPolicy) Includes(c VideoCodec) bool {
	switch p {
	case PolicyAVC:
		return c == CodecAVC
	case PolicyLegacy:
		return c == CodecAVC || c == CodecXvid
	case PolicyNonModern:
		// Unknown is deliberately included here and nowhere else: it may be
		// anything, so it is only searched when the policy is the broad one.
		return !c.IsModern()
	case PolicyAny:
		return true
	default:
		return false
	}
}

// Describe explains the policy for the filter form.
func (p CodecPolicy) Describe() string {
	switch p {
	case PolicyLegacy:
		return "x264/h264, Xvid/DivX"
	case PolicyAVC:
		return "x264/h264 only"
	case PolicyNonModern:
		return "anything not HEVC/AV1"
	case PolicyAny:
		return "any codec (score only)"
	default:
		return string(p)
	}
}
