package auto

// Off-peak scheduling (design docs/design/auto-mode.md §9): peak windows
// are declared per provider in a LiteLLM-flavored syntax (hours_utc ranges,
// weekdays, IANA timezone, cross-midnight wrap); a request outside them is
// off-peak. Misconfigured windows fail safe to "always peak" (standard
// tier: no relaxation, no off-peak marking) — a broken schedule must never
// widen concurrency or mislabel records.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Window is one daily peak window in the configured timezone. Hours are
// clock hours 0-23; Start > End wraps past midnight. Weekdays follow the
// 0=Sunday..6=Saturday convention; empty means every day.
type Window struct {
	StartHour int
	EndHour   int
	Weekdays  []int
}

// inWeekday reports whether t's weekday (in the window's timezone) is
// selected; an empty list selects every day.
func (w Window) inWeekday(t time.Time) bool {
	if len(w.Weekdays) == 0 {
		return true
	}
	wd := int(t.Weekday())
	for _, d := range w.Weekdays {
		if d == wd {
			return true
		}
	}
	return false
}

// inHours reports whether t's clock hour falls in the window. A window
// where Start == End matches nothing (a zero-length range is a config
// mistake, not "all day").
func (w Window) inHours(t time.Time) bool {
	h := t.Hour()
	if w.StartHour == w.EndHour {
		return false
	}
	if w.StartHour < w.EndHour {
		return h >= w.StartHour && h < w.EndHour
	}
	// Cross-midnight: e.g. 22-2 covers 22,23,0,1.
	return h >= w.StartHour || h < w.EndHour
}

// PeakWindows is the parsed peak schedule for one provider.
type PeakWindows struct {
	Windows []Window
	Loc     *time.Location
}

// InPeak reports whether t (converted to the schedule's timezone) falls in
// any declared window. An empty schedule means "always peak" — the
// fail-safe standard tier.
func (p PeakWindows) InPeak(t time.Time) bool {
	if len(p.Windows) == 0 {
		return true
	}
	if p.Loc != nil {
		t = t.In(p.Loc)
	}
	for _, w := range p.Windows {
		if w.inWeekday(t) && w.inHours(t) {
			return true
		}
	}
	return false
}

// IsOffPeak is InPeak negated: outside every declared window (or when the
// schedule is empty/misconfigured, which also reads standard-tier).
func (p PeakWindows) IsOffPeak(t time.Time) bool { return !p.InPeak(t) }

// offPeakSpec is the JSON form (LiteLLM-flavored syntax):
//
//	{"hours_utc": ["14-18", "22-2"], "weekdays": [1,2,3,4,5],
//	 "weekday_timezone": "Asia/Shanghai"}
//
// hours_utc entries are "H-H" clock-hour ranges interpreted in the
// configured timezone (the key name follows LiteLLM; the timezone field
// governs). Weekdays are 0=Sunday..6=Saturday.
type offPeakSpec struct {
	HoursUTC []string `json:"hours_utc"`
	Weekdays []int    `json:"weekdays"`
	Timezone string   `json:"weekday_timezone"`
}

// ParsePeakWindows parses one provider's peak schedule. Any error (bad
// range, unknown timezone, empty hours) is returned to the caller, whose
// fail-safe is the standard tier (design §9 配置错配 fail-safe).
func ParsePeakWindows(spec offPeakSpec) (PeakWindows, error) {
	out := PeakWindows{}
	if spec.Timezone != "" {
		loc, err := time.LoadLocation(spec.Timezone)
		if err != nil {
			return out, fmt.Errorf("auto: weekday_timezone %q: %w", spec.Timezone, err)
		}
		out.Loc = loc
	}
	for _, raw := range spec.HoursUTC {
		parts := strings.SplitN(strings.TrimSpace(raw), "-", 2)
		if len(parts) != 2 {
			return out, fmt.Errorf("auto: hours_utc %q: want \"H-H\"", raw)
		}
		if strings.TrimSpace(parts[0]) == strings.TrimSpace(parts[1]) {
			return out, fmt.Errorf("auto: hours_utc %q: zero-length range (rejected so the fail-safe stays standard tier)", raw)
		}
		start, err := strconv.Atoi(strings.TrimSpace(parts[0]))
		if err != nil || start < 0 || start > 23 {
			return out, fmt.Errorf("auto: hours_utc %q: bad start hour", raw)
		}
		end, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil || end < 0 || end > 23 {
			return out, fmt.Errorf("auto: hours_utc %q: bad end hour", raw)
		}
		out.Windows = append(out.Windows, Window{StartHour: start, EndHour: end, Weekdays: spec.Weekdays})
	}
	if len(out.Windows) == 0 {
		return out, fmt.Errorf("auto: hours_utc is empty")
	}
	return out, nil
}

// offPeakConfigFile is the on-disk config (serve --offpeak-config): peak
// schedules per provider. Absent providers fall back to the built-in
// defaults below.
type offPeakConfigFile struct {
	GLM      *offPeakSpec `json:"glm"`
	DeepSeek *offPeakSpec `json:"deepseek"`
}

// LoadOffPeakConfig reads the optional off-peak config file and returns the
// GLM and DeepSeek peak schedules. A missing file returns the built-in
// defaults; a malformed file returns an error whose caller-side fail-safe
// is the standard tier (no relaxation, no off-peak marking).
func LoadOffPeakConfig(path string) (glm, ds PeakWindows, err error) {
	glm, ds = DefaultGLMPeak(), DefaultDeepSeekPeak()
	if path == "" {
		return glm, ds, nil
	}
	raw, err := readFile(path)
	if err != nil {
		if os.IsNotExist(errors.Unwrap(err)) {
			return glm, ds, nil // absent config = built-in defaults
		}
		return glm, ds, err
	}
	var cfg offPeakConfigFile
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return glm, ds, fmt.Errorf("auto: parse %s: %w", path, err)
	}
	if cfg.GLM != nil {
		if glm, err = ParsePeakWindows(*cfg.GLM); err != nil {
			return glm, ds, err
		}
	}
	if cfg.DeepSeek != nil {
		if ds, err = ParsePeakWindows(*cfg.DeepSeek); err != nil {
			return glm, ds, err
		}
	}
	return glm, ds, nil
}

func readFile(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("auto: read %s: %w", path, err)
	}
	return b, nil
}

// DefaultGLMPeak is the GLM coding-plan peak: weekdays 14:00-18:00 UTC+8
// (design §9). Everything else — including weekends — is off-peak.
func DefaultGLMPeak() PeakWindows {
	spec := offPeakSpec{HoursUTC: []string{"14-18"}, Weekdays: []int{1, 2, 3, 4, 5}, Timezone: "Asia/Shanghai"}
	p, err := ParsePeakWindows(spec)
	if err != nil {
		panic("auto: built-in GLM peak windows must parse: " + err.Error())
	}
	return p
}

// DefaultDeepSeekPeak is the DeepSeek peak: weekdays 9-12 and 14-18 UTC+8
// (design §9). The two providers' peaks deliberately interleave; per §9,
// stacking both off-peak relaxations must not amplify one provider's peak
// into the other's.
func DefaultDeepSeekPeak() PeakWindows {
	spec := offPeakSpec{HoursUTC: []string{"9-12", "14-18"}, Weekdays: []int{1, 2, 3, 4, 5}, Timezone: "Asia/Shanghai"}
	p, err := ParsePeakWindows(spec)
	if err != nil {
		panic("auto: built-in DeepSeek peak windows must parse: " + err.Error())
	}
	return p
}
