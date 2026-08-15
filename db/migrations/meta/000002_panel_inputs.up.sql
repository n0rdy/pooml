-- Panels remember their assisted inputs so editing restores the same UX as
-- creating: dsl for metrics panels (compiled server-side to query, which
-- stays canonical), fts for logs panels (combined at render, like the logs
-- page). Both empty when the panel was authored as raw SQL.
ALTER TABLE dashboard_panels ADD COLUMN dsl TEXT;
ALTER TABLE dashboard_panels ADD COLUMN fts TEXT;
