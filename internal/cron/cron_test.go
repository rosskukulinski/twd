package cron

import (
	"strings"
	"testing"
	"time"
)

func ts(s string) time.Time {
	t, err := time.Parse("2006-01-02 15:04", s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestParseInvalid(t *testing.T) {
	for _, tc := range []struct{ expr, want string }{
		{"", "want 5 fields"},
		{"* * * *", "want 5 fields"},
		{"* * * * * *", "want 5 fields"},
		{"@daily", "want 5 fields"},
		{"60 * * * *", "out of range"},
		{"-1 * * * *", "not a number"},
		{"* 24 * * *", "out of range"},
		{"* * 0 * *", "out of range"},
		{"* * 32 * *", "out of range"},
		{"* * * 0 *", "out of range"},
		{"* * * 13 *", "out of range"},
		{"* * * * 0", "1 = Sunday"},
		{"* * * * 8", "out of range"},
		{"*/0 * * * *", "step"},
		{"*/61 * * * *", "step"},
		{"5-1 * * * *", "backwards"},
		{"1,,2 * * * *", "empty"},
		{"1, * * * *", "empty"},
		{"a * * * *", "not a number"},
		{"+1 * * * *", "not a number"},
		{"001 * * * *", "not a number"},
		{"99999999999999999999 * * * *", "not a number"},
		{"* * * FOO *", "not a number"},
		{"* * * * MON-FOO", "not a number"},
		{"* * * * JAN", "not a number"}, // month name in the weekday field
		{"* * * MON *", "not a number"}, // weekday name in the month field
		{"* * W * *", "not a number"},
		{"* * 32W * *", "out of range"},
		{"* * 5L * *", "not a number"},
		{"* * * * 2#6", "occurrence"},
		{"* * * * 2#0", "occurrence"},
		{"* * * * 8#1", "out of range"},
		{"* * * * 8L", "out of range"},
		{"L * * * *", "not a number"}, // L only in the day fields
		{"0 0 30 2 *", "never fires"},
		{"0 0 31 4,6,9,11 *", "never fires"},
		{"0 0 31W 2 *", "never fires"},
		{"* * * * *\x00", "invalid character"},
		{"* * * * *\n* * * * *", "invalid character"},
		{"* * * * ＊", "invalid character"},
		{strings.Repeat("1,", 100) + "1 * * * *", "longer than"},
	} {
		_, err := Parse(tc.expr)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Parse(%q) = %v, want error containing %q", tc.expr, err, tc.want)
		}
	}
}

func TestMatches(t *testing.T) {
	// 2026-11-13 is a Friday.
	for _, tc := range []struct {
		expr string
		yes  []string
		no   []string
	}{
		{"* * * * *", []string{"2026-11-13 00:00", "2026-11-13 23:59"}, nil},
		{"*/15 * * * *", []string{"2026-11-13 10:00", "2026-11-13 10:45"}, []string{"2026-11-13 10:01", "2026-11-13 10:50"}},
		{"5/20 * * * *", []string{"2026-11-13 10:05", "2026-11-13 10:25", "2026-11-13 10:45"}, []string{"2026-11-13 10:00", "2026-11-13 10:15"}},
		{"10-20/5 3 * * *", []string{"2026-11-13 03:10", "2026-11-13 03:20"}, []string{"2026-11-13 03:25", "2026-11-13 04:10"}},
		{"0 17 * * sun", []string{"2026-11-15 17:00"}, []string{"2026-11-13 17:00", "2026-11-15 17:01"}},
		{"0 17 * * 1", []string{"2026-11-15 17:00"}, []string{"2026-11-16 17:00"}}, // 1 = Sunday
		{"10 7 * * mon-fri", []string{"2026-11-13 07:10", "2026-11-16 07:10"}, []string{"2026-11-14 07:10", "2026-11-15 07:10"}},
		{"10 7 * * 2-6", []string{"2026-11-13 07:10"}, []string{"2026-11-14 07:10"}},
		{"0 15 1 * *", []string{"2026-12-01 15:00"}, []string{"2026-12-02 15:00"}},
		{"0 0 1 jan,Jul *", []string{"2026-01-01 00:00", "2026-07-01 00:00"}, []string{"2026-02-01 00:00"}},
		{"0 18 * * 6L", []string{"2026-11-27 18:00"}, []string{"2026-11-20 18:00", "2026-11-13 18:00"}}, // last Friday
		{"0 18 * * friL", []string{"2026-11-27 18:00"}, []string{"2026-11-20 18:00"}},
		{"59 23 LW * *", []string{"2026-11-30 23:59", "2026-10-30 23:59"}, []string{"2026-10-31 23:59"}}, // Oct 31 2026 is a Saturday
		{"0 0 L * *", []string{"2026-11-30 00:00", "2026-02-28 00:00", "2028-02-29 00:00"}, []string{"2028-02-28 00:00"}},
		{"0 0 L 2 *", []string{"2026-02-28 00:00"}, []string{"2026-02-27 00:00"}},
		{"0 0 * * 2#1", []string{"2026-11-02 00:00"}, []string{"2026-11-09 00:00"}}, // first Monday
		{"0 0 * * mon#2", []string{"2026-11-09 00:00"}, []string{"2026-11-02 00:00"}},
		{"0 0 * * 6#5", []string{"2026-10-30 00:00"}, []string{"2026-10-23 00:00"}},
		{"0 0 * * L", []string{"2026-11-14 00:00"}, []string{"2026-11-13 00:00"}}, // L alone = Saturday
		// nW: nearest weekday to the 15th. Nov 15 2026 is a Sunday → Mon 16.
		{"0 0 15W * *", []string{"2026-11-16 00:00"}, []string{"2026-11-15 00:00", "2026-11-13 00:00"}},
		// Aug 1 2026 is a Saturday → Mon Aug 3 (stays in the month).
		{"0 0 1W * *", []string{"2026-08-03 00:00"}, []string{"2026-07-31 00:00", "2026-08-01 00:00"}},
		// May 31 2026 is a Sunday → Fri May 29.
		{"0 0 31W * *", []string{"2026-05-29 00:00"}, []string{"2026-06-01 00:00"}},
		// Both day fields restricted: OR (Vixie).
		{"0 0 1 * mon", []string{"2026-11-01 00:00", "2026-11-02 00:00", "2026-11-09 00:00"}, []string{"2026-11-03 00:00"}},
		// Day of week starts with "*": AND.
		{"0 0 1 * */2", []string{"2026-11-01 00:00"}, []string{"2026-11-02 00:00"}}, // */2 → 1,3,5,7: Sun ok
		{"0 0 2 * */2", nil, []string{"2026-11-02 00:00"}},                          // Monday (2) not in */2
	} {
		s, err := Parse(tc.expr)
		if err != nil {
			t.Errorf("Parse(%q): %v", tc.expr, err)
			continue
		}
		for _, y := range tc.yes {
			if !s.Matches(ts(y)) {
				t.Errorf("%q should fire at %s", tc.expr, y)
			}
		}
		for _, n := range tc.no {
			if s.Matches(ts(n)) {
				t.Errorf("%q should not fire at %s", tc.expr, n)
			}
		}
	}
}

func TestMatchesIgnoresSecondsAndZone(t *testing.T) {
	s, _ := Parse("30 12 * * *")
	loc := time.FixedZone("x", -8*3600)
	if !s.Matches(time.Date(2026, 11, 13, 4, 30, 59, 999, loc)) { // 12:30:59 UTC
		t.Fatal("should match in UTC")
	}
}

func TestNext(t *testing.T) {
	for _, tc := range []struct{ expr, after, want string }{
		{"* * * * *", "2026-11-13 10:00", "2026-11-13 10:01"},
		{"*/15 * * * *", "2026-11-13 10:00", "2026-11-13 10:15"},
		{"0 0 * * *", "2026-12-31 23:59", "2027-01-01 00:00"},
		{"0 0 29 2 *", "2026-03-01 00:00", "2028-02-29 00:00"},
		{"0 0 29 2 *", "2096-03-01 00:00", "2104-02-29 00:00"}, // 2100 is not a leap year
		{"0 0 L * *", "2026-01-31 00:00", "2026-02-28 00:00"},
		{"0 9 * * mon#1", "2026-11-13 00:00", "2026-12-07 09:00"},
		{"30 2 * * *", "2026-11-13 02:30", "2026-11-14 02:30"},
	} {
		s, err := Parse(tc.expr)
		if err != nil {
			t.Fatalf("Parse(%q): %v", tc.expr, err)
		}
		got, ok := s.Next(ts(tc.after))
		if !ok || !got.Equal(ts(tc.want)) {
			t.Errorf("Next(%q, %s) = %v %v, want %s", tc.expr, tc.after, got, ok, tc.want)
		}
	}
	// Next with seconds rounds forward to the next whole minute.
	s, _ := Parse("* * * * *")
	got, _ := s.Next(time.Date(2026, 1, 1, 0, 0, 30, 0, time.UTC))
	if !got.Equal(time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC)) {
		t.Errorf("Next mid-minute = %v", got)
	}
}

