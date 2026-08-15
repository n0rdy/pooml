-- The logs panel form reuses the logs page's search controls verbatim,
-- including the AND/OR combinator - so panels store it too.
ALTER TABLE dashboard_panels ADD COLUMN op TEXT;
