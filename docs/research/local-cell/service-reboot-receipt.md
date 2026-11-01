# Linux user-service reboot receipt

Status: **owner receipt recorded 2026-10-30** on a disposable Linux arm64 VM at
commit 917a2b99; the outcome is recorded in the M3 row of the
[living progress record](implementation-plan.md#living-progress-record), and this
document still carries no receipt values. Fable ran this procedure after the
LC-S7A-R change (#856) was reviewed and merged. The driver is
[`TestOwnerRebootReceipt`](../../../internal/localcell/service_receipt_test.go),
compiled only on Linux with `-tags ownerreceipt`. The default suite exercises
only its hermetic contract helpers. The authenticated `service observe` action
returns the cached supervisor health snapshot without stepping, starting,
repairing or otherwise changing lifecycle state.

This receipt proves explicit user registration, an authenticated synthetic write,
a real reboot, automatic authenticated readiness after the owning user's login,
the surviving write, and service removal that retains data. The existing
[`TestNativeServiceLifecycle`](../../../internal/localcell/service_native_test.go)
proves authenticated persistence across a **same-boot** restart in the native CI
lane; that proof does not supply the reboot receipt.

## Prepare a disposable local VM

Use Ubuntu 24.04, or the newest LTS offered by the local hypervisor, such as
26.04. Use arm64 on an Apple silicon host and x86_64 on an x86 host; record the
actual guest architecture and OS. The commands below use Multipass and its
ordinary `ubuntu` user. Another local hypervisor is acceptable with the same
guest, persistent disk and user-session behavior. Do not run this procedure on
the macOS host, a shared server or a production machine.

From the host's reviewed **merged** checkout, record the commit and transfer an
archive; a guest-side clone is unnecessary:

```sh
git rev-parse HEAD
git archive --format=tar.gz --output=/tmp/witself-receipt.tar.gz HEAD
multipass launch 24.04 --name witself-receipt --cpus 4 --memory 8G --disk 30G
multipass transfer /tmp/witself-receipt.tar.gz witself-receipt:/home/ubuntu/witself-receipt.tar.gz
multipass shell witself-receipt
```

Use the selected newer LTS in the launch command if applicable. The archive must
contain this driver at the recorded merged commit. Inside the guest, as `ubuntu`,
prepare the source and PostgreSQL 16:

```sh
mkdir /home/ubuntu/witself
tar -xzf /home/ubuntu/witself-receipt.tar.gz -C /home/ubuntu/witself
sudo apt-get update
sudo apt-get install -y ca-certificates curl build-essential python3
if ! apt-cache show postgresql-16 >/dev/null 2>&1; then
  sudo apt-get install -y postgresql-common
  sudo /usr/share/postgresql-common/pgdg/apt.postgresql.org.sh -y
  sudo apt-get update
fi
sudo apt-get install -y postgresql-16
sudo systemctl disable --now postgresql
sudo systemctl disable --now postgresql@16-main
```

Ubuntu 24.04 supplies `postgresql-16`. The fallback uses the package-provided
setup script for the official [PGDG apt repository](https://apt.postgresql.org/)
when the chosen release does not supply that version. If PGDG does not support
the selected release, use Ubuntu 24.04. The system cluster stays disabled; the
driver creates its own PostgreSQL data directory and runs it as `ubuntu`.

Install the toolchain named in the merged [`go.mod`](../../../go.mod): this
revision requires **Go 1.27.3**, with `go 1.27.0` as the language version. These
commands download the matching Linux tarball from the
[official Go distribution](https://go.dev/dl/), verify its published checksum
without printing it, and install into the fresh guest:

```sh
cd /home/ubuntu
case "$(uname -m)" in
  aarch64) receipt_go_arch=arm64 ;;
  x86_64) receipt_go_arch=amd64 ;;
  *) printf '%s\n' 'Unsupported guest architecture'; exit 1 ;;
esac
curl --fail --silent --show-error --location \
  "https://go.dev/dl/go1.27.3.linux-${receipt_go_arch}.tar.gz" -o receipt-go.tar.gz
curl --fail --silent --show-error --location \
  'https://go.dev/dl/?mode=json&include=all' -o receipt-go-downloads.json
python3 - "$receipt_go_arch" <<'PY' || exit 1
import hashlib, json, pathlib, sys
catalog = json.loads(pathlib.Path('receipt-go-downloads.json').read_text())
filename = 'go1.27.3.linux-' + sys.argv[1] + '.tar.gz'
matches = [item for release in catalog for item in release['files']
           if item['filename'] == filename]
archive = pathlib.Path('receipt-go.tar.gz').read_bytes()
if len(matches) != 1 or hashlib.sha256(archive).hexdigest() != matches[0]['sha256']:
    raise SystemExit('Official Go archive checksum verification failed')
print('Official Go archive checksum verified')
PY
test ! -e /usr/local/go || exit 1
sudo tar -C /usr/local -xzf receipt-go.tar.gz
cd /home/ubuntu/witself
PATH=/usr/local/go/bin:/usr/bin:/bin GOTOOLCHAIN=local go version
PATH=/usr/local/go/bin:/usr/bin:/bin GOTOOLCHAIN=local go mod download
```

Run each setup step successfully before continuing. Dependency provisioning may
use the network in this disposable guest. The driver and its existing
`offlineBuildCommand` helper require the populated module cache and installed
toolchain; component builds disable module downloads, sumdb lookups and automatic
toolchain downloads. The three receipt invocations below also disable them.

## Phase 1: register and write

Use an ordinary login as `ubuntu`, never `sudo go test`. Keep the receipt root
at the same absolute path on the persistent guest disk across the reboot. The
example `/home/ubuntu/receipt` must be fresh: phase 1 refuses an existing manifest
or receipt directory. Roots under `/tmp`, `/var/tmp`, `$TMPDIR`, `/run` or
`/dev/shm` are rejected. Keep the example root short enough for Unix socket paths.

The driver pins both `WITSELF_HOME` and `DSH_HOME` under the root's `home`
directory, selects `WITSELF_RUNTIME=local`, and saves the component binaries and
harness host under `.receipt-build`. Do not move, replace or delete them between
phases. It explicitly installs a systemd **user** unit using the normal owning
user's configuration; do not redirect `HOME` or `XDG_CONFIG_HOME` to a test home.

From `/home/ubuntu/witself`, run the first of the three driver commands:

```sh
PATH=/usr/local/go/bin:/usr/bin:/bin GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off WITSELF_RECEIPT_PHASE=1 WITSELF_RECEIPT_ROOT=/home/ubuntu/receipt WITSELF_LOCALCELL_PGBIN=/usr/lib/postgresql/16/bin go test -tags ownerreceipt ./internal/localcell -run '^TestOwnerRebootReceipt$' -count=1 -timeout=30m -v
```

Phase 1 waits for authenticated control-socket health, uses the native proof's
root-operator/token/Unicode-operator write sequence, then saves
`receipt/phase1.json` with only `boot_id`, `server_pid`, `server_start_time`,
`operator_id` and `registered_at`. The receipt's sole secret is
`receipt/credential`, containing the bearer with mode `0600`. This means the only
additional receipt secret: the existing cell also maintains its private
PostgreSQL password, service control secret and service health bearer. Never
display, copy or attach that file, the database, local credentials or process
environment. Do not collect path listings or document bodies as receipt evidence.

The proof bearer and synthetic operator use the native helper's **one-hour
expiry**. Reboot and finish phase 2 within that hour. Expiry is a failed receipt;
do not mint a replacement to make the existing attempt pass.

## Reboot, log in and run phase 2

After phase 1 reports success, reboot the guest:

```sh
sudo reboot
```

After it boots, open a fresh login from the host:

```sh
multipass shell witself-receipt
```

SSH and Multipass's SSH-backed `shell`/`exec` create login sessions for the
owning user. A local console login is also suitable. No lingering is enabled or
required. The acceptance statement is **after the owning user's login**, not
before login. Do not manually start the unit, run the harness host, or run a cell
start/repair operation after the reboot. A manual start invalidates the automatic
readiness claim. Do not poll `health` or `status`, since those actions may step
the supervisor; phase 2 uses only `observe` before proving readiness. Keep this
login session open while phase 2 runs.

From `/home/ubuntu/witself`, run the second driver command:

```sh
PATH=/usr/local/go/bin:/usr/bin:/bin GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off WITSELF_RECEIPT_PHASE=2 WITSELF_RECEIPT_ROOT=/home/ubuntu/receipt WITSELF_LOCALCELL_PGBIN=/usr/lib/postgresql/16/bin go test -tags ownerreceipt ./internal/localcell -run '^TestOwnerRebootReceipt$' -count=1 -timeout=30m -v
```

Phase 2 checks that the boot ID changed and waits up to 180 seconds for
authenticated control-socket `observe` readiness without starting anything. It
requires a fresh PID/start-time identity, reads the same operator ID and Unicode
name with the persisted bearer, captures the owned process identities, and stops the tree
through the existing control action. It inventories the stopped PGDATA in memory
(relative paths, sizes and content digests), removes the user-service registration,
and requires an unchanged inventory, retained manifest, a disabled or absent unit,
and no remaining owned process. Inventory paths, contents and digests are never
logged or persisted. Only then does it delete the bearer and save
`receipt/phase2.json` with the five boolean results and timestamps. Every proof
must execute and pass; final stored booleans and the operation trace are checked
before deletion or success.
A proof returning false saves an unsuccessful attempt with the remaining
booleans false, then fails with that condition named. Phase 2 refuses to
overwrite that record. Helper failures may stop before a phase-2 record is
written; absence of the success marker is never acceptance.

Expected output, apart from Go's test-run framing and timing:

```text
RECEIPT PHASE1 OK — reboot the VM, log in as the same user, run phase 2
RECEIPT PHASE2 OK
```

The two markers belong to separate invocations on different boots.

## Record the result and clean up

Before cleanup, the owner records the value-free phase-2 results in the
[LC-S7 living progress row](implementation-plan.md#claude-review-and-living-progress-record):

| Field | Record |
|---|---|
| `boot_id_changed` | Actual boolean from phase 2 |
| `automatic_ready_after_login` | Actual boolean from phase 2 |
| `fresh_process_identity` | Actual boolean from phase 2 |
| `write_survived` | Actual boolean from phase 2 |
| `removed_retaining_data` | Actual boolean from phase 2 |
| VM | Actual architecture, Ubuntu release and local hypervisor |
| Source | Reviewed merged commit used for the archive |
| Date | Receipt date in UTC, with the emitted timestamps |
| Existing proof | Same-boot authenticated restart persistence is already CI-proven |

Record PASS only with all five booleans true and both driver invocations
successful. Record an unsuccessful attempt as failed or NOT_RUN, preserving the
first failing condition without private values. This document contains no
recorded receipt values. One VM row does **not** certify the glibc floor, other
D8 OS/architecture rows, channel installation, two-user isolation, sleep/resume,
macOS or Windows registration, or resource budgets.

After recording the result, run the third driver command from the same checkout
and user login:

```sh
PATH=/usr/local/go/bin:/usr/bin:/bin GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off WITSELF_RECEIPT_PHASE=cleanup WITSELF_RECEIPT_ROOT=/home/ubuntu/receipt WITSELF_LOCALCELL_PGBIN=/usr/lib/postgresql/16/bin go test -tags ownerreceipt ./internal/localcell -run '^TestOwnerRebootReceipt$' -count=1 -timeout=30m -v
```

Cleanup purges this cell with `Remove{Purge:true}` and deletes only its
`.receipt-build`, `home` and `receipt` directories. It refuses a still-registered
service so it never edits anything outside the receipt root. Successful phase 2
already removed that registration. Do not run cleanup before recording the
result: it deletes the receipt files.

Successful cleanup prints `RECEIPT CLEANUP OK`. The cell's stable lock files and
inert service metadata may remain under the root; cleanup does not delete the
owning user's configuration or broaden the purge beyond the requested artifacts.

If phase 1 or phase 2 failed while the registration remains, abandon that attempt
and remove only its exact service through the saved harness before cleanup:

```sh
WITSELF_RUNTIME=local WITSELF_HOME=/home/ubuntu/receipt/home/witself DSH_HOME=/home/ubuntu/receipt/home/dsh /home/ubuntu/receipt/.receipt-build/localcellharness service remove --root /home/ubuntu/receipt
```

Then run the cleanup command. This recovery action is not reboot evidence. If
cleanup cannot verify ownership or the saved harness is unavailable, preserve
the value-free failure and destroy the disposable VM through the hypervisor;
do not broaden filesystem deletion or act on an uncertain process. A new receipt
requires a fresh root and a new complete phase-1/reboot/login/phase-2 sequence.

[Local-cell research index](README.md)
