-- Feeds now have a lifecycle rather than being crawled the moment they are
-- seen. A search returns 50 podcasts and seeds 50 feeds; crawling all of them
-- meant storage and bandwidth tracked search impressions rather than what
-- anyone actually read.
--
-- dormant    seeded from a search, nobody has asked for it   -- not crawled
-- active     someone requested its episodes                  -- crawled when due
-- dead       gone, or failed too often                       -- retried rarely
-- redirected stub pointing at another feed                   -- not crawled

ALTER TABLE feeds
    ADD COLUMN activated_at      timestamptz,
    ADD COLUMN last_requested_at timestamptz;

ALTER TABLE feeds DROP CONSTRAINT feeds_status_check;
ALTER TABLE feeds ADD CONSTRAINT feeds_status_check
    CHECK (status IN ('dormant', 'active', 'dead', 'redirected'));

-- A feed we have already crawled was, by definition, wanted. Keep it active so
-- nothing that currently works stops working.
UPDATE feeds
SET activated_at = COALESCE(last_success_at, created_at)
WHERE last_success_at IS NOT NULL;

-- Seeded but never crawled: park it until somebody asks for its episodes.
UPDATE feeds
SET status = 'dormant'
WHERE last_success_at IS NULL AND status = 'active';

ALTER TABLE feeds ALTER COLUMN status SET DEFAULT 'dormant';

-- The claim query's access path. Dead feeds are included so that
-- CRAWLER_DEAD_RECHECK actually takes effect: the previous index and query
-- excluded them outright, so a feed marked dead was never retried despite the
-- crawler scheduling it.
DROP INDEX feeds_due_idx;
CREATE INDEX feeds_due_idx ON feeds (next_check_at) WHERE status IN ('active', 'dead');
