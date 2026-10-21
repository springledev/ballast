package scheduler

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule is a parsed standard 5-field cron expression: minute hour
// day-of-month month day-of-week. Day-of-week accepts 0-7, with both
// 0 and 7 meaning Sunday.
//
// Day-of-month and day-of-week combine as in classic (Vixie) cron: when
// both fields are restricted, a time matches if EITHER field matches,
// not both. A field starting with "*" (such as "*" or "*/2") does not
// count as restricted for this rule, so "0 0 */2 * 1" means every other
// day of the month that is also a Monday. This is a well-known cron
// quirk, kept here for familiarity since the spec calls these "cron
// expressions" (§4).
type Schedule struct {
	expr                          string
	minute, hour, dom, month, dow fieldSet
	// domStar and dowStar are true when that field starts with "*";
	// see the day rules on Schedule.
	domStar, dowStar bool
	// fixedTime is true when neither the minute nor the hour field
	// starts with "*": the job runs at specific wall-clock times, so
	// it must not re-fire in an hour repeated by a daylight-saving
	// change (see IsDue).
	fixedTime bool
}

// fieldSet is the set of integer values a cron field allows.
type fieldSet map[int]bool

// ParseCron parses a standard 5-field cron expression (minute hour
// dom month dow). Supported term syntax per comma-separated field:
// "*", "*/step", "n", "n-m", "n-m/step".
func ParseCron(expr string) (Schedule, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return Schedule{}, fmt.Errorf("cron expression %q: want 5 space-separated fields (minute hour dom month dow), got %d", expr, len(fields))
	}

	minute, err := parseField(fields[0], 0, 59)
	if err != nil {
		return Schedule{}, fmt.Errorf("cron expression %q: minute field: %w", expr, err)
	}
	hour, err := parseField(fields[1], 0, 23)
	if err != nil {
		return Schedule{}, fmt.Errorf("cron expression %q: hour field: %w", expr, err)
	}
	dom, err := parseField(fields[2], 1, 31)
	if err != nil {
		return Schedule{}, fmt.Errorf("cron expression %q: day-of-month field: %w", expr, err)
	}
	month, err := parseField(fields[3], 1, 12)
	if err != nil {
		return Schedule{}, fmt.Errorf("cron expression %q: month field: %w", expr, err)
	}
	dow, err := parseField(fields[4], 0, 7)
	if err != nil {
		return Schedule{}, fmt.Errorf("cron expression %q: day-of-week field: %w", expr, err)
	}
	if dow[7] {
		dow[0] = true
		delete(dow, 7)
	}

	sched := Schedule{
		expr:    expr,
		minute:  minute,
		hour:    hour,
		dom:     dom,
		month:   month,
		dow:     dow,
		domStar: strings.HasPrefix(fields[2], "*"),
		dowStar: strings.HasPrefix(fields[4], "*"),

		fixedTime: !strings.HasPrefix(fields[0], "*") && !strings.HasPrefix(fields[1], "*"),
	}
	if _, ok := sched.nextWall(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)); !ok {
		return Schedule{}, fmt.Errorf("cron expression %q never fires: no real date has the chosen day of the month, month and day of the week", expr)
	}
	return sched, nil
}

// String returns the original expression text.
func (s Schedule) String() string { return s.expr }

// Matches reports whether t falls on this schedule, to minute
// precision, per t's own location.
func (s Schedule) Matches(t time.Time) bool {
	return s.minute[t.Minute()] && s.hour[t.Hour()] && s.month[int(t.Month())] && s.dayMatches(t)
}

// dayMatches applies the day-of-month/day-of-week rules to t's date,
// exactly as Vixie cron does: if either field starts with "*", both
// must match (a plain "*" matches every day, so the other field
// decides); otherwise either may.
func (s Schedule) dayMatches(t time.Time) bool {
	domMatch := s.dom[t.Day()]
	dowMatch := s.dow[int(t.Weekday())]
	if s.domStar || s.dowStar {
		return domMatch && dowMatch
	}
	return domMatch || dowMatch
}

// nextSearchLimit bounds nextWall's search, in years. The Gregorian
// calendar repeats exactly every 400 years (146097 days, a whole
// number of weeks), so a schedule with no firing in any 400-year span
// never fires at all, and one that can fire always does within it.
// Rare schedules really do need long spans: February 29th that is
// also a Sunday ("0 0 29 2 */7") can go 40 years between firings.
// Skipping whole months and days keeps even a full search cheap.
const nextSearchLimit = 400

