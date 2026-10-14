-- 001_init.sql — feed service schema.
-- The versioned form of the schema, for a migration tool such as
-- golang-migrate. The feed service also creates this schema on startup
-- (idempotently), which is all Snapvault currently relies on; keep the two in
-- sync until a migration step is added to the pipeline.

CREATE TABLE IF NOT EXISTS feed_items (
    id           TEXT PRIMARY KEY,
    owner        TEXT NOT NULL,
    thumb_object TEXT,
    width        INTEGER,
    height       INTEGER,
    uploaded_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The gallery is always read newest-first; this index keeps that query fast.
CREATE INDEX IF NOT EXISTS feed_items_uploaded_at_idx
    ON feed_items (uploaded_at DESC);
