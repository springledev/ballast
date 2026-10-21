package scheduler

import (
	"strings"
	"testing"
	"time"

	_ "time/tzdata" // hermetic America/New_York for the DST tests
)

func mustParse(t *testing.T, expr string) Schedule {
	t.Helper()
	s, err := ParseCron(expr)
	if err != nil {
		t.Fatalf("ParseCron(%q): %v", expr, err)
	}
	return s
}

func TestParseCronRejectsWrongFieldCount(t *testing.T) {
	for _, expr := range []string{"", "* * * *", "* * * * * *", "0 3 * *"} {
		if _, err := ParseCron(expr); err == nil {
			t.Errorf("ParseCron(%q) = nil error, want error", expr)
		}
	}
}

func TestParseCronRejectsOutOfRange(t *testing.T) {
	for _, expr := range []string{
		"60 * * * *", // minute out of range
		"* 24 * * *", // hour out of range
		"* * 32 * *", // dom out of range
		"* * 0 * *",  // dom out of range (0 invalid)
		"* * * 13 *", // month out of range
		"* * * * 8",  // dow out of range
		"* * * * *,", // trailing empty term
	} {
		if _, err := ParseCron(expr); err == nil {
			t.Errorf("ParseCron(%q) = nil error, want error", expr)
		}
	}
}

func TestParseCronRejectsGarbage(t *testing.T) {
	for _, expr := range []string{"a * * * *", "*/0 * * * *", "*/-1 * * * *", "1-2-3 * * * *"} {
		if _, err := ParseCron(expr); err == nil {
			t.Errorf("ParseCron(%q) = nil error, want error", expr)
		}
	}
}

func TestScheduleMatchesEveryMinute(t *testing.T) {
	s := mustParse(t, "* * * * *")
	if !s.Matches(time.Date(2026, 3, 5, 13, 47, 0, 0, time.UTC)) {
		t.Error("wildcard schedule should match any time")
	}
}

func TestScheduleMatchesNightly(t *testing.T) {
	s := mustParse(t, "0 3 * * *")
	if !s.Matches(time.Date(2026, 3, 5, 3, 0, 0, 0, time.UTC)) {
		t.Error("expected 03:00 to match nightly 0 3 * * *")
	}
	if s.Matches(time.Date(2026, 3, 5, 3, 1, 0, 0, time.UTC)) {
		t.Error("expected 03:01 not to match nightly 0 3 * * *")
	}
	if s.Matches(time.Date(2026, 3, 5, 4, 0, 0, 0, time.UTC)) {
		t.Error("expected 04:00 not to match nightly 0 3 * * *")
	}
}

func TestScheduleMatchesWeeklySunday(t *testing.T) {
	s := mustParse(t, "0 3 * * 0")
	// 2026-03-08 is a Sunday.
	if !s.Matches(time.Date(2026, 3, 8, 3, 0, 0, 0, time.UTC)) {
		t.Error("expected Sunday 03:00 to match weekly 0 3 * * 0")
	}
	if s.Matches(time.Date(2026, 3, 9, 3, 0, 0, 0, time.UTC)) {
		t.Error("expected Monday 03:00 not to match weekly 0 3 * * 0")
	}
}

func TestScheduleDayOfWeekAliasSeven(t *testing.T) {
	s := mustParse(t, "0 3 * * 7")
	if !s.Matches(time.Date(2026, 3, 8, 3, 0, 0, 0, time.UTC)) {
		t.Error("dow 7 should alias Sunday like dow 0")
	}
}

func TestScheduleStepAndList(t *testing.T) {
	s := mustParse(t, "*/15 8-10 * * 1,3,5")
	// Monday 2026-03-09 09:30 is within hour range and a 15-step minute.
	if !s.Matches(time.Date(2026, 3, 9, 9, 30, 0, 0, time.UTC)) {
		t.Error("expected match for step/range/list schedule")
	}
	if s.Matches(time.Date(2026, 3, 9, 9, 31, 0, 0, time.UTC)) {
		t.Error("minute 31 should not match */15")
	}
	if s.Matches(time.Date(2026, 3, 9, 11, 0, 0, 0, time.UTC)) {
		t.Error("hour 11 should not match 8-10")
	}
	// 2026-03-10 is Tuesday, not in 1,3,5 (Mon,Wed,Fri).
	if s.Matches(time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)) {
		t.Error("Tuesday should not match dow list 1,3,5")
	}
}

