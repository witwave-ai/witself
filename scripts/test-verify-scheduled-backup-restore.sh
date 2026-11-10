#!/usr/bin/env bash
# Hermetic operator-command tests: fake ciphertext, identities, S3 and PostgreSQL.
# NRC_TEST_SCRIPT optionally selects a disposable mutant for mutation proof.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
python3 - "$repo_root" "${NRC_TEST_SCRIPT:-$repo_root/scripts/verify-scheduled-backup-restore.sh}" <<'PY'
import gzip
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import stat
import subprocess
import sys
import tempfile


ROOT = Path(sys.argv[1])
SCRIPT = Path(sys.argv[2])
BASH = shutil.which("bash")
DSN = "postgres://fixture-user:fixture-private-password@fixture.invalid/disposable"
MARKER = "fixture-private-child-diagnostic"
COMMON = ["--no-psqlrc", "--no-password", "--set=ON_ERROR_STOP=1", "--dbname", "<DSN>"]
SQL_TAGS = ("empty", "schema", "tables", "indexes", "constraints", "rows")
STUB = r'''
import json
import os
from pathlib import Path
import re
import sys

command = Path(sys.argv[0]).name
args = sys.argv[1:]
failure = os.environ.get("NRC_FAILURE", "")
marker = "fixture-private-child-diagnostic"
event = {"command": command, "args": args}
if command == "psql":
    if "--dbname" not in args or args[args.index("--dbname") + 1] != os.environ["NRC_EXPECT_DSN"]:
        sys.exit(91)
    event["args"] = ["<DSN>" if arg == os.environ["NRC_EXPECT_DSN"] else arg for arg in args]
    sql = args[args.index("--command") + 1] if "--command" in args else ""
    match = re.search(r"nrc1:(empty|schema|tables|indexes|constraints|rows)\b", sql)
    event["stage"] = match.group(1) if match else "restore"
    event["pgoptions"] = os.environ.get("PGOPTIONS", "")
fd = os.open(os.environ["NRC_EVENTS"], os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o600)
os.write(fd, (json.dumps(event) + "\n").encode())
os.close(fd)
sys.stderr.write(marker + "\n")
sys.stderr.write(os.environ["NRC_EXPECT_DSN"] + "\n")
if command == "aws":
    if failure == "fetch":
        sys.exit(42)
    Path(args[-1]).write_bytes(b"" if failure == "empty_object" else Path(os.environ["NRC_INPUT"]).read_bytes())
elif command == "age":
    if failure == "decrypt":
        sys.exit(42)
    sys.stdout.buffer.write(b"not gzip data" if failure == "decompress" else Path(args[-1]).read_bytes())
elif command == "psql":
    stage = event["stage"]
    if stage == "restore":
        sys.stdin.buffer.read()
        sys.stdout.write(marker + "\n")
        if failure == "restore":
            sys.exit(42)
    else:
        if failure == stage + "_query":
            sys.exit(42)
        answers = {"empty": "0", "schema": "17", "tables": "2", "indexes": "0", "constraints": "0", "rows": "4"}
        answers.update(json.loads(os.environ.get("NRC_ANSWERS", "{}")))
        sys.stdout.write(answers[stage] + "\n")
else:
    sys.exit(92)
'''


def require(condition, message):
    if not condition:
        raise AssertionError(message)


