-- +goose Up
-- Deterministic per-message failures are backend-owned delivery state. They
-- are intentionally independent from processing_generation, which remains a
-- pure fencing token and advances for every new claim regardless of outcome.
ALTER TABLE agent_message_deliveries
  ADD COLUMN failure_count BIGINT NOT NULL DEFAULT 0;

ALTER TABLE agent_message_deliveries
  ADD CONSTRAINT agent_message_deliveries_failure_count_range
  CHECK (failure_count BETWEEN 0 AND 4611686018427387903);

-- +goose Down
ALTER TABLE agent_message_deliveries
  DROP CONSTRAINT agent_message_deliveries_failure_count_range,
  DROP COLUMN failure_count;