func TestScheduleDomDowOrQuirk(t *testing.T) {
	// Both restricted: matches on day-of-month 3 OR any Sunday.
	// 2026-03-01 is a Sunday, so 2026-03-03 is a Tuesday: dom-only match.
	s := mustParse(t, "0 3 3 * 0")
	if !s.Matches(time.Date(2026, 3, 3, 3, 0, 0, 0, time.UTC)) {
		t.Error("expected match on day-of-month 3 (a Sunday-independent OR branch)")
	}
	// 2026-03-08 is a Sunday but not day 3.
	if !s.Matches(time.Date(2026, 3, 8, 3, 0, 0, 0, time.UTC)) {
		t.Error("expected match on Sunday even though not day 3 (OR quirk)")
	}
	// 2026-03-09 is Monday and not day 3: neither side matches.
	if s.Matches(time.Date(2026, 3, 9, 3, 0, 0, 0, time.UTC)) {
		t.Error("expected no match when neither dom nor dow matches")
	}
}

func TestScheduleStarPrefixedDayFieldNeedsBoth(t *testing.T) {
	// Like Vixie cron, a day field starting with "*" is not
	// "restricted", so dom and dow must BOTH match. 2026-03-01 is a
	// Sunday.
	tests := []struct {
		expr string
		day  int // in March 2026, at 03:00 UTC
		want bool
	}{
		{"0 3 */2 * 1", 9, true},  // Monday, odd day
		{"0 3 */2 * 1", 2, false}, // Monday, even day
		{"0 3 */2 * 1", 3, false}, // odd day, Tuesday: would match under OR
		{"0 3 1 * */2", 1, true},  // day 1, Sunday
		{"0 3 1 * */2", 8, false}, // Sunday, not day 1: would match under OR
		{"0 3 1 * */2", 4, false}, // Wednesday, not day 1
	}
	for _, tt := range tests {
		s := mustParse(t, tt.expr)
		if got := s.Matches(time.Date(2026, 3, tt.day, 3, 0, 0, 0, time.UTC)); got != tt.want {
			t.Errorf("%q on March %d: Matches = %v, want %v", tt.expr, tt.day, got, tt.want)
		}
	}
}

