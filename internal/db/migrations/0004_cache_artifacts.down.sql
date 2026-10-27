CREATE TABLE cache_reads (
    hash         TEXT PRIMARY KEY,
    read_count   BIGINT NOT NULL DEFAULT 0,
    last_read_at TIMESTAMPTZ
);

INSERT INTO cache_reads (hash, read_count, last_read_at)
SELECT key, read_count, last_read_at FROM cache_artifacts WHERE read_count > 0;

DROP TABLE cache_artifacts;
