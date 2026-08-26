-- 001_init.sql — feed service schema.
-- Applied by the CI pipeline against Cloud SQL before deploy. The feed service
-- also creates this schema on startup (idempotently) so local runs need no
-- separate migration step.

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