// IsDue reports whether the schedule fired at least once in the
// window (lastRun, now], to minute precision: the minute lastRun falls
// in is excluded, since that firing is the one lastRun served. A zero
// lastRun (the service has never been backed up at all) is always due.
//
// The schedule is evaluated in now's location, whatever location
// lastRun carries (the DB stores UTC): "0 3 * * *" means 03:00 on the
// wall clock of the machine running Ballast, as users expect from cron.
//
// Daylight-saving changes follow classic (Vixie) cron:
//
//   - Clocks jump forward: wall-clock minutes that never happen (e.g.
//     02:30 on the spring-forward night) still count, so a job
//     scheduled inside the skipped hour runs right after the jump
//     instead of silently missing that night.
//   - Clocks fall back: a fixed-time job (neither minute nor hour
//     starts with "*") does not fire again during the repeated hour,
//     because that wall-clock time was already covered — even when
//     lastRun itself falls inside the repeated hour. Jobs with a
//     wildcard minute or hour (hourly, every 15 minutes, ...) keep
//     firing by real elapsed time through the repeated hour.
//
// Cost does not grow with the gap: the search skips whole months,
// days, and hours that cannot match, so a job last run years ago is
// as cheap to check as one last run a minute ago.
func (s Schedule) IsDue(now, lastRun time.Time) bool {
	if lastRun.IsZero() {
		return true
	}
	if !now.After(lastRun) {
		return false
	}
	last := lastRun.In(now.Location())
	// Work on the wall clock: every wall-clock minute in (from, to]
	// was either lived through or skipped by a jump forward since
	// lastRun; both count.
	from, to := maxWallReached(last), maxWallReached(now)
	if next, ok := s.nextWall(from); ok && !next.After(to) {
		return true
	}
	return !s.fixedTime && s.firesInRepeatedMinutes(last, now)
}

// Next returns the first firing strictly after from, in from's
// location, or false when the schedule never fires again.
func (s Schedule) Next(from time.Time) (time.Time, bool) {
	w, ok := s.nextWall(wallClock(from))
	if !ok {
		return time.Time{}, false
	}
	return time.Date(w.Year(), w.Month(), w.Day(), w.Hour(), w.Minute(), 0, 0, from.Location()), true
}

// Prev returns the latest firing at or before t, in t's location, or
// false when the schedule has no firing within the search limit. It
// tells which scheduled firing a moment belongs to (for example, which
// firing a run that started at t was for).
func (s Schedule) Prev(t time.Time) (time.Time, bool) {
	end := wallClock(t)
	limit := end.AddDate(-nextSearchLimit, 0, 0)
	// Look back over a window that doubles until it holds a firing, so a
	// frequent schedule is found at once and a rare one costs a few
	// extra searches, not one per minute.
	for span := 25 * time.Hour; ; span *= 2 {
		start := end.Add(-span)
		if start.Before(limit) {
			start = limit
		}
		var found time.Time
		for w, ok := s.nextWall(start.Add(-time.Minute)); ok && !w.After(end); w, ok = s.nextWall(w) {
			found = w
		}
		if !found.IsZero() {
			return time.Date(found.Year(), found.Month(), found.Day(), found.Hour(), found.Minute(), 0, 0, t.Location()), true
		}
		if !start.After(limit) {
			return time.Time{}, false
		}
	}
}

// Gap returns the time between the first two firings after from, so
// callers can size things (such as alert reminders) to the schedule.
// It reports false when the schedule does not fire twice.
func (s Schedule) Gap(from time.Time) (time.Duration, bool) {
	first, ok := s.nextWall(wallClock(from))
	if !ok {
		return 0, false
	}
	second, ok := s.nextWall(first)
	if !ok {
		return 0, false
	}
	return second.Sub(first), true
}

// maxWallReached returns the latest wall-clock minute shown at or
// before t. That is t's own wall clock, except inside an hour repeated
// by clocks falling back, where the wall clock already went further
// before the change.
func maxWallReached(t time.Time) time.Time {
	w := wallClock(t)
	if start, _ := t.ZoneBounds(); !start.IsZero() {
		if before := wallClock(start.Add(-time.Minute)); before.After(w) {
			w = before
		}
	}
	return w
}

