PRAGMA journal_mode = WAL;

-- Hashed API keys. The plaintext key is shown to the user once at creation
-- and never stored.
CREATE TABLE api_keys (
    id INTEGER PRIMARY KEY,
    label TEXT NOT NULL,              -- human-readable, e.g. "fluentbit-prod"
    key_hash TEXT NOT NULL,           -- bcrypt/argon2 hash of the plaintext key
    created_at INTEGER NOT NULL
);

-- Alert definitions plus runtime state.
CREATE TABLE alerts (
    id INTEGER PRIMARY KEY,
    name TEXT NOT NULL,
    query TEXT NOT NULL,              -- SQL run against logs.db / metrics.db
    check_interval_ms INTEGER NOT NULL,
    cooldown_ms INTEGER NOT NULL,
    target TEXT NOT NULL,             -- JSON: routing config (e.g. Pushover device, priority)
    enabled INTEGER NOT NULL DEFAULT 1,

    -- runtime state, updated by the evaluator
    last_checked_at INTEGER,
    last_fired_at INTEGER,
    currently_firing INTEGER NOT NULL DEFAULT 0,
    last_error TEXT,
    last_error_at INTEGER,

    created_at INTEGER NOT NULL
);

CREATE INDEX idx_alerts_enabled ON alerts(enabled, last_checked_at);

-- Audit trail of alert firings. Subject to retention.alert_firings_days.
CREATE TABLE alert_firings (
    id INTEGER PRIMARY KEY,
    alert_id INTEGER NOT NULL REFERENCES alerts(id) ON DELETE CASCADE,
    fired_at INTEGER NOT NULL,
    matched_rows TEXT NOT NULL,       -- JSON of rows returned by the alert query
    notification_sent INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX idx_alert_firings_alert_time ON alert_firings(alert_id, fired_at DESC);

-- Settings: key-value, mixed encrypted/plain.
-- is_encrypted=1 marks values encrypted with POOML_ENCRYPTION_KEY.
CREATE TABLE settings (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL,
    is_encrypted INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL
);

-- Prometheus scrape targets configured via the UI.
CREATE TABLE scrape_targets (
    id INTEGER PRIMARY KEY,
    service TEXT NOT NULL,            -- stamped on every scraped metric
    host TEXT NOT NULL,               -- stamped on every scraped metric
    url TEXT NOT NULL,
    auth_header TEXT,                 -- e.g., "Authorization: Bearer ..." or "X-API-Key: ..."
    is_auth_encrypted INTEGER NOT NULL DEFAULT 0,
    scrape_interval_ms INTEGER NOT NULL DEFAULT 30000,
    enabled INTEGER NOT NULL DEFAULT 1,
    last_scraped_at INTEGER,
    last_error TEXT,
    last_error_at INTEGER,
    created_at INTEGER NOT NULL
);

-- Saved metric dashboards.
CREATE TABLE dashboards (
    id INTEGER PRIMARY KEY,
    name TEXT NOT NULL,
    description TEXT,
    created_at INTEGER NOT NULL
);

CREATE TABLE dashboard_panels (
    id INTEGER PRIMARY KEY,
    dashboard_id INTEGER NOT NULL REFERENCES dashboards(id) ON DELETE CASCADE,
    title TEXT NOT NULL,
    query TEXT NOT NULL,              -- SQL run against metrics.db (or both via ATTACH)
    chart_type TEXT,                  -- "line" / "bar" / "stat" / NULL = auto-detect
    position INTEGER NOT NULL,
    width INTEGER NOT NULL DEFAULT 1  -- 1, 2, or 3 grid columns
);
