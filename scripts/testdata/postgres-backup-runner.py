#!/usr/bin/env python3
"""Run the production backup shell against synthetic commands, never a service."""

import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile


def require(condition, message):
    if not condition:
        raise AssertionError(message)


def event(name, **details):
    # Each append is a single write; pipeline commands can run concurrently.
    fd = os.open(os.environ["BACKUP_TEST_EVENTS"], os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o600)
    try:
        os.write(fd, (json.dumps({"event": name, **details}) + "\n").encode())
    finally:
        os.close(fd)


def fake(command):
    args = sys.argv[3:]
    failures = set(os.environ.get("BACKUP_TEST_FAILURE", "").split(","))
    # Real command errors can contain endpoint, SQL, credentials, or dump data.
    # Every child emits a sentinel so even the successful path tests log hygiene.
    sys.stderr.write("fixture-private-child-diagnostic\n")
    if command == "psql":
        # Guard the fixture itself: even mock invocations must name codex_a.
        require("--dbname=codex_a" in args, "fixture attempted a forbidden database")
        require("--no-psqlrc" in args and "--set=ON_ERROR_STOP=1" in args,
                "SQL must fail on errors and ignore local configuration")
        require(os.environ["PGUSER"] == "witself_backup_metrics"
                and os.environ["PGPASSWORD"] == "fixture-metrics-password",
                "telemetry must use the dedicated table writer")
        sql = sys.stdin.read()
        require("CREATE " not in sql and "GRANT " not in sql and "REVOKE " not in sql,
                "runtime writer must never create or grant objects")
        require("witself_ops.backup_runs" in sql, "telemetry must use the dedicated table")
        variables = dict(arg[len("--set="):].split("=", 1) for arg in args if arg.startswith("--set="))
        if "INSERT INTO" in sql:
            require("RETURNING started_at" in sql and "clock_timestamp()" in sql,
                    "attempt must begin with a database timestamp")
            action = "metrics_init"
            if action not in failures:
                sys.stdout.write("2026-09-19 03:00:00.123456+00\n")
        else:
            require(variables.get("run_started") == "2026-09-19 03:00:00.123456+00",
                    "updates must fence the exact recorded attempt")
            require("WHERE started_at = :'run_started'::timestamptz" in sql,
                    "updates must bind the timestamp safely")
            if "succeeded = false" in sql:
                action = "metrics_failure"
                require(variables.get("bytes", "").isdigit(),
                        "failure records require valid bytes even if HEAD failed")
                if failures.intersection(("head", "head_invalid", "head_empty")):
                    require(variables.get("bytes") == "0",
                            "failed object verification must retain safe zero bytes")
                require(variables.get("error_text") in (
                    "configuration", "package_setup", "stream_upload", "object_verification",
                    "object_promotion", "staging_cleanup", "success_record"),
                    "failure records may contain only bounded, static error stages")
            elif "succeeded = true" in sql:
                action = "metrics_success"
                require(variables.get("bytes") == "123", "success must record ciphertext byte count")
                require(variables.get("object_key", "").endswith("-fixture-pod.sql.gz.age"),
                        "success must record the completed object key")
            else:
                raise AssertionError("unknown telemetry SQL")
        event(action)
        return 42 if action in failures else 0
    if command == "apk":
        require(args == ["add", "--no-cache", "age", "aws-cli"], "unexpected package setup")
        event("apk")
        if "term" in failures:
            os.kill(os.getppid(), signal.SIGTERM)
        return 42 if "apk" in failures else 0
    if command == "pg_dump":
        require("--no-owner" in args and "--no-privileges" in args and "--no-password" in args,
                "logical dump must remain noninteractive and portable")
        require(os.environ["PGDATABASE"] == "codex_a", "fixture dump database escaped codex_a")
        require(os.environ["PGUSER"] == "witself_backup_dump"
                and os.environ["PGPASSWORD"] == "fixture-dump-password"
                and os.environ["PGOPTIONS"] == "-c default_transaction_read_only=on",
                "dump must use a distinct read-only database login and transaction")
        event(command)
        sys.stdout.write("SYNTHETIC_SQL")
        return 42 if command in failures else 0
    if command in ("gzip", "age"):
        content = sys.stdin.read()
        event(command)
        if command == "gzip":
            require(args == ["-c"], "gzip must stream to stdout")
            sys.stdout.write("COMPRESSED:" + content)
        else:
            require(args == ["-r", "fixture-age-public-recipient"], "age must use the secret recipient")
            sys.stdout.write("ENCRYPTED:" + content)
        # Emit plausible partial output before failing to catch missing pipefail.
        return 42 if command in failures else 0
    if command == "aws":
        require(args[:2] == ["--endpoint-url", "https://fixture.r2.cloudflarestorage.com"],
                "upload must use the configured R2 endpoint")
        require(os.environ["AWS_CONFIG_FILE"] == "/dev/null"
                and os.environ["AWS_SHARED_CREDENTIALS_FILE"] == "/dev/null"
                and os.environ["AWS_EC2_METADATA_DISABLED"] == "true",
                "backup must avoid ambient AWS credential providers")
        require(os.environ["AWS_ACCESS_KEY_ID"] == "fixture-access"
                and os.environ["AWS_SECRET_ACCESS_KEY"] == "fixture-secret",
                "R2 credentials must reach only the child process")
        if "s3api" in args:
            operation = args[args.index("s3api") + 1:]
            require(operation[:3] == ["head-object", "--bucket", "fixture-bucket"],
                    "byte verification must inspect only the configured bucket")
            require(operation[operation.index("--key") + 1].endswith("-fixture-pod.sql.gz.age.incomplete"),
                    "byte verification must inspect the staged ciphertext")
            require(operation[-4:] == ["--query", "ContentLength", "--output", "text"],
                    "object metadata must remain bounded to the byte count")
            event("head")
            sys.stdout.write("None\n" if "head_invalid" in failures else
                             "0\n" if "head_empty" in failures else "123\n")
            return 42 if "head" in failures else 0
        operation = args[args.index("s3") + 1:]
        if operation[0:2] == ["cp", "-"]:
            destination = operation[2]
            content = sys.stdin.read()
            require(content == "ENCRYPTED:COMPRESSED:SYNTHETIC_SQL", "upload received non-encrypted content")
            require(destination.endswith(".sql.gz.age.incomplete"), "stream must land on incomplete object")
            action = "upload"
        elif operation[0] == "cp":
            source, destination = operation[1:3]
            require(source.endswith(".sql.gz.age.incomplete") and source == destination + ".incomplete",
                    "promotion must only rename the complete ciphertext object")
            action = "promote"
        elif operation[0] == "rm":
            destination = operation[1]
            require(destination.endswith(".incomplete"), "cleanup may only delete the temporary object")
            action = "cleanup"
        else:
            raise AssertionError("unexpected S3 operation")
        require(destination.startswith("s3://fixture-bucket/fixture-prefix/fixture-cell/"),
                "backup escaped the single configured destination")
        require("fixture-pod" in destination, "backup object must have a unique pod identity")
        event(action)
        return 42 if action in failures else 0
    raise AssertionError("unmocked command")