// nextWall returns the first wall-clock minute strictly after from
// that matches the schedule, skipping whole months, days, and hours
// that cannot match. Both from and the result are wall-clock readings
// expressed in UTC (see wallClock), so no daylight-saving gaps occur.
func (s Schedule) nextWall(from time.Time) (time.Time, bool) {
	t := from.Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(nextSearchLimit, 0, 0)
	for !t.After(limit) {
		y, mo, d := t.Date()
		switch {
		case !s.month[int(mo)]:
			t = time.Date(y, mo+1, 1, 0, 0, 0, 0, time.UTC)
		case !s.dayMatches(t):
			t = time.Date(y, mo, d+1, 0, 0, 0, 0, time.UTC)
		case !s.hour[t.Hour()]:
			t = time.Date(y, mo, d, t.Hour()+1, 0, 0, 0, time.UTC)
		case !s.minute[t.Minute()]:
			t = t.Add(time.Minute)
		default:
			return t, true
		}
	}
	return time.Time{}, false
}

// firesInRepeatedMinutes reports whether the schedule matches any
// minute in (last, now] that repeats an earlier wall-clock minute
// because clocks fell back. Only non-fixed-time jobs fire there (see
// IsDue). It visits each zone change in the window once — about two a
// year — and only walks the minutes of repeated hours.
func (s Schedule) firesInRepeatedMinutes(last, now time.Time) bool {
	first := last.Truncate(time.Minute).Add(time.Minute)
	b, end := last.ZoneBounds()
	if b.IsZero() {
		// last is in the zone's first period: start at its end.
		b = end
	}
	for !b.IsZero() && !b.After(now) {
		_, offBefore := b.Add(-time.Second).Zone()
		_, offAfter := b.Zone()
		if back := time.Duration(offBefore-offAfter) * time.Second; back > 0 {
			t := b
			if t.Before(first) {
				t = first
			}
			for ; t.Before(b.Add(back)) && !t.After(now); t = t.Add(time.Minute) {
				if s.Matches(wallClock(t)) {
					return true
				}
			}
		}
		_, next := b.ZoneBounds()
		if !next.After(b) {
			break
		}
		b = next
	}
	return false
}

// wallClock returns t's wall-clock reading (date, hour, minute in t's
// own location) as a UTC time, so wall-clock minutes can be compared
// and stepped through without daylight-saving jumps.
func wallClock(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, time.UTC)
}

// parseField parses one comma-separated cron field into the set of
// matching integers within [min,max].
func parseField(spec string, min, max int) (fieldSet, error) {
	set := fieldSet{}
	for _, part := range strings.Split(spec, ",") {
		if part == "" {
			return nil, fmt.Errorf("empty term in %q", spec)
		}
		rangePart, step, err := splitStep(part)
		if err != nil {
			return nil, err
		}
		lo, hi := min, max
		if rangePart != "*" {
			lo, hi, err = parseRange(rangePart)
			if err != nil {
				return nil, err
			}
		}
		if lo < min || hi > max || lo > hi {
			return nil, fmt.Errorf("term %q out of range [%d,%d]", part, min, max)
		}
		for v := lo; v <= hi; v += step {
			set[v] = true
		}
	}
	return set, nil
}

// splitStep splits "a-b/step" or "*/step" into its range/wildcard
// part and step (defaulting to 1 when no step is given).
func splitStep(part string) (rangePart string, step int, err error) {
	if i := strings.IndexByte(part, '/'); i >= 0 {
		step, err = strconv.Atoi(part[i+1:])
		if err != nil || step <= 0 {
			return "", 0, fmt.Errorf("invalid step in %q", part)
		}
		return part[:i], step, nil
	}
	return part, 1, nil
}

// parseRange parses "n" or "n-m" into a lo,hi pair.
func parseRange(part string) (lo, hi int, err error) {
	if i := strings.IndexByte(part, '-'); i >= 0 {
		lo, err = strconv.Atoi(part[:i])
		if err != nil {
			return 0, 0, fmt.Errorf("invalid range start in %q", part)
		}
		hi, err = strconv.Atoi(part[i+1:])
		if err != nil {
			return 0, 0, fmt.Errorf("invalid range end in %q", part)
		}
		return lo, hi, nil
	}
	v, err := strconv.Atoi(part)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid value %q", part)
	}
	return v, v, nil
}