// TestNextAgreesWithMatches: Next is the first minute Matches accepts.
func TestNextAgreesWithMatches(t *testing.T) {
	exprs := []string{"*/7 * * * *", "0 */5 * * *", "0 0 L * *", "59 23 LW * *", "0 18 * * 6L", "0 0 15W * *", "0 0 1 * mon", "13 4 * 2-4 2#2,fri"}
	start := ts("2026-01-01 00:00")
	for _, e := range exprs {
		s, err := Parse(e)
		if err != nil {
			t.Fatal(err)
		}
		cur := start
		for i := 0; i < 50; i++ {
			nx, ok := s.Next(cur)
			if !ok {
				t.Fatalf("%q: no next after %v", e, cur)
			}
			for m := cur.Add(time.Minute); m.Before(nx); m = m.Add(time.Minute) {
				if s.Matches(m) {
					t.Fatalf("%q: Next(%v) = %v skips %v", e, cur, nx, m)
				}
			}
			if !s.Matches(nx) {
				t.Fatalf("%q: Next returned non-matching %v", e, nx)
			}
			cur = nx
		}
	}
}

func TestValidateTriggers(t *testing.T) {
	if err := ValidateTriggers(nil); err != nil {
		t.Fatal(err)
	}
	if err := ValidateTriggers([]string{"* * * * *", "0 0 * * *"}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateTriggers([]string{"* * * * *", "* * * * *"}); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("duplicate: %v", err)
	}
	if err := ValidateTriggers([]string{"* * * * *", "bad"}); err == nil {
		t.Fatal("invalid accepted")
	}
	many := make([]string, MaxTriggers+1)
	for i := range many {
		many[i] = strings.Repeat(" ", i) + "* * * * *" // distinct strings, same schedule
	}
	if err := ValidateTriggers(many); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("too many: %v", err)
	}
}