def run_case(name, *, source="file", failure="", answers=None, status=0,
             reason=None, modify=None, keep=False, trace=False, unset_dsn=False,
             dsn=DSN,
             identity_mode=0o600, identity_kind="file", expect=None,
             no_commands=False, no_fetch_decrypt=False):
    with tempfile.TemporaryDirectory(prefix="nrc1-test-") as directory:
        work = Path(directory)
        binaries = work / "bin"
        binaries.mkdir()
        temp = work / "tmp"
        temp.mkdir()
        for command in ("aws", "age", "psql"):
            stub = binaries / command
            stub.write_text("#!" + sys.executable + "\n" + STUB)
            stub.chmod(0o700)
        identity = work / "fake-identity"
        if identity_kind == "directory":
            identity.mkdir(mode=0o700)
        elif identity_kind != "missing":
            identity.write_text("OBVIOUSLY FAKE TEST IDENTITY; NOT A KEY\n")
            identity.chmod(identity_mode)
            if identity_kind == "symlink":
                target = identity.with_name("fake-identity-target")
                identity.rename(target)
                identity.symlink_to(target)
        original = gzip.compress(b"CREATE TABLE fixture (id integer);\n", mtime=0)
        input_file = work / "fixture.sql.gz.age"
        input_file.write_bytes(b"" if failure == "local_empty_object" else original)
        original = input_file.read_bytes()
        events_file = work / "events.jsonl"
        args = ["--age-identity-file", str(identity)]
        if source in ("file", "both"):
            args += ["--file", str(input_file)]
        if source in ("object", "both"):
            args += ["--object", "s3://fixture-bucket/fixture.sql.gz.age",
                     "--endpoint-url", "https://fixture.invalid"]
        if keep:
            args += ["--keep-workdir"]
        if expect is not None:
            args += ["--expect-schema-version", expect]
        if modify:
            args = modify(args, work)
        child_environment = {
            "PATH": str(binaries) + os.pathsep + os.defpath,
            "HOME": str(work), "WITSELF_HOME": str(work / "witself"),
            "DSH_HOME": str(work / "dsh"), "TMPDIR": str(temp),
            "LC_ALL": "C", "NRC_EXPECT_DSN": dsn,
            "NRC_EVENTS": str(events_file), "NRC_INPUT": str(input_file),
            "NRC_FAILURE": failure, "NRC_ANSWERS": json.dumps(answers or {}),
        }
        if not unset_dsn:
            child_environment["RESTORE_DATABASE_URL"] = dsn
        result = subprocess.run([BASH] + (["-x"] if trace else []) + [str(SCRIPT)] + args,
                                env=child_environment, capture_output=True, text=True, timeout=30)
        output = result.stdout + result.stderr
        require(DSN not in output and (not dsn or dsn not in output) and MARKER not in output,
                name + ": private output leaked")
        require(not re.search(r"[a-fA-F0-9]{64}", output), name + ": full digest printed")
        require(input_file.read_bytes() == original, name + ": local input changed")
        require(result.returncode == status, name + ": wrong exit status")
        events = [json.loads(line) for line in events_file.read_text().splitlines()] if events_file.exists() else []
        if no_commands:
            require(not events, name + ": refusal performed external work")
        if no_fetch_decrypt:
            require(all(e["command"] == "psql" and e["stage"] == "empty" for e in events),
                    name + ": refusal fetched or decrypted data")
        if status == 0:
            expected = ("scheduled-backup-restore: PASS object=fixture.sql.gz.age bytes="
                        + str(len(original)) + " sha256=" + hashlib.sha256(original).hexdigest()[:12]
                        + " schema_version=17 tables=2 rows=4\n")
            require(result.stdout == expected, name + ": incorrect PASS line")
            if not trace and not keep:
                require(result.stderr == "", name + ": unexpected success stderr")
            psql = [e for e in events if e["command"] == "psql"]
            require([e["stage"] for e in psql] == ["empty", "restore", "schema", "tables", "indexes", "constraints", "rows"],
                    name + ": incorrect SQL stages")
            for event in psql:
                if event["stage"] == "restore":
                    require(event["args"] == COMMON + ["--single-transaction"], name + ": incorrect restore argv")
                else:
                    sql = event["args"][-1]
                    require(event["args"] == COMMON + ["--tuples-only", "--no-align", "--command", sql],
                            name + ": incorrect query argv")
                    require(all(token in event["pgoptions"] for token in
                                ("default_transaction_read_only=on", "search_path=pg_catalog", "row_security=off")),
                            name + ": query does not enforce read-only exact counts")
            ages = [e for e in events if e["command"] == "age"]
            require(len(ages) == 1, name + ": incorrect age call count")
            object_path = ages[0]["args"][-1]
            require(ages[0]["args"] == ["--decrypt", "-i", str(identity), object_path], name + ": incorrect age argv")
            aws = [e for e in events if e["command"] == "aws"]
            if source == "object":
                require(len(aws) == 1 and aws[0]["args"] == ["--endpoint-url", "https://fixture.invalid", "s3", "cp",
                        "s3://fixture-bucket/fixture.sql.gz.age", object_path], name + ": incorrect download argv")
                require(str(Path(object_path).parent.parent) == str(temp), name + ": download escaped workdir")
            else:
                require(not aws and object_path == str(input_file), name + ": local file was copied or downloaded")
        else:
            require("scheduled-backup-restore: PASS" not in output, name + ": failure printed PASS")
            if reason:
                require([line for line in result.stderr.splitlines() if line.startswith("scheduled-backup-restore: FAIL ")]
                        == ["scheduled-backup-restore: FAIL " + reason],
                        name + ": incorrect static failure reason")
            else:
                require("scheduled-backup-restore: FAIL " in result.stderr, name + ": missing failure line")
        directories = list(temp.iterdir())
        if keep:
            require(len(directories) == 1 and directories[0].is_dir(), name + ": workdir not retained")
            retained = directories[0]
            require(stat.S_IMODE(retained.stat().st_mode) == 0o700, name + ": unsafe workdir mode")
            require("scheduled-backup-restore: workdir=" + str(retained) in result.stderr, name + ": retained path not reported")
            errors = retained / "errors.log"
            require(errors.exists() and stat.S_IMODE(errors.stat().st_mode) == 0o600, name + ": unsafe error file mode")
            require(MARKER in errors.read_text(), name + ": diagnostic was not retained privately")
            require(DSN not in errors.read_text(), name + ": DSN was written to private diagnostics")
        else:
            require(not directories, name + ": workdir leaked")
    print("scheduled-backup-restore test: PASS " + name)


