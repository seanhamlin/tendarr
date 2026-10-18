package settings

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// AutoUpgrade configures scheduled upgrades: asking Radarr/Sonarr to grab a
// release that a release check found, one item at a time.
//
// Like AutoCheck, one budget covers both services, since every upgrade is a
// search against the same indexers followed by a download.
type AutoUpgrade struct {
	Radarr bool
	Sonarr bool
	// Every is the gap between upgrades.
	Every time.Duration
}

// Enabled reports whether any service is scheduled.
func (a AutoUpgrade) Enabled() bool { return a.Radarr || a.Sonarr }

// Limits for the form.
const (
	MinUpgradeHours = 1
	MaxUpgradeHours = 168
)

const (
	keyUpgradeRadarr = "autoupgrade.radarr"
	keyUpgradeSonarr = "autoupgrade.sonarr"
	keyUpgradeHours  = "autoupgrade.every_hours"
)

// AutoUpgradeForm is the form as typed, so invalid input can be shown back.
type AutoUpgradeForm struct {
	Radarr     bool
	Sonarr     bool
	EveryHours string
}

// DefaultAutoUpgradeForm is off, at one upgrade every two hours.
func DefaultAutoUpgradeForm() AutoUpgradeForm {
	return AutoUpgradeForm{EveryHours: "2"}
}

func autoUpgradeFormFromMap(m map[string]string) AutoUpgradeForm {
	f := DefaultAutoUpgradeForm()
	f.Radarr = m[keyUpgradeRadarr] == "1"
	f.Sonarr = m[keyUpgradeSonarr] == "1"
	if v := m[keyUpgradeHours]; v != "" {
		f.EveryHours = v
	}
	return f
}

// ValidateAutoUpgrade parses and checks the form.
func ValidateAutoUpgrade(f AutoUpgradeForm) (AutoUpgrade, Errors) {
	errs := Errors{}
	au := AutoUpgrade{Radarr: f.Radarr, Sonarr: f.Sonarr}
	if n, err := strconv.Atoi(strings.TrimSpace(f.EveryHours)); err != nil || n < MinUpgradeHours || n > MaxUpgradeHours {
		errs["every_hours"] = fmt.Sprintf("must be a whole number of hours from %d to %d", MinUpgradeHours, MaxUpgradeHours)
	} else {
		au.Every = time.Duration(n) * time.Hour
	}
	return au, errs
}

// AutoUpgradeValues encodes validated settings for storage.
func AutoUpgradeValues(f AutoUpgradeForm) map[string]string {
	b := func(v bool) string {
		if v {
			return "1"
		}
		return "0"
	}
	return map[string]string{
		keyUpgradeRadarr: b(f.Radarr),
		keyUpgradeSonarr: b(f.Sonarr),
		keyUpgradeHours:  strings.TrimSpace(f.EveryHours),
	}
}
