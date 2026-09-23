-- Feeds are the crawl queue: one row per RSS feed URL, carrying everything
-- needed to decide when to fetch it next and how to fetch it cheaply.
CREATE TABLE feeds (
    id                    bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    url                   text        NOT NULL UNIQUE,

    -- Conditional GET validators, replayed as If-None-Match / If-Modified-Since.
    etag                  text,
    last_modified         text,
    -- sha256 of the last body we parsed. Hosts that ignore conditional GET
    -- still send a full body; comparing hashes lets us skip the parse.
    body_hash             bytea,

    status                text        NOT NULL DEFAULT 'active'
                              CHECK (status IN ('active', 'dead', 'redirected')),
    -- Set when this feed permanently moved to a URL another row already owns.
    redirected_to_feed_id bigint      REFERENCES feeds (id) ON DELETE SET NULL,

    check_interval        interval    NOT NULL DEFAULT '6 hours',
    next_check_at         timestamptz NOT NULL DEFAULT now(),

    consecutive_failures  integer     NOT NULL DEFAULT 0,
    last_checked_at       timestamptz,
    last_success_at       timestamptz,
    last_modified_at      timestamptz,
    last_status_code      integer,
    last_error            text,

    -- Incremented on every crawl that actually parsed a body. Episodes are
    -- stamped with it, so the API can serve exactly the set of episodes the
    -- most recent crawl saw.
    crawl_seq             bigint      NOT NULL DEFAULT 0,

    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT feeds_redirect_is_not_self CHECK (redirected_to_feed_id IS DISTINCT FROM id)
);

-- The claim query's access path: due, not dead, oldest first.
CREATE INDEX feeds_due_idx ON feeds (next_check_at) WHERE status <> 'dead';

-- Podcasts are the catalogue. itunes_id is nullable so other sources can seed
-- rows later; feed_id is nullable because iTunes sometimes returns no feedUrl.
CREATE TABLE podcasts (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    itunes_id       bigint UNIQUE,
    feed_id         bigint REFERENCES feeds (id) ON DELETE SET NULL,

    title           text        NOT NULL DEFAULT '',
    artist_name     text        NOT NULL DEFAULT '',
    artwork_url_30  text        NOT NULL DEFAULT '',
    artwork_url_100 text        NOT NULL DEFAULT '',
    artwork_url_600 text        NOT NULL DEFAULT '',
    genres          text[]      NOT NULL DEFAULT '{}',
    explicit        boolean     NOT NULL DEFAULT false,

    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX podcasts_feed_id_idx ON podcasts (feed_id);

-- Episodes are keyed by (feed, guid). guid_source records whether that guid
-- came from the feed's <guid> or from a hash of the enclosure URL, which is
-- the fallback when <guid> is missing or repeated within one document.
CREATE TABLE episodes (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    feed_id        bigint      NOT NULL REFERENCES feeds (id) ON DELETE CASCADE,
    guid           text        NOT NULL,
    guid_source    text        NOT NULL DEFAULT 'guid'
                       CHECK (guid_source IN ('guid', 'enclosure_hash')),

    title          text        NOT NULL DEFAULT '',
    description    text        NOT NULL DEFAULT '',
    audio_url      text        NOT NULL DEFAULT '',
    audio_length   bigint      NOT NULL DEFAULT 0,
    audio_type     text        NOT NULL DEFAULT '',
    author         text        NOT NULL DEFAULT '',

    -- pub_date is parsed for cadence maths; pub_date_raw preserves the feed's
    -- original string, which is what the API has always returned.
    pub_date       timestamptz,
    pub_date_raw   text        NOT NULL DEFAULT '',

    link           text        NOT NULL DEFAULT '',
    explicit       boolean     NOT NULL DEFAULT false,
    duration       text        NOT NULL DEFAULT '',
    episode_no     integer,
    season_no      integer,
    episode_type   text        NOT NULL DEFAULT '',
    image_url      text        NOT NULL DEFAULT '',

    -- Index of the item within the feed document. The API has always returned
    -- episodes in feed order rather than date order, so that order is stored.
    position       integer     NOT NULL DEFAULT 0,
    -- feeds.crawl_seq at the last crawl that saw this episode.
    last_crawl_seq bigint      NOT NULL DEFAULT 0,

    first_seen_at  timestamptz NOT NULL DEFAULT now(),
    last_seen_at   timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),

    UNIQUE (feed_id, guid)
);

-- Serving an episode list: one feed, current crawl, in feed order.
CREATE INDEX episodes_feed_position_idx ON episodes (feed_id, last_crawl_seq, position);
-- Recent-episode cadence, used to pick the next check interval.
CREATE INDEX episodes_feed_pub_date_idx ON episodes (feed_id, pub_date DESC NULLS LAST);
