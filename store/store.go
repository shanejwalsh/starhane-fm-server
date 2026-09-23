// Package store is every SQL query the application runs.
//
// Queries are hand-written against pgx rather than generated: the set is small,
// and keeping it in one package means a code generator can be introduced later
// without touching callers.
package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Feed status values.
const (
	// StatusActive is a feed that is crawled on its normal schedule.
	StatusActive = "active"
	// StatusDead is a feed that is gone or has failed too many times. Dead
	// feeds are re-checked rarely rather than never.
	StatusDead = "dead"
	// StatusRedirected is a feed that permanently moved to a URL another row
	// already owns, so this row is kept only to resolve old references.
	StatusRedirected = "redirected"
)

// Guid sources.
const (
	// GuidFromFeed means the episode's <guid> was used directly.
	GuidFromFeed = "guid"
	// GuidFromEnclosureHash means <guid> was missing or repeated within the
	// document, so a hash of the enclosure URL was used instead.
	GuidFromEnclosureHash = "enclosure_hash"
)

// Store runs queries against a Postgres pool.
type Store struct {
	pool *pgxpool.Pool
}

// New returns a Store backed by pool.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Pool exposes the underlying pool for health checks and shutdown.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Feed is a row of the feeds table: one RSS feed and its crawl state.
type Feed struct {
	ID  int64  `db:"id"`
	URL string `db:"url"`

	// ETag and LastModified are the validators from the last successful fetch,
	// replayed as If-None-Match and If-Modified-Since. Empty means absent.
	ETag         string `db:"etag"`
	LastModified string `db:"last_modified"`
	// BodyHash is the sha256 of the last parsed body. Hosts that ignore
	// conditional GET still send a full body, so comparing hashes is what
	// actually saves the parse.
	BodyHash []byte `db:"body_hash"`

	Status             string `db:"status"`
	RedirectedToFeedID *int64 `db:"redirected_to_feed_id"`

	CheckInterval time.Duration `db:"check_interval"`
	NextCheckAt   time.Time     `db:"next_check_at"`

	ConsecutiveFailures int        `db:"consecutive_failures"`
	LastCheckedAt       *time.Time `db:"last_checked_at"`
	// LastSuccessAt being nil is how a never-yet-crawled feed is recognised.
	LastSuccessAt  *time.Time `db:"last_success_at"`
	LastModifiedAt *time.Time `db:"last_modified_at"`
	LastStatusCode int        `db:"last_status_code"`
	LastError      string     `db:"last_error"`

	CrawlSeq int64 `db:"crawl_seq"`

	CreatedAt time.Time `db:"created_at"`
	UpdatedAt time.Time `db:"updated_at"`
}

// NeverCrawled reports whether the feed has yet to be fetched and parsed once.
func (f Feed) NeverCrawled() bool { return f.LastSuccessAt == nil }

// Podcast is a row of the podcasts table.
type Podcast struct {
	ID            int64    `db:"id"`
	ItunesID      *int64   `db:"itunes_id"`
	FeedID        *int64   `db:"feed_id"`
	Title         string   `db:"title"`
	ArtistName    string   `db:"artist_name"`
	ArtworkURL30  string   `db:"artwork_url_30"`
	ArtworkURL100 string   `db:"artwork_url_100"`
	ArtworkURL600 string   `db:"artwork_url_600"`
	Genres        []string `db:"genres"`
	Explicit      bool     `db:"explicit"`

	CreatedAt time.Time `db:"created_at"`
	UpdatedAt time.Time `db:"updated_at"`
}

// Episode is a row of the episodes table.
type Episode struct {
	ID         int64  `db:"id"`
	FeedID     int64  `db:"feed_id"`
	Guid       string `db:"guid"`
	GuidSource string `db:"guid_source"`

	Title       string `db:"title"`
	Description string `db:"description"`
	AudioURL    string `db:"audio_url"`
	AudioLength int64  `db:"audio_length"`
	AudioType   string `db:"audio_type"`
	Author      string `db:"author"`

	PubDate *time.Time `db:"pub_date"`
	// PubDateRaw is the feed's original date string, which is what the API has
	// always returned.
	PubDateRaw string `db:"pub_date_raw"`

	Link        string `db:"link"`
	Explicit    bool   `db:"explicit"`
	Duration    string `db:"duration"`
	EpisodeNo   *int32 `db:"episode_no"`
	SeasonNo    *int32 `db:"season_no"`
	EpisodeType string `db:"episode_type"`
	ImageURL    string `db:"image_url"`

	Position     int32 `db:"position"`
	LastCrawlSeq int64 `db:"last_crawl_seq"`

	FirstSeenAt time.Time `db:"first_seen_at"`
	LastSeenAt  time.Time `db:"last_seen_at"`
	UpdatedAt   time.Time `db:"updated_at"`
}

// collectOne runs a query expected to return exactly one row and maps it onto T
// by column name. pgx.ErrNoRows is returned unwrapped so callers can test it
// with errors.Is.
func collectOne[T any](ctx context.Context, q pgx.Rows, err error) (T, error) {
	var zero T
	if err != nil {
		return zero, err
	}
	row, err := pgx.CollectExactlyOneRow(q, pgx.RowToStructByNameLax[T])
	if err != nil {
		return zero, err
	}
	return row, nil
}
