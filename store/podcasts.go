package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

const podcastColumns = `id, itunes_id, feed_id, title, artist_name, artwork_url_30,
	artwork_url_100, artwork_url_600, genres, explicit, created_at, updated_at`

// PodcastUpsert is a podcast as returned by iTunes, ready to be written.
type PodcastUpsert struct {
	ItunesID      int64
	Title         string
	ArtistName    string
	ArtworkURL30  string
	ArtworkURL100 string
	ArtworkURL600 string
	Genres        []string
	Explicit      bool

	// FeedURL may be empty: iTunes does not always return one. A podcast
	// without a feed is still worth storing, it just cannot be crawled.
	FeedURL string
}

// UpsertPodcastWithFeed stores a podcast and its feed together, returning both.
//
// This is the lazy catalogue: every podcast the API hands back from a search or
// a lookup lands here, and its feed becomes due for crawling.
//
// The feed is nil when iTunes gave no feed URL.
func (s *Store) UpsertPodcastWithFeed(ctx context.Context, p PodcastUpsert) (Podcast, *Feed, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Podcast{}, nil, fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	var feed *Feed
	var feedID *int64
	if p.FeedURL != "" {
		const upsertFeed = `
			INSERT INTO feeds (url)
			VALUES ($1)
			ON CONFLICT (url) DO UPDATE SET updated_at = now()
			RETURNING ` + feedColumns

		rows, err := tx.Query(ctx, upsertFeed, p.FeedURL)
		f, err := collectOne[Feed](ctx, rows, err)
		if err != nil {
			return Podcast{}, nil, fmt.Errorf("upserting feed %q: %w", p.FeedURL, err)
		}
		feed = &f
		feedID = &f.ID
	}

	const upsertPodcast = `
		INSERT INTO podcasts (
		    itunes_id, feed_id, title, artist_name,
		    artwork_url_30, artwork_url_100, artwork_url_600, genres, explicit
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (itunes_id) DO UPDATE SET
		    title           = EXCLUDED.title,
		    artist_name     = EXCLUDED.artist_name,
		    artwork_url_30  = EXCLUDED.artwork_url_30,
		    artwork_url_100 = EXCLUDED.artwork_url_100,
		    artwork_url_600 = EXCLUDED.artwork_url_600,
		    genres          = EXCLUDED.genres,
		    explicit        = EXCLUDED.explicit,
		    -- Keep the feed we already know if this response omitted one.
		    feed_id         = COALESCE(EXCLUDED.feed_id, podcasts.feed_id),
		    updated_at      = now()
		RETURNING ` + podcastColumns

	genres := p.Genres
	if genres == nil {
		genres = []string{}
	}

	rows, err := tx.Query(ctx, upsertPodcast,
		p.ItunesID, feedID, p.Title, p.ArtistName,
		p.ArtworkURL30, p.ArtworkURL100, p.ArtworkURL600, genres, p.Explicit,
	)
	podcast, err := collectOne[Podcast](ctx, rows, err)
	if err != nil {
		return Podcast{}, nil, fmt.Errorf("upserting podcast %d: %w", p.ItunesID, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Podcast{}, nil, fmt.Errorf("committing podcast %d: %w", p.ItunesID, err)
	}
	return podcast, feed, nil
}

// PodcastByItunesID returns a podcast by its iTunes collection ID. It returns
// pgx.ErrNoRows when there is none.
func (s *Store) PodcastByItunesID(ctx context.Context, itunesID int64) (Podcast, error) {
	const query = `SELECT ` + podcastColumns + ` FROM podcasts WHERE itunes_id = $1`

	rows, err := s.pool.Query(ctx, query, itunesID)
	podcast, err := collectOne[Podcast](ctx, rows, err)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Podcast{}, err
		}
		return Podcast{}, fmt.Errorf("reading podcast %d: %w", itunesID, err)
	}
	return podcast, nil
}

