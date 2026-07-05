-- +goose Up
-- Fleet-admin read pattern: the cell-wide event tail ("what just
-- happened on this cell, newest first, across every account") that
-- feeds the operator dashboard's events pane and `witself-admin
-- events list|watch`. The existing indexes lead on account_id or verb;
-- a cell-wide ORDER BY occurred_at DESC, id DESC needs its own.
-- Additive-only: no upgrader required.
CREATE INDEX account_events_by_time
  ON account_events (occurred_at DESC, id DESC);

-- +goose Down
DROP INDEX account_events_by_time;
