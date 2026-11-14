// Package cron parses and evaluates Cloudflare Workers cron trigger
// expressions (wrangler.jsonc "triggers": {"crons": [...]}).
//
// The syntax matches Cloudflare's: five fields, evaluated in UTC, with
// minute granularity:
//
//	field         values                         characters
//	minute        0-59                           * , - /
//	hour          0-23                           * , - /
//	day of month  1-31                           * , - / L W
//	month         1-12, JAN-DEC                  * , - /
//	day of week   1-7 (1 = Sunday), SUN-SAT      * , - / L #
//
// Note that day-of-week numbering is 1-7 with 1 = Sunday (Quartz style),
// not the 0-6 of Vixie cron; 0 is rejected so that a schedule copied from a
// crontab fails loudly instead of running on the wrong day. Names are
// case-insensitive.
//
// Extensions:
//
//	L      (day of month) the last day of the month
//	LW     (day of month) the last weekday (Mon-Fri) of the month
//	nW     (day of month) the weekday nearest day n, within the same month
//	L      (day of week)  Saturday (Quartz)
//	nL     (day of week)  the last weekday n of the month ("6L", "friL")
//	n#k    (day of week)  the k-th (1-5) weekday n of the month ("2#1")
//
// When both day of month and day of week are restricted (neither starts
// with "*"), a day matches if either matches, as in Vixie cron.
//
// Parsing is strict and bounded: the expression is at most MaxLen bytes,
// every number is range-checked, and a schedule that can never fire (e.g.
// "0 0 30 2 *") is rejected.
package cron

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// MaxLen bounds an expression's length.
const MaxLen = 128

// MaxTriggers bounds the cron triggers of one workload.
const MaxTriggers = 10

// horizon bounds Next's search (and Parse's "never fires" check). The
// rarest expressible schedules pin a date to a weekday (e.g. Feb 29 on a
// Monday, via "*/28" and "mon"); in the Gregorian calendar any such
// coincidence recurs within 40 years, so 50 years finds every satisfiable
// schedule from any starting point.
const horizon = 50 * 366 * 24 * time.Hour

// Schedule is a parsed cron expression.
type Schedule struct {
	expr   string
	minute uint64 // bit i: minute i
	hour   uint64 // bit i: hour i
	month  uint64 // bit i: month i (1-12)
	dom    dayField
	dow    dayField
}

// dayField is a day-of-month or day-of-week field: a bitset plus the
// special predicates (L, W, #).
type dayField struct {
	star bool   // the field starts with "*": unrestricted for the OR rule
	bits uint64 // dom: bit d (1-31); dow: bit w (1-7, 1 = Sunday)
	// day of month
	last        bool  // L
	lastWeekday bool  // LW
	nearest     []int // nW
	// day of week
	lastOf []int    // nL: weekday n (1-7)
	nth    [][2]int // n#k: weekday n, k-th occurrence
}

// String returns the expression as written.
func (s *Schedule) String() string { return s.expr }

// Parse parses a cron expression.
func Parse(expr string) (*Schedule, error) {
	if len(expr) > MaxLen {
		return nil, fmt.Errorf("cron %q: longer than %d bytes", trunc(expr), MaxLen)
	}
	for i := 0; i < len(expr); i++ {
		if c := expr[i]; c < 0x20 && c != '\t' || c >= 0x7f {
			return nil, fmt.Errorf("cron %q: invalid character %q", expr, c)
		}
	}
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron %q: want 5 fields (minute hour day-of-month month day-of-week), got %d", expr, len(fields))
	}
	s := &Schedule{expr: expr}
	var err error
	if s.minute, err = parseSimple(fields[0], 0, 59, nil); err != nil {
		return nil, fmt.Errorf("cron %q: minute: %w", expr, err)
	}
	if s.hour, err = parseSimple(fields[1], 0, 23, nil); err != nil {
		return nil, fmt.Errorf("cron %q: hour: %w", expr, err)
	}
	if s.dom, err = parseDOM(fields[2]); err != nil {
		return nil, fmt.Errorf("cron %q: day of month: %w", expr, err)
	}
	if s.month, err = parseSimple(fields[3], 1, 12, monthNames); err != nil {
		return nil, fmt.Errorf("cron %q: month: %w", expr, err)
	}
	if s.dow, err = parseDOW(fields[4]); err != nil {
		return nil, fmt.Errorf("cron %q: day of week: %w", expr, err)
	}
	if _, ok := s.Next(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)); !ok {
		return nil, fmt.Errorf("cron %q: never fires", expr)
	}
	return s, nil
}

