// Package crawl fetches, parses and stores podcast feeds.
//
// Crawler.CrawlFeed is the single unit of work, shared by the crawler binary's
// worker pool and by the API's synchronous first crawl of a cold feed.
package crawl

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/shanejwalsh/starhane-fm-server/config"
	"github.com/shanejwalsh/starhane-fm-server/store"
)

// cadenceSample is how many recent episodes inform the cadence estimate.
const cadenceSample = 6

// Crawler crawls feeds and records what it found.
type Crawler struct {
	store      *store.Store
	fetcher    *Fetcher
	schedule   Schedule
	pruneGrace int
	logger     *slog.Logger
}

// NewCrawler builds a Crawler from configuration.
func NewCrawler(s *store.Store, cfg config.Crawler, logger *slog.Logger) *Crawler {
	return &Crawler{
		store: s,
		fetcher: NewFetcher(FetcherOptions{
			Timeout:      cfg.HTTPTimeout,
			UserAgent:    cfg.UserAgent,
			MaxBodyBytes: cfg.MaxBodyBytes,
			HostRPS:      cfg.HostRPS,
			HostBurst:    cfg.HostBurst,
		}),
		schedule: Schedule{
			Min:         cfg.MinInterval,
			Max:         cfg.MaxInterval,
			DeadRecheck: cfg.DeadRecheck,
			MaxFailures: cfg.MaxFailures,
		},
		pruneGrace: cfg.EpisodeGraceCrawls,
		logger:     logger,
	}
}

// Report describes one crawl, for logging and for the caller to act on.
type Report struct {
	FeedID    int64
	URL       string
	Host      string
	Outcome   Outcome
	Status    int
	Episodes  int
	Pruned    int
	Duration  time.Duration
	Err       error
	MovedTo   string
	NextCheck time.Time
}

// CrawlFeed fetches a feed, stores whatever it found, and schedules the next
// check. It returns an error only when the outcome could not be recorded: a
// feed that failed to fetch is a successful crawl that stored a failure.
func (c *Crawler) CrawlFeed(ctx context.Context, feed store.Feed) (Report, error) {
	start := time.Now()
	logger := c.logger.With(
		slog.Int64("feed_id", feed.ID),
		slog.String("host", hostOf(feed.URL)),
		slog.String("feed_url", feed.URL),
	)

	fetched := c.fetcher.Fetch(ctx, feed.URL, feed.ETag, feed.LastModified, feed.BodyHash)

	result, report := c.apply(ctx, feed, fetched, logger)
	report.FeedID = feed.ID
	report.URL = result.Feed.URL
	report.Host = hostOf(feed.URL)
	report.Outcome = fetched.Outcome
	report.Status = fetched.StatusCode
	report.NextCheck = result.Feed.NextCheckAt

	counts, err := c.store.ApplyCrawl(ctx, result)
	if err != nil {
		return report, err
	}
	report.Episodes = counts.Upserted
	report.Pruned = counts.Pruned
	report.Duration = time.Since(start)

	c.log(ctx, logger, report)
	return report, nil
}

// apply turns a fetch result into the feed's next state.
func (c *Crawler) apply(ctx context.Context, feed store.Feed, fetched FetchResult, logger *slog.Logger) (store.CrawlResult, Report) {
	now := time.Now()
	next := feed
	next.LastStatusCode = fetched.StatusCode
	report := Report{}

	// Crawling a feed at all means it belongs in the rotation. A dormant feed
	// only reaches here through the API's first-request path, which activates
	// it just beforehand — but the struct we were handed predates that. Without
	// this, a first crawl that failed or was rate limited would write the
	// dormant status back and put the feed to sleep again.
	if next.Status == store.StatusDormant {
		next.Status = store.StatusActive
	}

	switch fetched.Outcome {
	case OutcomeChanged:
		parsed, err := ParseFeed(fetched.Body)
		if err != nil {
			// A body we cannot parse is a failure like any other: back off and
			// keep whatever episodes we already had.
			return c.applyFailure(ctx, next, err, now), Report{Err: err}
		}

		// A publisher can also announce a move inside the document itself.
		if moved := c.applyMove(ctx, &next, parsed.NewFeedURL, now, logger); moved {
			return store.CrawlResult{Feed: next}, Report{MovedTo: next.URL}
		}

		next.ETag = fetched.ETag
		next.LastModified = fetched.LastModified
		next.BodyHash = fetched.BodyHash
		next.Status = store.StatusActive
		next.ConsecutiveFailures = 0
		next.LastError = ""
		next.LastSuccessAt = &now
		next.LastModifiedAt = &now
		next.CheckInterval = c.schedule.IntervalAfterChange(c.recentDates(ctx, feed.ID, parsed.Episodes))
		next.NextCheckAt = c.schedule.NextCheck(now, next.CheckInterval)

		return store.CrawlResult{
			Feed:       next,
			Parsed:     true,
			Episodes:   parsed.Episodes,
			PruneGrace: c.pruneGrace,
		}, report

	case OutcomeNotModified:
		next.ETag = fetched.ETag
		next.LastModified = fetched.LastModified
		if len(fetched.BodyHash) > 0 {
			next.BodyHash = fetched.BodyHash
		}
		next.Status = store.StatusActive
		next.ConsecutiveFailures = 0
		next.LastError = ""
		next.LastSuccessAt = &now
		next.CheckInterval = c.schedule.IntervalAfterNoChange(feed.CheckInterval)
		next.NextCheckAt = c.schedule.NextCheck(now, next.CheckInterval)

		return store.CrawlResult{Feed: next}, report

	case OutcomeMoved:
		c.applyMove(ctx, &next, fetched.NewURL, now, logger)
		return store.CrawlResult{Feed: next}, Report{MovedTo: next.URL}

	case OutcomeGone:
		next.Status = store.StatusDead
		next.LastError = fetched.Err.Error()
		next.ConsecutiveFailures = feed.ConsecutiveFailures + 1
		next.NextCheckAt = now.Add(c.schedule.DeadRecheck)
		return store.CrawlResult{Feed: next}, Report{Err: fetched.Err}

	case OutcomeRateLimited:
		// Being throttled is the host working as intended, not the feed being
		// broken, so it does not count towards the failure budget.
		delay := fetched.RetryAfter
		if delay <= 0 {
			delay = c.schedule.IntervalAfterError(feed.CheckInterval)
		}
		next.LastError = fetched.Err.Error()
		next.NextCheckAt = now.Add(delay)
		return store.CrawlResult{Feed: next}, Report{Err: fetched.Err}

	default:
		return c.applyFailure(ctx, next, fetched.Err, now), Report{Err: fetched.Err}
	}
}