func TestIsDueLongGapSchedules(t *testing.T) {
	tests := []struct {
		name    string
		expr    string
		lastRun time.Time
		now     time.Time
		want    bool
	}{
		{
			// Jan 1st on a Monday: 2007, then not until 2018.
			name:    "Jan 1st Monday, 11-year gap reached",
			expr:    "0 0 */31 1 1",
			lastRun: time.Date(2007, 1, 1, 0, 0, 0, 0, time.UTC),
			now:     time.Date(2018, 1, 1, 0, 0, 0, 0, time.UTC),
			want:    true,
		},
		{
			name:    "Jan 1st Monday, not yet inside the 11-year gap",
			expr:    "0 0 */31 1 1",
			lastRun: time.Date(2007, 1, 1, 0, 0, 0, 0, time.UTC),
			now:     time.Date(2017, 12, 31, 23, 59, 0, 0, time.UTC),
			want:    false,
		},
		{
			// Feb 29th on a Sunday: 2088, then (2100 is not a leap
			// year) not until 2128.
			name:    "Feb 29th Sunday, 40-year gap reached",
			expr:    "0 0 29 2 */7",
			lastRun: time.Date(2088, 2, 29, 0, 0, 0, 0, time.UTC),
			now:     time.Date(2128, 2, 29, 0, 0, 0, 0, time.UTC),
			want:    true,
		},
		{
			name:    "Feb 29th Sunday, not yet inside the 40-year gap",
			expr:    "0 0 29 2 */7",
			lastRun: time.Date(2088, 2, 29, 0, 0, 0, 0, time.UTC),
			now:     time.Date(2128, 2, 28, 23, 59, 0, 0, time.UTC),
			want:    false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mustParse(t, tt.expr).IsDue(tt.now, tt.lastRun); got != tt.want {
				t.Errorf("IsDue = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsDueNeverRun(t *testing.T) {
	s := mustParse(t, "0 3 * * *")
	if !s.IsDue(time.Date(2026, 3, 5, 12, 0, 0, 0, time.UTC), time.Time{}) {
		t.Error("a job that has never run must be due")
	}
}

func TestIsDueFiredSinceLastRun(t *testing.T) {
	s := mustParse(t, "0 3 * * *")
	lastRun := time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)
	now := time.Date(2026, 3, 5, 12, 0, 0, 0, time.UTC)
	if !s.IsDue(now, lastRun) {
		t.Error("schedule fired at 03:00 between last run and now: expected due")
	}
}

func TestIsDueNotFiredSinceLastRun(t *testing.T) {
	s := mustParse(t, "0 3 * * *")
	lastRun := time.Date(2026, 3, 5, 3, 30, 0, 0, time.UTC)
	now := time.Date(2026, 3, 5, 20, 0, 0, 0, time.UTC)
	if s.IsDue(now, lastRun) {
		t.Error("schedule already ran today after 03:00: expected not due")
	}
}

func TestIsDueOverdueByYears(t *testing.T) {
	s := mustParse(t, "0 3 * * *")
	lastRun := time.Date(2015, 1, 1, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 3, 5, 12, 0, 0, 0, time.UTC)
	if !s.IsDue(now, lastRun) {
		t.Error("a job overdue by years must be reported due")
	}
}

func TestIsDueRareSchedules(t *testing.T) {
	leapDay := mustParse(t, "0 0 29 2 *")
	tests := []struct {
		name    string
		lastRun time.Time
		now     time.Time
		want    bool
	}{
		{
			name:    "no Feb 29th between leap years",
			lastRun: time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC),
			now:     time.Date(2028, 2, 28, 23, 59, 0, 0, time.UTC),
			want:    false,
		},
		{
			name:    "next Feb 29th reached",
			lastRun: time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC),
			now:     time.Date(2028, 2, 29, 0, 0, 0, 0, time.UTC),
			want:    true,
		},
		{
			name:    "2100 is not a leap year: 8-year gap from 2096 to 2104",
			lastRun: time.Date(2096, 3, 1, 0, 0, 0, 0, time.UTC),
			now:     time.Date(2104, 2, 29, 0, 0, 0, 0, time.UTC),
			want:    true,
		},
		{
			name:    "now before lastRun is never due",
			lastRun: time.Date(2028, 3, 1, 0, 0, 0, 0, time.UTC),
			now:     time.Date(2028, 2, 29, 0, 0, 0, 0, time.UTC),
			want:    false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := leapDay.IsDue(tt.now, tt.lastRun); got != tt.want {
				t.Errorf("IsDue(now=%v, lastRun=%v) = %v, want %v", tt.now, tt.lastRun, got, tt.want)
			}
		})
	}
}

func TestIsDueCostDoesNotGrowWithGap(t *testing.T) {
	// A minute-by-minute scan of 30 years is ~16M steps per call; the
	// jump-ahead search needs a few thousand at most. Guarded by
	// allocations rather than wall-clock time so it cannot flake on a
	// loaded CI machine; BenchmarkIsDueLongGap tracks speed.
	s := mustParse(t, "0 0 29 2 *")
	lastRun := time.Date(1996, 3, 1, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 3, 5, 12, 0, 0, 0, time.UTC)
	allocs := testing.AllocsPerRun(10, func() { s.IsDue(now, lastRun) })
	if allocs > 100 {
		t.Errorf("IsDue allocated %.0f times for a 30-year gap; expected a jump-ahead search", allocs)
	}
}

func BenchmarkIsDueLongGap(b *testing.B) {
	s, err := ParseCron("30 2 * * 0")
	if err != nil {
		b.Fatal(err)
	}
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		b.Fatal(err)
	}
	lastRun := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 3, 5, 12, 0, 0, 0, ny)
	for i := 0; i < b.N; i++ {
		s.IsDue(now, lastRun)
	}
}

func TestParseCronRejectsSchedulesThatNeverFire(t *testing.T) {
	for _, expr := range []string{"0 0 30 2 *", "0 0 31 2 *", "0 0 31 4,6,9,11 *", "0 0 30,31 2 *", "0 0 30 2 */2"} {
		if _, err := ParseCron(expr); err == nil || !strings.Contains(err.Error(), "never fires") {
			t.Errorf("ParseCron(%q) error = %v, want a never-fires error", expr, err)
		}
	}
	for _, expr := range []string{"0 0 29 2 *", "0 0 30 2 1", "0 0 31 * *", "0 0 31 1-3 *", "0 0 29 2 */7", "0 0 */31 1 1"} {
		if _, err := ParseCron(expr); err != nil {
			t.Errorf("ParseCron(%q) = %v, want it accepted", expr, err)
		}
	}
}

