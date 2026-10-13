#!/usr/bin/env python3
"""Private controller for the Bash entrypoint; all reports use a numeric allowlist."""
import argparse
import json
import math
import os
from pathlib import Path
import re
import secrets
import select
import signal
import socket
import subprocess
import sys
import threading
import time

MIB = 1024 * 1024


class Failure(Exception):
    """A fixed, value-free diagnostic code."""


def positive(value):
    try:
        number = float(value)
        if not math.isfinite(number) or number <= 0:
            raise ValueError
        return number
    except ValueError as error:
        raise argparse.ArgumentTypeError("expected a positive finite number") from error


def integer(value):
    if not re.fullmatch(r"[0-9]+", value) or int(value) < 1:
        raise argparse.ArgumentTypeError("expected a positive integer")
    return int(value)


def arguments():
    parser = argparse.ArgumentParser(description=__doc__, allow_abbrev=False)
    parser.add_argument("--from", dest="start", type=integer, required=True)
    parser.add_argument("--to", type=integer)
    parser.add_argument("--out", type=Path, required=True)
    source = parser.add_mutually_exclusive_group(required=True)
    source.add_argument("--synthetic", action="store_true")
    source.add_argument("--restore", type=Path)
    parser.add_argument("--image")
    parser.add_argument("--pull", action="store_true")
    parser.add_argument("--memory", default="384m")
    parser.add_argument("--cpus", type=positive, default=0.38)
    parser.add_argument("--shared-buffers", default="128MB")
    parser.add_argument("--maintenance-work-mem", default="64MB")
    parser.add_argument("--max-wal-size", default="400MB")
    parser.add_argument("--scenario", action="append", choices=("quiet", "load", "kill", "mem16"))
    parser.add_argument("--append-rate", type=positive, default=1)
    parser.add_argument("--long-tx", type=positive)
    parser.add_argument("--kill-file")
    parser.add_argument("--kill-after", type=positive, default=1)
    parser.add_argument("--verify-sql", type=Path)
    parser.add_argument("--entries", type=integer, default=330304)
    parser.add_argument("--transcripts", type=integer, default=2000)
    parser.add_argument("--mutations", type=integer, default=174212)
    parser.add_argument("--run-inputs", type=integer, default=30225)
    parser.add_argument("--other-rows", type=integer, default=300)
    parser.add_argument("--size-profile", choices=("serving", "smoke", "restore"), default="serving")
    parser.add_argument("--file-limit", type=positive, default=45)
    parser.add_argument("--total-limit", type=positive, default=75)
    parser.add_argument("--memory-rise-limit", type=positive, default=20)
    parser.add_argument("--lock-go-seconds", type=positive, default=15)
    parser.add_argument("--lock-founder-seconds", type=positive, default=30)
    parser.add_argument("--timeout", type=positive, default=600)
    parser.add_argument("--load-timeout", type=positive, default=3600)
    args = parser.parse_args()
    if args.lock_founder_seconds <= args.lock_go_seconds:
        parser.error("lock founder threshold must exceed go threshold")
    for name in ("work", "container", "root"):
        value = os.environ.get("WITSELF_REHEARSAL_" + name.upper())
        if not value:
            parser.error("controller requires the rehearsal wrapper")
        setattr(args, name, value)
    for value in (args.memory, args.shared_buffers, args.maintenance_work_mem, args.max_wal_size):
        if not re.fullmatch(r"[1-9][0-9]*(?:[kKmMgG](?:[bB])?)?", value):
            parser.error("invalid memory setting")
    args.scenario = list(dict.fromkeys(args.scenario or ["quiet"]))
    if "kill" in args.scenario and not args.kill_file:
        parser.error("kill scenario requires --kill-file")
    if args.long_tx is not None and args.long_tx <= 5:
        parser.error("long transaction must exceed the five second lead-in")
    if args.size_profile == "restore" and not args.restore:
        parser.error("size profile restore requires --restore")
    args.restore_header = None
    if args.restore == Path("-"):
        if sys.stdin.isatty():
            parser.error("restore - requires a piped archive")
        # Do not prefetch archive bytes into a buffered reader before the pump.
        args.restore_header = b""
        while len(args.restore_header) < 5:
            part = os.read(sys.stdin.fileno(), 5 - len(args.restore_header))
            if not part:
                break
            args.restore_header += part
        if args.restore_header != b"PGDMP":
            parser.error("restore requires a local custom-format dump")
    elif args.restore:
        try:
            with args.restore.open("rb") as dump:
                if dump.read(5) != b"PGDMP":
                    parser.error("restore requires a local custom-format dump")
        except OSError:
            parser.error("restore requires a local custom-format dump")
    if args.verify_sql and not args.verify_sql.is_file():
        parser.error("verification file is unavailable")
    return args


def evaluate_size(metrics, profile):
    return {key: {"measured_bytes": metrics[key], "target_bytes": target * MIB,
                  "result": "info" if profile == "restore" else "skip" if profile == "smoke" else
                  ("go" if 0.9 * target * MIB <= metrics[key] <= 1.1 * target * MIB else "no-go")}
            for key, target in (("heap_bytes", 460), ("total_bytes", 1387), ("column_bytes", 1055))}


def evaluate_file(record, scenario, file_limit=45, memory_limit=20, lock_go=15, lock_founder=30):
    lock = max(record["lock_hold_ms"], record.get("appender_longest_wait_ms", 0)) / 1000
    return {"wall": "go" if scenario != "quiet" or record["wall_ms"] <= file_limit * 1000 else "no-go",
            "memory": "go" if record["memory_rise_bytes"] <= memory_limit * MIB else "no-go",
            "oom": "no-go" if record["oom_killed"] or record["restarted"] else "go",
            "locks": "go" if lock <= lock_go else ("founder" if lock <= lock_founder else "no-go"),
            "measurement": "go" if record["measurement_ok"] else "no-go",
            "migration": "go" if record["converged"] else "no-go"}


