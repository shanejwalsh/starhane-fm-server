DROP INDEX IF EXISTS feeds_due_idx;

-- Everything dormant becomes active again, which is what the old schema meant
-- by "seeded": crawled on the next pass.
UPDATE feeds SET status = 'active' WHERE status = 'dormant';

ALTER TABLE feeds ALTER COLUMN status SET DEFAULT 'active';

ALTER TABLE feeds DROP CONSTRAINT feeds_status_check;
ALTER TABLE feeds ADD CONSTRAINT feeds_status_check
    CHECK (status IN ('active', 'dead', 'redirected'));

ALTER TABLE feeds
    DROP COLUMN activated_at,
    DROP COLUMN last_requested_at;

CREATE INDEX feeds_due_idx ON feeds (next_check_at) WHERE status <> 'dead';