func TestIsDueMinuteBoundaries(t *testing.T) {
	s := mustParse(t, "0 3 * * *")
	tests := []struct {
		name    string
		lastRun time.Time
		now     time.Time
		want    bool
	}{
		{
			name:    "run started inside the firing minute has served it",
			lastRun: time.Date(2026, 3, 5, 3, 0, 30, 0, time.UTC),
			now:     time.Date(2026, 3, 5, 3, 30, 0, 0, time.UTC),
			want:    false,
		},
		{
			name:    "run started exactly on the firing minute has served it",
			lastRun: time.Date(2026, 3, 5, 3, 0, 0, 0, time.UTC),
			now:     time.Date(2026, 3, 5, 3, 0, 59, 0, time.UTC),
			want:    false,
		},
		{
			name:    "run started the second before the firing minute has not",
			lastRun: time.Date(2026, 3, 5, 2, 59, 59, 0, time.UTC),
			now:     time.Date(2026, 3, 5, 3, 0, 10, 0, time.UTC),
			want:    true,
		},
		{
			name:    "firing minute not reached yet",
			lastRun: time.Date(2026, 3, 4, 3, 0, 0, 0, time.UTC),
			now:     time.Date(2026, 3, 5, 2, 59, 59, 0, time.UTC),
			want:    false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := s.IsDue(tt.now, tt.lastRun); got != tt.want {
				t.Errorf("IsDue(now=%v, lastRun=%v) = %v, want %v", tt.now, tt.lastRun, got, tt.want)
			}
		})
	}
}

func TestIsDueUsesNowsLocation(t *testing.T) {
	s := mustParse(t, "0 3 * * *")
	plus10 := time.FixedZone("UTC+10", 10*60*60)
	minus5 := time.FixedZone("UTC-5", -5*60*60)
	tests := []struct {
		name    string
		lastRun time.Time
		now     time.Time
		want    bool
	}{
		{
			// 16:00 UTC = 02:00 local; now 03:05 local. Local 03:00 fired.
			name:    "UTC+10: local 03:00 fired, UTC 03:00 did not",
			lastRun: time.Date(2026, 3, 5, 16, 0, 0, 0, time.UTC),
			now:     time.Date(2026, 3, 6, 3, 5, 0, 0, plus10),
			want:    true,
		},
		{
			// 02:00 UTC = 21:00 local; now 23:00 local = 04:00 UTC.
			// UTC 03:00 is in the window but local 03:00 is not.
			name:    "UTC-5: UTC 03:00 fired, local 03:00 did not",
			lastRun: time.Date(2026, 3, 5, 2, 0, 0, 0, time.UTC),
			now:     time.Date(2026, 3, 4, 23, 0, 0, 0, minus5),
			want:    false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := s.IsDue(tt.now, tt.lastRun); got != tt.want {
				t.Errorf("IsDue(now=%v, lastRun=%v) = %v, want %v", tt.now, tt.lastRun, got, tt.want)
			}
		})
	}
}

