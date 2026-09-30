# Rehearsal fixture loader

## Purpose

An internal Go tool that builds a synthetic, founder-size account for rehearsing an
account move. It is not shipped and is not a Witself command. It writes transcripts
through the public API. Run it only after the rehearsal's live authorization,
account ownership and credential custody decisions are complete.

## Safety

`mark --yes` accepts an active account no more than 168 hours old, with no
transcripts, and verifies both credentials against the requested account. It creates
one versioned marker transcript. Repeating mark with exactly that marker is safe.
Every subsequent account verb checks the marker; load uses the agent's list.

Every HTTP call uses the same redirect-refusing client. Its closed route list is:

| Where | Method | Path |
|---|---|---|
| Control plane | GET | `/v1/directory/{account}` |
| Control plane | POST | `/v1/accounts/{account}:close` |
| Cell | GET | `/v1/self?observational=true&include_facts=false&include_salient=false&include_counts=false&include_checkpoint=false&include_message_checkpoint=false&include_email_checkpoint=false&include_avatar_checkpoint=false&include_plan_entitlements=false` |
| Cell | GET | `/v1/account` |
| Cell | GET, POST | `/v1/transcripts` |
| Cell | POST | `/v1/transcripts/{id}/entries:batch` |
| Cell | GET | `/v1/transcripts/{id}?tail=true&limit=1` |
| Cell | GET | `/v1/export` |

URLs require HTTPS except loopback for tests. The directory cell must match
`--expect-cell`; the cluster ingress must serve the directory endpoint's host.
Tokens are read only from files with mode 0600 or stricter, then used only in
Authorization headers. The tool never reads a kubeconfig itself. HTTP connection
reuse is disabled to prevent transparent transport retries of an export.

kubectl runs without a shell, with this prefix:
`[--kubeconfig <path>] --context <context> --request-timeout=20s`.
Only these five remaining argument forms are used:

```text
get nodes -o jsonpath={range .items[*]}{.metadata.name}{"\t"}{.status.allocatable.memory}{"\n"}{end}
get --raw /apis/metrics.k8s.io/v1beta1/nodes
get --raw /api/v1/nodes/<node>/proxy/stats/summary
-n witself get pod witself-postgresql-0 -o jsonpath={.spec.nodeName}{"\t"}{.status.containerStatuses[?(@.name=="postgresql")].restartCount}{"\t"}{.status.containerStatuses[?(@.name=="postgresql")].ready}{"\t"}{.status.containerStatuses[?(@.name=="postgresql")].lastState.terminated.reason}
-n witself get ingress witself-server -o jsonpath={.spec.rules[*].host}
```

The probe fails closed. It validates node names before constructing a proxy path.
It reads all nodes, the PostgreSQL pod and exactly one matching PVC. Node memory
uses allocatable memory as its denominator. Volume and filesystem use
`1 - available/capacity`. Equality reaches a threshold.

| Flag | Default | Allowed range |
|---|---:|---:|
| `--max-node-memory-percent` | 90 | 50–95 |
| `--max-node-memory-rise` | 5 points | 1–10 |
| `--max-pvc-percent` | 70 | 30–75 |
| `--max-node-fs-percent` | 75 | 30–78 |

The initial volume projection reserves the estimated remaining logical bytes and
1 GiB for WAL. Before an export, each node must have room for twice the estimated
archive below the filesystem limit. The standalone operator-only measure reserves
twice the maximum goal because it cannot reconstruct the agent's local estimate.
Any PostgreSQL restart stops with exit 6; review it before any resume. Probes run
before writes once the last reading is 15 seconds old, and before retries.

The fixed UTC quiet windows are **23:30–01:00** for nightly account backups and
**02:45–04:00** for the PostgreSQL dump. A measure cannot start less than 20 minutes
before either window. A run entering either window stops. Never change a threshold
without the founder's authorization.

The target is `max(367001600, ceil(founder bytes × 11/10))`, with a ceiling of
500,000,000 bytes. The estimate goal adds 3%. The deterministic generator uses
identifier share 0.05 and Zipf exponent 1.1. A local estimator guides loading;
only a verified export proves the archive size. A short measure permits one scaled
top-up; a measure below half the estimate refuses any top-up. Resume regenerates
the estimate from server tails. There is no state file. The 90-transcript limit
keeps the marker and every load transcript inside the API's 100-row list.

Batches contain at most 100 entries and 1 MiB of request JSON, with one request in
flight and a default cap of 1,000,000 logical bytes/second (range 100,000–2,000,000).
Only create, append and tail reads retry transient failures, after 2 and 4 seconds.
Any 3xx or 4xx stops immediately. Exports are **never retried** and are streamed
through gzip, tar and checksum validation without a local archive. At most two
exports occur per invocation. The dispatcher must separately enforce the rehearsal
budget of two exports across all invocations.

Output is value-free: no credentials, response bodies, entry content, transcript
or entry IDs, or digests. Node names appear only in a quoted kubectl re-run command.