// FuzzParse: Parse never panics, and an accepted schedule always has a
// next firing that it matches.
func FuzzParse(f *testing.F) {
	for _, s := range []string{"* * * * *", "*/5 1-3 L,15W * mon#2", "0 18 * * friL", "59 23 LW * *", "1-2/3,4 * * jan-dec/2 sun-sat"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, expr string) {
		s, err := Parse(expr)
		if err != nil {
			return
		}
		nx, ok := s.Next(ts("2026-01-01 00:00"))
		if !ok || !s.Matches(nx) {
			t.Fatalf("%q: accepted but Next = %v %v", expr, nx, ok)
		}
	})
}

// TestParseWorstCaseIsCheap: rejecting an unsatisfiable schedule that
// passes the month check every day (Next scans the whole horizon) stays
// cheap, since mgmt parses tenant-supplied expressions on upload.
func TestParseWorstCaseIsCheap(t *testing.T) {
	expr := "0 0 */31 * 2#5" // day 1 AND the 5th Monday: impossible
	st := time.Now()
	for i := 0; i < MaxTriggers; i++ {
		if _, err := Parse(expr); err == nil || !strings.Contains(err.Error(), "never fires") {
			t.Fatalf("Parse(%q) = %v", expr, err)
		}
	}
	if d := time.Since(st); d > 2*time.Second {
		t.Fatalf("%d worst-case parses took %v", MaxTriggers, d)
	}
	t.Logf("%d worst-case parses: %v", MaxTriggers, time.Since(st))
}