def run_case(root, failure="", overrides=None):
    with tempfile.TemporaryDirectory(prefix="witself-backup-test-") as directory:
        work = Path(directory)
        binaries = work / "bin"
        binaries.mkdir()
        this_file = str(Path(__file__).resolve())
        for command in ("apk", "psql", "pg_dump", "gzip", "age", "aws"):
            binary = binaries / command
            # The launcher has no shell interpolation of environment values.
            binary.write_text("#!" + sys.executable + "\n"
                              + "import os, sys\n"
                              + "os.execv(" + repr(sys.executable) + ", "
                              + repr([sys.executable, this_file, "--fake", command])
                              + " + sys.argv[1:])\n")
            binary.chmod(0o700)
        env = {
            "PATH": str(binaries) + ":/usr/bin:/bin",
            "HOME": str(work),
            "WITSELF_HOME": str(work / "witself"),
            "DSH_HOME": str(work / "dsh"),
            "BACKUP_TEST_EVENTS": str(work / "events.jsonl"),
            "BACKUP_TEST_FAILURE": failure,
            "PGDATABASE": "codex_a",
            "METRICS_DATABASE": "codex_a",
            "PGHOST": "fixture.invalid",
            "PGUSER": "witself_backup_dump",
            "PGPASSWORD": "fixture-dump-password",
            "METRICS_USER": "witself_backup_metrics",
            "METRICS_PASSWORD": "fixture-metrics-password",
            "AGE_RECIPIENT": "fixture-age-public-recipient",
            "AWS_ACCESS_KEY_ID": "fixture-access",
            "AWS_SECRET_ACCESS_KEY": "fixture-secret",
            "R2_ENDPOINT": "https://fixture.r2.cloudflarestorage.com",
            "R2_BUCKET": "fixture-bucket",
            "R2_PREFIX": "fixture-prefix/",
            "CELL_NAME": "fixture-cell",
            "POD_UID": "fixture-pod",
        }
        for key, value in (overrides or {}).items():
            if value is None:
                env.pop(key, None)
            else:
                env[key] = value
        result = subprocess.run(["/bin/bash", str(root / ".gitops/charts/apps/files/postgres-backup.sh")],
                                env=env, capture_output=True, text=True, timeout=15)
        path = work / "events.jsonl"
        events = [json.loads(line)["event"] for line in path.read_text().splitlines()] if path.exists() else []
        require(result.stdout == "", "backup must not print dump or command output")
        require(result.stderr in ("PostgreSQL backup completed.\n",
                                  "PostgreSQL backup failed; inspect Job status and backup alerts.\n"),
                "backup diagnostics must remain value-free")
        return result.returncode, events


