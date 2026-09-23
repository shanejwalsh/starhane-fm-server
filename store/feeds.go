package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// feedColumns is every column Feed has a field for. RowToStructByNameLax errors
// on a column with no matching field, so the list and the struct move together.
const feedColumns = `id, url, etag, last_modified, body_hash, status, redirected_to_feed_id,
	activated_at, last_requested_at, check_interval, next_check_at, consecutive_failures,
	last_checked_at, last_success_at, last_modified_at, last_status_code, last_error,
	crawl_seq, created_at, updated_at`

// UpsertFeed returns the feed for url, creating it if it is new.
//
// A new feed is dormant: the API upserts one as a side effect of returning a
// podcast, but nothing crawls it until somebody asks for its episodes. A single
// search seeds fifty feeds and a user opens at most one of them.
func (s *Store) UpsertFeed(ctx context.Context, url string) (Feed, error) {
	// DO UPDATE rather than DO NOTHING so RETURNING always produces a row.
	const query = `
		INSERT INTO feeds (url)
		VALUES ($1)
		ON CONFLICT (url) DO UPDATE SET updated_at = now()
		RETURNING ` + feedColumns

	rows, err := s.pool.Query(ctx, query, url)
	feed, err := collectOne[Feed](ctx, rows, err)
	if err != nil {
		return Feed{}, fmt.Errorf("upserting feed %q: %w", url, err)
	}
	return feed, nil
}

// FeedByID returns a single feed. It returns pgx.ErrNoRows when there is none.
func (s *Store) FeedByID(ctx context.Context, id int64) (Feed, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+feedColumns+` FROM feeds WHERE id = $1`, id)
	return collectOne[Feed](ctx, rows, err)
}

// FeedByURL returns a single feed by URL. It returns pgx.ErrNoRows when there
// is none.
func (s *Store) FeedByURL(ctx context.Context, url string) (Feed, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+feedColumns+` FROM feeds WHERE url = $1`, url)
	return collectOne[Feed](ctx, rows, err)
}

// ClaimDueFeeds takes up to limit feeds that are due for a crawl.
//
// Only active and dead feeds are candidates. Dormant feeds have never been
// asked for, and redirected ones are stubs pointing elsewhere. Dead feeds are
// included so that their scheduled re-check actually happens — they are the
// reason CRAWLER_DEAD_RECHECK exists.
//
// Postgres is the queue. SKIP LOCKED lets several crawler processes claim
// disjoint batches without coordinating, and pushing next_check_at forward by
// lease means a worker that dies mid-crawl releases its feeds back to the queue
// rather than stranding them.
func (s *Store) ClaimDueFeeds(ctx context.Context, limit int, lease time.Duration) ([]Feed, error) {
	const query = `
		UPDATE feeds
		SET next_check_at   = now() + $2::interval,
		    last_checked_at = now(),
		    updated_at      = now()
		WHERE id IN (
		    SELECT id
		    FROM feeds
		    WHERE status IN ('active', 'dead')
		      AND next_check_at <= now()
		    ORDER BY next_check_at
		    LIMIT $1
		    FOR UPDATE SKIP LOCKED
		)
		RETURNING ` + feedColumns

	rows, err := s.pool.Query(ctx, query, limit, lease)
	if err != nil {
		return nil, fmt.Errorf("claiming due feeds: %w", err)
	}
	feeds, err := pgx.CollectRows(rows, pgx.RowToStructByNameLax[Feed])
	if err != nil {
		return nil, fmt.Errorf("claiming due feeds: %w", err)
	}
	return feeds, nil
}

// MarkFeedRequested records that somebody asked for a feed's episodes,
// activating it if it was dormant.
//
// This is what puts a feed into the crawl rotation — searching for a podcast
// does not, opening it does. throttle keeps a popular feed from taking a write
// on every read: the statement's WHERE clause matches nothing once the feed has
// been touched recently.
//
// It reports whether the feed was activated by this call.
func (s *Store) MarkFeedRequested(ctx context.Context, feedID int64, throttle time.Duration) (bool, error) {
	const query = `
		UPDATE feeds
		SET last_requested_at = now(),
		    activated_at      = COALESCE(activated_at, now()),
		    status            = CASE WHEN status = 'dormant' THEN 'active' ELSE status END,
		    next_check_at     = CASE WHEN status = 'dormant' THEN now() ELSE next_check_at END,
		    updated_at        = now()
		WHERE id = $1
		  AND (status = 'dormant'
		       OR last_requested_at IS NULL
		       OR last_requested_at < now() - $2::interval)
		RETURNING status`

	var status string
	err := s.pool.QueryRow(ctx, query, feedID, throttle).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		// Already active and recently requested: nothing to do.
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("marking feed %d requested: %w", feedID, err)
	}
	return true, nil
}