// ValidateTriggers checks a workload's cron trigger list: at most
// MaxTriggers, no duplicates, each one valid.
func ValidateTriggers(crons []string) error {
	if len(crons) > MaxTriggers {
		return fmt.Errorf("triggers.crons: %d triggers, at most %d are allowed", len(crons), MaxTriggers)
	}
	seen := map[string]bool{}
	for _, c := range crons {
		if _, err := Parse(c); err != nil {
			return fmt.Errorf("triggers.crons: %w", err)
		}
		if seen[c] {
			return fmt.Errorf("triggers.crons: %q is listed twice", c)
		}
		seen[c] = true
	}
	return nil
}

var monthNames = map[string]int{"JAN": 1, "FEB": 2, "MAR": 3, "APR": 4, "MAY": 5, "JUN": 6, "JUL": 7, "AUG": 8, "SEP": 9, "OCT": 10, "NOV": 11, "DEC": 12}
var dayNames = map[string]int{"SUN": 1, "MON": 2, "TUE": 3, "WED": 4, "THU": 5, "FRI": 6, "SAT": 7}

// parseSimple parses a field of "*", numbers, names, ranges, steps and lists.
func parseSimple(f string, lo, hi int, names map[string]int) (uint64, error) {
	var bits uint64
	for _, item := range strings.Split(f, ",") {
		b, err := parseItem(item, lo, hi, names)
		if err != nil {
			return 0, err
		}
		bits |= b
	}
	return bits, nil
}

// parseItem parses one list item: "*", "n", "a-b", with an optional "/step".
func parseItem(item string, lo, hi int, names map[string]int) (uint64, error) {
	if item == "" {
		return 0, errors.New("empty list item")
	}
	rng, stepS, hasStep := strings.Cut(item, "/")
	step := 1
	if hasStep {
		n, err := parseNum(stepS, 1, hi-lo+1, nil)
		if err != nil {
			return 0, fmt.Errorf("step %q: %w", stepS, err)
		}
		step = n
	}
	var a, b int
	switch {
	case rng == "*":
		a, b = lo, hi
	case strings.Contains(rng, "-"):
		as, bs, _ := strings.Cut(rng, "-")
		var err error
		if a, err = parseNum(as, lo, hi, names); err != nil {
			return 0, err
		}
		if b, err = parseNum(bs, lo, hi, names); err != nil {
			return 0, err
		}
		if b < a {
			return 0, fmt.Errorf("range %q runs backwards", rng)
		}
	default:
		n, err := parseNum(rng, lo, hi, names)
		if err != nil {
			return 0, err
		}
		a, b = n, n
		if hasStep {
			b = hi // "a/n": from a to the end, every n
		}
	}
	var bits uint64
	for i := a; i <= b; i += step {
		bits |= 1 << uint(i)
	}
	return bits, nil
}

// parseNum parses a decimal number (at most 2 digits) or a name, in [lo, hi].
func parseNum(s string, lo, hi int, names map[string]int) (int, error) {
	if names != nil {
		if n, ok := names[strings.ToUpper(s)]; ok {
			return n, nil
		}
	}
	if s == "" || len(s) > 2 {
		return 0, fmt.Errorf("%q is not a number in %d-%d", s, lo, hi)
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, fmt.Errorf("%q is not a number in %d-%d", s, lo, hi)
		}
	}
	n, _ := strconv.Atoi(s)
	if n < lo || n > hi {
		if names != nil && lo == 1 && hi == 7 && n == 0 {
			return 0, errors.New("0 is not a day of week: days are 1-7 with 1 = Sunday (or use SUN-SAT)")
		}
		return 0, fmt.Errorf("%d is out of range %d-%d", n, lo, hi)
	}
	return n, nil
}

func parseDOM(f string) (dayField, error) {
	d := dayField{star: strings.HasPrefix(f, "*")}
	for _, item := range strings.Split(f, ",") {
		u := strings.ToUpper(item)
		switch {
		case u == "L":
			d.last = true
		case u == "LW":
			d.lastWeekday = true
		case strings.HasSuffix(u, "W"):
			n, err := parseNum(u[:len(u)-1], 1, 31, nil)
			if err != nil {
				return d, fmt.Errorf("%q: %w", item, err)
			}
			d.nearest = append(d.nearest, n)
		default:
			b, err := parseItem(item, 1, 31, nil)
			if err != nil {
				return d, err
			}
			d.bits |= b
		}
	}
	return d, nil
}

