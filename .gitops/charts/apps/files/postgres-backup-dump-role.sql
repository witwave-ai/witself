-- Operator-only, one-time provisioning in the application database (witself).
-- Run with psql --no-psqlrc --set=ON_ERROR_STOP=1 --single-transaction.
-- Set the login password privately afterwards with \password; never commit it.
CREATE ROLE witself_backup_dump LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE
  NOINHERIT NOREPLICATION NOBYPASSRLS;
ALTER ROLE witself_backup_dump SET default_transaction_read_only = on;
GRANT CONNECT ON DATABASE :"DBNAME" TO witself_backup_dump;
GRANT USAGE ON SCHEMA public TO witself_backup_dump;
GRANT SELECT ON ALL TABLES IN SCHEMA public TO witself_backup_dump;
GRANT SELECT ON ALL SEQUENCES IN SCHEMA public TO witself_backup_dump;
-- The application migrates as witself. Apply equivalent defaults explicitly
-- if an operator introduces another object owner or a non-public schema.
ALTER DEFAULT PRIVILEGES FOR ROLE witself IN SCHEMA public
  GRANT SELECT ON TABLES TO witself_backup_dump;
ALTER DEFAULT PRIVILEGES FOR ROLE witself IN SCHEMA public
  GRANT SELECT ON SEQUENCES TO witself_backup_dump;
