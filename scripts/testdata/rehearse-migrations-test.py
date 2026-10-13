"""Offline rehearsal contracts. Never executes a real Docker client."""

import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from types import SimpleNamespace
from urllib.parse import urlsplit


ROOT = Path(__file__).resolve().parents[2]
# Keep the shell runner's explicit-root interface and allow direct -I execution.
if len(sys.argv) > 1 and Path(sys.argv[1]).is_dir():
    ROOT = Path(sys.argv.pop(1)).resolve()
SCRIPT = ROOT / "scripts/rehearse-store-migrations.sh"
sys.dont_write_bytecode = True

MODULE_SPEC = importlib.util.spec_from_file_location(
    "rehearsal_controller", ROOT / "scripts/lib/rehearse_store_migrations.py")
CONTROLLER = importlib.util.module_from_spec(MODULE_SPEC)
MODULE_SPEC.loader.exec_module(CONTROLLER)

STUB = r'''
import json, os, signal, stat, sys, time
from pathlib import Path
kind = Path(sys.argv[0]).name
args = sys.argv[1:]
case = os.environ.get("REHEARSAL_STUB_CASE", "success")
stdin_path = None
if kind == "docker" and "exec" in args and "-i" in args:
    stdin_path = os.environ["REHEARSAL_STUB_LOG"] + ".stdin-" + str(os.getpid())
record = {"kind": kind, "args": args, "environment": dict(os.environ), "stdin_file": stdin_path}
record["stdin_devnull"] = stat.S_ISCHR(os.fstat(0).st_mode) and os.fstat(0).st_rdev == os.stat(os.devnull).st_rdev
if kind == "helper" and "--password-file" in args:
    password_path = args[args.index("--password-file") + 1]
    record["password_mode"] = stat.S_IMODE(os.stat(password_path).st_mode)
with os.fdopen(os.open(os.environ["REHEARSAL_STUB_LOG"], os.O_WRONLY | os.O_CREAT | os.O_APPEND,
                       0o600), "w", encoding="utf-8") as stream:
    stream.write(json.dumps(record) + "\n")
def option(name, default=None):
    if name in args and args.index(name) + 1 < len(args):
        return args[args.index(name) + 1]
    return default
def emit(value):
    print(json.dumps(value), flush=True)
state_path = Path(os.environ["REHEARSAL_STUB_LOG"] + ".versions")
versions = json.loads(state_path.read_text()) if state_path.exists() else {}
oom_marker = Path(os.environ["REHEARSAL_STUB_LOG"] + ".late-oom")
late_oom = case == "late-oom" and oom_marker.exists()
file_marker = Path(os.environ["REHEARSAL_STUB_LOG"] + ".file-active")
if kind == "docker":
    if args[:1] == ["--host"]:
        args = args[2:]
    command = args[0] if args else ""
    container_marker = Path(os.environ["REHEARSAL_STUB_LOG"] + ".container")
    volume_marker = Path(os.environ["REHEARSAL_STUB_LOG"] + ".volume")
    if command == "run":
        container_marker.touch()
    elif command == "volume":
        if args[1] == "create":
            volume_marker.touch()
        elif args[1] == "rm":
            if case == "volume-cleanup-failure" or container_marker.exists():
                sys.exit(1)
            volume_marker.unlink(missing_ok=True)
        elif args[1] == "inspect":
            seen = Path(os.environ["REHEARSAL_STUB_LOG"] + ".volume-inspected")
            if case == "volume-preexisting" and not seen.exists():
                volume_marker.touch()
            seen.touch()
            if not volume_marker.exists():
                sys.exit(1)
    elif command == "rm":
        if case == "cleanup-failure":
            sys.exit(1)
        container_marker.unlink(missing_ok=True)
    elif command == "port":
        print("127.0.0.1:" + ("5599" if case == "forbidden-port" else "45432"))
    elif command == "inspect":
        if not container_marker.exists():
            sys.exit(1)
        fmt = option("--format", "")
        if "RestartCount" in fmt:
            print(1 if case == "restart" else 0)
        else:
            emit({"Running": not late_oom, "OOMKilled": case == "oom" or late_oom,
                  "Restarting": False, "ExitCode": 137 if late_oom else 0})
    elif command == "exec":
        if stdin_path:
            short_read = "pg_restore" in args and case == "restore-short-read"
            payload = sys.stdin.buffer.read(1) if short_read else sys.stdin.buffer.read()
            with os.fdopen(os.open(stdin_path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), "wb") as stream:
                stream.write(payload)
            if short_read:
                sys.stdin.close()
                sys.exit(0)
        if "pg_restore" in args and case == "restore-interrupt":
            Path(os.environ["REHEARSAL_STUB_READY"]).touch()
            time.sleep(40)
        if "pg_restore" in args and case == "restore-failure":
            print("private@example.test acct_fixture_private " + "a" * 64, file=sys.stderr)
            sys.exit(1)
        if "rehearsal-memory" in args:
            if case == "long-tx-expired-before-file":
                time.sleep(6.2)
            marker = Path(os.environ["REHEARSAL_STUB_LOG"] + ".memory-active-" + Path(args[-1]).name)
            marker.touch()
            count = 0
            try:
                while marker.exists():
                    if case == "late-oom" and oom_marker.exists():
                        sys.exit(1)
                    if case == "bad-memory":
                        print("not-an-integer", flush=True)
                    elif case == "bad-memory-fields":
                        emit({"ready": True, "anon": True, "shmem": 1000000,
                              "file": 300000000, "current": 402653184})
                    elif case == "missing-anon" and file_marker.exists():
                        # Model the builtin sampler's fail-closed memory.stat read.
                        sys.exit(1)
                    elif case not in ("stalled-memory", "stalled-memory-before-file") or count < 3:
                        rise = 30 * 1024 * 1024 if file_marker.exists() else 0
                        anon = 100000000 + (rise if case == "anon-rise-current-pinned" else 0)
                        cache = 270000000 + (rise if case == "file-rise-anon-flat" else 0)
                        current = 370000000 + (rise if case == "file-rise-anon-flat" else 0)
                        if case == "anon-rise-current-pinned":
                            current = 402653184
                        emit({"ready": True, "anon": anon, "shmem": 1000000,
                              "file": cache, "current": current})
                    count += 1
                    time.sleep(.05)
            finally:
                marker.unlink(missing_ok=True)
        elif "rm" in args and args[-1].startswith("/rehearsal/memory-"):
            if case == "memory-slow-stop":
                time.sleep(1.2)
            marker = Path(os.environ["REHEARSAL_STUB_LOG"] + ".memory-active-" + Path(args[-1]).name)
            marker.unlink(missing_ok=True)
        elif any("/sys/fs/cgroup/memory.max" in arg for arg in args):
            print("max" if case == "unlimited-memory" else "402653184")
        elif any("/sys/fs/cgroup/" in arg for arg in args):
            # Repeated one-shot samples are an invalid regression.
            sys.exit(2)
        elif "psql" in args and "-i" in args:
            pass
        elif "psql" in args and "rolpassword" in option("-c", ""):
            print("f" if case == "scram-unset" else "t")
        elif "psql" in args or "pg_isready" in args:
            print("1")
    sys.exit(0)
if kind == "go":
    if args == ["env", "GOVERSION"]:
        print("go1.27.2")
        sys.exit(0)
    sys.exit(2)
mode = option("--mode", args[0] if args else "")
if mode == "image":
    emit({"image": "local/postgresql:fixture"})
elif mode == "list":
    emit([
        {"version": 97, "name": "0097_index_account_export_order_hot.sql", "explicit_locks": False},
        {"version": 98, "name": "0098_index_account_export_order.sql", "explicit_locks": True},
    ])
elif mode in ("seed", "measure"):
    multiplier = 100 if case == "restore-out-of-band" else 1
    emit({"entries": 2000, "transcripts": 200, "mutations": 300, "inputs": 300,
          "heap_bytes": multiplier * 460 * 1024 * 1024, "total_bytes": multiplier * 1387 * 1024 * 1024,
          "column_bytes": multiplier * 1055 * 1024 * 1024})
elif mode == "inspect":
    if late_oom:
        sys.exit(1)
    version = versions.get(option("--dsn"), 97)
    emit({"version": version, "invalid_indexes": 1 if case == "invalid-index" and version == 98 else 0})
elif mode == "migrate":
    target = int(option("--to", "98"))
    name = "0098_index_account_export_order.sql" if target == 98 else "0097_index_account_export_order_hot.sql"
    print("START " + name, flush=True)
    if target == 98:
        file_marker.touch()
    if target == 98 and case == "late-oom":
        oom_marker.touch()
        time.sleep(.25)
        sys.exit(1)
    kill_marker = Path(os.environ["REHEARSAL_STUB_LOG"] + ".killed-once")
    if target == 98 and case == "kill" and not kill_marker.exists():
        kill_marker.touch()
        time.sleep(40)
    if target == 98 and case in ("interrupt", "fail"):
        Path(os.environ["REHEARSAL_STUB_READY"]).touch()
    if target == 98 and case == "interrupt":
        time.sleep(40)
    if target == 98 and case == "fail":
        print("private@example.test acct_fixture_private " + "a" * 64, file=sys.stderr)
        sys.exit(1)
    if target == 98 and case == "migrate-sqlstate":
        print("private stderr sentinel sqlstate=55P03 private@example.test acct_fixture_private " + "a" * 64,
              file=sys.stderr)
        sys.exit(1)
    baseline_delay = float(os.environ.get("REHEARSAL_STUB_BASELINE_DELAY", ".25"))
    if case != "kill-after-file" or target != 98:
        time.sleep(baseline_delay if target == 97 else (1.5 if case in ("stalled-observer", "stalled-memory") else .25))
    versions[option("--dsn")] = target
    state_path.write_text(json.dumps(versions))
    print("FILE " + name + " " + ("46001" if case == "slow" and target == 98 else "1"), flush=True)
    if target == 98:
        file_marker.unlink(missing_ok=True)
    if target == 98 and case == "kill-after-file":
        time.sleep(1)
elif mode == "observe":
    if case == "stalled-observer":
        emit({"ready": True, "lock_held": False, "lock_waiting": False,
              "backend_count": 0, "appender_waiting": False})
        time.sleep(40)
    while True:
        if case == "bad-observer":
            print("malformed-telemetry", flush=True)
        elif case == "bad-observer-fields":
            emit({"ready": True, "lock_held": "unknown", "lock_waiting": False,
                  "backend_count": -1, "appender_waiting": False})
        else:
            emit({"ready": True, "lock_held": False, "lock_waiting": case == "lock-queued",
                  "backend_count": 0, "appender_waiting": case == "appender-blocked"})
        time.sleep(.05)
elif mode == "append":
    while True:
        emit({"unexpected": True} if case == "bad-appender-fields" else {"ready": True, "wait_ms": 1})
        time.sleep(.05)
elif mode == "long-tx":
    emit({"ready": True})
    time.sleep(float(option("--long-tx", "10")) if case in (
        "long-tx-complete", "long-tx-expired-before-file", "stalled-memory-before-file") else .1)
elif mode == "verify":
    if case == "verify-timeout":
        time.sleep(40)
    if case == "verify-failure":
        sys.exit(1)
    emit({"passed": True})
else:
    print("unknown stub mode", file=sys.stderr)
    sys.exit(2)
'''