def without(flag):
    def change(args, _work):
        index = args.index(flag)
        return args[:index] + args[index + 2:]
    return change


def identity_source(args, work, kind):
    identity = work / "fake-identity"
    source = identity
    if kind != "direct":
        source = work / "identity-source.age"
        if kind == "hardlink":
            os.link(identity, source)
        else:
            source.symlink_to(identity)
    return [str(source) if arg.endswith("fixture.sql.gz.age") else arg for arg in args]


run_case("happy_file_exact_argv_and_receipt")
run_case("happy_object_exact_argv_and_receipt", source="object")
run_case("expected_schema_matches", expect="17")
run_case("read_only_private_identity", identity_mode=0o400)
run_case("xtrace_does_not_expose_dsn", trace=True)
run_case("usage_both_sources", source="both", status=2, no_commands=True)
run_case("usage_neither_source", source="none", status=2, no_commands=True)
run_case("usage_missing_endpoint", source="object", modify=without("--endpoint-url"), status=2, no_commands=True)
run_case("usage_non_s3_uri", source="object",
         modify=lambda args, _: ["https://fixture.invalid/object" if a.startswith("s3://") else a for a in args],
         status=2, no_commands=True)
run_case("usage_missing_identity_argument", modify=without("--age-identity-file"), status=2, no_commands=True)
run_case("usage_missing_identity_file", identity_kind="missing", status=2, no_commands=True)
run_case("usage_irregular_identity", identity_kind="directory", status=2, no_commands=True)
run_case("usage_symlink_identity", identity_kind="symlink", status=2, no_commands=True)
for label, mode in (("group_readable", 0o640), ("group_executable", 0o610), ("other_readable", 0o604), ("other_writable", 0o602)):
    run_case("usage_identity_" + label, identity_mode=mode, status=2, no_commands=True)