// applyMove points a feed at its new URL, or retires it when another row
// already owns that URL.
//
// It reports whether the feed actually moved.
func (c *Crawler) applyMove(ctx context.Context, feed *store.Feed, newURL string, now time.Time, logger *slog.Logger) bool {
	newURL = normaliseMoveTarget(newURL, feed.URL)
	if newURL == "" {
		return false
	}

	// Check the new URL for a second time before writing: two feeds cannot
	// share a URL, and the target may already be in the catalogue.
	existing, err := c.store.FeedByURL(ctx, newURL)
	switch {
	case err == nil && existing.ID != feed.ID:
		// The destination is already crawled under its own row, so this one
		// becomes a redirect stub rather than a duplicate.
		feed.Status = store.StatusRedirected
		feed.RedirectedToFeedID = &existing.ID
		feed.NextCheckAt = now.Add(c.schedule.DeadRecheck)
		logger.InfoContext(ctx, "feed moved to a feed we already have",
			slog.String("new_url", newURL),
			slog.Int64("target_feed_id", existing.ID),
		)

	case err != nil && !errors.Is(err, pgx.ErrNoRows):
		logger.WarnContext(ctx, "could not check the redirect target, keeping the current URL",
			slog.String("new_url", newURL),
			slog.Any("error", err),
		)
		return false

	default:
		// Free to take the new URL. Validators belong to the old one, so drop
		// them and refetch from scratch next time.
		feed.URL = newURL
		feed.ETag = ""
		feed.LastModified = ""
		feed.BodyHash = nil
		feed.Status = store.StatusActive
		feed.ConsecutiveFailures = 0
		feed.LastError = ""
		feed.NextCheckAt = now
		logger.InfoContext(ctx, "feed moved", slog.String("new_url", newURL))
	}

	return true
}

// applyFailure records a failed crawl and decides whether the feed is dead.
func (c *Crawler) applyFailure(ctx context.Context, feed store.Feed, cause error, now time.Time) store.CrawlResult {
	feed.ConsecutiveFailures++
	if cause != nil {
		feed.LastError = cause.Error()
	}

	if c.schedule.IsDead(feed.ConsecutiveFailures) {
		feed.Status = store.StatusDead
		feed.NextCheckAt = now.Add(c.schedule.DeadRecheck)
		return store.CrawlResult{Feed: feed}
	}

	feed.CheckInterval = c.schedule.IntervalAfterError(feed.CheckInterval)
	feed.NextCheckAt = c.schedule.NextCheck(now, feed.CheckInterval)
	return store.CrawlResult{Feed: feed}
}

// recentDates gathers publication dates to estimate cadence from, preferring
// the ones just parsed and falling back to what is stored.
func (c *Crawler) recentDates(ctx context.Context, feedID int64, episodes []store.EpisodeUpsert) []time.Time {
	dates := make([]time.Time, 0, cadenceSample)
	for _, e := range episodes {
		if e.PubDate != nil {
			dates = append(dates, *e.PubDate)
			if len(dates) == cadenceSample {
				return dates
			}
		}
	}
	if len(dates) >= 2 {
		return dates
	}

	stored, err := c.store.RecentEpisodeDates(ctx, feedID, cadenceSample)
	if err != nil {
		return dates
	}
	return append(dates, stored...)
}

func (c *Crawler) log(ctx context.Context, logger *slog.Logger, report Report) {
	attrs := []any{
		slog.String("outcome", string(report.Outcome)),
		slog.Int("status", report.Status),
		slog.Int("episodes", report.Episodes),
		slog.Duration("duration", report.Duration),
		slog.Time("next_check_at", report.NextCheck),
	}
	if report.Pruned > 0 {
		attrs = append(attrs, slog.Int("episodes_pruned", report.Pruned))
	}
	if report.MovedTo != "" {
		attrs = append(attrs, slog.String("moved_to", report.MovedTo))
	}

	if report.Err != nil {
		logger.WarnContext(ctx, "feed crawl failed", append(attrs, slog.Any("error", report.Err))...)
		return
	}
	logger.InfoContext(ctx, "feed crawled", attrs...)
}

// normaliseMoveTarget rejects move announcements that are empty or point at
// where we already are.
func normaliseMoveTarget(newURL, currentURL string) string {
	newURL = strings.TrimSpace(newURL)
	if newURL == "" || newURL == currentURL {
		return ""
	}
	return newURL
}
