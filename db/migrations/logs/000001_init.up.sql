PRAGMA journal_mode = WAL;

CREATE TABLE logs (
    id INTEGER PRIMARY KEY,           -- auto-increment, used for streaming cursors and pagination
    timestamp INTEGER NOT NULL,       -- client event time, milliseconds since Unix epoch (UTC)
    ingested_at INTEGER NOT NULL,     -- server receive time, milliseconds since Unix epoch (UTC)
    level INTEGER NOT NULL,           -- 0=trace .. 5=fatal; parsers fall back to INFO (2)
    service TEXT NOT NULL,
    host TEXT NOT NULL,
    message TEXT,                     -- extracted message, best-effort (NULL when parser couldn't extract)
    parsed TEXT,                      -- JSON of parser-extracted fields (NULL when nothing structured)
    raw TEXT NOT NULL                 -- original log line as received
);

CREATE INDEX idx_timestamp ON logs(timestamp DESC);
CREATE INDEX idx_level ON logs(level, timestamp DESC);
CREATE INDEX idx_service ON logs(service, timestamp DESC);
CREATE INDEX idx_host ON logs(host, timestamp DESC);

-- Full-text search over the raw log line, porter-stemmed so morphological
-- variants match: searching "order" finds "orders"/"ordering". Stemming
-- happens at index AND query time, so exact lookups still work.
-- content=logs links the FTS index to the logs table; the triggers below keep
-- the index in sync (FTS5 does NOT auto-sync content-linked tables).
CREATE VIRTUAL TABLE logs_fts USING fts5(
    raw,
    content=logs,
    content_rowid=id,
    tokenize='porter unicode61'
);

CREATE TRIGGER logs_ai AFTER INSERT ON logs BEGIN
    INSERT INTO logs_fts(rowid, raw) VALUES (new.id, new.raw);
END;

CREATE TRIGGER logs_ad AFTER DELETE ON logs BEGIN
    INSERT INTO logs_fts(logs_fts, rowid, raw) VALUES('delete', old.id, old.raw);
END;

CREATE TRIGGER logs_au AFTER UPDATE ON logs BEGIN
    INSERT INTO logs_fts(logs_fts, rowid, raw) VALUES('delete', old.id, old.raw);
    INSERT INTO logs_fts(rowid, raw) VALUES (new.id, new.raw);
END;