func parseDOW(f string) (dayField, error) {
	d := dayField{star: strings.HasPrefix(f, "*")}
	for _, item := range strings.Split(f, ",") {
		u := strings.ToUpper(item)
		switch {
		case u == "L":
			d.bits |= 1 << 7 // Saturday
		case strings.Contains(u, "#"):
			ws, ks, _ := strings.Cut(u, "#")
			w, err := parseNum(ws, 1, 7, dayNames)
			if err != nil {
				return d, fmt.Errorf("%q: %w", item, err)
			}
			k, err := parseNum(ks, 1, 5, nil)
			if err != nil {
				return d, fmt.Errorf("%q: occurrence: %w", item, err)
			}
			d.nth = append(d.nth, [2]int{w, k})
		case len(u) > 1 && strings.HasSuffix(u, "L"):
			w, err := parseNum(u[:len(u)-1], 1, 7, dayNames)
			if err != nil {
				return d, fmt.Errorf("%q: %w", item, err)
			}
			d.lastOf = append(d.lastOf, w)
		default:
			b, err := parseItem(item, 1, 7, dayNames)
			if err != nil {
				return d, err
			}
			d.bits |= b
		}
	}
	return d, nil
}

func daysIn(y int, m time.Month) int {
	return time.Date(y, m+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

func weekday(y int, m time.Month, d int) int {
	return int(time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Weekday()) + 1 // 1 = Sunday
}

func isWeekday(w int) bool { return w >= 2 && w <= 6 }

func (f *dayField) matchDOM(y int, m time.Month, d int) bool {
	if f.bits&(1<<uint(d)) != 0 {
		return true
	}
	n := daysIn(y, m)
	if f.last && d == n {
		return true
	}
	if f.lastWeekday {
		l := n
		for !isWeekday(weekday(y, m, l)) {
			l--
		}
		if d == l {
			return true
		}
	}
	for _, t := range f.nearest {
		if t > n {
			continue // this month has no day t
		}
		w, near := weekday(y, m, t), t
		switch {
		case w == 7 && t == 1: // Saturday the 1st → Monday the 3rd
			near = 3
		case w == 7:
			near = t - 1
		case w == 1 && t == n: // Sunday the last → Friday
			near = t - 2
		case w == 1:
			near = t + 1
		}
		if d == near {
			return true
		}
	}
	return false
}

func (f *dayField) matchDOW(y int, m time.Month, d int) bool {
	w := weekday(y, m, d)
	if f.bits&(1<<uint(w)) != 0 {
		return true
	}
	for _, lw := range f.lastOf {
		if w == lw && d+7 > daysIn(y, m) {
			return true
		}
	}
	for _, nk := range f.nth {
		if w == nk[0] && (d-1)/7+1 == nk[1] {
			return true
		}
	}
	return false
}

func (s *Schedule) matchDay(y int, m time.Month, d int) bool {
	if s.month&(1<<uint(m)) == 0 {
		return false
	}
	dom, dow := s.dom.matchDOM(y, m, d), s.dow.matchDOW(y, m, d)
	if s.dom.star || s.dow.star {
		return dom && dow
	}
	return dom || dow
}

// Matches reports whether the schedule fires in t's minute (UTC).
func (s *Schedule) Matches(t time.Time) bool {
	t = t.UTC()
	return s.minute&(1<<uint(t.Minute())) != 0 && s.hour&(1<<uint(t.Hour())) != 0 && s.matchDay(t.Year(), t.Month(), t.Day())
}

// Next returns the first minute strictly after t (UTC) at which the
// schedule fires, searching at most 50 years ahead.
func (s *Schedule) Next(t time.Time) (time.Time, bool) {
	t = t.UTC().Truncate(time.Minute).Add(time.Minute)
	end := t.Add(horizon)
	day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	for ; day.Before(end); day = day.AddDate(0, 0, 1) {
		if !s.matchDay(day.Year(), day.Month(), day.Day()) {
			continue
		}
		h0, m0 := 0, 0
		if day.Equal(time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)) {
			h0, m0 = t.Hour(), t.Minute()
		}
		for h := h0; h < 24; h++ {
			if s.hour&(1<<uint(h)) == 0 {
				continue
			}
			mStart := 0
			if h == h0 {
				mStart = m0
			}
			for m := mStart; m < 60; m++ {
				if s.minute&(1<<uint(m)) != 0 {
					return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute), true
				}
			}
		}
	}
	return time.Time{}, false
}

func trunc(s string) string {
	if len(s) > 32 {
		return s[:32] + "…"
	}
	return s
}
