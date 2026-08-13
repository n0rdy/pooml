-- Switch logs_fts to the porter stemming tokenizer so morphological variants
-- match: searching "order" finds "orders"/"ordering". Stemming happens at
-- index AND query time, so exact lookups still work. Requires dropping and
-- rebuilding the virtual table; content is re-derived from logs.

DROP TRIGGER logs_ai;
DROP TRIGGER logs_ad;
DROP TRIGGER logs_au;
DROP TABLE logs_fts;

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

INSERT INTO logs_fts(rowid, raw) SELECT id, raw FROM logs;
