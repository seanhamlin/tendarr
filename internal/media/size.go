// Package media holds the small value types shared by every feature: sizes,
// durations, codecs, external ids and ratings.
package media

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

var sizePattern = regexp.MustCompile(`^(\d+(?:\.\d+)?)\s*([kmgt]?)(?:i?b)?$`)

// ParseSize turns "4G", "500MB", "1.5T" or a bare byte count into bytes.
// Units are binary (1G = 1024^3), matching how Radarr and Sonarr display sizes.
func ParseSize(input string) (int64, error) {
	value := strings.ToLower(strings.TrimSpace(input))
	if value == "" {
		return 0, fmt.Errorf("size cannot be empty")
	}

	m := sizePattern.FindStringSubmatch(value)
	if m == nil {
		return 0, fmt.Errorf("%q is not a valid size; use forms like 500M, 4G or 1.5T", input)
	}

	amount, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a valid size", input)
	}

	multiplier := map[string]float64{
		"":  1,
		"k": 1 << 10,
		"m": 1 << 20,
		"g": 1 << 30,
		"t": 1 << 40,
	}[m[2]]

	bytes := amount * multiplier
	if bytes > math.MaxInt64/2 {
		return 0, fmt.Errorf("%q is too large", input)
	}

	return int64(bytes), nil
}

// FormatBytes renders a byte count the way the *arr apps do: binary units,
// one decimal place.
func FormatBytes(bytes int64) string {
	if bytes < 0 {
		return "-"
	}

	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	value := float64(bytes)
	unit := 0
	for value >= 1024 && unit < len(units)-1 {
		value /= 1024
		unit++
	}

	if unit == 0 {
		return fmt.Sprintf("%d B", bytes)
	}

	return fmt.Sprintf("%.1f %s", value, units[unit])
}