def evaluate_scenario(record, total_limit=75):
    checks = {"files": "go" if record["files"] and all(all(v == "go" for v in f["checks"].values()) for f in record["files"]) else "no-go",
              "total": "go" if record["scenario"] != "quiet" or record["total_ms"] <= total_limit * 1000 else "no-go",
              "invalid_indexes": "go" if record["invalid_indexes"] == 0 else "no-go",
              "verification": "go" if record["verification"] in ("pass", "not-requested") else "no-go"}
    if not record.get("measurement_ok", True):
        checks["measurement"] = "no-go"
    if record["scenario"] == "kill":
        checks["kill"] = "go" if record["kill_injected"] and record["attempts"] <= 2 and record["orphan_ms"] <= 10000 else "no-go"
    if record["scenario"] == "load":
        checks["appender"] = "go" if record["appender_ok"] else "no-go"
        checks["long_transaction"] = "go" if record.get("long_transaction_ok", True) else "no-go"
    return checks


def longest_blocked_wait(records, start, end):
    """Longest sampled Lock-wait episode overlapping a migration file."""
    began, longest = None, 0
    for stamp, data in records:
        if data["appender_waiting"] and began is None:
            began = stamp - 0.2
        if began is not None and not data["appender_waiting"]:
            if stamp >= start and began <= end:
                longest = max(longest, (stamp - began) * 1000)
            began = None
    if began is not None and records[-1][0] >= start and began <= end:
        longest = max(longest, (records[-1][0] + 0.2 - began) * 1000)
    return round(longest, 3)


class Stream:
    def __init__(self, proc, kind):
        self.proc, self.kind, self.records, self.bad = proc, kind, [], False
        self.ended_at = None
        self.thread = threading.Thread(target=self.read, daemon=True)
        self.thread.start()

    def read(self):
        try:
            for line in self.proc.stdout:
                data = json.loads(line)
                if not isinstance(data, dict):
                    raise ValueError
                if type(data.get("ready")) is not bool:
                    raise ValueError
                if self.kind == "observer" and (type(data.get("lock_held")) is not bool or
                        type(data.get("lock_waiting")) is not bool or
                        type(data.get("appender_waiting")) is not bool or
                        type(data.get("backend_count")) is not int or data["backend_count"] < 0):
                    raise ValueError
                if self.kind == "appender" and (type(data.get("wait_ms")) not in (int, float) or
                        not math.isfinite(data["wait_ms"]) or data["wait_ms"] < 0):
                    raise ValueError
                if self.kind == "memory" and any(type(data.get(key)) is not int or data[key] < 0
                                                 for key in ("anon", "shmem", "file", "current")):
                    raise ValueError
                self.records.append((time.monotonic(), data))
        except (ValueError, OSError):
            self.bad = True
        finally:
            self.ended_at = time.monotonic()

    def ready(self, timeout=15):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if self.bad or self.proc.poll() is not None:
                raise Failure(self.kind + "_failed")
            if self.records and self.records[-1][1].get("ready"):
                return
            time.sleep(0.02)
        raise Failure(self.kind + "_not_ready")


