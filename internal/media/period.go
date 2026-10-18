package media

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Period is a calendar duration such as "2y" or "90d".
//
// Kept as calendar units rather than a time.Duration because "18 months ago"
// is a calendar question; converting to hours first quietly drifts by days
// across leap years and month lengths.
type Period struct {
	Years, Months, Days int
	raw                 string
}

var periodPattern = regexp.MustCompile(`^(\d+)\s*([a-z]+)$`)

// ParsePeriod accepts "2y", "18m", "6w", "90d" and their longer spellings.
func ParsePeriod(input string) (Period, error) {
	value := strings.ToLower(strings.TrimSpace(input))
	if value == "" {
		return Period{}, fmt.Errorf("duration cannot be empty")
	}

	m := periodPattern.FindStringSubmatch(value)
	if m == nil {
		return Period{}, fmt.Errorf("%q is not a valid duration; use forms like 2y, 18m, 6w or 90d", input)
	}

	amount, err := strconv.Atoi(m[1])
	if err != nil || amount <= 0 {
		return Period{}, fmt.Errorf("duration %q must be greater than zero", input)
	}
	if amount > 100*366 {
		return Period{}, fmt.Errorf("duration %q is unreasonably long", input)
	}

	p := Period{raw: fmt.Sprintf("%d%s", amount, m[2][:1])}
	switch m[2] {
	case "y", "yr", "yrs", "year", "years":
		p.Years = amount
	case "m", "mo", "mon", "month", "months":
		p.Months = amount
	case "w", "wk", "wks", "week", "weeks":
		p.Days = amount * 7
	case "d", "day", "days":
		p.Days = amount
	default:
		return Period{}, fmt.Errorf("%q has an unknown unit; use y, m, w or d", input)
	}

	return p, nil
}

// MustPeriod is ParsePeriod for compile-time constants.
func MustPeriod(input string) Period {
	p, err := ParsePeriod(input)
	if err != nil {
		panic(err)
	}
	return p
}

// Before returns the instant this period before t.
func (p Period) Before(t time.Time) time.Time {
	return t.AddDate(-p.Years, -p.Months, -p.Days)
}

// String returns the canonical short form, e.g. "2y".
func (p Period) String() string {
	return p.raw
}

// Describe returns a human form, e.g. "2 years".
func (p Period) Describe() string {
	plural := func(n int, unit string) string {
		if n == 1 {
			return fmt.Sprintf("1 %s", unit)
		}
		return fmt.Sprintf("%d %ss", n, unit)
	}

	switch {
	case p.Years > 0:
		return plural(p.Years, "year")
	case p.Months > 0:
		return plural(p.Months, "month")
	case p.Days > 0 && p.Days%7 == 0:
		return plural(p.Days/7, "week")
	default:
		return plural(p.Days, "day")
	}
}

// DaysBetween counts whole days from a to b.
func DaysBetween(a, b time.Time) int {
	if b.Before(a) {
		return 0
	}
	return int(b.Sub(a).Hours() / 24)
}
