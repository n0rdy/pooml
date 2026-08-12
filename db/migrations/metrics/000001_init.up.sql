PRAGMA journal_mode = WAL;

CREATE TABLE metrics (
    id INTEGER PRIMARY KEY,
    timestamp INTEGER NOT NULL,       -- ms since Unix epoch (UTC)
    name TEXT NOT NULL,               -- e.g., "http_requests_total"
    type INTEGER NOT NULL,            -- 0=counter, 1=gauge
    value REAL NOT NULL,
    service TEXT NOT NULL,
    host TEXT NOT NULL,
    labels TEXT                       -- JSON of label k/v pairs; NULL if none
);

CREATE INDEX idx_metric_name_time ON metrics(name, service, timestamp DESC);