class HarnessTests(unittest.TestCase):
    def setUp(self):
        self.scratch = tempfile.TemporaryDirectory(prefix="witself-rehearsal-contract-")
        self.addCleanup(self.scratch.cleanup)
        self.root = Path(self.scratch.name)
        self.bin = self.root / "bin"
        self.bin.mkdir()
        for executable in ("bash", "cat", "date", "dirname", "env", "mktemp", "mkdir",
                           "rm", "sleep", "touch", "uname", "kill", "head", "tail"):
            found = shutil.which(executable)
            if found:
                (self.bin / executable).symlink_to(found)
        (self.bin / "python3").symlink_to(sys.executable)
        for name in ("docker", "helper", "go"):
            target = self.bin / name
            target.write_text("#!" + sys.executable + "\n" + STUB, encoding="utf-8")
            target.chmod(0o700)
        self.log = self.root / "calls.jsonl"
        self.ready = self.root / "ready"
        self.out = self.root / "out"
        self.environment = {
            "PATH": str(self.bin),
            "TMPDIR": str(self.root),
            "WITSELF_REHEARSAL_TEST_MODE": "1",
            "WITSELF_REHEARSAL_TEST_BIN": str(self.bin),
            "WITSELF_REHEARSAL_HELPER": str(self.bin / "helper"),
            "REHEARSAL_STUB_LOG": str(self.log),
            "REHEARSAL_STUB_READY": str(self.ready),
        }
        self.command = ["/bin/bash", str(SCRIPT), "--from", "97", "--to", "98",
                        "--synthetic", "--size-profile", "smoke", "--entries", "2000",
                        "--transcripts", "200", "--mutations", "300", "--run-inputs", "300",
                        "--out", str(self.out)]

    def calls(self):
        if not self.log.exists():
            return []
        return [json.loads(line) for line in self.log.read_text(encoding="utf-8").splitlines()]

    def run_case(self, case="success", extra=(), command=None, input=None):
        self.environment["REHEARSAL_STUB_CASE"] = case
        result = subprocess.run(command or self.command + list(extra), env=self.environment,
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                **({"stdin": subprocess.DEVNULL} if input is None else {"input": input}),
                                timeout=25, check=False)
        result.stdout = result.stdout.decode("utf-8", errors="replace")
        result.stderr = result.stderr.decode("utf-8", errors="replace")
        return result

    def assert_cleaned(self):
        docker_calls = [call["args"] for call in self.calls() if call["kind"] == "docker"]
        runs = [args for args in docker_calls if "run" in args]
        self.assertTrue(runs, "container was not started")
        name = runs[0][runs[0].index("--name") + 1]
        self.assertRegex(name, r"^witself-rehearsal-[A-Za-z0-9_-]+$")
        self.assertTrue(any("rm" in args and "-f" in args and "-v" in args and name in args
                            for args in docker_calls), "cleanup must remove container")
        volume = name + "-data"
        removals = [index for index, args in enumerate(docker_calls)
                    if "volume" in args and "rm" in args and volume in args]
        self.assertTrue(removals, "cleanup must remove the named data volume")
        container_removals = [index for index, args in enumerate(docker_calls)
                              if args == ["--host", "unix:///var/run/docker.sock", "rm", "-f", "-v", name]]
        self.assertTrue(container_removals, "cleanup must force-remove the container and its volumes")
        self.assertLess(container_removals[-1], removals[0],
                        "cleanup must remove the container before the named data volume")
        self.assertTrue(any("volume" in args and "inspect" in args and volume in args
                            for args in docker_calls[removals[-1] + 1:]),
                        "cleanup must prove that the named data volume is gone")
        self.assertFalse(any("witself-test-pg" in arg for args in docker_calls for arg in args))
        self.assertFalse(list(self.root.glob("calls.jsonl.memory-active-*")),
                         "container-side memory sampler was not stopped")
        self.assertFalse(list(self.root.glob("witself-rehearsal.*")),
                         "wrapper work directory was not removed")

    def report(self):
        self.assertTrue((self.out / "report.md").is_file(), "Markdown report is missing")
        report = json.loads((self.out / "report.json").read_text(encoding="utf-8"))
        for path in (self.out / "report.md", self.out / "report.json"):
            text = path.read_text(encoding="utf-8")
            self.assertFalse(bool(re.search(r"[a-fA-F0-9]{64}", text)), "report leaked a digest")
            self.assertIsNone(re.search(r"[\w.+-]+@[\w.-]+\.[A-Za-z]{2,}", text), "report leaked email")
            self.assertIsNone(re.search(r"\b(?:acct|account)_[A-Za-z0-9_-]+", text), "report leaked account ID")
        return report

    def restore_command(self, *extra):
        command = [part for part in self.command if part != "--synthetic"]
        command[command.index("--size-profile") + 1] = "restore"
        return command + ["--restore", "-"] + list(extra)

    def assert_only_wrapper_cleanup(self):
        calls = self.calls()
        self.assertEqual(len(calls), 4, "stdin refusal must run only the wrapper's four cleanup calls")
        name = calls[-1]["environment"]["WITSELF_REHEARSAL_CONTAINER"]
        self.assertRegex(name, r"^witself-rehearsal-[A-Za-z0-9_-]+$")
        docker = ["--host", "unix:///var/run/docker.sock"]
        self.assertEqual([(call["kind"], call["args"]) for call in calls], [
            ("docker", docker + ["rm", "-f", "-v", name]),
            ("docker", docker + ["inspect", name]),
            ("docker", docker + ["volume", "rm", name + "-data"]),
            ("docker", docker + ["volume", "inspect", name + "-data"]),
        ])

    def assert_password_private(self, result):
        # Only the private stdin capture may contain this per-run credential.
        # Boolean assertions deliberately avoid rendering it on a test failure.
        password_calls = [call for call in self.calls() if call["kind"] == "docker" and
                          "exec" in call["args"] and "-i" in call["args"] and "psql" in call["args"]]
        self.assertEqual(len(password_calls), 1, "password must be set once over stdin")
        payload = Path(password_calls[0]["stdin_file"]).read_bytes()
        match = re.fullmatch(rb"ALTER ROLE postgres PASSWORD '([a-f0-9]{64})';\s*", payload)
        self.assertTrue(match is not None, "password SQL was not sent over private stdin")
        password = match.group(1).decode("ascii")
        private_outputs = [json.dumps(self.calls()), result.stdout, result.stderr]
        private_outputs += [path.read_text(encoding="utf-8") for path in
                            (self.out / "report.json", self.out / "report.md") if path.exists()]
        self.assertFalse(any(password in output for output in private_outputs),
                         "per-run password escaped private stdin or its file")
        self.assertFalse(any(re.search(r"[a-fA-F0-9]{64}", output) for output in private_outputs),
                         "a private value escaped into argv, environment, report, or process output")

    def test_tcp_scram_password_is_private_and_local_socket_trust(self):
        result = self.run_case()
        self.assertEqual(result.returncode, 0, "authenticated rehearsal failed")
        runs = [call["args"] for call in self.calls() if call["kind"] == "docker" and "run" in call["args"]]
        self.assertEqual(len(runs), 1)
        entrypoint = runs[0][runs[0].index("-c") + 1]
        self.assertIn("--auth-local=trust", entrypoint)
        self.assertIn("--auth-host=scram-sha-256", entrypoint)
        self.assertIn("host all all all scram-sha-256", entrypoint)
        self.assertNotIn("host all all all trust", entrypoint)
        self.assertNotIn("-A trust", entrypoint)
        host_lines = re.findall(r"""['"](host[^'"\n]*)['"]""", entrypoint)
        self.assertEqual(host_lines, ["host all all all scram-sha-256"])
        self.assertNotIn("trust", entrypoint.replace("--auth-local=trust", ""))
        self.assert_password_private(result)
        helpers = [call for call in self.calls() if call["kind"] == "helper" and "--dsn" in call["args"]]
        self.assertTrue(helpers)
        self.assertTrue(all(call.get("password_mode") == 0o600 for call in helpers),
                        "password file must be mode 0600 while helpers use it")
        self.assert_cleaned()

    def test_scram_unset_stops_before_baseline_and_keeps_password_private(self):
        result = self.run_case("scram-unset")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.report()["failure"], "password_setup_failed")
        self.assert_password_private(result)
        self.assertFalse(any("CREATE DATABASE rehearsal_base" in argument
                             for call in self.calls() for argument in call["args"]),
                         "an unverified password must stop database creation")
        self.assertFalse(any(call["kind"] == "helper" and "migrate" in call["args"]
                             for call in self.calls()), "an unverified password must stop migration")
        self.assert_cleaned()

    def test_every_helper_dsn_uses_private_password_file(self):
        result = self.run_case(extra=("--scenario", "quiet", "--scenario", "load"))
        self.assertEqual(result.returncode, 0, "authenticated rehearsal failed")
        calls = [call for call in self.calls() if call["kind"] == "helper" and "--dsn" in call["args"]]
        self.assertTrue(calls)
        for call in calls:
            args = call["args"]
            self.assertTrue("--password-file" in args, "helper DSN is missing its password file")
            path = Path(args[args.index("--password-file") + 1])
            self.assertEqual(path.name, "pg-password")
            self.assertEqual(str(path.parent), str(Path(call["environment"]["DOCKER_CONFIG"]).parent))
            self.assertTrue(urlsplit(args[args.index("--dsn") + 1]).password is None,
                            "helper DSN must never carry a password")
            self.assertFalse(path.parent.exists(), "private work directory survived wrapper exit")
        self.assert_cleaned()

    def test_restore_stdin_bytes_pass_through_wrapper(self):
        archive = b"PGDMP" + os.urandom(1024 * 1024 + 3)
        result = self.run_case(command=self.restore_command(), input=archive)
        self.assertEqual(result.returncode, 0, "stdin archive rehearsal failed")
        restores = [call for call in self.calls() if call["kind"] == "docker" and "pg_restore" in call["args"]]
        self.assertEqual(len(restores), 1)
        captured = Path(restores[0]["stdin_file"]).read_bytes()
        self.assertTrue(hashlib.sha256(captured).digest() == hashlib.sha256(archive).digest(),
                        "stdin restore changed or omitted archive bytes")
        self.assertTrue(all(call["stdin_devnull"] for call in self.calls() if call["kind"] in ("helper", "go")),
                        "a non-restore child inherited the archive pipe")
        self.assertEqual(self.report()["status"], "go")
        self.assert_cleaned()

    def test_restore_stdin_bad_header_refused_before_container_or_volume_create(self):
        result = self.run_case(command=self.restore_command(), input=b"WRONGarchive")
        self.assertEqual(result.returncode, 2)
        self.assertFalse(any(call["kind"] == "docker" and ("run" in call["args"] or
                             ("volume" in call["args"] and "create" in call["args"])) for call in self.calls()))
        self.assert_only_wrapper_cleanup()

    def test_restore_stdin_tty_refused(self):
        master, slave = os.openpty()
        try:
            result = subprocess.run(self.restore_command(), env=self.environment, stdin=slave,
                                    stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=10, check=False)
        finally:
            os.close(master)
            os.close(slave)
        self.assertEqual(result.returncode, 2)
        self.assertIn(b"restore - requires a piped archive", result.stderr)
        self.assertFalse(any(call["kind"] == "docker" and ("run" in call["args"] or
                             ("volume" in call["args"] and "create" in call["args"])) for call in self.calls()))
        self.assert_only_wrapper_cleanup()

    def test_restore_stdin_short_read_is_failure_before_measure(self):
        archive = b"PGDMP" + os.urandom(1024 * 1024 + 3)
        started = time.monotonic()
        # Include tool/container startup without imposing a tight wall-clock gate.
        result = self.run_case("restore-short-read", command=self.restore_command("--load-timeout", "60"),
                               input=archive)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(result.returncode, 1, "short restore read must exit cleanly without a shutdown abort")
        self.assertLess(time.monotonic() - started, 60, "broken restore pipe exceeded the load timeout")
        self.assertEqual(self.report()["failure"], "restore_failed")
        self.assertFalse(self.report()["restored_data_ready"])
        self.assertFalse(any(call["kind"] == "helper" and "measure" in call["args"] for call in self.calls()),
                         "a partial restore reached measurement")
        self.assert_cleaned()

    def test_restore_stdin_nonzero_exit_is_failure_before_measure(self):
        result = self.run_case("restore-failure", command=self.restore_command(), input=b"PGDMPfixture")
        self.assertNotEqual(result.returncode, 0)
        report = self.report()
        self.assertEqual(report["failure"], "restore_failed")
        self.assertIs(report["restored_data_ready"], False)
        self.assertFalse(any(call["kind"] == "helper" and "measure" in call["args"] for call in self.calls()),
                         "a failed restore reached measurement")
        private_line = "private@example.test acct_fixture_private " + "a" * 64
        outputs = [result.stdout, result.stderr, json.dumps(report),
                   (self.out / "report.md").read_text(encoding="utf-8")]
        self.assertFalse(any(private_line in output for output in outputs),
                         "private restore diagnostics escaped into output or report")
        self.assert_cleaned()

    def test_restore_stdin_timeout_does_not_wait_for_producer(self):
        self.environment["REHEARSAL_STUB_CASE"] = "success"
        process = subprocess.Popen(self.restore_command("--load-timeout", "1"), env=self.environment,
                                   stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                   start_new_session=True)
        try:
            process.stdin.write(b"PGDMP")
            process.stdin.flush()
            deadline = time.monotonic() + 20
            while process.poll() is None and time.monotonic() < deadline:
                if any("pg_restore" in call["args"] for call in self.calls()):
                    break
                time.sleep(.02)
            self.assertTrue(any("pg_restore" in call["args"] for call in self.calls()),
                            "stdin restore did not start before its timeout could be tested")
            # Keep the producer open past the restore deadline. Timeout must
            # cancel the reader, avoiding Python's buffered-input shutdown abort.
            process.wait(timeout=25)
            self.assertEqual(process.returncode, 1, "restore timeout must exit cleanly without waiting for EOF")
            self.assertEqual(self.report()["failure"], "restore_failed")
            self.assertFalse(any(call["kind"] == "helper" and "measure" in call["args"] for call in self.calls()))
            self.assert_cleaned()
        finally:
            process.stdin.close()
            process.stdin = None
            if process.poll() is None:
                process.send_signal(signal.SIGTERM)
                try:
                    process.wait(timeout=8)
                except subprocess.TimeoutExpired:
                    os.killpg(process.pid, signal.SIGKILL)
            process.communicate()

    def test_restore_size_profile_requires_restore(self):
        result = self.run_case(extra=("--size-profile", "restore"))
        self.assertEqual(result.returncode, 2)
        self.assertFalse(any(call["kind"] == "docker" and "run" in call["args"] for call in self.calls()))

    def test_restore_size_checks_are_informational_and_readiness_is_separate(self):
        result = self.run_case("restore-out-of-band", command=self.restore_command(), input=b"PGDMPfixture")
        self.assertEqual(result.returncode, 0, "restored sizes must not block rehearsal")
        report = self.report()
        self.assertEqual(report["status"], "go")
        self.assertEqual(report["data_source"], "restore")
        self.assertTrue(report["size_checks"])
        self.assertTrue(all(check["result"] == "info" for check in report["size_checks"].values()))
        self.assertFalse(report["serving_readiness"])
        self.assertTrue(report["restored_data_ready"])
        self.assertIn("Size checks are informational for restored data.", (self.out / "report.md").read_text())
        self.assert_cleaned()

    def test_restore_stdin_no_go_is_not_ready(self):
        result = self.run_case("slow", command=self.restore_command("--scenario", "quiet"),
                               input=b"PGDMPfixture")
        self.assertNotEqual(result.returncode, 0)
        report = self.report()
        self.assertEqual(report["status"], "no-go")
        self.assertEqual(report["data_source"], "restore")
        self.assertIs(report["restored_data_ready"], False)
        self.assert_cleaned()

    def test_named_volume_is_labeled_created_before_run_and_removed(self):
        result = self.run_case()
        self.assertEqual(result.returncode, 0, "named volume rehearsal failed")
        calls = [call for call in self.calls() if call["kind"] == "docker"]
        runs = [index for index, call in enumerate(calls) if "run" in call["args"]]
        self.assertEqual(len(runs), 1)
        args = calls[runs[0]]["args"]
        name = args[args.index("--name") + 1]
        creates = [index for index, call in enumerate(calls) if "volume" in call["args"] and "create" in call["args"]]
        self.assertEqual(len(creates), 1)
        self.assertLess(creates[0], runs[0])
        create = calls[creates[0]]["args"]
        self.assertIn(name + "-data", create)
        self.assertEqual(create[create.index("--label") + 1], "com.witwave.witself.rehearsal=" + name)
        self.assertEqual(args[args.index("--mount") + 1], "type=volume,source=" + name + "-data,target=/rehearsal")
        self.assertNotIn("--volume", args)
        for call in calls:
            self.assertEqual(call["args"][:2], ["--host", "unix:///var/run/docker.sock"])
            configuration = Path(call["environment"]["DOCKER_CONFIG"])
            self.assertEqual(configuration.name, "docker")
            self.assertEqual(configuration.parent.parent, self.root)
            self.assertTrue(configuration.parent.name.startswith("witself-rehearsal."))
        self.assert_cleaned()

    def test_named_volume_cleanup_failure_is_loud_and_nonzero(self):
        result = self.run_case("volume-cleanup-failure")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.report()["status"], "go")
        runs = [call["args"] for call in self.calls() if call["kind"] == "docker" and "run" in call["args"]]
        name = runs[0][runs[0].index("--name") + 1]
        self.assertIn("rehearse-store-migrations: cleanup_failed volume=" + name + "-data\n", result.stderr)
        self.assert_cleaned()

    def test_named_volume_preexisting_refused_before_run(self):
        result = self.run_case("volume-preexisting")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.report()["failure"], "volume_exists")
        self.assertFalse(any(call["kind"] == "docker" and "run" in call["args"] for call in self.calls()))
        self.assertFalse(any(call["kind"] == "docker" and "volume" in call["args"] and "create" in call["args"]
                             for call in self.calls()))

    def test_success_cleanup_no_implicit_pull_locale_unset(self):
        self.assertNotIn("LANG", self.environment)
        self.assertNotIn("LC_ALL", self.environment)
        result = self.run_case()
        self.assertEqual(result.returncode, 0, result.stderr[-600:])
        self.assert_cleaned()
        report = self.report()
        self.assertFalse(report["serving_readiness"], "smoke mode cannot certify serving readiness")
        self.assertEqual(report["status"], "go")
        self.assertEqual(report["data_source"], "synthetic")
        self.assertIs(report["restored_data_ready"], False)
        self.assertRegex(report["helper_go_version"], r"^go[0-9]+\.[0-9]+(\.[0-9]+)?$")
        self.assertEqual(report["helper_go_version"], "go1.27.2")
        self.assertFalse(any("pull" in call["args"] for call in self.calls()))
        for call in self.calls():
            if call["kind"] == "helper":
                self.assertFalse(any(":5599" in argument for argument in call["args"]))

    def test_persistent_memory_sampler_uses_one_exec_and_owned_stop_marker(self):
        result = self.run_case()
        self.assertEqual(result.returncode, 0, result.stderr[-600:])
        calls = [call["args"] for call in self.calls() if call["kind"] == "docker"]
        samplers = [args for args in calls if "rehearsal-memory" in args]
        self.assertEqual(len(samplers), 1, "each file must own exactly one persistent memory exec")
        marker = samplers[0][-1]
        self.assertRegex(marker, r"^/rehearsal/memory-[0-9]+$")
        self.assertTrue(any("exec" in args and "rm" in args and marker in args for args in calls),
                        "cleanup must remove the container-side sampler marker before ending the client")
        self.assertFalse(any("cat" in args and "/sys/fs/cgroup/memory.current" in args for args in calls))
        maximum_reads = [args for args in calls if "exec" in args and
                         any("/sys/fs/cgroup/memory.max" in argument for argument in args)]
        self.assertEqual(len(maximum_reads), 1, "memory.max must be read once per run")
        self.assertEqual(self.report()["memory_max_bytes"], 402653184)
        self.assert_cleaned()

    def test_anon_rise_fails_when_current_is_pinned_at_cap(self):
        result = self.run_case("anon-rise-current-pinned")
        self.assertNotEqual(result.returncode, 0)
        report = self.report()
        record = report["scenarios"][0]["files"][0]
        self.assertEqual(record["memory_rise_bytes"], 30 * CONTROLLER.MIB)
        self.assertEqual(record["memory_current_rise_bytes"], 0)
        self.assertEqual(record["memory_baseline_anon_bytes"], 100000000)
        self.assertEqual(record["memory_peak_anon_bytes"], 100000000 + 30 * CONTROLLER.MIB)
        self.assertEqual(record["memory_peak_shmem_bytes"], 1000000)
        self.assertEqual(record["memory_peak_file_bytes"], 270000000)
        self.assertEqual(record["checks"]["memory"], "no-go")
        self.assertEqual(record["checks"]["measurement"], "go")
        markdown = (self.out / "report.md").read_text()
        self.assertIn("memory.stat", markdown)
        self.assertIn("anonymous", markdown.lower())
        self.assert_cleaned()

    def test_page_cache_rise_does_not_fail_flat_anon(self):
        result = self.run_case("file-rise-anon-flat")
        self.assertEqual(result.returncode, 0, result.stderr[-600:])
        record = self.report()["scenarios"][0]["files"][0]
        self.assertEqual(record["memory_rise_bytes"], 0)
        self.assertEqual(record["memory_current_rise_bytes"], 30 * CONTROLLER.MIB)
        self.assertEqual(record["memory_peak_file_bytes"], 270000000 + 30 * CONTROLLER.MIB)
        self.assertEqual(record["checks"]["memory"], "go")
        self.assert_cleaned()

    def test_missing_anon_fails_measurement(self):
        result = self.run_case("missing-anon")
        self.assertNotEqual(result.returncode, 0)
        record = self.report()["scenarios"][0]["files"][0]
        self.assertFalse(record["measurement_ok"])
        self.assertEqual(record["checks"]["measurement"], "no-go")
        self.assert_cleaned()

    def test_memory_sampler_stop_overhead_excluded_from_file_interval(self):
        result = self.run_case("memory-slow-stop")
        self.assertEqual(result.returncode, 0, result.stderr[-600:])
        record = self.report()["scenarios"][0]["files"][0]
        self.assertTrue(record["measurement_ok"])
        self.assertLess(record["max_memory_sample_gap_ms"], 1000)
        self.assert_cleaned()

    def test_unlimited_memory_max_is_reported_as_null(self):
        result = self.run_case("unlimited-memory")
        self.assertEqual(result.returncode, 0, result.stderr[-600:])
        self.assertIsNone(self.report()["memory_max_bytes"])
        self.assert_cleaned()

    def test_failure_cleanup_and_private_error_not_reported(self):
        self.assertNotEqual(self.run_case("fail").returncode, 0)
        self.assert_cleaned()
        self.report()

    def test_migration_failure_retains_only_sqlstate(self):
        result = self.run_case("migrate-sqlstate")
        self.assertNotEqual(result.returncode, 0)
        record = self.report()["scenarios"][0]["files"][0]
        self.assertEqual(record["failure"], "migrate_failed_sqlstate_55P03")
        for name in ("report.json", "report.md"):
            self.assertNotIn("private stderr sentinel", (self.out / name).read_text())
        self.assert_cleaned()

    def test_sigint_cleanup(self):
        self.assert_interrupt_cleanup("interrupt", self.command)

    def test_second_sigint_during_slow_controller_cleanup(self):
        self.assert_delayed_cleanup_signal(signal.SIGINT)

    def test_term_during_slow_controller_cleanup(self):
        # Bash may suppress a repeated INT while its first INT trap unwinds.
        # A different signal proves cleanup installed its ignore traps.
        self.assert_delayed_cleanup_signal(signal.SIGTERM)

    def test_hup_during_slow_controller_cleanup(self):
        self.assert_delayed_cleanup_signal(signal.SIGHUP)

    def assert_delayed_cleanup_signal(self, second_signal):
        # This private controller delays TERM completion so the second signal
        # lands inside the wrapper's cleanup, not during normal controller work.
        shim = self.bin / "python3"
        shim.unlink()
        shim.write_text("#!" + sys.executable + "\n" + r'''
import os, signal, subprocess, time
from pathlib import Path
ready = Path(os.environ["REHEARSAL_STUB_READY"])
def stop(*_):
    ready.with_suffix(".terminating").touch()
    time.sleep(1)
    raise SystemExit(0)
signal.signal(signal.SIGTERM, stop)
subprocess.run(["docker", "run", "--name", os.environ["WITSELF_REHEARSAL_CONTAINER"]], check=True)
ready.touch()
time.sleep(40)
''')
        shim.chmod(0o700)
        self.environment["REHEARSAL_STUB_CASE"] = "success"
        process = subprocess.Popen(self.command, env=self.environment, start_new_session=True,
                                   stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        try:
            deadline = time.monotonic() + 10
            while not self.ready.exists() and process.poll() is None and time.monotonic() < deadline:
                time.sleep(.02)
            self.assertTrue(self.ready.exists(), "stub controller did not start")
            process.send_signal(signal.SIGINT)
            terminating = self.ready.with_suffix(".terminating")
            while not terminating.exists() and process.poll() is None and time.monotonic() < deadline:
                time.sleep(.02)
            self.assertTrue(terminating.exists(), "controller did not receive cleanup TERM")
            process.send_signal(second_signal)
            process.communicate(timeout=5)
            self.assert_cleaned()
            self.assertEqual(process.returncode, 130)
        finally:
            if process.poll() is None:
                os.killpg(process.pid, signal.SIGKILL)
            process.communicate()

    def test_cleanup_failure_is_loud_and_changes_success_exit(self):
        result = self.run_case("cleanup-failure")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.report()["status"], "go", "controller did not reach success before cleanup")
        runs = [call["args"] for call in self.calls() if call["kind"] == "docker" and "run" in call["args"]]
        name = runs[0][runs[0].index("--name") + 1]
        self.assertIn("rehearse-store-migrations: cleanup_failed container=" + name + "\n", result.stderr)
        self.assert_cleaned()

    def test_internal_options_and_abbreviations_are_usage_errors(self):
        for option, value in (("--container", "foo"), ("--co", "foo"), ("--work", "/x"),
                              ("--wo", "/x"), ("--root", "/x"), ("--ro", "/x")):
            with self.subTest(option=option):
                result = self.run_case(extra=(option, value))
                self.assertEqual(result.returncode, 2, result.stderr[-600:])
                self.assertFalse(any(call["kind"] == "docker" and "run" in call["args"]
                                     for call in self.calls()))
                self.assertFalse(list(self.root.glob("witself-rehearsal.*")))

    def test_wrapper_replaces_inherited_internal_environment(self):
        self.environment.update({"WITSELF_REHEARSAL_WORK": "/x", "WITSELF_REHEARSAL_CONTAINER": "foo",
                                 "WITSELF_REHEARSAL_ROOT": "/x"})
        result = self.run_case()
        self.assertEqual(result.returncode, 0, result.stderr[-600:])
        self.assert_cleaned()

    def assert_interrupt_cleanup(self, case, command):
        self.environment["REHEARSAL_STUB_CASE"] = case
        process = subprocess.Popen(command, env=self.environment, start_new_session=True,
                                   stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        try:
            deadline = time.monotonic() + 12
            while not self.ready.exists() and process.poll() is None and time.monotonic() < deadline:
                time.sleep(.05)
            self.assertTrue(self.ready.exists(), "interrupt target did not start")
            process.send_signal(signal.SIGINT)
            process.communicate(timeout=8)
            self.assertNotEqual(process.returncode, 0)
            self.assert_cleaned()
        finally:
            if process.poll() is None:
                os.killpg(process.pid, signal.SIGKILL)
            process.communicate()

    def test_pull_requires_explicit_flag(self):
        result = self.run_case(extra=("--pull",))
        self.assertEqual(result.returncode, 0, result.stderr[-600:])
        self.assertTrue(any("pull" in call["args"] for call in self.calls()))
        self.assert_cleaned()

    def test_scenarios_clone_independently_and_load_appends(self):
        result = self.run_case(extra=("--scenario", "quiet", "--scenario", "load"))
        self.assertEqual(result.returncode, 0, result.stderr[-600:])
        migrations = []
        for call in self.calls():
            args = call["args"]
            if call["kind"] == "helper" and "migrate" in args and "--to" in args:
                if args[args.index("--to") + 1] == "98":
                    migrations.append(args[args.index("--dsn") + 1])
        self.assertEqual(len(migrations), 2)
        self.assertEqual(len(set(migrations)), 2, "scenarios reused the same database")
        self.assertTrue(any(call["kind"] == "helper" and "append" in call["args"]
                            for call in self.calls()))
        self.assertEqual(sum(call["kind"] == "docker" and "exec" in call["args"] and
                             any("/sys/fs/cgroup/memory.max" in argument for argument in call["args"])
                             for call in self.calls()), 1, "multiple scenarios must share one memory.max read")
        self.assert_cleaned()
        self.report()

    def test_named_kill_restarts_and_converges(self):
        result = self.run_case("kill", extra=("--scenario", "kill", "--kill-file",
                               "0098_index_account_export_order.sql", "--kill-after", "0.05"))
        self.assertEqual(result.returncode, 0, result.stderr[-600:])
        scenario = self.report()["scenarios"][0]
        self.assertEqual(scenario["attempts"], 2)
        self.assertTrue(scenario["kill_injected"])
        self.assertLessEqual(scenario["orphan_ms"], 10000)
        self.assertTrue(scenario["files"][0]["converged"])
        self.assert_cleaned()

    def test_kill_refuses_completed_file_during_helper_cleanup(self):
        # The fixture emits FILE immediately after START, then delays helper
        # exit. The kill timer therefore does not race a tiny pre-FILE margin.
        result = self.run_case("kill-after-file", extra=("--scenario", "kill", "--kill-file",
                               "0098_index_account_export_order.sql", "--kill-after", "0.4"))
        self.assertNotEqual(result.returncode, 0)
        scenario = self.report()["scenarios"][0]
        self.assertFalse(scenario["kill_injected"], "killing after FILE did not interrupt the migration")
        self.assertEqual(scenario["attempts"], 1)
        self.assertTrue(scenario["files"][0]["converged"])
        self.assertEqual(scenario["checks"]["kill"], "no-go")
        self.assert_cleaned()

    def test_long_transaction_must_hold_requested_duration(self):
        result = self.run_case("long-tx-early", extra=("--scenario", "load", "--long-tx", "6"))
        self.assertNotEqual(result.returncode, 0)
        scenario = self.report()["scenarios"][0]
        self.assertFalse(scenario["long_transaction_ok"])
        self.assertEqual(scenario["checks"]["long_transaction"], "no-go")
        self.assert_cleaned()

    def test_long_transaction_full_duration_passes(self):
        result = self.run_case("long-tx-complete", extra=("--scenario", "load", "--long-tx", "6"))
        self.assertEqual(result.returncode, 0, result.stderr[-600:])
        scenario = self.report()["scenarios"][0]
        self.assertTrue(scenario["long_transaction_ok"])
        self.assertEqual(scenario["checks"]["long_transaction"], "go")
        self.assert_cleaned()

    def test_long_transaction_must_overlap_first_file_after_sampler_warmup(self):
        result = self.run_case("long-tx-expired-before-file", extra=("--scenario", "load", "--long-tx", "6"))
        self.assertNotEqual(result.returncode, 0)
        scenario = self.report()["scenarios"][0]
        self.assertFalse(scenario["long_transaction_ok"])
        self.assertEqual(scenario["checks"]["long_transaction"], "no-go")
        self.assertTrue(scenario["files"][0]["measurement_ok"])
        self.assert_cleaned()

    def test_stale_memory_baseline_fails_even_for_short_file(self):
        result = self.run_case("stalled-memory-before-file", extra=("--scenario", "load", "--long-tx", "6"))
        self.assertNotEqual(result.returncode, 0)
        record = self.report()["scenarios"][0]["files"][0]
        self.assertFalse(record["measurement_ok"])
        self.assertGreater(record["max_memory_sample_gap_ms"], 1000)
        self.assert_cleaned()

    def test_stalled_observer_fails_measurement_gap(self):
        result = self.run_case("stalled-observer")
        self.assertNotEqual(result.returncode, 0)
        record = self.report()["scenarios"][0]["files"][0]
        self.assertFalse(record["measurement_ok"])
        self.assertEqual(record["checks"]["measurement"], "no-go")
        self.assertGreater(record["max_lock_sample_gap_ms"], 1000)
        self.assert_cleaned()

    def test_stalled_memory_stream_fails_measurement_gap(self):
        result = self.run_case("stalled-memory")
        self.assertNotEqual(result.returncode, 0)
        record = self.report()["scenarios"][0]["files"][0]
        self.assertFalse(record["measurement_ok"])
        self.assertEqual(record["checks"]["measurement"], "no-go")
        self.assertGreater(record["max_memory_sample_gap_ms"], 1000)
        self.assert_cleaned()

    def test_forbidden_port_refused_before_helper_connection(self):
        self.assertNotEqual(self.run_case("forbidden-port").returncode, 0)
        self.assert_cleaned()
        self.assertFalse(any(":5599" in arg for call in self.calls() if call["kind"] == "helper"
                             for arg in call["args"]))

    def test_oom_is_failure(self):
        self.assertNotEqual(self.run_case("oom").returncode, 0)
        self.assert_cleaned()
        self.report()

    def test_oom_evidence_survives_dead_container_memory_reads(self):
        self.assertNotEqual(self.run_case("late-oom").returncode, 0)
        report = self.report()
        records = [record for scenario in report["scenarios"] for record in scenario["files"]]
        self.assertTrue(records, "failed memory read discarded per-file OOM evidence")
        self.assertTrue(any(record["oom_killed"] is True for record in records))
        self.assertTrue(any(record["checks"]["oom"] == "no-go" for record in records))
        self.assertTrue(any(record["measurement_ok"] is False for record in records))
        self.assertEqual(report["status"], "no-go")
        self.assert_cleaned()

    def test_timeout_retains_file_failure_and_timing(self):
        # Keep the setup migration below the same deadline under host load;
        # the target migration still sleeps until the controller times it out.
        self.environment["REHEARSAL_STUB_BASELINE_DELAY"] = ".01"
        self.assertNotEqual(self.run_case("interrupt", extra=("--timeout", "2")).returncode, 0)
        report = self.report()
        self.assertTrue(self.ready.exists(), "target migration did not start")
        self.assertTrue(report["scenarios"], "timeout discarded scenario evidence")
        scenario = report["scenarios"][0]
        self.assertTrue(scenario["files"], "timeout discarded per-file evidence")
        record = scenario["files"][0]
        self.assertEqual(record["failure"], "migration_timeout")
        self.assertFalse(record["converged"])
        self.assertGreater(record["wall_ms"], 0)
        self.assertEqual(scenario["checks"]["files"], "no-go")
        self.assert_cleaned()

    def test_restart_is_failure(self):
        self.assertNotEqual(self.run_case("restart").returncode, 0)
        self.assert_cleaned()
        self.report()

    def test_malformed_memory_fails_closed(self):
        self.assertNotEqual(self.run_case("bad-memory").returncode, 0)
        self.assert_cleaned()
        self.report()

    def test_boolean_memory_value_fails_closed(self):
        self.assertNotEqual(self.run_case("bad-memory-fields").returncode, 0)
        self.assert_cleaned()
        self.report()

    def test_malformed_observer_fails_closed(self):
        self.assertNotEqual(self.run_case("bad-observer").returncode, 0)
        self.assert_cleaned()
        self.report()

    def test_malformed_observer_fields_fail_closed(self):
        self.assertNotEqual(self.run_case("bad-observer-fields").returncode, 0)
        self.assert_cleaned()
        self.report()

    def test_malformed_appender_fields_fail_closed(self):
        self.assertNotEqual(self.run_case("bad-appender-fields", extra=("--scenario", "load")).returncode, 0)
        self.assert_cleaned()
        self.report()

    def test_load_reports_sampled_block_and_separate_batch_bound(self):
        result = self.run_case("appender-blocked", extra=("--scenario", "load"))
        self.assertEqual(result.returncode, 0, result.stderr[-600:])
        record = self.report()["scenarios"][0]["files"][0]
        self.assertGreater(record["appender_longest_wait_ms"], 0)
        self.assertEqual(record["appender_longest_batch_ms"], 1)
        self.assert_cleaned()

    def test_queued_strong_lock_counts_toward_lock_tier(self):
        result = self.run_case("lock-queued", extra=("--lock-go-seconds", ".02", "--lock-founder-seconds", ".05"))
        self.assertNotEqual(result.returncode, 0)
        report = self.report()
        record = report["scenarios"][0]["files"][0]
        self.assertGreater(record["lock_queued_ms"], 50)
        self.assertGreaterEqual(record["lock_hold_ms"], record["lock_queued_ms"])
        self.assertEqual(record["checks"]["locks"], "no-go")
        self.assertEqual(report["scenarios"][0]["checks"]["files"], "no-go")
        self.assertEqual(report["thresholds"]["lock_go_seconds"], .02)
        self.assertEqual(report["thresholds"]["lock_founder_seconds"], .05)
        self.assert_cleaned()

    def test_appender_wait_rechecks_file_locks_after_measurement(self):
        result = self.run_case("appender-blocked", extra=("--scenario", "load", "--lock-go-seconds", ".02",
                                                        "--lock-founder-seconds", ".05"))
        self.assertNotEqual(result.returncode, 0)
        scenario = self.report()["scenarios"][0]
        record = scenario["files"][0]
        self.assertEqual(record["lock_hold_ms"], 0)
        self.assertEqual(record["lock_queued_ms"], 0)
        self.assertGreater(record["appender_longest_wait_ms"], 50)
        self.assertEqual(record["checks"]["locks"], "no-go")
        self.assertEqual(scenario["checks"]["files"], "no-go")
        self.assert_cleaned()

    def test_short_locks_and_appender_wait_pass_subsecond_tiers(self):
        result = self.run_case(extra=("--scenario", "load", "--lock-go-seconds", ".02",
                                      "--lock-founder-seconds", ".05"))
        self.assertEqual(result.returncode, 0, result.stderr[-600:])
        record = self.report()["scenarios"][0]["files"][0]
        self.assertEqual(record["checks"]["locks"], "go")
        self.assertEqual(record["lock_queued_ms"], 0)
        self.assert_cleaned()

    def test_lock_tiers_require_founder_above_go(self):
        for value in (".02", ".01"):
            with self.subTest(founder=value):
                result = self.run_case(extra=("--lock-go-seconds", ".02", "--lock-founder-seconds", value))
                self.assertEqual(result.returncode, 2)
                self.assertFalse(any(call["kind"] == "docker" and "run" in call["args"]
                                     for call in self.calls()))

    def test_verification_uses_requested_timeout_and_distinct_failure(self):
        verification = self.root / "verify.sql"
        verification.write_text("SELECT 1;\n")
        begin = time.monotonic()
        result = self.run_case("verify-timeout", extra=("--verify-sql", str(verification), "--timeout", "2"))
        self.assertNotEqual(result.returncode, 0)
        self.assertLess(time.monotonic() - begin, 15, "verification ignored the two second command timeout")
        scenario = self.report()["scenarios"][0]
        self.assertEqual(scenario["verification"], "verification_timeout")
        self.assertEqual(scenario["checks"]["verification"], "no-go")
        self.assert_cleaned()

    def test_verification_failure_remains_distinct_from_timeout(self):
        verification = self.root / "verify.sql"
        verification.write_text("SELECT 1;\n")
        result = self.run_case("verify-failure", extra=("--verify-sql", str(verification), "--timeout", "2"))
        self.assertNotEqual(result.returncode, 0)
        scenario = self.report()["scenarios"][0]
        self.assertEqual(scenario["verification"], "fail")
        self.assertEqual(scenario["checks"]["verification"], "no-go")
        self.assert_cleaned()

    def test_invalid_index_is_failure(self):
        self.assertNotEqual(self.run_case("invalid-index").returncode, 0)
        self.assert_cleaned()
        self.report()

    def test_restore_failure_cleanup_and_report_privacy(self):
        archive = self.root / "archive.dump"
        archive.write_bytes(b"PGDMP\x00fixture")
        command = [part for part in self.command if part != "--synthetic"]
        command += ["--restore", str(archive)]
        self.assertNotEqual(self.run_case("restore-failure", command=command).returncode, 0)
        self.assert_cleaned()
        self.report()

    def test_restore_sigint_cleanup(self):
        archive = self.root / "archive.dump"
        archive.write_bytes(b"PGDMP\x00fixture")
        command = [part for part in self.command if part != "--synthetic"]
        command += ["--restore", str(archive)]
        self.assert_interrupt_cleanup("restore-interrupt", command)


class ThresholdTests(unittest.TestCase):
    def test_builtin_sampler_rejects_missing_or_nonnumeric_anon(self):
        harness = object.__new__(CONTROLLER.Harness)
        harness.a = SimpleNamespace(container="witself-rehearsal-fixture")
        harness.memory_serial = 0
        commands = []

        def intercept(command):
            commands.append(command)
            raise RuntimeError("sampler intercepted")

        harness.spawn = intercept
        with self.assertRaisesRegex(RuntimeError, "sampler intercepted"):
            harness.start_memory()
        command = commands[0]
        program = command[command.index("-c") + 1]
        with tempfile.TemporaryDirectory(prefix="witself-memory-stat-") as directory:
            root = Path(directory)
            current = root / "memory.current"
            current.write_text("402653184\n")
            stat = root / "memory.stat"
            program = program.replace("/sys/fs/cgroup/memory.stat", str(stat))
            program = program.replace("/sys/fs/cgroup/memory.current", str(current))
            for anon in ("", "anon not-a-number\n"):
                with self.subTest(anon=anon):
                    stat.write_text(anon + "file 300000000\nshmem 1000000\n")
                    result = subprocess.run(["/bin/sh", "-c", program, "rehearsal-memory", str(root / "marker")],
                                            stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=2, check=False)
                    self.assertEqual(result.returncode, 1)
                    self.assertEqual(result.stdout, b"", "missing anonymous memory must not become a zero sample")

    @staticmethod
    def file_record(**overrides):
        record = {"wall_ms": 45000, "lock_hold_ms": 15000,
                  "memory_rise_bytes": 20 * CONTROLLER.MIB, "oom_killed": False,
                  "restarted": False, "measurement_ok": True, "converged": True}
        return dict(record, **overrides)

    def scenario_record(self, **overrides):
        record = {"scenario": "quiet", "files": [{"checks": CONTROLLER.evaluate_file(self.file_record(), "quiet")}],
                  "total_ms": 75000, "invalid_indexes": 0, "verification": "not-requested",
                  "kill_injected": True, "attempts": 2, "orphan_ms": 10000, "appender_ok": True}
        return dict(record, **overrides)

    def test_clone_uses_load_timeout_and_ordinary_sql_keeps_default(self):
        harness = object.__new__(CONTROLLER.Harness)
        harness.a = SimpleNamespace(container="witself-rehearsal-fixture", load_timeout=123)
        calls = []
        harness.docker = lambda *args, **kwargs: calls.append(kwargs)
        harness.sql("postgres", "SELECT 1")
        self.assertEqual(calls[-1]["timeout"], 30)

        def capture_clone(_database, query, **kwargs):
            self.assertIn("TEMPLATE rehearsal_base", query)
            calls.append(kwargs)
            raise RuntimeError("clone intercepted")

        harness.sql = capture_clone
        with self.assertRaisesRegex(RuntimeError, "clone intercepted"):
            harness.scenario("quiet", 0)
        self.assertEqual(calls[-1]["timeout"], 123)

    def test_file_threshold_boundaries(self):
        self.assertTrue(all(value == "go" for value in CONTROLLER.evaluate_file(self.file_record(), "quiet").values()))
        for field, value, check in (("wall_ms", 45001, "wall"),
                                    ("memory_rise_bytes", 20 * CONTROLLER.MIB + 1, "memory"),
                                    ("oom_killed", True, "oom"),
                                    ("restarted", True, "oom"),
                                    ("measurement_ok", False, "measurement"),
                                    ("converged", False, "migration")):
            with self.subTest(check=check, field=field):
                self.assertEqual(CONTROLLER.evaluate_file(self.file_record(**{field: value}), "quiet")[check], "no-go")
        self.assertEqual(CONTROLLER.evaluate_file(self.file_record(wall_ms=45001), "load")["wall"], "go")

    def test_lock_tiers_and_founder_requires_nonzero(self):
        for milliseconds, expected in ((15000, "go"), (15001, "founder"), (30000, "founder"), (30001, "no-go")):
            with self.subTest(milliseconds=milliseconds):
                checks = CONTROLLER.evaluate_file(self.file_record(lock_hold_ms=milliseconds), "quiet")
                self.assertEqual(checks["locks"], expected)
                overall = CONTROLLER.evaluate_scenario(self.scenario_record(files=[{"checks": checks}]))
                self.assertEqual(overall["files"], "go" if expected == "go" else "no-go")

    def test_scenario_threshold_boundaries(self):
        self.assertTrue(all(value == "go" for value in CONTROLLER.evaluate_scenario(self.scenario_record()).values()))
        for field, value, check in (("total_ms", 75001, "total"), ("invalid_indexes", 1, "invalid_indexes"),
                                    ("verification", "fail", "verification")):
            with self.subTest(check=check):
                self.assertEqual(CONTROLLER.evaluate_scenario(self.scenario_record(**{field: value}))[check], "no-go")
        self.assertEqual(CONTROLLER.evaluate_scenario(self.scenario_record(scenario="load", appender_ok=False))["appender"], "no-go")

    def test_empty_scenario_files_cannot_pass(self):
        self.assertEqual(CONTROLLER.evaluate_scenario(self.scenario_record(files=[]))["files"], "no-go")

    def test_kill_attempts_orphan_and_injection_boundaries(self):
        self.assertEqual(CONTROLLER.evaluate_scenario(self.scenario_record(scenario="kill"))["kill"], "go")
        for field, value in (("attempts", 3), ("orphan_ms", 10001), ("kill_injected", False)):
            with self.subTest(field=field):
                self.assertEqual(CONTROLLER.evaluate_scenario(self.scenario_record(scenario="kill", **{field: value}))["kill"], "no-go")

    def test_serving_size_boundaries_and_smoke_skip(self):
        metrics = {"heap_bytes": 460 * CONTROLLER.MIB, "total_bytes": 1387 * CONTROLLER.MIB,
                   "column_bytes": 1055 * CONTROLLER.MIB}
        self.assertTrue(all(check["result"] == "go" for check in CONTROLLER.evaluate_size(metrics, "serving").values()))
        self.assertTrue(all(check["result"] == "skip" for check in CONTROLLER.evaluate_size(metrics, "smoke").values()))
        for key, target in metrics.items():
            for value in (int(target * .9) - 1, int(target * 1.1) + 1):
                with self.subTest(key=key, value=value):
                    measured = dict(metrics, **{key: value})
                    self.assertEqual(CONTROLLER.evaluate_size(measured, "serving")[key]["result"], "no-go")


if __name__ == "__main__":
    # A runner started under nohup or as a background job inherits SIGINT and
    # SIGHUP as ignored, and a shell cannot trap a signal ignored at entry, so
    # the signal-cleanup tests would fail for a reason outside the harness.
    # Restore the defaults before any wrapper is spawned.
    if signal.getsignal(signal.SIGINT) == signal.SIG_IGN:
        signal.signal(signal.SIGINT, signal.default_int_handler)
    if signal.getsignal(signal.SIGHUP) == signal.SIG_IGN:
        signal.signal(signal.SIGHUP, signal.SIG_DFL)
    unittest.main(verbosity=2)