def tests(root):
    status, events = run_case(root)
    require(status == 0, "success case must exit successfully")
    require(events.count("metrics_success") == 1 and "metrics_failure" not in events,
            "completed backup must record exactly one success")
    expected = ["metrics_init", "apk", "upload", "head", "promote", "cleanup", "metrics_success"]
    require([event for event in events if event in expected] == expected,
            "success metric must follow upload, promotion, and cleanup")

    for failure in ("pg_dump", "gzip", "age", "upload", "head", "head_invalid", "head_empty", "promote", "cleanup", "apk", "term"):
        status, events = run_case(root, failure)
        require(status != 0, failure + " failure must fail the job")
        require(events.count("metrics_failure") == 1 and "metrics_success" not in events,
                failure + " failure must finish its attempt unsuccessfully")
        if failure in ("pg_dump", "gzip", "age", "upload", "head", "head_invalid", "head_empty", "apk", "term"):
            require("promote" not in events, failure + " failure must not publish final ciphertext")
        if failure in ("apk", "term"):
            require("pg_dump" not in events and "upload" not in events,
                    "setup failure must stop before dumping")
        else:
            require("cleanup" in events, failure + " failure must attempt incomplete-object cleanup")

    status, events = run_case(root, "metrics_init")
    require(status != 0 and events == ["metrics_init"],
            "initial telemetry failure must stop all backup work")
    status, events = run_case(root, overrides={"PGHOST": None})
    require(status != 0 and events == [],
            "missing database configuration must fail before attempting SQL")
    status, events = run_case(root, "metrics_success")
    require(status != 0 and events[-2:] == ["metrics_success", "metrics_failure"],
            "failed success write must fail the job and attempt the failure metric")
    status, events = run_case(root, "metrics_failure", {"AGE_RECIPIENT": None})
    require(status != 0 and events == ["metrics_init", "metrics_failure"],
            "failed failure telemetry must preserve the original job failure")
    status, events = run_case(root, "pg_dump,cleanup,metrics_failure")
    require(status == 42 and "metrics_failure" in events and "cleanup" in events
            and "metrics_success" not in events and "promote" not in events,
            "cleanup and telemetry failures must preserve the upstream failure and prevent promotion")

    for overrides in (
        {"AGE_RECIPIENT": None},
        {"AWS_SECRET_ACCESS_KEY": None},
        {"R2_ENDPOINT": "https://untrusted.example.test"},
        {"R2_ENDPOINT": "http://fixture.r2.cloudflarestorage.com"},
        {"R2_BUCKET": "bad/bucket"},
        {"R2_PREFIX": "/bad-prefix"},
        {"CELL_NAME": "bad/cell"},
        {"POD_UID": "bad/pod"},
        {"PGPASSWORD": None},
    ):
        status, events = run_case(root, overrides=overrides)
        require(status != 0 and events == ["metrics_init", "metrics_failure"],
                "invalid configuration must fail before package, dump, or upload work")
    print("PostgreSQL backup offline runner checks passed (26 cases)")


if __name__ == "__main__":
    if len(sys.argv) > 1 and sys.argv[1] == "--fake":
        sys.exit(fake(sys.argv[2]))
    tests(Path(sys.argv[1]))
