-- +goose NO TRANSACTION
-- +goose Up
-- Cover the account-scoped admission window so a ticket opening does not scan
-- the account's full ticket history while holding its account row lock.
-- Build concurrently so old replicas can keep writing during a rolling upgrade.
-- An interrupted build may leave an invalid index, or a valid index before
-- Goose records the version. Remove either state before retrying the build.
DROP INDEX CONCURRENTLY IF EXISTS support_tickets_by_account_opened;
CREATE INDEX CONCURRENTLY support_tickets_by_account_opened
  ON support_tickets (account_id, opened_at DESC);

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS support_tickets_by_account_opened;