class Harness:
    def __init__(self, args):
        self.a, self.children = args, []
        self.memory_serial = 0
        self.phase = "initialization"
        self.env = {"PATH": os.environ["PATH"], "LC_ALL": "C", "LANG": "C", "HOME": args.work,
                    "TMPDIR": args.work, "DOCKER_CONFIG": args.work + "/docker",
                    "GOTOOLCHAIN": "local", "GOPROXY": "off"}
        # Only the offline suite can inject executables and stub state.
        self.testing = os.environ.get("WITSELF_REHEARSAL_TEST_MODE") == "1"
        if self.testing:
            self.env.update({k: v for k, v in os.environ.items() if k.startswith("REHEARSAL_STUB_")})
        self.helper = os.environ.get("WITSELF_REHEARSAL_HELPER") if self.testing else None
        self.report = {"format_version": 1, "status": "no-go", "serving_readiness": False,
                       "restored_data_ready": False,
                       "memory_max_bytes": None, "helper_go_version": None,
                       "size_profile": args.size_profile, "sample_interval_ms": 200,
                       "data_source": "synthetic" if args.synthetic else "restore",
                       "settings": {"memory": args.memory, "memory_swap": args.memory, "cpus": args.cpus,
                                    "shared_buffers": args.shared_buffers, "maintenance_work_mem": args.maintenance_work_mem,
                                    "max_wal_size": args.max_wal_size, "append_batches_per_second": args.append_rate,
                                    "long_transaction_seconds": args.long_tx, "kill_after_seconds": args.kill_after},
                       "thresholds": {"quiet_file_seconds": args.file_limit, "quiet_total_seconds": args.total_limit,
                                      "memory_rise_mib": args.memory_rise_limit, "lock_go_seconds": args.lock_go_seconds,
                                      "lock_founder_seconds": args.lock_founder_seconds, "kill_attempts": 2, "orphan_seconds": 10,
                                      "maximum_sample_gap_ms": 1000},
                       "size_checks": {}, "scenarios": [], "failure": None}

    def call(self, command, timeout=30, stdin=None):
        proc = None
        try:
            proc = subprocess.Popen(command, stdin=subprocess.PIPE if stdin else subprocess.DEVNULL,
                                    stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=self.env)
            self.children.append(proc)
            stdout, stderr = proc.communicate(input=stdin, timeout=timeout)
        except subprocess.TimeoutExpired as error:
            raise Failure("command_timeout") from error
        except OSError as error:
            raise Failure("command_unavailable_or_timeout") from error
        finally:
            if proc is not None:
                self.stop(proc)
        if proc.returncode:
            state = re.search(rb"\b(?:SQLSTATE |sqlstate=)([A-Z0-9]{5})\b", stderr)
            suffix = "_sqlstate_" + state.group(1).decode("ascii") if state else ""
            seed_phase = re.search(rb"\bseed_phase=([a-z_]{1,64})\b", stderr)
            if seed_phase and seed_phase.group(1) in (b"dictionary", b"parents", b"transcripts", b"entries", b"curation",
                    b"mutations", b"supporting_rows", b"supporting_graph", b"lifecycle_graph", b"email_graph", b"analyze"):
                suffix += "_" + seed_phase.group(1).decode("ascii")
            raise Failure(self.phase + "_command_failed" + suffix)
        return stdout.decode("utf-8", errors="strict").strip()

    def docker(self, *args, **kwargs):
        return self.call(["docker", "--host", "unix:///var/run/docker.sock", *args], **kwargs)

    def sql(self, db, query, timeout=30):
        return self.docker("exec", self.a.container, "psql", "-X", "-U", "postgres", "-d", db,
                           "-v", "ON_ERROR_STOP=1", "-At", "-c", query, timeout=timeout)

    def dsn(self, database):
        return "postgres://postgres@127.0.0.1:%d/%s?sslmode=disable" % (self.port, database)

    def cmd(self, mode, db=None, *args):
        cmd = [self.helper, "--mode", mode]
        if db:
            cmd += ["--dsn", self.dsn(db), "--password-file", self.a.work + "/pg-password"]
        return cmd + list(args)

    def helper_json(self, mode, db=None, *args):
        try:
            timeout = self.a.load_timeout if mode in ("seed", "measure") else self.a.timeout
            return json.loads(self.call(self.cmd(mode, db, *args), timeout=timeout))
        except (ValueError, UnicodeError) as error:
            raise Failure("invalid_helper_output") from error

    def spawn(self, command, capture_stderr=False):
        proc = subprocess.Popen(command, env=self.env, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                                stderr=subprocess.PIPE if capture_stderr else subprocess.DEVNULL,
                                text=True, start_new_session=True)
        self.children.append(proc)
        return proc

    @staticmethod
    def stop(proc):
        forced = False
        if proc.poll() is None:
            proc.terminate()
            try:
                proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                forced = True
                proc.kill()
                proc.wait()
        return forced

    def prepare(self):
        self.phase = "helper_build"
        go = str(Path(os.environ["GOROOT"]) / "bin/go") if os.environ.get("GOROOT") and not self.testing else "go"
        version = self.call([go, "env", "GOVERSION"])
        if re.fullmatch(r"go[0-9]+\.[0-9]+(?:\.[0-9]+)?", version):
            self.report["helper_go_version"] = version
        if not self.helper:
            self.helper = self.a.work + "/helper"
            # Build with the ordinary local cache; never use the network to obtain modules.
            build_env = dict(os.environ, GOPROXY="off", GOSUMDB="off", GOTOOLCHAIN="local")
            # `go test` exports its selected GOROOT. Use that matching binary,
            # rather than mixing it with an older binary on the curated PATH.
            proc = subprocess.Popen([go, "build", "-o", self.helper, "./internal/tools/migrationrehearsal"],
                                    cwd=self.a.root, env=build_env, stdin=subprocess.DEVNULL,
                                    stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            self.children.append(proc)
            try:
                proc.wait(timeout=self.a.timeout)
            finally:
                self.stop(proc)
            if proc.returncode:
                raise Failure("helper_build_failed")
        self.phase = "migration_inventory"
        files = self.helper_json("list")
        if not isinstance(files, list) or not files:
            raise Failure("invalid_migration_inventory")
        for f in files:
            if not isinstance(f.get("version"), int) or not re.fullmatch(r"[0-9]{4,}_[a-z0-9_]+\.sql", f.get("name", "")):
                raise Failure("invalid_migration_inventory")
        target = self.a.to if self.a.to is not None else max(f["version"] for f in files)
        versions = {f["version"] for f in files}
        if self.a.start not in versions or target not in versions or target <= self.a.start:
            raise Failure("invalid_version_range")
        self.files = [f for f in files if self.a.start < f["version"] <= target]
        if "kill" in self.a.scenario and self.a.kill_file not in [f["name"] for f in self.files]:
            raise Failure("kill_file_outside_range")
        self.report.update({"from": self.a.start, "to": target})
        self.phase = "image_selection"
        image = self.a.image or self.helper_json("image", None, "--values", str(Path(self.a.root) / ".gitops/cells/civo-sandbox-use1-serving/values.yaml"))["image"]
        if self.a.pull:
            self.docker("pull", image, timeout=self.a.timeout)
        self.phase = "image_inspect"
        self.docker("image", "inspect", image)
        # Both the mirrored Bitnami and official PostgreSQL images are supported.
        # The named <container>-data volume is removed, and proven gone, by the owning Bash cleanup trap.
        entrypoint = '''set -eu
mkdir -p /rehearsal/data
chown postgres /rehearsal/data
run_pg() {
  if command -v gosu >/dev/null 2>&1; then gosu postgres "$@"
  elif command -v su-exec >/dev/null 2>&1; then su-exec postgres "$@"
  elif command -v runuser >/dev/null 2>&1; then runuser -u postgres -- "$@"
  else exit 1; fi
}
run_pg initdb -D /rehearsal/data -U postgres --auth-local=trust --auth-host=scram-sha-256 >/dev/null
printf '%s\\n' 'host all all all scram-sha-256' >> /rehearsal/data/pg_hba.conf
run_pg postgres -D /rehearsal/data -c listen_addresses='*' "$@"
'''
        with socket.socket() as listener:
            listener.bind(("127.0.0.1", 0))
            chosen_port = listener.getsockname()[1]
        if chosen_port == 5599 or chosen_port < 1024:
            raise Failure("reserved_or_invalid_port")
        self.phase = "volume_create"
        volume = self.a.container + "-data"
        try:
            self.docker("volume", "inspect", volume)
        except Failure as error:
            if str(error) != "volume_create_command_failed":
                raise
        else:
            raise Failure("volume_exists")
        self.docker("volume", "create", "--label", "com.witwave.witself.rehearsal=" + self.a.container, volume)
        password = secrets.token_hex(32)
        descriptor = os.open(self.a.work + "/pg-password", os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(descriptor, "w", encoding="ascii") as password_file:
            password_file.write(password)
        self.phase = "container_start"
        self.docker("run", "-d", "--pull=never", "--name", self.a.container, "--memory", self.a.memory,
                    "--memory-swap", self.a.memory, "--cpus", str(self.a.cpus), "--publish", "127.0.0.1:%d:5432" % chosen_port,
                    "--mount", "type=volume,source=" + volume + ",target=/rehearsal", "--user", "0", "--entrypoint", "/bin/sh",
                    "--env", "PATH=/opt/bitnami/postgresql/bin:/usr/lib/postgresql/18/bin:/usr/lib/postgresql/17/bin:/usr/lib/postgresql/16/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
                    image, "-c", entrypoint, "rehearsal", "-c", "shared_buffers=" + self.a.shared_buffers,
                    "-c", "maintenance_work_mem=" + self.a.maintenance_work_mem, "-c", "max_wal_size=" + self.a.max_wal_size,
                    timeout=90)
        mapping = self.docker("port", self.a.container, "5432/tcp")
        match = re.fullmatch(r"127\.0\.0\.1:([0-9]+)", mapping)
        if not match:
            raise Failure("non_loopback_port_mapping")
        self.port = int(match.group(1))
        if self.port == 5599 or not 1024 <= self.port <= 65535:
            raise Failure("reserved_or_invalid_port")
        self.phase = "memory_limit"
        memory_max = self.docker("exec", self.a.container, "sh", "-c",
                                 "IFS= read -r value < /sys/fs/cgroup/memory.max; printf '%s\\n' \"$value\"")
        if re.fullmatch(r"[0-9]+", memory_max):
            self.report["memory_max_bytes"] = int(memory_max)
        elif memory_max != "max":
            raise Failure("invalid_memory_limit")
        deadline = time.monotonic() + 90
        while True:
            try:
                if self.sql("postgres", "SELECT 1") == "1":
                    break
            except Failure:
                pass
            if time.monotonic() > deadline:
                raise Failure("postgres_not_ready")
            time.sleep(0.2)
        self.phase = "password_setup"
        try:
            self.docker("exec", "-i", self.a.container, "psql", "-X", "-U", "postgres", "-d", "postgres",
                        "-v", "ON_ERROR_STOP=1", "-q", stdin=("ALTER ROLE postgres PASSWORD '" + password + "';").encode("ascii"))
            if self.sql("postgres", "SELECT rolpassword LIKE 'SCRAM-SHA-256$%' FROM pg_authid WHERE rolname='postgres'") != "t":
                raise Failure("password_setup_failed")
        except Failure as error:
            raise Failure("password_setup_failed") from error
        self.phase = "baseline_create"
        self.sql("postgres", "CREATE DATABASE rehearsal_base")
        self.phase = "baseline_migrate"
        self.call(self.cmd("migrate", "rehearsal_base", "--to", str(self.a.start)), timeout=self.a.timeout)
        if self.a.synthetic:
            self.phase = "synthetic_load"
            metrics = self.helper_json("seed", "rehearsal_base", "--entries", str(self.a.entries),
                                       "--transcripts", str(self.a.transcripts), "--mutations", str(self.a.mutations),
                                       "--inputs", str(self.a.run_inputs), "--other-rows", str(self.a.other_rows))
        else:
            self.phase = "restore"
            # A full custom dump replaces the empty, versioned database. Its
            # restored Goose version must still match the requested baseline.
            if self.a.restore_header is not None:
                self.restore_stdin()
            else:
                with self.a.restore.open("rb") as dump:
                    proc = subprocess.Popen(["docker", "--host", "unix:///var/run/docker.sock", "exec", "-i", self.a.container,
                                             "pg_restore", "-U", "postgres", "-d", "rehearsal_base", "--clean", "--if-exists",
                                             "--no-owner", "--no-privileges", "--exit-on-error"], stdin=dump,
                                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, env=self.env)
                    self.children.append(proc)
                    try:
                        proc.wait(timeout=self.a.load_timeout)
                    finally:
                        self.stop(proc)
                if proc.returncode:
                    raise Failure("restore_failed")
            metrics = self.helper_json("measure", "rehearsal_base")
        self.phase = "baseline_inspect"
        if self.helper_json("inspect", "rehearsal_base")["version"] != self.a.start:
            raise Failure("loaded_schema_mismatch")
        for key in ("entries", "heap_bytes", "total_bytes", "column_bytes"):
            if not isinstance(metrics.get(key), int) or metrics[key] < 1:
                raise Failure("invalid_size_measurement")
        self.report["size_checks"] = evaluate_size(metrics, self.a.size_profile)
        self.report["loaded_entries"] = metrics["entries"]
        self.report["shape"] = {key: value for key, value in metrics.items()
                                if key in ("entries", "transcripts", "mutations", "inputs", "requests", "payload_rows", "reply_rows",
                                           "body_min_bytes", "body_median_bytes", "body_max_bytes", "all_column_bytes")
                                and type(value) is int and value >= 0}
        if any(check["result"] == "no-go" for check in self.report["size_checks"].values()):
            raise Failure("serving_size_acceptance_failed")

    def restore_stdin(self):
        proc = subprocess.Popen(["docker", "--host", "unix:///var/run/docker.sock", "exec", "-i", self.a.container,
                                 "pg_restore", "-U", "postgres", "-d", "rehearsal_base", "--clean", "--if-exists",
                                 "--no-owner", "--no-privileges", "--exit-on-error"], stdin=subprocess.PIPE,
                                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, env=self.env)
        self.children.append(proc)
        failed = threading.Event()
        # A stalled producer must not leave a buffered stdin lock held at exit.
        cancelled = threading.Event()

        def pump():
            try:
                with proc.stdin:
                    proc.stdin.write(self.a.restore_header)
                    while not cancelled.is_set():
                        ready, _, _ = select.select([sys.stdin.fileno()], [], [], 0.1)
                        if not ready:
                            continue
                        chunk = os.read(sys.stdin.fileno(), MIB)
                        if not chunk:
                            break
                        proc.stdin.write(chunk)
            except (BrokenPipeError, OSError, ValueError):
                failed.set()

        deadline = time.monotonic() + self.a.load_timeout
        thread = threading.Thread(target=pump, daemon=True)
        thread.start()
        try:
            proc.wait(timeout=self.a.load_timeout)
            thread.join(timeout=max(0, deadline - time.monotonic()))
            if proc.returncode or thread.is_alive() or failed.is_set():
                raise Failure("restore_failed")
        except subprocess.TimeoutExpired as error:
            raise Failure("restore_failed") from error
        finally:
            cancelled.set()
            self.stop(proc)
            thread.join(timeout=2)

    def start_memory(self):
        # Docker exec startup is much slower than the sampling interval on some
        # local engines. One exec owns the entire loop; all samples use shell
        # builtins except sleep, and the marker gives cleanup an explicit fence.
        self.memory_serial += 1
        marker = "/rehearsal/memory-%d" % self.memory_serial
        program = r'''set -eu
marker=$1
trap 'rm -f "$marker"' EXIT
trap 'exit 0' HUP INT TERM
: > "$marker"
while [ -f "$marker" ]; do
  anon= shmem= file=
  while read -r key value; do
    case "$key" in
      anon) anon=$value;;
      shmem) shmem=$value;;
      file) file=$value;;
    esac
  done < /sys/fs/cgroup/memory.stat
  IFS= read -r current < /sys/fs/cgroup/memory.current
  for value in "$anon" "$shmem" "$file" "$current"; do
    case "$value" in ''|*[!0-9]*) exit 1;; esac
  done
  printf '{"ready":true,"anon":%s,"shmem":%s,"file":%s,"current":%s}\n' "$anon" "$shmem" "$file" "$current"
  sleep 0.2
done
'''
        stream = Stream(self.spawn(["docker", "--host", "unix:///var/run/docker.sock", "exec",
                                    self.a.container, "sh", "-c", program, "rehearsal-memory", marker]), "memory")
        try:
            stream.ready()
            deadline = time.monotonic() + 15
            while len(stream.records) < 3:
                if stream.bad or stream.proc.poll() is not None or not stream.thread.is_alive():
                    raise Failure("memory_failed")
                if time.monotonic() >= deadline:
                    raise Failure("memory_not_ready")
                time.sleep(0.02)
        except BaseException:
            self.stop_memory(stream, marker)
            raise
        return stream, marker

    def stop_memory(self, stream, marker):
        # Killing only the Docker client need not kill its container-side exec.
        # Remove this loop's marker first and wait for it to acknowledge exit.
        removed = True
        try:
            self.docker("exec", self.a.container, "rm", "-f", marker, timeout=5)
        except Failure:
            removed = False
        ended = True
        try:
            stream.proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            ended = False
        self.stop(stream.proc)
        stream.thread.join(timeout=2)
        return removed and ended and stream.proc.returncode == 0 and not stream.thread.is_alive()

    def state(self):
        data = json.loads(self.docker("inspect", "--format", "{{json .State}}", self.a.container))
        restart = int(self.docker("inspect", "--format", "{{.RestartCount}}", self.a.container))
        if not all(isinstance(data.get(k), bool) for k in ("Running", "OOMKilled", "Restarting")):
            raise Failure("invalid_container_state")
        return data, restart

    def file(self, db, scenario, f, observer, not_before=None):
        state_before, restarts_before = self.state()
        memory, memory_marker = self.start_memory()
        baseline_stamp, baseline_data = memory.records[-1]
        attempts, wall_ms, injected, orphan_ms, converged = 0, 0, False, 0, False
        file_failure, kill_at = None, None
        begin = time.monotonic()
        try:
            if not_before is not None:
                time.sleep(max(0, not_before - time.monotonic()))
            baseline_stamp, baseline_data = memory.records[-1]
            begin = time.monotonic()
            while attempts < (2 if scenario == "kill" and f["name"] == self.a.kill_file else 1):
                attempts += 1
                proc = self.spawn(self.cmd("migrate", db, "--to", str(f["version"])), capture_stderr=True)
                started, timings, stream_bad = [], [], []
                stderr_bytes = bytearray()

                def drain_stderr():
                    # Retain at most 64 KiB, but keep draining so a verbose
                    # helper cannot block. No raw diagnostic reaches a report.
                    while True:
                        chunk = proc.stderr.buffer.read(4096)
                        if not chunk:
                            break
                        stderr_bytes.extend(chunk[:max(0, 65536 - len(stderr_bytes))])

                stderr_reader = threading.Thread(target=drain_stderr, daemon=True)
                stderr_reader.start()

                def read_migration():
                    for line in proc.stdout:
                        parts = line.strip().split()
                        if parts == ["START", f["name"]]:
                            started.append(time.monotonic())
                        elif len(parts) == 3 and parts[:2] == ["FILE", f["name"]] and parts[2].isdigit():
                            timings.append(int(parts[2]))
                        else:
                            stream_bad.append(True)

                reader = threading.Thread(target=read_migration, daemon=True)
                reader.start()
                deadline = time.monotonic() + self.a.timeout
                kill_at = None
                while proc.poll() is None:
                    now = time.monotonic()
                    if started and not timings and scenario == "kill" and f["name"] == self.a.kill_file and attempts == 1 and now >= started[0] + self.a.kill_after:
                        proc.kill()
                        kill_at, injected = now, True
                        break
                    if now >= deadline:
                        self.stop(proc)
                        raise Failure("migration_timeout")
                    time.sleep(0.01)
                proc.wait()
                reader.join(timeout=2)
                stderr_reader.join(timeout=2)
                if injected and attempts == 1:
                    wall_ms += (kill_at - started[0]) * 1000
                    # Wait for a post-kill observation with no original backend.
                    while time.monotonic() < deadline:
                        if observer.bad or observer.proc.poll() is not None:
                            raise Failure("observer_failed")
                        if observer.records and observer.records[-1][0] > kill_at and observer.records[-1][1].get("backend_count") == 0:
                            orphan_ms = (observer.records[-1][0] - kill_at) * 1000
                            break
                        time.sleep(0.02)
                    else:
                        raise Failure("orphan_did_not_exit")
                    continue
                if proc.returncode:
                    state = re.search(rb"\b(?:SQLSTATE |sqlstate=)([A-Z0-9]{5})\b", stderr_bytes)
                    file_failure = "migrate_failed" + ("_sqlstate_" + state.group(1).decode("ascii") if state else "")
                converged = proc.returncode == 0 and len(timings) == 1 and not stream_bad
                if injected and attempts == 2 and proc.returncode == 0 and not timings and not stream_bad:
                    # The killed client may have lost its connection immediately
                    # after PostgreSQL committed the version row.
                    converged = self.helper_json("inspect", db)["version"] == f["version"]
                wall_ms += timings[0] if timings else (time.monotonic() - (started[0] if started else begin)) * 1000
                break
        except Failure as error:
            if str(error) == "interrupted":
                raise
            file_failure = str(error)
            converged = False
            wall_ms = max(wall_ms, (time.monotonic() - begin) * 1000)
            if injected and kill_at is not None:
                orphan_ms = max(orphan_ms, (time.monotonic() - kill_at) * 1000)
        finally:
            # Docker teardown is preparation/cleanup, not part of the measured
            # file interval. Capture liveness before intentionally ending it.
            end = time.monotonic()
            memory_live = memory.proc.poll() is None and memory.thread.is_alive()
            memory_stopped = self.stop_memory(memory, memory_marker)
        # A dead container may reject marker removal. Preserve its retained OOM
        # state and partial samples rather than discarding the per-file record.
        state_after, restarts_after = self.state()
        samples = [(begin, baseline_data)] + [(stamp, data) for stamp, data in memory.records
                                         if begin < stamp <= end]
        if not memory_stopped and file_failure is None:
            file_failure = "memory_sampler_stop_failed"
        lock_records = [(stamp, data) for stamp, data in observer.records if begin - 0.2 <= stamp <= end]
        lock_ms, queued_ms, covered_until, queued_until = 0, 0, begin, begin
        for i, (stamp, data) in enumerate(lock_records):
            next_stamp = lock_records[i + 1][0] if i + 1 < len(lock_records) else end
            interval_end = min(end, next_stamp)
            if data.get("lock_held") or data.get("lock_waiting"):
                lock_ms += max(0, interval_end - max(begin, covered_until, stamp - 0.2)) * 1000
                covered_until = interval_end
            if data.get("lock_waiting"):
                queued_ms += max(0, interval_end - max(begin, queued_until, stamp - 0.2)) * 1000
                queued_until = interval_end
        lock_times = [begin] + [stamp for stamp, _ in lock_records if begin < stamp < end] + [end]
        memory_times = [baseline_stamp] + [stamp for stamp, _ in samples if begin < stamp < end] + [end]
        lock_gap = max(b - a for a, b in zip(lock_times, lock_times[1:])) * 1000
        memory_gap = max(b - a for a, b in zip(memory_times, memory_times[1:])) * 1000
        measurement_ok = (memory_live and memory_stopped and not memory.bad and not observer.bad and
                          observer.proc.poll() is None and observer.thread.is_alive() and bool(lock_records) and
                          max(lock_gap, memory_gap) <= self.report["thresholds"]["maximum_sample_gap_ms"])
        record = {"file": f["name"], "explicit_locks": bool(f.get("explicit_locks")), "wall_ms": round(wall_ms, 3),
                  "failure": file_failure,
                  "lock_hold_ms": round(lock_ms, 3), "lock_queued_ms": round(queued_ms, 3),
                  "memory_baseline_bytes": baseline_data["current"],
                  "memory_baseline_anon_bytes": baseline_data["anon"],
                  "memory_peak_anon_bytes": max(data["anon"] for _, data in samples),
                  "memory_peak_shmem_bytes": max(data["shmem"] for _, data in samples),
                  "memory_peak_file_bytes": max(data["file"] for _, data in samples),
                  "memory_rise_bytes": max(0, max(data["anon"] for _, data in samples) - baseline_data["anon"]),
                  "memory_current_rise_bytes": max(0, max(data["current"] for _, data in samples) - baseline_data["current"]),
                  "memory_samples": len(samples), "max_memory_sample_gap_ms": round(memory_gap, 3),
                  "max_lock_sample_gap_ms": round(lock_gap, 3),
                  "oom_killed": state_before["OOMKilled"] or state_after["OOMKilled"],
                  "restarted": restarts_before != 0 or restarts_after != restarts_before or
                  not state_before["Running"] or state_before["Restarting"] or not state_after["Running"] or state_after["Restarting"],
                  "measurement_ok": measurement_ok, "converged": converged, "attempts": attempts,
                  "kill_injected": injected, "orphan_ms": round(orphan_ms, 3), "appender_longest_wait_ms": 0,
                  "appender_longest_batch_ms": 0}
        record["checks"] = evaluate_file(record, scenario, self.a.file_limit, self.a.memory_rise_limit,
                                          self.a.lock_go_seconds, self.a.lock_founder_seconds)
        return record, begin, end

    def scenario(self, name, index):
        self.phase = "scenario_" + name
        db = "rehearsal_scenario_%d" % index
        self.sql("postgres", "CREATE DATABASE %s TEMPLATE rehearsal_base" % db, timeout=self.a.load_timeout)
        # Role-in-database settings persist across every migrator reconnect/retry.
        if name in ("kill", "mem16"):
            self.sql("postgres", "ALTER ROLE postgres IN DATABASE %s SET client_connection_check_interval='5s'" % db)
        if name == "mem16":
            self.sql("postgres", "ALTER ROLE postgres IN DATABASE %s SET maintenance_work_mem='16MB'" % db)
            self.sql("postgres", "ALTER ROLE postgres IN DATABASE %s SET max_parallel_maintenance_workers=0" % db)
        observer = Stream(self.spawn(self.cmd("observe", db)), "observer")
        observer.ready()
        appender, long_tx, spans, long_started = None, None, [], None
        result = {"scenario": name, "files": [], "total_ms": 0, "invalid_indexes": -1,
                  "verification": "not-requested", "kill_injected": False, "orphan_ms": 0, "attempts": 0, "appender_ok": True,
                  "long_transaction_ok": True}
        self.report["scenarios"].append(result)
        try:
            if name == "load":
                appender = Stream(self.spawn(self.cmd("append", db, "--append-rate", str(self.a.append_rate))), "appender")
                appender.ready()
                if self.a.long_tx:
                    long_tx = Stream(self.spawn(self.cmd("long-tx", db, "--long-tx", str(self.a.long_tx))), "long_transaction")
                    long_tx.ready()
                    long_started = long_tx.records[0][0]
            for f in self.files:
                not_before = long_started + 5 if long_started is not None and not spans else None
                record, start, end = self.file(db, name, f, observer, not_before=not_before)
                result["files"].append(record)
                spans.append((record, start, end))
                if not record["converged"] or record["failure"]:
                    break
            if appender:
                result["appender_ok"] = not appender.bad and appender.proc.poll() is None
                forced = self.stop(appender.proc)
                appender.thread.join(timeout=2)
                result["appender_ok"] = result["appender_ok"] and not forced and not appender.bad
                for record, start, end in spans:
                    waits = [data.get("wait_ms", 0) for stamp, data in appender.records
                             if stamp >= start and stamp - data.get("wait_ms", 0) / 1000 <= end]
                    if any(not isinstance(v, (int, float)) or not math.isfinite(v) or v < 0 for v in waits):
                        raise Failure("invalid_appender_measurement")
                    record["appender_longest_batch_ms"] = max(waits, default=0)
                    record["appender_longest_wait_ms"] = longest_blocked_wait(observer.records, start, end)
            if long_tx:
                # Honor the requested hold duration even if the migration files
                # finish early, and reject a helper that lost its snapshot.
                try:
                    long_tx.proc.wait(timeout=max(1, self.a.long_tx - (time.monotonic() - long_started) + 5))
                    long_tx.thread.join(timeout=2)
                    result["long_transaction_ok"] = (long_tx.proc.returncode == 0 and not long_tx.bad and
                                                       long_tx.ended_at is not None and
                                                       long_tx.ended_at - long_started + 0.2 >= self.a.long_tx and
                                                       bool(spans) and spans[0][1] < long_started + self.a.long_tx)
                except subprocess.TimeoutExpired:
                    result["long_transaction_ok"] = False
            final = self.helper_json("inspect", db)
            result["invalid_indexes"] = final["invalid_indexes"]
            if final["version"] != self.report["to"]:
                raise Failure("scenario_schema_mismatch")
            if self.a.verify_sql:
                try:
                    self.call(self.cmd("verify", db, "--verify-sql", str(self.a.verify_sql)), timeout=self.a.timeout)
                    result["verification"] = "pass"
                except Failure as error:
                    result["verification"] = "verification_timeout" if str(error) == "command_timeout" else "fail"
        except Failure as error:
            if str(error) in ("memory_failed", "memory_not_ready"):
                result["measurement_ok"] = False
            raise
        finally:
            for stream in (observer, appender, long_tx):
                if stream:
                    self.stop(stream.proc)
            # Appender Lock-wait episodes are only complete after its shutdown.
            # Final file and scenario verdicts must use these populated fields.
            for record, _, _ in spans:
                record["checks"] = evaluate_file(record, name, self.a.file_limit, self.a.memory_rise_limit,
                                                  self.a.lock_go_seconds, self.a.lock_founder_seconds)
            result["total_ms"] = round(sum(f["wall_ms"] for f in result["files"]), 3)
            result["kill_injected"] = any(f["kill_injected"] for f in result["files"])
            result["orphan_ms"] = max((f["orphan_ms"] for f in result["files"]), default=0)
            result["attempts"] = max((f["attempts"] for f in result["files"]), default=0)
            result["checks"] = evaluate_scenario(result, self.a.total_limit)

    def write_report(self):
        self.a.out.mkdir(parents=True, exist_ok=True)
        encoded = json.dumps(self.report, indent=2, sort_keys=True, allow_nan=False) + "\n"
        # Reports never contain external strings (images, queries, DSNs, errors,
        # dump records). Enforce this also at the serialization boundary.
        if re.search(r"[0-9a-fA-F]{64}|[\w.+-]+@[\w.-]+\.[A-Za-z]{2,}|\b(?:acct|acc|account)_[A-Za-z0-9]+", encoded):
            raise Failure("report_privacy_guard")
        (self.a.out / "report.json").write_text(encoded)
        lines = ["# Store migration rehearsal", "", "Result: **%s**" % self.report["status"], "",
                 "Serving-scale evidence: %s." % ("yes" if self.report["serving_readiness"] else "no"),
                 "Memory and lock measurements use 200 ms sampling; sub-interval peaks/locks may be missed.",
                 "The memory check gates anonymous memory from memory.stat; memory.current rise is informational.",
                 "memory_max_bytes: %s." % json.dumps(self.report["memory_max_bytes"]),
                 "helper_go_version: %s." % json.dumps(self.report["helper_go_version"]),
                 "Lock time includes held and queued strong locks with a conservative one-sample boundary allowance. Founder requires review and exits nonzero.", ""]
        lines += ["Appender wait is the longest sampled Lock-wait episode overlapping the file; JSON also records whole-batch latency as an upper bound.", ""]
        if self.a.size_profile == "restore":
            lines += ["Size checks are informational for restored data.", ""]
        threshold = self.report["thresholds"]
        lines += ["Quiet limits: %.3f seconds per file and %.3f seconds total. Memory rise limit: %.3f MiB per file; no OOM or restart is allowed." %
                  (threshold["quiet_file_seconds"], threshold["quiet_total_seconds"], threshold["memory_rise_mib"]),
                  "Lock tiers for both lock time and appender wait: up to %.3f seconds go; through %.3f seconds founder; above that no-go. Kill: at most two attempts, no invalid indexes, orphan at most 10 seconds." %
                  (threshold["lock_go_seconds"], threshold["lock_founder_seconds"]),
                  "Resource settings: " + json.dumps(self.report["settings"], sort_keys=True) + ".", ""]
        if self.report["failure"]:
            lines += ["Failure: `%s`." % self.report["failure"], ""]
        lines += ["## Size acceptance", "", "| Metric | Measured MiB | Target MiB | Result |", "|---|---:|---:|---|"]
        for key, check in self.report["size_checks"].items():
            lines.append("| %s | %.2f | %.2f | %s |" % (key, check["measured_bytes"] / MIB, check["target_bytes"] / MIB, check["result"]))
        for scenario in self.report["scenarios"]:
            lines += ["", "## " + scenario["scenario"], "", "| File | Wall ms | Lock ms | Queued ms | Anon rise MiB | Peak anon MiB | Peak shmem MiB | Appender wait ms | Checks |", "|---|---:|---:|---:|---:|---:|---:|---:|---|"]
            for f in scenario["files"]:
                lines.append("| %s | %.3f | %.3f | %.3f | %.2f | %.2f | %.2f | %.3f | %s |" %
                             (f["file"], f["wall_ms"], f["lock_hold_ms"], f["lock_queued_ms"], f["memory_rise_bytes"] / MIB,
                              f["memory_peak_anon_bytes"] / MIB, f["memory_peak_shmem_bytes"] / MIB,
                              f["appender_longest_wait_ms"], ", ".join(k + "=" + v for k, v in f["checks"].items())))
            lines += ["", "Total: %.3f ms. Invalid indexes: %d. Orphan lifetime: %.3f ms. Attempts: %d. Verification: %s." %
                      (scenario["total_ms"], scenario["invalid_indexes"], scenario["orphan_ms"], scenario["attempts"], scenario["verification"]),
                      "Checks: " + ", ".join(k + "=" + v for k, v in scenario["checks"].items()) + "."]
        (self.a.out / "report.md").write_text("\n".join(lines) + "\n")

    def run(self):
        try:
            self.prepare()
            for index, name in enumerate(self.a.scenario):
                self.scenario(name, index)
            passed = all(all(v == "go" for v in s["checks"].values()) for s in self.report["scenarios"])
            self.report["status"] = "go" if passed else "no-go"
            self.report["serving_readiness"] = passed and self.a.size_profile == "serving"
            self.report["restored_data_ready"] = passed and self.report["data_source"] == "restore"
        except Failure as error:
            self.report["failure"] = str(error)
        except (OSError, ValueError, KeyError, TypeError, subprocess.TimeoutExpired):
            self.report["failure"] = "rehearsal_failed"
        finally:
            for proc in self.children:
                self.stop(proc)
            self.write_report()
        return 0 if self.report["status"] == "go" else 1


def main():
    # TERM from the shell cleanup must unwind and stop all helper children.
    signal.signal(signal.SIGTERM, lambda *_: (_ for _ in ()).throw(Failure("interrupted")))
    signal.signal(signal.SIGINT, lambda *_: (_ for _ in ()).throw(Failure("interrupted")))
    return Harness(arguments()).run()


if __name__ == "__main__":
    sys.exit(main())