// PodcastWithFeedByItunesID returns a podcast together with its feed. The feed
// is nil when the podcast has none.
func (s *Store) PodcastWithFeedByItunesID(ctx context.Context, itunesID int64) (Podcast, *Feed, error) {
	podcast, err := s.PodcastByItunesID(ctx, itunesID)
	if err != nil {
		return Podcast{}, nil, err
	}
	if podcast.FeedID == nil {
		return podcast, nil, nil
	}

	feed, err := s.FeedByID(ctx, *podcast.FeedID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return podcast, nil, nil
		}
		return Podcast{}, nil, fmt.Errorf("reading feed %d: %w", *podcast.FeedID, err)
	}
	return podcast, &feed, nil
}

// UpsertPodcasts stores many podcasts and their feeds in one transaction.
//
// A search returns up to 50 results, and seeding the catalogue from them one
// transaction at a time would add real latency to every search. Feeds are
// written first so the podcasts can reference them.
func (s *Store) UpsertPodcasts(ctx context.Context, podcasts []PodcastUpsert) error {
	if len(podcasts) == 0 {
		return nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	feedIDs, err := upsertFeedURLs(ctx, tx, podcasts)
	if err != nil {
		return err
	}

	const upsertPodcast = `
		INSERT INTO podcasts (
		    itunes_id, feed_id, title, artist_name,
		    artwork_url_30, artwork_url_100, artwork_url_600, genres, explicit
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (itunes_id) DO UPDATE SET
		    title           = EXCLUDED.title,
		    artist_name     = EXCLUDED.artist_name,
		    artwork_url_30  = EXCLUDED.artwork_url_30,
		    artwork_url_100 = EXCLUDED.artwork_url_100,
		    artwork_url_600 = EXCLUDED.artwork_url_600,
		    genres          = EXCLUDED.genres,
		    explicit        = EXCLUDED.explicit,
		    feed_id         = COALESCE(EXCLUDED.feed_id, podcasts.feed_id),
		    updated_at      = now()`

	batch := &pgx.Batch{}
	for _, p := range podcasts {
		var feedID *int64
		if id, ok := feedIDs[p.FeedURL]; ok {
			feedID = &id
		}
		genres := p.Genres
		if genres == nil {
			genres = []string{}
		}
		batch.Queue(upsertPodcast,
			p.ItunesID, feedID, p.Title, p.ArtistName,
			p.ArtworkURL30, p.ArtworkURL100, p.ArtworkURL600, genres, p.Explicit,
		)
	}

	results := tx.SendBatch(ctx, batch)
	for range podcasts {
		if _, err := results.Exec(); err != nil {
			results.Close()
			return fmt.Errorf("upserting podcasts: %w", err)
		}
	}
	if err := results.Close(); err != nil {
		return fmt.Errorf("upserting podcasts: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing podcasts: %w", err)
	}
	return nil
}

// upsertFeedURLs writes every distinct feed URL in podcasts, returning their
// ids keyed by URL.
func upsertFeedURLs(ctx context.Context, tx pgx.Tx, podcasts []PodcastUpsert) (map[string]int64, error) {
	urls := make([]string, 0, len(podcasts))
	seen := make(map[string]bool, len(podcasts))
	for _, p := range podcasts {
		if p.FeedURL == "" || seen[p.FeedURL] {
			continue
		}
		seen[p.FeedURL] = true
		urls = append(urls, p.FeedURL)
	}
	if len(urls) == 0 {
		return nil, nil
	}

	const upsertFeed = `
		INSERT INTO feeds (url)
		SELECT unnest($1::text[])
		ON CONFLICT (url) DO UPDATE SET updated_at = now()
		RETURNING id, url`

	rows, err := tx.Query(ctx, upsertFeed, urls)
	if err != nil {
		return nil, fmt.Errorf("upserting feeds: %w", err)
	}
	defer rows.Close()

	feedIDs := make(map[string]int64, len(urls))
	for rows.Next() {
		var id int64
		var url string
		if err := rows.Scan(&id, &url); err != nil {
			return nil, fmt.Errorf("upserting feeds: %w", err)
		}
		feedIDs[url] = id
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("upserting feeds: %w", err)
	}
	return feedIDs, nil
}
