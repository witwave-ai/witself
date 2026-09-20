-- Operator-only, one-time provisioning in the exporter's postgres database.
-- Run with psql --no-psqlrc --set=ON_ERROR_STOP=1 --single-transaction.
-- Set the login password privately afterwards with \password; never commit it.
CREATE ROLE witself_backup_metrics LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE
  NOINHERIT NOREPLICATION NOBYPASSRLS;
CREATE SCHEMA witself_ops;
REVOKE ALL ON SCHEMA witself_ops FROM PUBLIC;
CREATE TABLE witself_ops.backup_runs (
  started_at timestamptz PRIMARY KEY,
  finished_at timestamptz,
  succeeded boolean NOT NULL DEFAULT false,
  bytes bigint NOT NULL DEFAULT 0 CHECK (bytes >= 0),
  object_key text NOT NULL DEFAULT '',
  error_text varchar(160) NOT NULL DEFAULT '',
  CHECK (finished_at IS NULL OR finished_at >= started_at),
  CHECK (NOT succeeded OR (finished_at IS NOT NULL AND bytes > 0 AND object_key <> ''))
);
REVOKE ALL ON witself_ops.backup_runs FROM PUBLIC;
GRANT CONNECT ON DATABASE :"DBNAME" TO witself_backup_metrics;
GRANT USAGE ON SCHEMA witself_ops TO witself_backup_metrics;
GRANT SELECT, INSERT, UPDATE ON witself_ops.backup_runs TO witself_backup_metrics;
-- Bitnami PostgreSQL 18.8.0's existing exporter connects as postgres, the
-- provisioning administrator. A separately customized exporter login needs
-- only USAGE on witself_ops and SELECT on witself_ops.backup_runs.
