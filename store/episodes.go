package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

const episodeColumns = `id, feed_id, guid, guid_source, title, description, audio_url,
	audio_length, audio_type, author, pub_date, pub_date_raw, link, explicit, duration,
	episode_no, season_no, episode_type, image_url, position, last_crawl_seq,
	first_seen_at, last_seen_at, updated_at`

// EpisodeUpsert is one episode as parsed from a feed, ready to be written.
type EpisodeUpsert struct {
	// Guid identifies the episode within its feed. When the feed's <guid> is
	// missing or repeated, the crawler substitutes a hash of the enclosure URL
	// and says so in GuidSource.
	Guid       string
	GuidSource string

	Title       string
	Description string
	AudioURL    string
	AudioLength int64
	AudioType   string
	Author      string

	PubDate    *time.Time
	PubDateRaw string

	Link        string
	Explicit    bool
	Duration    string
	EpisodeNo   *int32
	SeasonNo    *int32
	EpisodeType string
	ImageURL    string

	// Position is the item's index within the feed document.
	Position int32
}

// upsertEpisodes writes episodes for one crawl. It is idempotent: crawling the
// same feed twice updates rows in place rather than duplicating them.
func upsertEpisodes(ctx context.Context, tx pgx.Tx, feedID, crawlSeq int64, episodes []EpisodeUpsert) (int, error) {
	const query = `
		INSERT INTO episodes (
		    feed_id, guid, guid_source, title, description, audio_url, audio_length,
		    audio_type, author, pub_date, pub_date_raw, link, explicit, duration,
		    episode_no, season_no, episode_type, image_url, position, last_crawl_seq
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20)
		ON CONFLICT (feed_id, guid) DO UPDATE SET
		    guid_source    = EXCLUDED.guid_source,
		    title          = EXCLUDED.title,
		    description    = EXCLUDED.description,
		    audio_url      = EXCLUDED.audio_url,
		    audio_length   = EXCLUDED.audio_length,
		    audio_type     = EXCLUDED.audio_type,
		    author         = EXCLUDED.author,
		    pub_date       = EXCLUDED.pub_date,
		    pub_date_raw   = EXCLUDED.pub_date_raw,
		    link           = EXCLUDED.link,
		    explicit       = EXCLUDED.explicit,
		    duration       = EXCLUDED.duration,
		    episode_no     = EXCLUDED.episode_no,
		    season_no      = EXCLUDED.season_no,
		    episode_type   = EXCLUDED.episode_type,
		    image_url      = EXCLUDED.image_url,
		    position       = EXCLUDED.position,
		    last_crawl_seq = EXCLUDED.last_crawl_seq,
		    last_seen_at   = now(),
		    updated_at     = now()`

	batch := &pgx.Batch{}
	for _, e := range episodes {
		batch.Queue(query,
			feedID, e.Guid, e.GuidSource, e.Title, e.Description, e.AudioURL, e.AudioLength,
			e.AudioType, e.Author, e.PubDate, e.PubDateRaw, e.Link, e.Explicit, e.Duration,
			e.EpisodeNo, e.SeasonNo, e.EpisodeType, e.ImageURL, e.Position, crawlSeq,
		)
	}

	results := tx.SendBatch(ctx, batch)
	defer results.Close()

	upserted := 0
	for i := range episodes {
		if _, err := results.Exec(); err != nil {
			return 0, fmt.Errorf("upserting episode %q of feed %d: %w", episodes[i].Guid, feedID, err)
		}
		upserted++
	}
	if err := results.Close(); err != nil {
		return 0, fmt.Errorf("upserting episodes of feed %d: %w", feedID, err)
	}
	return upserted, nil
}

// EpisodesByFeed returns the episodes the most recent crawl of this feed saw,
// in the order they appeared in the feed document.
//
// Filtering on the feed's current crawl_seq means an episode a publisher has
// removed stops being served immediately, while its row survives.
func (s *Store) EpisodesByFeed(ctx context.Context, feedID int64) ([]Episode, error) {
	const query = `
		SELECT ` + episodeColumns + `
		FROM episodes e
		JOIN feeds f ON f.id = e.feed_id
		WHERE e.feed_id = $1
		  AND e.last_crawl_seq = f.crawl_seq
		ORDER BY e.position`

	rows, err := s.pool.Query(ctx, query, feedID)
	if err != nil {
		return nil, fmt.Errorf("reading episodes of feed %d: %w", feedID, err)
	}
	episodes, err := pgx.CollectRows(rows, pgx.RowToStructByNameLax[Episode])
	if err != nil {
		return nil, fmt.Errorf("reading episodes of feed %d: %w", feedID, err)
	}
	return episodes, nil
}

// CountEpisodes returns how many episodes are stored for a feed, across all
// crawls.
func (s *Store) CountEpisodes(ctx context.Context, feedID int64) (int, error) {
	var count int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM episodes WHERE feed_id = $1`, feedID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("counting episodes of feed %d: %w", feedID, err)
	}
	return count, nil
}
