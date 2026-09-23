package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// feedColumns is every column Feed has a field for. RowToStructByNameLax errors
// on a column with no matching field, so the list and the struct move together.
const feedColumns = `id, url, etag, last_modified, body_hash, status, redirected_to_feed_id,
	check_interval, next_check_at, consecutive_failures, last_checked_at, last_success_at,
	last_modified_at, last_status_code, last_error, crawl_seq, created_at, updated_at`

// UpsertFeed returns the feed for url, creating it if it is new.
//
// A new feed is due immediately, which is what makes the catalogue lazy: the
// API upserts a feed as a side effect of returning a podcast, and the crawler
// picks it up on its next pass.
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
		    WHERE status <> 'dead'
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
}

// ApplyCrawl writes a crawl's outcome: the feed's new state and, when the body
// was parsed, the episodes it contained.
//
// It runs in one transaction so that crawl_seq and the episodes stamped with it
// can never disagree — a reader would otherwise briefly see an episode list
// that is empty or half-updated.
func (s *Store) ApplyCrawl(ctx context.Context, result CrawlResult) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("beginning transaction: %w", err)
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
		return 0, fmt.Errorf("updating feed %d: %w", f.ID, err)
	}

	upserted := 0
	if result.Parsed && len(result.Episodes) > 0 {
		upserted, err = upsertEpisodes(ctx, tx, f.ID, crawlSeq, result.Episodes)
		if err != nil {
			return 0, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("committing crawl of feed %d: %w", f.ID, err)
	}
	return upserted, nil
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
