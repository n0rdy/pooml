-- Backfill for the opinionated level fallback: since this migration, parsers
-- default unextractable levels to INFO (2), so level is never NULL and level
-- filters need no `OR level IS NULL` conditioning. Align history.
UPDATE logs SET level = 2 WHERE level IS NULL;
