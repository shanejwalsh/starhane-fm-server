package crawl

import (
	"testing"
	"time"
)

func testSchedule() Schedule {
	return Schedule{
		Min:         time.Hour,
		Max:         24 * time.Hour,
		DeadRecheck: 30 * 24 * time.Hour,
		MaxFailures: 10,
	}
}

func TestIntervalAfterChangeFollowsEpisodeCadence(t *testing.T) {
	s := testSchedule()
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	daily := []time.Time{base, base.Add(24 * time.Hour), base.Add(48 * time.Hour), base.Add(72 * time.Hour)}
	if got := s.IntervalAfterChange(daily); got != 24*time.Hour {
		t.Errorf("daily feed: interval = %s, want 24h", got)
	}

	// A weekly show's cadence exceeds the cap, so it clamps to Max rather than
	// waiting a week.
	weekly := []time.Time{base, base.Add(7 * 24 * time.Hour), base.Add(14 * 24 * time.Hour)}
	if got := s.IntervalAfterChange(weekly); got != s.Max {
		t.Errorf("weekly feed: interval = %s, want the %s cap", got, s.Max)
	}

	// A burst of same-day releases must not drive the interval below Min.
	burst := []time.Time{base, base.Add(time.Minute), base.Add(2 * time.Minute)}
	if got := s.IntervalAfterChange(burst); got != s.Min {
		t.Errorf("burst feed: interval = %s, want the %s floor", got, s.Min)
	}

	// Too few dated episodes to measure anything.
	if got := s.IntervalAfterChange(nil); got != defaultCadence {
		t.Errorf("no dates: interval = %s, want the %s default", got, defaultCadence)
	}
	if got := s.IntervalAfterChange([]time.Time{base}); got != defaultCadence {
		t.Errorf("one date: interval = %s, want the %s default", got, defaultCadence)
	}
}

func TestIntervalBackoff(t *testing.T) {
	s := testSchedule()

	cases := []struct {
		name    string
		current time.Duration
		got     time.Duration
		want    time.Duration
	}{
		{"unchanged grows gently", 2 * time.Hour, s.IntervalAfterNoChange(2 * time.Hour), 3 * time.Hour},
		{"unchanged clamps at max", 20 * time.Hour, s.IntervalAfterNoChange(20 * time.Hour), s.Max},
		{"error grows faster", 2 * time.Hour, s.IntervalAfterError(2 * time.Hour), 4 * time.Hour},
		{"error clamps at max", 20 * time.Hour, s.IntervalAfterError(20 * time.Hour), s.Max},
		{"zero current uses the default", 0, s.IntervalAfterNoChange(0), 9 * time.Hour},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, tc.got, tc.want)
		}
	}

	// Errors must back off at least as hard as quiet feeds do.
	if s.IntervalAfterError(time.Hour) < s.IntervalAfterNoChange(time.Hour) {
		t.Error("an erroring feed should back off at least as much as an unchanged one")
	}
}

func TestIsDead(t *testing.T) {
	s := testSchedule()
	cases := map[int]bool{0: false, 1: false, 9: false, 10: true, 11: true}
	for failures, want := range cases {
		if got := s.IsDead(failures); got != want {
			t.Errorf("IsDead(%d) = %v, want %v", failures, got, want)
		}
	}

	// A zero budget disables the whole idea rather than killing everything.
	none := Schedule{MaxFailures: 0}
	if none.IsDead(100) {
		t.Error("MaxFailures of 0 should never mark a feed dead")
	}
}

func TestNextCheckStaysWithinJitterBounds(t *testing.T) {
	s := testSchedule()
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	const interval = 10 * time.Hour

	lowest, highest := time.Duration(1<<62), time.Duration(0)
	for range 500 {
		delay := s.NextCheck(now, interval).Sub(now)
		if delay < lowest {
			lowest = delay
		}
		if delay > highest {
			highest = delay
		}
	}

	minAllowed := time.Duration(float64(interval) * (1 - jitterFraction))
	maxAllowed := time.Duration(float64(interval) * (1 + jitterFraction))
	if lowest < minAllowed || highest > maxAllowed {
		t.Errorf("jittered delays spanned %s..%s, want within %s..%s", lowest, highest, minAllowed, maxAllowed)
	}
	// Jitter exists to break lockstep, so it must actually vary.
	if lowest == highest {
		t.Error("jitter produced no variation")
	}
}

func TestMedianGapIgnoresOutliers(t *testing.T) {
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	// Three daily gaps and one ancient back-catalogue entry. The median must
	// stay at a day; a mean would be dragged towards the outlier.
	dates := []time.Time{
		base.Add(-365 * 24 * time.Hour),
		base,
		base.Add(24 * time.Hour),
		base.Add(48 * time.Hour),
		base.Add(72 * time.Hour),
	}
	if got := medianGap(dates); got != 24*time.Hour {
		t.Errorf("medianGap = %s, want 24h", got)
	}

	// Order must not matter.
	shuffled := []time.Time{dates[3], dates[0], dates[4], dates[1], dates[2]}
	if got := medianGap(shuffled); got != 24*time.Hour {
		t.Errorf("medianGap on shuffled input = %s, want 24h", got)
	}

	if got := medianGap(nil); got != 0 {
		t.Errorf("medianGap(nil) = %s, want 0", got)
	}
	// Identical timestamps produce no positive gap to measure.
	if got := medianGap([]time.Time{base, base}); got != 0 {
		t.Errorf("medianGap of identical dates = %s, want 0", got)
	}
}