// CrawlResult is the outcome of one crawl, ready to be persisted.
//
// Feed carries the already-computed next state: the crawl package owns the
// scheduling decisions, this package only writes them down.
type CrawlResult struct {
	Feed Feed
	// Parsed reports that a body was parsed this time round, as opposed to a
	// 304, an unchanged body hash or a failure. Only a parse advances
	// crawl_seq and touches episodes.
	Parsed   bool
	Episodes []EpisodeUpsert
	// PruneGrace is how many crawls an episode may be missing from the feed
	// before its row is deleted. Zero disables pruning.
	PruneGrace int
}

// ApplyCrawl writes a crawl's outcome: the feed's new state and, when the body
// was parsed, the episodes it contained.
//
// It runs in one transaction so that crawl_seq and the episodes stamped with it
// can never disagree — a reader would otherwise briefly see an episode list
// that is empty or half-updated.
func (s *Store) ApplyCrawl(ctx context.Context, result CrawlResult) (CrawlCounts, error) {
	var counts CrawlCounts

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return counts, fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	const updateFeed = `
		UPDATE feeds
		SET url                   = $2,
		    etag                  = $3,
		    last_modified         = $4,
		    body_hash             = $5,
		    status                = $6,
		    redirected_to_feed_id = $7,
		    check_interval        = $8,
		    next_check_at         = $9,
		    consecutive_failures  = $10,
		    last_checked_at       = now(),
		    last_success_at       = $11,
		    last_modified_at      = $12,
		    last_status_code      = $13,
		    last_error            = $14,
		    crawl_seq             = crawl_seq + CASE WHEN $15 THEN 1 ELSE 0 END,
		    updated_at            = now()
		WHERE id = $1
		RETURNING crawl_seq`

	f := result.Feed
	var crawlSeq int64
	err = tx.QueryRow(ctx, updateFeed,
		f.ID, f.URL, f.ETag, f.LastModified, f.BodyHash, f.Status, f.RedirectedToFeedID,
		f.CheckInterval, f.NextCheckAt, f.ConsecutiveFailures, f.LastSuccessAt,
		f.LastModifiedAt, f.LastStatusCode, f.LastError, result.Parsed,
	).Scan(&crawlSeq)
	if err != nil {
		return counts, fmt.Errorf("updating feed %d: %w", f.ID, err)
	}

	if result.Parsed && len(result.Episodes) > 0 {
		counts.Upserted, err = upsertEpisodes(ctx, tx, f.ID, crawlSeq, result.Episodes)
		if err != nil {
			return counts, err
		}

		// Prune only after a parse that actually produced episodes. A feed
		// that suddenly has no items is far more likely broken than genuinely
		// empty, and pruning on that would delete its whole history.
		counts.Pruned, err = pruneEpisodes(ctx, tx, f.ID, crawlSeq, result.PruneGrace)
		if err != nil {
			return counts, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return counts, fmt.Errorf("committing crawl of feed %d: %w", f.ID, err)
	}
	return counts, nil
}

// CrawlCounts is how many episode rows a crawl wrote and removed.
type CrawlCounts struct {
	Upserted int
	Pruned   int
}

// RecentEpisodeDates returns the publication dates of a feed's most recent
// episodes, newest first. The gaps between them are what the scheduler uses to
// guess how often the feed is worth re-checking.
func (s *Store) RecentEpisodeDates(ctx context.Context, feedID int64, limit int) ([]time.Time, error) {
	const query = `
		SELECT pub_date
		FROM episodes
		WHERE feed_id = $1 AND pub_date IS NOT NULL
		ORDER BY pub_date DESC
		LIMIT $2`

	rows, err := s.pool.Query(ctx, query, feedID, limit)
	if err != nil {
		return nil, fmt.Errorf("reading recent episode dates for feed %d: %w", feedID, err)
	}
	dates, err := pgx.CollectRows(rows, pgx.RowTo[time.Time])
	if err != nil {
		return nil, fmt.Errorf("reading recent episode dates for feed %d: %w", feedID, err)
	}
	return dates, nil
}
