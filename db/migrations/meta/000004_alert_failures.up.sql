-- An alert whose query cannot run is blind, and used to say so only as red
-- text on the Alerts page. These track the current failure streak so the
-- evaluator can notify about it (see CONTEXT.md > Alerting).
ALTER TABLE alerts ADD COLUMN failure_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE alerts ADD COLUMN failing_since INTEGER;
ALTER TABLE alerts ADD COLUMN failure_notified_at INTEGER;