run_case("usage_unset_dsn", unset_dsn=True, status=2, no_commands=True)
run_case("usage_empty_dsn", dsn="", status=2, no_commands=True)
run_case("usage_multiline_dsn", dsn=DSN + "\nfixture", status=2, reason="invalid_database_url", no_commands=True)
run_case("usage_carriage_return_dsn", dsn=DSN + "\rfixture", status=2, reason="invalid_database_url", no_commands=True)
run_case("usage_dsn_as_option", modify=lambda args, _: args + ["--restore-database-url", DSN], status=2, no_commands=True)
run_case("usage_unknown_option", modify=lambda args, _: args + ["--unknown"], status=2, no_commands=True)
run_case("usage_endpoint_with_local_file", modify=lambda args, _: args + ["--endpoint-url", "https://fixture.invalid"],
         status=2, no_commands=True)
run_case("usage_missing_input_file", modify=lambda args, work: [str(work / "missing.age") if a.endswith("fixture.sql.gz.age") else a for a in args],
         status=2, no_commands=True)
run_case("usage_irregular_input_file", modify=lambda args, work: [str(work) if a.endswith("fixture.sql.gz.age") else a for a in args],
         status=2, no_commands=True)
for name, kind in (("identity_as_input", "direct"), ("identity_hardlink_as_input", "hardlink"),
                   ("identity_symlink_as_input", "symlink")):
    run_case(name, modify=lambda args, work, link_kind=kind: identity_source(args, work, link_kind),
             status=2, reason="identity_is_source", no_commands=True)
for option, value in (("--file", "dummy"), ("--age-identity-file", "dummy")):
    run_case("usage_duplicate_" + option[2:].replace("-", "_"),
             modify=lambda args, _, flag=option, val=value: args + [flag, val], status=2, no_commands=True)
for option in ("--file", "--object", "--endpoint-url", "--age-identity-file", "--expect-schema-version"):
    run_case("usage_missing_value_" + option[2:].replace("-", "_"),
             modify=lambda args, _, flag=option: args + [flag], status=2, no_commands=True)
for version in ("0", "-1", "not-a-number"):
    run_case("usage_invalid_schema_" + version.replace("-", "_"), expect=version, status=2, no_commands=True)
run_case("target_not_empty_stops_before_fetch", source="object", answers={"empty": "1"}, status=1,
         reason="target_not_empty", no_fetch_decrypt=True)
run_case("target_not_empty_stops_before_decrypt", answers={"empty": "1"}, status=1,
         reason="target_not_empty", no_fetch_decrypt=True)
run_case("empty_local_object", failure="local_empty_object", status=1, reason="empty_object")
run_case("empty_downloaded_object", source="object", failure="empty_object", status=1, reason="empty_object")
for stage in ("fetch", "decrypt", "decompress", "restore"):
    run_case("failure_" + stage + "_private_output_and_cleanup", source="object", failure=stage, status=1, reason=stage)
run_case("keep_workdir_retains_private_diagnostics", failure="restore", status=1, reason="restore", keep=True)
run_case("keep_workdir_success_permissions", source="object", keep=True)
for name, answers, reason in (
        ("schema_version_missing", {"schema": "0"}, "schema_version_missing"),
        ("schema_version_empty", {"schema": ""}, "schema_version_missing"),
        ("no_user_tables", {"tables": "0"}, "no_user_tables"),
        ("invalid_index", {"indexes": "1"}, "invalid_indexes"),
        ("unvalidated_constraint", {"constraints": "1"}, "unvalidated_constraints")):
    run_case(name, answers=answers, status=1, reason=reason)
run_case("expected_schema_mismatch", expect="18", status=1, reason="schema_version_mismatch")
for stage in SQL_TAGS:
    run_case("query_failure_" + stage, failure=stage + "_query", status=1,
             reason="schema_version_missing" if stage == "schema" else "check_failed")
    run_case("malformed_result_" + stage, answers={stage: "not-a-count"}, status=1,
             reason="schema_version_missing" if stage == "schema" else "check_failed")
print("scheduled-backup-restore tests: PASS")
PY
