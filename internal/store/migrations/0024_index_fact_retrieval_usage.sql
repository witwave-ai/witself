-- +goose Up
-- Ranking reads aggregate only explicit fact deliveries. Automatic self
-- hydration is intentionally absent so background context loading cannot
-- reinforce its own ordering.
CREATE INDEX usage_events_fact_ranking
    ON usage_events (account_id, realm_id, agent_id, subject_id, occurred_at DESC)
    WHERE dimension = 'fact_returned'
      AND unit = 'fact'
      AND subject_type = 'fact'
      AND COALESCE(metadata->>'retrieval_mode', 'exact') IN ('exact', 'search', 'temporal');

-- +goose Down
DROP INDEX usage_events_fact_ranking;