func TestIsDueAcrossDaylightSavingChanges(t *testing.T) {
	// America/New_York in 2026: clocks jump 02:00 EST → 03:00 EDT on
	// March 8, and fall back 02:00 EDT → 01:00 EST on November 1.
	// time/tzdata is embedded in the test binary, so this does not
	// depend on the host's zoneinfo.
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("loading America/New_York: %v", err)
	}
	edt := time.FixedZone("EDT", -4*60*60)
	est := time.FixedZone("EST", -5*60*60)
	// at pins an instant with an explicit offset (unambiguous even in
	// the repeated hour), then views it in New York.
	at := func(y int, m time.Month, d, hh, mm int, zone *time.Location) time.Time {
		return time.Date(y, m, d, hh, mm, 0, 0, zone).In(ny)
	}

	tests := []struct {
		name    string
		cron    string
		lastRun time.Time
		now     time.Time
		want    bool
	}{
		{
			name:    "fall back: fixed-time job does not re-run in the repeated hour",
			cron:    "30 1 * * *",
			lastRun: at(2026, time.November, 1, 1, 30, edt),
			now:     at(2026, time.November, 1, 1, 45, est),
			want:    false,
		},
		{
			name:    "fall back: fixed-time job runs normally the next night",
			cron:    "30 1 * * *",
			lastRun: at(2026, time.November, 1, 1, 30, edt),
			now:     at(2026, time.November, 2, 1, 31, est),
			want:    true,
		},
		{
			name:    "fall back: fixed-time job after the repeated hour still fires",
			cron:    "30 2 * * *",
			lastRun: at(2026, time.October, 31, 2, 30, edt),
			now:     at(2026, time.November, 1, 2, 31, est),
			want:    true,
		},
		{
			name:    "fall back: hourly job keeps firing through the repeated hour",
			cron:    "0 * * * *",
			lastRun: at(2026, time.November, 1, 1, 0, edt),
			now:     at(2026, time.November, 1, 1, 5, est),
			want:    true,
		},
		{
			// A late sweep (or a sweep that acted during the repeated
			// hour) must not make the repeat of 01:30 count as new.
			name:    "fall back: fixed-time job with lastRun inside the repeated hour",
			cron:    "30 1 * * *",
			lastRun: at(2026, time.November, 1, 1, 10, est),
			now:     at(2026, time.November, 1, 1, 45, est),
			want:    false,
		},
		{
			name:    "fall back: wildcard job with lastRun inside the repeated hour keeps firing",
			cron:    "*/15 * * * *",
			lastRun: at(2026, time.November, 1, 1, 10, est),
			now:     at(2026, time.November, 1, 1, 15, est),
			want:    true,
		},
		{
			name:    "fall back: wildcard job not due between its repeated firings",
			cron:    "0 * * * *",
			lastRun: at(2026, time.November, 1, 1, 0, est),
			now:     at(2026, time.November, 1, 1, 59, est),
			want:    false,
		},
		{
			name:    "spring forward: wildcard job in the skipped hour runs once after the jump",
			cron:    "* 2 * * *",
			lastRun: at(2026, time.March, 7, 3, 0, est),
			now:     at(2026, time.March, 8, 3, 0, edt),
			want:    true,
		},
		{
			name:    "spring forward: job in the skipped hour runs right after the jump",
			cron:    "30 2 * * *",
			lastRun: at(2026, time.March, 7, 2, 30, est),
			now:     at(2026, time.March, 8, 3, 0, edt),
			want:    true,
		},
		{
			name:    "spring forward: not due before the jump",
			cron:    "30 2 * * *",
			lastRun: at(2026, time.March, 7, 2, 30, est),
			now:     at(2026, time.March, 8, 1, 59, est),
			want:    false,
		},
		{
			name:    "spring forward: no second run once the skipped firing was served",
			cron:    "30 2 * * *",
			lastRun: at(2026, time.March, 8, 3, 0, edt),
			now:     at(2026, time.March, 8, 23, 0, edt),
			want:    false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := mustParse(t, tt.cron)
			if got := s.IsDue(tt.now, tt.lastRun); got != tt.want {
				t.Errorf("IsDue(now=%v, lastRun=%v) = %v, want %v", tt.now, tt.lastRun, got, tt.want)
			}
		})
	}
}

func TestScheduleGap(t *testing.T) {
	from := time.Date(2026, 10, 9, 5, 0, 15, 0, time.UTC)
	tests := []struct {
		expr string
		want time.Duration
	}{
		{"0 5 * * *", 24 * time.Hour},
		{"0 * * * *", time.Hour},
		{"*/15 * * * *", 15 * time.Minute},
		{"0 5 * * 1", 7 * 24 * time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.expr, func(t *testing.T) {
			s, err := ParseCron(tt.expr)
			if err != nil {
				t.Fatal(err)
			}
			if got, ok := s.Gap(from); !ok || got != tt.want {
				t.Fatalf("Gap = %v, %v; want %v", got, ok, tt.want)
			}
		})
	}
}

func TestScheduleNext(t *testing.T) {
	loc := time.FixedZone("test", 2*3600)
	s, err := ParseCron("30 4 * * *")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := s.Next(time.Date(2026, 10, 20, 12, 0, 0, 0, loc))
	want := time.Date(2026, 10, 21, 4, 30, 0, 0, loc)
	if !ok || !got.Equal(want) {
		t.Fatalf("Next = %v, %v; want %v", got, ok, want)
	}
}