| Exit | Meaning |
|---:|---|
| 0 | Done |
| 2 | Usage error |
| 3 | Refused before writes |
| 4 | Cluster threshold, quiet window or kubectl failure |
| 5 | HTTP, transport or response error |
| 6 | PostgreSQL restarted; review before resume |
| 7 | Target not reached or entry cap reached |
| 130 | Interrupted |

## Build

Build from a merged `main` commit, writing the binary outside the checkout:

```sh
go build -o "$HOME/.witself/handoff/bin/fixture-loader" ./tools/fixture-loader
```

## Procedure

Run these in the approved window using placeholders replaced locally. The registry
cell name may differ from the infrastructure cell name used in the kube context.
A dry run performs reads and planning, and makes no write or export.

```sh
fixture-loader check --kube-context 'witself-<cell>'
fixture-loader mark --account 'acc_<id>' --agent-token-file '<agent token file>' --operator-token-file '<operator token file>' --expect-cell '<registry cell name>' --yes
fixture-loader load --account 'acc_<id>' --agent-token-file '<agent token file>' --operator-token-file '<operator token file>' --expect-cell '<registry cell name>' --kube-context 'witself-<cell>' --founder-archive-bytes '<bytes>' --dry-run
fixture-loader load --account 'acc_<id>' --agent-token-file '<agent token file>' --operator-token-file '<operator token file>' --expect-cell '<registry cell name>' --kube-context 'witself-<cell>' --founder-archive-bytes '<bytes>'
fixture-loader measure --account 'acc_<id>' --operator-token-file '<operator token file>' --expect-cell '<registry cell name>' --kube-context 'witself-<cell>'
fixture-loader close --account 'acc_<id>' --operator-token-file '<operator token file>' --yes
```

`--entries <count>` selects entries mode instead of founder size; it never measures.
`--no-measure` in target mode reports only an estimate. Every target-mode invocation
without that flag must measure, even when resuming an account already above the
estimate goal. Before a rerun, set `--max-measures` to the remaining export budget;
a third export needs a new founder go. Do not automatically run the standalone
measure after a load that used the budget. Record lifecycle state separately before
and after the account move.

## Founder archive size

The bearer goes through a 0600 header file, never through a command argument.
Only the filtered fields below may be printed. Never print the unfiltered response:
backup catalog records carry digests.

```sh
umask 077
hdr=$(mktemp)
{ printf 'Authorization: Bearer '; tr -d '\r\n' < '<fleet token file>'; printf '\n'; } > "$hdr"
curl --silent --show-error --fail --connect-timeout 10 --max-time 60 --header "@$hdr" \
  'https://self.witwave.ai/v1/backups/status?account_id=acc_<id>' \
  | jq '.account.backups.catalog | map(select(.committed_at != null)) | max_by(.committed_at) | {size, committed_at}'
rm -f "$hdr"
```

Require a committed backup from the rehearsal's UTC day; requesting another backup
is a separate live action. The loader itself never holds the fleet credential.

## Clean-up

Run `close --yes`. If a move is stuck or interrupted and the account is not active,
the loader refuses. Check by hand that it is the synthetic account, then use:

```sh
witself account close --account 'acc_<id>' --token-file '<operator token file>' --yes
witself account forget --account '<local name>' --yes
```

Forget each local name saved by `witself account create --name`. Delete every
credential file: the owner's output file and the minted agent and operator files
for each account. Data stays for the 720-hour grace, then the purge worker removes
it. It appears in every nightly PostgreSQL dump of the cell until then. A nightly
account backup can exist if the account was routed at 00:00Z. Verify the eventual
purge separately; this tool does not shorten retention or remove backup objects.

## What it costs a cell

These figures are estimates, not measurements of this tool against a cell.
A 405–470 MB target may need 270k–315k entries and 1.15–1.35 GB of logical content.
Loading takes at least about 20 minutes at the default cap, realistically 30–65
minutes. Each of at most two full measures adds about 7–8 minutes at the observed
clean backup-export rate, or about 9 minutes at the slower overlapping-export rate.
Total time is about 35–90 minutes. Transfer is about 1.3 GB in each direction for
writes and echoed responses, plus 0.4–0.5 GB downloaded per measure.

PostgreSQL may use another 1.15–1.35 GB (13–16% of 8 GiB), plus up to 1 GiB of
transient WAL. Each measure needs about 0.5 GB of node-disk spool. Source space
remains allocated inside PostgreSQL after the move; target data persists for at
least 30 days after close. A restart can interrupt every account on the cell. An
export snapshot may approach the long-transaction alert or the server's 15-minute
limit; the loader cannot prevent either.

No Civo resource or provisioned volume size changes: estimated additional Civo
charge is $0. A possible nightly account backup costs about $0.02 over 90 days at
list price. Roughly 30–32 larger nightly PostgreSQL dumps may retain 13–16 GB,
about $0.20–0.24 per month until an expiry rule exists. Free-tier headroom is
unverified. The evacuation archive is short-lived under existing rules.

Local gates and existing CI jobs gain one package; estimated total cost is 15–40
seconds per run with package race tests expected under 30 seconds. There is no new
job, release artifact, deployment or roll-state matrix.
