package auto

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// shanghai builds a fixed clock time expressed in UTC+8.
func shanghai(year int, month time.Month, day, hour, min int) time.Time {
	return time.Date(year, month, day, hour, min, 0, 0, time.FixedZone("UTC+8", 8*3600))
}

func mustParse(t *testing.T, spec offPeakSpec) PeakWindows {
	t.Helper()
	p, err := ParsePeakWindows(spec)
	if err != nil {
		t.Fatalf("parse %v: %v", spec, err)
	}
	return p
}

func TestWindowCrossMidnight(t *testing.T) {
	loc := time.UTC
	p := PeakWindows{Loc: loc, Windows: []Window{{StartHour: 22, EndHour: 2}}}
	// 23:00 and 01:00 are inside the 22-2 wrap; 03:00 and 21:00 are not.
	for _, tc := range []struct {
		h    int
		want bool
	}{{23, true}, {1, true}, {2, false}, {3, false}, {21, false}, {22, true}} {
		got := p.InPeak(time.Date(2026, 11, 25, tc.h, 0, 0, 0, loc)) // a Wednesday
		if got != tc.want {
			t.Errorf("hour %d in-peak = %v, want %v", tc.h, got, tc.want)
		}
	}
}

func TestWindowZeroLengthRejected(t *testing.T) {
	// A zero-length range is a config mistake: rejected at parse so the
	// caller fail-safes to the standard tier instead of matching nothing
	// (which would silently widen every hour into off-peak).
	if _, err := ParsePeakWindows(offPeakSpec{HoursUTC: []string{"9-9"}}); err == nil {
		t.Error("9-9 window should be rejected at parse")
	}
}

func TestWindowWeekdaySelection(t *testing.T) {
	// Mon-Fri only: Saturday 2026-11-28 hour 15 is outside, Monday 2026-11-30 hour 15 inside.
	p := mustParse(t, offPeakSpec{HoursUTC: []string{"14-18"}, Weekdays: []int{1, 2, 3, 4, 5}})
	if p.InPeak(shanghai(2026, 11, 28, 15, 0)) {
		t.Error("Saturday 15:00 should not be in peak")
	}
	if !p.InPeak(shanghai(2026, 11, 30, 15, 0)) {
		t.Error("Monday 15:00 should be in peak")
	}
}

func TestWindowIANATimezone(t *testing.T) {
	// Peak 14-18 Asia/Shanghai. 06:30 UTC on a weekday = 14:30 Shanghai -> peak.
	p := mustParse(t, offPeakSpec{HoursUTC: []string{"14-18"}, Weekdays: []int{1, 2, 3, 4, 5}, Timezone: "Asia/Shanghai"})
	utc := time.Date(2026, 11, 25, 6, 30, 0, 0, time.UTC)
	if !p.InPeak(utc) {
		t.Error("06:30 UTC (14:30 Shanghai, Wednesday) should be in peak")
	}
	// 05:59 UTC = 13:59 Shanghai -> not peak; 10:01 UTC = 18:01 Shanghai -> not peak.
	if p.InPeak(time.Date(2026, 11, 25, 5, 59, 0, 0, time.UTC)) {
		t.Error("13:59 Shanghai should be outside the 14-18 peak")
	}
	if p.InPeak(time.Date(2026, 11, 25, 10, 1, 0, 0, time.UTC)) {
		t.Error("18:01 Shanghai should be outside the 14-18 peak")
	}
}

func TestParseErrorsFailSafe(t *testing.T) {
	for name, spec := range map[string]offPeakSpec{
		"empty hours":      {HoursUTC: []string{}},
		"bad range":        {HoursUTC: []string{"14"}},
		"hour too big":     {HoursUTC: []string{"14-24"}},
		"hour negative":    {HoursUTC: []string{"-1-5"}},
		"unknown timezone": {HoursUTC: []string{"14-18"}, Timezone: "Mars/Olympus"},
	} {
		if _, err := ParsePeakWindows(spec); err == nil {
			t.Errorf("%s: expected parse error, got nil", name)
		}
	}
	// Empty schedule means always peak = off-peak disabled.
	var empty PeakWindows
	if !empty.InPeak(time.Now()) {
		t.Error("empty schedule should always read as peak")
	}
}

func TestDefaultWindows(t *testing.T) {
	glm, ds := DefaultGLMPeak(), DefaultDeepSeekPeak()
	// GLM peak: weekdays 14-18 UTC+8. Weekend is off-peak even mid-window.
	if glm.IsOffPeak(shanghai(2026, 11, 25, 14, 0)) || glm.IsOffPeak(shanghai(2026, 11, 25, 17, 59)) {
		t.Error("Wednesday 14:00-17:59 should be GLM peak")
	}
	if !glm.IsOffPeak(shanghai(2026, 11, 25, 13, 59)) || !glm.IsOffPeak(shanghai(2026, 11, 25, 18, 0)) {
		t.Error("13:59/18:00 should be GLM off-peak")
	}
	if !glm.IsOffPeak(shanghai(2026, 11, 28, 15, 0)) {
		t.Error("Saturday 15:00 should be GLM off-peak (weekends included)")
	}
	// DeepSeek peak: weekdays 9-12 and 14-18; the 12-14 lunch gap is off-peak.
	if ds.IsOffPeak(shanghai(2026, 11, 25, 9, 0)) || ds.IsOffPeak(shanghai(2026, 11, 25, 14, 0)) {
		t.Error("9:00/14:00 should be DeepSeek peak")
	}
	if !ds.IsOffPeak(shanghai(2026, 11, 25, 13, 0)) {
		t.Error("13:00 lunch gap should be DeepSeek off-peak")
	}
}

func TestLoadOffPeakConfig(t *testing.T) {
	// Missing file -> built-in defaults, no error.
	glm, _, err := LoadOffPeakConfig(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("missing config: %v", err)
	}
	_ = glm
	// Malformed file -> error (caller fail-safes to standard tier).
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadOffPeakConfig(bad); err == nil {
		t.Error("malformed config should error")
	}
	// Valid override parses and replaces only the given provider.
	good := filepath.Join(t.TempDir(), "ok.json")
	body := `{"glm":{"hours_utc":["1-4"],"weekdays":[6,0],"weekday_timezone":"UTC"}}`
	if err := os.WriteFile(good, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	glm2, ds2, err := LoadOffPeakConfig(good)
	if err != nil {
		t.Fatalf("valid config: %v", err)
	}
	if !glm2.InPeak(time.Date(2026, 11, 28, 2, 0, 0, 0, time.UTC)) { // Saturday 02:00 UTC
		t.Error("override 1-4 weekends should put Saturday 02:00 UTC in peak")
	}
	if ds2.Loc == nil || ds2.Loc.String() != "Asia/Shanghai" {
		t.Error("deepseek defaults should survive a glm-only override")
	}
}
