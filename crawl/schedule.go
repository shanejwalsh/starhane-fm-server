package crawl

import (
	"math/rand/v2"
	"sort"
	"time"
)

// Schedule decides how often a feed is worth re-checking.
type Schedule struct {
	// Min and Max clamp every interval this produces.
	Min time.Duration
	Max time.Duration
	// DeadRecheck is how long to wait before retrying a dead feed.
	DeadRecheck time.Duration
	// MaxFailures is how many consecutive failures mark a feed dead.
	MaxFailures int
}

// Backoff factors. A feed that changed is checked at its episode cadence; one
// that did not is checked progressively less often, and one that is erroring is
// backed off harder still.
const (
	unchangedFactor = 1.5
	errorFactor     = 2.0
	// defaultCadence is used when a feed has too few dated episodes to measure.
	defaultCadence = 6 * time.Hour
	// jitterFraction spreads feeds that would otherwise stay in lockstep after
	// being claimed together.
	jitterFraction = 0.1
)

// IntervalAfterChange returns the interval for a feed that just published
// something new.
//
// Episode cadence is the best available guess at when the next one lands, so a
// daily show is polled daily and a monthly one monthly, both within the clamps.
func (s Schedule) IntervalAfterChange(pubDates []time.Time) time.Duration {
	cadence := medianGap(pubDates)
	if cadence <= 0 {
		cadence = defaultCadence
	}
	return s.clamp(cadence)
}

// IntervalAfterNoChange backs a quiet feed off gradually.
func (s Schedule) IntervalAfterNoChange(current time.Duration) time.Duration {
	if current <= 0 {
		current = defaultCadence
	}
	return s.clamp(time.Duration(float64(current) * unchangedFactor))
}

// IntervalAfterError backs a broken feed off faster than a merely quiet one.
func (s Schedule) IntervalAfterError(current time.Duration) time.Duration {
	if current <= 0 {
		current = defaultCadence
	}
	return s.clamp(time.Duration(float64(current) * errorFactor))
}

// IsDead reports whether a feed has failed often enough to stop crawling on a
// normal schedule.
func (s Schedule) IsDead(consecutiveFailures int) bool {
	return s.MaxFailures > 0 && consecutiveFailures >= s.MaxFailures
}

// NextCheck returns when a feed should next be fetched, with jitter applied so
// that feeds claimed in the same batch do not all come due together again.
func (s Schedule) NextCheck(now time.Time, interval time.Duration) time.Time {
	return now.Add(jitter(interval))
}

// clamp keeps an interval within Min and Max.
func (s Schedule) clamp(d time.Duration) time.Duration {
	if s.Min > 0 && d < s.Min {
		return s.Min
	}
	if s.Max > 0 && d > s.Max {
		return s.Max
	}
	return d
}

// medianGap returns the median time between consecutive publication dates.
//
// The median rather than the mean because a single ancient back-catalogue entry
// or a burst of same-day releases should not drag the estimate around.
func medianGap(dates []time.Time) time.Duration {
	if len(dates) < 2 {
		return 0
	}

	sorted := make([]time.Time, len(dates))
	copy(sorted, dates)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Before(sorted[j]) })

	gaps := make([]time.Duration, 0, len(sorted)-1)
	for i := 1; i < len(sorted); i++ {
		if gap := sorted[i].Sub(sorted[i-1]); gap > 0 {
			gaps = append(gaps, gap)
		}
	}
	if len(gaps) == 0 {
		return 0
	}

	sort.Slice(gaps, func(i, j int) bool { return gaps[i] < gaps[j] })
	middle := len(gaps) / 2
	if len(gaps)%2 == 1 {
		return gaps[middle]
	}
	return (gaps[middle-1] + gaps[middle]) / 2
}

// jitter varies a duration by up to jitterFraction either way.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	spread := float64(d) * jitterFraction
	offset := (rand.Float64()*2 - 1) * spread
	jittered := time.Duration(float64(d) + offset)
	if jittered < 0 {
		return 0
	}
	return jittered
}
