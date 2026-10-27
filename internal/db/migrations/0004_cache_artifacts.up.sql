-- Replaces cache_reads with one row per stored artifact, keyed by storage
-- key ("nx/<hash>", "turbo/<hash>", or a bare "<hash>" stored before keys
-- were namespaced). The artifact bytes still live in the storage backend;
-- this holds what the backend can't: read activity (admin UI, janitor) and
-- client-supplied metadata that must be returned on download (Turborepo's
-- x-artifact-* headers).
CREATE TABLE cache_artifacts (
    key          TEXT PRIMARY KEY,

    -- NULL for artifacts uploaded before this table existed, whose rows
    -- only come from carried-over cache_reads data.
    size         BIGINT,
    created_at   TIMESTAMPTZ,

    duration_ms  BIGINT NOT NULL DEFAULT 0,
    tag          TEXT NOT NULL DEFAULT '',
    sha          TEXT NOT NULL DEFAULT '',
    dirty_hash   TEXT NOT NULL DEFAULT '',

    read_count   BIGINT NOT NULL DEFAULT 0,
    last_read_at TIMESTAMPTZ
);

INSERT INTO cache_artifacts (key, read_count, last_read_at)
SELECT hash, read_count, last_read_at FROM cache_reads;

DROP TABLE cache_reads;
