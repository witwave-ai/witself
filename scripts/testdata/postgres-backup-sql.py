#!/usr/bin/env python3
"""Exercise operator SQL and the rendered exporter query in codex_a, then roll back."""
import os
import re
from pathlib import Path
import subprocess
import sys
import tempfile

root = Path(sys.argv[1]).resolve()
url = os.environ.get("WITSELF_TEST_DATABASE_URL", "")
if os.environ.get("WITSELF_TEST_REQUIRE_DATABASE") != "1" or url != "postgres://postgres:test@127.0.0.1:5599/codex_a":
    sys.exit("backup SQL checks require the exact permitted codex_a URL and WITSELF_TEST_REQUIRE_DATABASE=1")

with tempfile.TemporaryDirectory(prefix="witself-backup-sql-") as home:
    env = dict(os.environ, HOME=home, WITSELF_HOME=home + "/witself", DSH_HOME=home + "/dsh")
    rendered = subprocess.run([
        "helm", "template", "witself-apps", str(root / ".gitops/charts/apps"),
        "--values", str(root / ".gitops/cells/civo-sandbox-use1-serving/values.yaml"),
        "--set", "apps.civoPostgres.backup.enabled=true",
    ], env=env, capture_output=True, text=True, timeout=30)
    if rendered.returncode:
        sys.exit("backup SQL checks could not render the chart")
    extracted = subprocess.run([
        "ruby", "-ryaml", "-e",
        "docs=YAML.load_stream(STDIN.read).compact; "
        "pg=docs.find { |d| d['kind']=='Application' && d.dig('metadata','name')=='witself-postgresql' }; "
        "print YAML.safe_load(pg.dig('spec','source','helm','values'), aliases: false)"
        ".dig('metrics','customMetrics','witself_postgres_backup','query')",
    ], input=rendered.stdout, env=env, capture_output=True, text=True, timeout=30)
    query = extracted.stdout.strip().rstrip(";")
    if extracted.returncode or "witself_ops.backup_runs" not in query:
        sys.exit("backup SQL checks could not extract the rendered exporter query")

    def check_query(expected, label):
        import json
        return "SELECT pg_temp.backup_assert((SELECT to_jsonb(q) FROM (" + query + ") q) = '" + json.dumps(expected) + "'::jsonb, '" + label + "');\n"

    def expected(success, attempt, succeeded, failures):
        return dict(last_success_timestamp_seconds=success, last_attempt_timestamp_seconds=attempt,
                    last_attempt_succeeded=succeeded, failures_total=failures)

    sql = """
BEGIN;
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '15s';
CREATE FUNCTION pg_temp.backup_assert(ok boolean, label text) RETURNS void
LANGUAGE plpgsql AS $$ BEGIN IF ok IS DISTINCT FROM true THEN RAISE EXCEPTION 'backup fixture failed: %', label; END IF; END $$;
DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='witself') THEN CREATE ROLE witself; END IF; END $$;
GRANT USAGE, CREATE ON SCHEMA public TO witself;
SET LOCAL ROLE witself;
CREATE TABLE public.witself_backup_privilege_fixture (value integer);
INSERT INTO public.witself_backup_privilege_fixture VALUES (10);
CREATE SEQUENCE public.witself_backup_sequence_fixture;
RESET ROLE;
"""
    sql += (root / ".gitops/charts/apps/files/postgres-backup-dump-role.sql").read_text()
    sql += (root / ".gitops/charts/apps/files/postgres-backup-metrics-role.sql").read_text()
    sql += """
SELECT pg_temp.backup_assert(NOT EXISTS (
 SELECT 1 FROM pg_roles WHERE rolname IN ('witself_backup_dump','witself_backup_metrics')
 AND (rolsuper OR rolcreatedb OR rolcreaterole OR rolinherit OR rolreplication OR rolbypassrls)
), 'restricted role attributes');
SELECT pg_temp.backup_assert((SELECT rolconfig @> ARRAY['default_transaction_read_only=on']
 FROM pg_roles WHERE rolname='witself_backup_dump'), 'dump defaults to read-only');
SET LOCAL ROLE witself;
CREATE TABLE public.witself_backup_future_fixture (value integer);
CREATE SEQUENCE public.witself_backup_future_sequence_fixture;
RESET ROLE;
SET LOCAL ROLE witself_backup_dump;
SELECT pg_temp.backup_assert((SELECT value FROM public.witself_backup_privilege_fixture)=10, 'dump can read existing data');
SELECT * FROM public.witself_backup_future_fixture;
SELECT last_value FROM public.witself_backup_sequence_fixture;
SELECT last_value FROM public.witself_backup_future_sequence_fixture;
DO $$ BEGIN
 BEGIN INSERT INTO public.witself_backup_privilege_fixture VALUES (11); RAISE EXCEPTION 'dump wrote application data'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN UPDATE public.witself_backup_privilege_fixture SET value=12; RAISE EXCEPTION 'dump updated application data'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN DELETE FROM public.witself_backup_privilege_fixture; RAISE EXCEPTION 'dump deleted application data'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN CREATE TABLE public.witself_backup_forbidden (value integer); RAISE EXCEPTION 'dump created persistent object'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN UPDATE witself_ops.backup_runs SET succeeded=true; RAISE EXCEPTION 'dump changed telemetry'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
END $$;
RESET ROLE;
SET LOCAL ROLE witself_backup_metrics;
DO $$ BEGIN
 BEGIN SELECT value FROM public.witself_backup_privilege_fixture; RAISE EXCEPTION 'writer read application data'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN INSERT INTO public.witself_backup_privilege_fixture VALUES (11); RAISE EXCEPTION 'writer changed application data'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN CREATE TABLE witself_ops.forbidden (value integer); RAISE EXCEPTION 'writer created telemetry object'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN CREATE TABLE public.witself_backup_forbidden (value integer); RAISE EXCEPTION 'writer created public object'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN DELETE FROM witself_ops.backup_runs; RAISE EXCEPTION 'writer deleted telemetry'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN TRUNCATE witself_ops.backup_runs; RAISE EXCEPTION 'writer truncated telemetry'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN ALTER TABLE witself_ops.backup_runs ADD COLUMN forbidden integer; RAISE EXCEPTION 'writer altered telemetry'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
END $$;
RESET ROLE;
"""
    sql += check_query(expected(0, 0, -1, 0), "never attempted")
    sql += """
SET LOCAL ROLE witself_backup_metrics;
INSERT INTO witself_ops.backup_runs (started_at) VALUES (to_timestamp(100));
UPDATE witself_ops.backup_runs SET finished_at=to_timestamp(101), succeeded=true,
 bytes=42, object_key='fixture/complete.sql.gz.age' WHERE started_at=to_timestamp(100);
RESET ROLE;
"""
    sql += check_query(expected(101, 100, 1, 0), "first successful attempt")
    sql += """
SET LOCAL ROLE witself_backup_metrics;
INSERT INTO witself_ops.backup_runs (started_at) VALUES (to_timestamp(200));
RESET ROLE;
"""
    sql += check_query(expected(101, 200, -1, 0), "running attempt is neither failure nor success")
    sql += """
SET LOCAL ROLE witself_backup_metrics;
UPDATE witself_ops.backup_runs SET finished_at=to_timestamp(201), succeeded=false,
 error_text='stream_upload' WHERE started_at=to_timestamp(200);
DO $$ BEGIN
 BEGIN UPDATE witself_ops.backup_runs SET error_text=repeat('x',161); RAISE EXCEPTION 'unbounded errors accepted'; EXCEPTION WHEN string_data_right_truncation THEN NULL; END;
 BEGIN INSERT INTO witself_ops.backup_runs (started_at,succeeded) VALUES (to_timestamp(400),true); RAISE EXCEPTION 'incomplete success accepted'; EXCEPTION WHEN check_violation THEN NULL; END;
END $$;
RESET ROLE;
"""
    sql += check_query(expected(101, 200, 0, 1), "failure retains last success and increments counter")
    sql += """
SET LOCAL ROLE witself_backup_metrics;
INSERT INTO witself_ops.backup_runs (started_at,finished_at,succeeded,bytes,object_key)
 VALUES (to_timestamp(300),to_timestamp(301),true,43,'fixture/recovered.sql.gz.age');
RESET ROLE;
"""
    sql += check_query(expected(301, 300, 1, 1), "recovery retains historical failure count")
    # Execute the production heredoc statements too: this catches parameter
    # binding, timestamp round trips, and SQL syntax drift hidden by command fakes.
    runner = (root / ".gitops/charts/apps/files/postgres-backup.sh").read_text()
    statements = re.findall(r"<<'SQL'[^\n]*\n(.*?)\nSQL", runner, re.S)
    if len(statements) != 3:
        sys.exit("backup SQL checks could not identify the three runtime statements")
    start_sql = next(statement for statement in statements if "INSERT INTO" in statement)
    success_sql = next(statement for statement in statements if "succeeded = true" in statement)
    failure_sql = next(statement for statement in statements if "succeeded = false" in statement)
    sql += "SET LOCAL ROLE witself_backup_metrics;\n"
    sql += start_sql.rstrip(";") + "\n\\gset runner_\n"
    sql += "\\set run_started :runner_started_at\n\\set object_key 'fixture/runner.sql.gz.age'\n\\set bytes 123\n"
    sql += success_sql + "\n"
    sql += "SELECT pg_temp.backup_assert((SELECT succeeded AND bytes=123 AND object_key='fixture/runner.sql.gz.age' FROM witself_ops.backup_runs WHERE started_at=:'run_started'::timestamptz), 'runtime success statement');\n"
    sql += start_sql.rstrip(";") + "\n\\gset runner_\n"
    sql += "\\set run_started :runner_started_at\n\\set error_text stream_upload\n\\set bytes 0\n"
    sql += failure_sql + "\n"
    sql += "SELECT pg_temp.backup_assert((SELECT NOT succeeded AND finished_at IS NOT NULL AND bytes=0 AND error_text='stream_upload' FROM witself_ops.backup_runs WHERE started_at=:'run_started'::timestamptz), 'runtime failure statement');\n"
    sql += "RESET ROLE;\nROLLBACK;\n"
    result = subprocess.run([
        "psql", "--no-psqlrc", "--no-password", "--set=ON_ERROR_STOP=1", "--quiet", "--dbname=" + url,
    ], input=sql, env=env, capture_output=True, text=True, timeout=60)
    if result.returncode:
        # No dump or credentials are present in SQL, but keep output bounded anyway.
        lines = [line for line in result.stderr.splitlines() if "ERROR:" in line]
        sys.exit("backup SQL checks failed: " + (lines[0] if lines else "database command failed"))
    print("PostgreSQL backup SQL checks passed (transaction rolled back; roles, grants, telemetry and query states verified)")
