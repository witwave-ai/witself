# Witself Release And Build Notes

Status: implemented release path with additional hardening targets. The current
automation is defined by `.github/workflows/release.yml`, `.goreleaser.yaml`,
the Homebrew renderer and publisher, the latest-image reconciler, the Helm
chart, `install.sh`, and `install.ps1`; those executable sources win if this
document drifts.

Narrative-memory decision (accepted 2026-07-14): release artifacts have no
backend LLM, model, embedder, or provider credential. PostgreSQL supplies the
deterministic lexical baseline; inference and any vector generation are client
responsibilities. Optional client-supplied vector profiles, portable JSONB
vector rows, and deterministic hybrid ranking are implemented under the
contract in
[narrative-memory-and-curation.md](narrative-memory-and-curation.md).

## Current Automated Release And Rollout Boundary

A stable release starts from a green commit on `main`. Tags include the `v`
prefix, while artifact, container, chart, and GitOps versions omit it:

```sh
VERSION="${RELEASE_VERSION:?set RELEASE_VERSION without a v prefix}"
git tag "v${VERSION}"
git push origin "v${VERSION}"
```

The tag-triggered `release` workflow reruns the Go, PostgreSQL, lint, nested
Pulumi-module, vulnerability, and native Windows x64 provider-integration gates
before publishing. GoReleaser then publishes the macOS and Linux archives, the
Windows x64 `witself` CLI archive, checksum Sigstore bundle and transitional
detached-signature compatibility assets,
archive SBOMs, GitHub release, multi-architecture CLI, server, and PostgreSQL backup images,
and signed immutable image manifests. The workflow renders the `witself`,
`witself-infra`, and `witself-admin` Homebrew formulae from those exact archives
and publishes them to the tap in one non-force commit. After the tap records the
completed release, a serialized reconciliation job promotes both GHCR `latest`
tags to that tap version. A default-branch `workflow_run` guardian repeats the
same reconciliation after every release completion, including a historical
workflow rerun. The workflow separately publishes and attests the
version-matched OCI Helm chart and attaches build-provenance attestations. A
manual `workflow_dispatch` is a non-publishing snapshot build that still renders
and syntax-checks the formulae.

Publishing a tag does **not** deploy a cell. After every required release
artifact exists, roll only the intended canary or wave with
`scripts/roll-cell.sh`, review the resulting GitOps diff, and commit it to
`main`. Argo CD reconciles that desired state only in cells where the bootstrap
application is actually installed and healthy. See
[Deployment Cells](deployment-cells.md) and [`.gitops/README.md`](../.gitops/README.md).

Before pushing prepared values, `make check-local` selects the required local
checks. A strictly classified application-pin-only roll to an already verified
release can use the [focused local rollout gate](local-roll-gate.md). Full
GitHub CI, schema/backup requirements, backup-first ordering, and live
verification remain unchanged.

The release train (`scripts/roll-train.sh`) supports both tag-only
`repository:VERSION` images and digest-pinned `repository@sha256:...` images.
For a digest-pinned cell, convergence requires the selected pod and deployment
image digests to match `apps.witselfServer.imageDigest` read from that cell's
values after the roll; the train does not resolve the registry again. Its
downgrade guard compares versions from image tags for tag-only images and uses
the `chartVersion` and `imageTag` recorded alongside the matching digest for
digest-pinned images, refusing a newer live pin or an unrecognized digest.

The public plan Worker, legal Worker (`witself-legal`), and Cloudflare
control-plane Worker/container are separate deployments. For normal application
releases, deploy all three from the same clean, exactly tagged checkout only
after its release workflow succeeds:

```sh
cd infra/cloudflare/control-plane
npm ci
export EMAIL_DIRECTORY_KV_ID="${EMAIL_DIRECTORY_KV_ID:?set the dedicated 32-character agent-email KV namespace id}"
npm run deploy:plans
npm run deploy:legal
npm run deploy
```

All three commands use the committed Wrangler lock. `npm run deploy:legal`
runs `wrangler deploy --config ../../../web/legal/wrangler.toml` independently
of the control-plane config renderer; a control-plane deploy does not publish
`self.witwave.ai/legal*`. After publication, verify both Version/Effective lines:

```sh
curl --fail --silent --show-error -H 'Accept: text/markdown' \
  https://self.witwave.ai/legal/privacy | grep '^\*\*Version '
curl --fail --silent --show-error -H 'Accept: text/markdown' \
  https://self.witwave.ai/legal/dpa | grep '^\*\*Version '
```

Require them to match the Privacy Policy and DPA Markdown headers in the release
tag, including each Effective date. The legal Worker's consent manifest derives
its versions from those headers, and the control plane reads its Terms and
Privacy versions through `LEGAL_DOCUMENTS`; normal application releases must
use the same tag for both deployments. Also verify exact served Markdown bytes
and the complete manifest as described in
[Publish the legal pages](runbooks.md#publish-the-legal-pages).

A policy-only publication may deploy only `witself-legal` from a clean, tested
commit merged to `main`, without a new application tag or control-plane/plan
deploy. The runbook's prerequisites still apply: preserve prior exact text and
fingerprints, advance the affected version, pass the full local gate required
by `CLAUDE.md` and full PR/post-merge CI, and verify that the live control plane
and released CLI already use the canonical legal manifest. Record the exact
merged commit and verify the served Markdown and manifest after publication.
This exception does not cover Worker logic or configuration changes and does
not weaken any normal application release gate.

The control-plane renderer
injects the release version, full commit, and commit timestamp as Docker build
arguments, and `npm run deploy` polls `/v1/version` until all four identity
fields match. The renderer rejects a dirty checkout and a HEAD without exactly
one `vMAJOR.MINOR.PATCH` tag, so a local source build cannot silently identify
itself as `dev`/`none` in production. Feature-specific runtime secrets and
activation ordering remain separate rollout gates; transcript retention's
disabled-to-preview sequence is in
[Transcript Retention](transcript-retention.md#control-plane-lifecycle-rollout).

Synthetic uptime probes use a dedicated `1,6,11,16,21,26,31,36,41,46,51,56 * * * *` Cron Trigger and the
existing `DIRECTORY` KV binding. The ordinary CP deploy installs this trigger,
the scheduled cell probe handler, and the public, uncached `/metrics/probes`
endpoint. After deploy, verify that the Worker has exactly two Cron Triggers:
`*/5 * * * *` for maintenance and `1,6,11,16,21,26,31,36,41,46,51,56 * * * *` for probes (minutes 1, 6,
through 56 UTC). The deployment verifier pins both expressions. The handler
dispatches each cron to its own work; separate invocations give probes their
own six-connection budget, even if cron timings overlap. The four-worker probe
pool leaves headroom for KV operations. No new namespace or secret needs
operator setup. The container-reaching CP `/v1/version` probe is disabled by the plain variable
`CP_UPTIME_PROBES_CONTROL_PLANE_ENABLED: "false"`, pinned in the template and
both generated-config and deployed-version verification. It is omitted from
results and metrics while disabled. The serving cell's scrape job and alert
rules arrive through normal GitOps sync when monitoring is enabled, with
`platform.monitoring.uptimeProbes.enabled` default off. The replacement serving
cell `civo-sandbox-use1-serving` enables monitoring, incident alerting,
`uptimeProbes` and `entitlementDelivery` in its checked-in values file.
No probe-specific `deploy:plans` step is needed. See
[Synthetic uptime probes](observability-and-operations.md#synthetic-uptime-probes)
for targets, result fields, alert routing, and the existing-plan cost rationale.
Cell probes remain active without warming the CP container. Future CP-target
activation requires a reviewed cost decision because the five-minute probe
would prevent the container's ten-minute idle shutdown. Flip the template's
plain variable and both verifier pins together in source, then use the normal
CP deploy; do not add a secret or change the gate out of band.

The managed outbound agent-email sending adapter is another separately gated
Cloudflare deployment. The tag workflow validates its committed lockfile,
tests, and Worker bundle but intentionally does not provision its Email Sending
domain, Queues, Durable Objects, secrets, signer cohort, or event targets. Run
the [sending-adapter procedure](../infra/cloudflare/agent-email-send/README.md)
only from the same clean, exactly tagged checkout after the release workflow
succeeds. Before invoking Wrangler, independently require an empty
`git status --porcelain`, exactly one `vMAJOR.MINOR.PATCH` tag at `HEAD`, and a
tag/commit equal to the released cell artifacts. Deploy both adapter gates dark
first and record the Git tag, commit, Cloudflare account, and returned Worker
version ID in the private rollout record. Secret or gate changes can create a
new Worker version, so reverify those bindings and record the successor version
before enabling any cell or account layer.

The current Helm chart does not render a migration Job. With a database DSN,
`witself-server serve` applies its embedded Goose migrations before opening the
service; a migration failure prevents that process from serving. A rollout is
therefore not complete until the replacement pods are Ready and report the
expected build from `/v1/version`. This startup behavior must be considered
before any future explicit migration Job is introduced.

### Schema-change train rehearsal

Before a train that changes the store schema, run the migration rehearsal from
the candidate checkout and retain its reports with the train evidence. This is
local development tooling: it starts one disposable PostgreSQL container,
migrates with the store's embedded Goose files, loads data, and runs each
scenario against a fresh database cloned from that loaded baseline. It neither
deploys a cell nor connects to a remote database. The rehearsal is separate from
`make check`; the ordinary gate runs its offline contract tests only.

Use Bash 3.2 or later, Python 3, a local Go toolchain with cached dependencies,
and a local Docker engine exposing cgroup v2 `memory.stat`, `memory.current`, and
`memory.max`. The helper build is offline. `make rehearse-migrations` passes its
selected Go toolchain's `GOROOT` to the wrapper; the report records the selected
binary's `go env GOVERSION` as `helper_go_version` (or `null` if it is not a
recognized release version). Docker uses an empty private configuration and the
local socket.

For example, to rehearse schema 97 to 98 with the serving synthetic shape
(choose the candidate train's actual range):

```sh
make rehearse-migrations REHEARSAL_ARGS='--from 97 --to 98 --synthetic --scenario quiet --scenario load --scenario mem16 --out /tmp/witself-migration-rehearsal'
```

`--from` is required; `--to` defaults to the latest embedded migration. A range
with no pending file is refused. The synthetic fixture targets the schema-97
table graph. Other baselines and archives must still contain the tables and
columns used by the helper's size measurements; older schemas are unverified. The
default image comes from the pinned PostgreSQL image in
`.gitops/cells/civo-sandbox-use1-serving/values.yaml`, using the repository's
values reader. `--image` overrides it. The image must already be local unless
`--pull` is explicitly supplied. The container uses a random localhost port and
a unique `witself-rehearsal-` name; it never uses the shared test container or
port 5599. The local PostgreSQL socket uses trust authentication; TCP uses SCRAM
with a per-run password in a 0600 file inside the private work directory. Exit
and signal traps remove the container, then its named `<container>-data` volume,
and check with `docker volume inspect` that the volume is gone. If it remains,
the wrapper prints `rehearse-store-migrations: cleanup_failed volume=<container>-data`
and exits non-zero. A pre-existing volume of that name is refused with
`volume_exists`. Cleanup also removes the private work directory and password file.

Resource defaults are `--memory 384m` (also the swap limit) and `--cpus 0.38`,
with `--shared-buffers 128MB`, `--maintenance-work-mem 64MB`, and
`--max-wal-size 400MB`. Override these flags only when the retained evidence
clearly identifies the intended resource envelope.

Choose exactly one data source: `--synthetic`, `--restore FILE` for a local
custom-format PostgreSQL archive supplied by the operator, or `--restore -` to
stream that archive from stdin. FILE mode reads an archive the operator supplies;
the harness never writes or fetches one. Real serving backups use `--restore -`
(never decrypt to a file), with `--size-profile restore`:

```sh
age --decrypt -i IDENTITY_FILE ARCHIVE | GOROOT=$(go env GOROOT) bash scripts/rehearse-store-migrations.sh --restore - --size-profile restore --from 97 --to 98 --scenario quiet --out /tmp/witself-migration-rehearsal
```

With `--restore -`, no plaintext archive file is written. The restored database lives in a named Docker volume inside the Docker Desktop VM disk until cleanup removes it; host disk encryption (FileVault) protects it at rest.

`--size-profile restore` requires `--restore` and records size checks as `info`;
size does not stop the rehearsal. Size checks are informational for restored data.
With this profile, a restored run reports `restored_data_ready: true` only when
every scenario passes, and keeps `serving_readiness: false`.

Synthetic defaults are 330,304 transcript entries over
three accounts in a 90/8/2 split and about 2,000 transcripts, 174,212 curation
mutations, and 30,225 curation run inputs. `--entries`, `--transcripts`,
`--mutations`, and `--run-inputs` change the synthetic shape. Before running a
scenario, the default `--size-profile serving` checks the measured transcript heap,
total relation size, and stored body-plus-payload bytes against approximately
460 MiB, 1,387 MiB, and 1,055 MiB respectively, each within ten percent. Read those
measurements in the report; row counts alone do not establish serving scale.
Use `--size-profile smoke` only for a deliberately tiny shape: it skips the
serving-size checks and reports `serving_readiness: false`. That mode exercises
the harness and report parser and cannot establish that a schema-change train
is safe at serving scale.

The supporting fixture populates every table indexed by migrations 97 and 98;
`--other-rows` defaults to 300. Curation mutations need about 30,000 parent
requests to retain the requested distinct-request distribution and foreign keys.
The stored-column measurement uses the requested nullable expression
`sum(pg_column_size(body)+pg_column_size(payload))`; rows without a payload are
excluded by PostgreSQL. The JSON also includes the null-aware sum separately.

Repeat `--scenario` to select the required experiments; the default is `quiet`:

| Scenario | Experiment |
| --- | --- |
| `quiet` | Apply each file without concurrent writes. |
| `load` | Append 100-entry batches with foreign-key checks at `--append-rate` batches per second (default one). `--long-tx SECONDS` also holds a REPEATABLE READ transaction, establishing its snapshot five seconds before the first file. |
| `kill` | Use `--kill-file BASENAME` and `--kill-after SECONDS` to kill the migrator during the selected file, restart it, and check convergence and orphan lifetime. |
| `mem16` | Set the role's `maintenance_work_mem=16MB`, `max_parallel_maintenance_workers=0`, and `client_connection_check_interval=5s` before applying files. |

The kill scenario fails if the selected file finishes before the requested
interruption; killing later helper cleanup does not count. A requested long
transaction must remain open for its full duration, even when migration files
finish earlier.

For a migration that creates derived state, pass `--verify-sql FILE` containing
the verification query. Keep this query free of private output: reports retain
the verification outcome, not data returned by the query. Verification uses
`--timeout`; an expired deadline reports `verification_timeout`, distinct from
a verification failure. The report also records invalid indexes and, for the
kill scenario, convergence attempts and the orphaned backend's observed lifetime.

`--out DIR` receives value-free `report.md` and `report.json`. Per-file evidence
includes elapsed time, sampled strong relation-lock time (held or queued),
anonymous-memory rise over the pre-file baseline, OOM/restart status, and the
appender's longest blocked wait under load. `lock_hold_ms` includes both held and
queued time; `lock_queued_ms` records queued time alone. The report also includes
whole-batch appender transaction latency as a conservative upper bound; that
measure includes ordinary execution time.

The memory check uses anonymous memory (`anon`) from cgroup v2 `memory.stat`.
Page cache is reclaimed at the memory cap, so total `memory.current` can stay
flat while a migration consumes real anonymous-memory headroom. PostgreSQL's
`shared_buffers` are already resident on a long-running server. Each file records
`memory_baseline_anon_bytes`, `memory_peak_anon_bytes`, `memory_peak_shmem_bytes`,
and `memory_peak_file_bytes`; `memory_rise_bytes` is peak anon minus the pre-file
anon baseline. `memory_current_rise_bytes` retains the total-memory rise as
informational evidence and does not gate the result. `memory_max_bytes` records
the container cap, or `null` when `memory.max` is `max`. Missing or nonnumeric
anon samples fail measurement. Concurrent index builds may exceed the unchanged
20 MiB default because their sorts consume anonymous memory sized by
`maintenance_work_mem`; retain that no-go as evidence.

Memory uses a continuous sampler through Docker exec; locks and appender blocking
use a PostgreSQL observer. Both target a sample every 200 ms; short events
may fall between samples. The report records the largest sampling gaps and
fails measurement if a memory or lock-observer gap exceeds one second. That
tolerance accommodates local scheduling and Docker overhead without treating a
stalled sampler as complete evidence. Read these limits as separate checks:

| Check | Default decision |
| --- | --- |
| Quiet elapsed time | Each file at most 45 seconds; all files together at most 75 seconds. Override with `--file-limit` and `--total-limit` (seconds). |
| Memory | Each file's anonymous memory rises at most 20 MiB over its anon baseline; override with `--memory-rise-limit` (MiB). An OOM kill fails. |
| Locks and blocked appender | Use the worse tier of held-or-queued strong relation-lock time and the appender's longest blocked wait. At most 15 seconds is `go`; more than 15 and at most 30 seconds is `founder`; more than 30 seconds is `no-go`. Override with `--lock-go-seconds` and `--lock-founder-seconds`; founder must exceed go. |
| Interrupted migration | Converge within two attempts, leave no invalid indexes, and leave no orphan alive longer than ten seconds with the client connection check interval set. |

Exit zero means every requested scenario passes. `founder` requires a founder
decision and is not an automatic `go`; preserve that finding in the train
review. `--timeout` bounds each migration attempt and verification query
(default 600 seconds).
`--load-timeout` separately bounds synthetic loading, archive restoration, or
baseline cloning
(default 3,600 seconds); loading is outside the migration timing thresholds.
Threshold overrides are experiment inputs, not permission to discard a
failed serving-scale result. Retain evidence for the intended migration range,
data shape, resource caps, and all requested scenarios before proceeding.

The real PostgreSQL integration tests are opt-in through
`WITSELF_TEST_REHEARSAL=1` and are excluded from `make check`.
`TestTinyRehearsalIntegration` rehearses a tiny synthetic dataset from schema 97
to 98 and validates report parsing. `TestTinyRestoreRehearsalIntegration` starts
a loopback-only `witself-rehearsal-src-*` source container, makes a `pg_dump -Fc`,
and pipes it to `--restore - --size-profile restore`. It is the precondition for
a real-data restore rehearsal. Run both tests on
a host with Docker and `postgres:18-alpine3.23` already available locally, or set
`WITSELF_REHEARSAL_IMAGE` to another local image. The integration tests never pull
an image. Their success is plumbing evidence, not full-scale timing or memory
evidence.

```sh
WITSELF_TEST_REHEARSAL=1 go test ./internal/tools/migrationrehearsal -run '^TestTiny(Restore)?RehearsalIntegration$' -count=1 -v -timeout 30m
```

### Cloudflare Worker Dependencies

Wrangler is pinned once in
[`infra/cloudflare/wrangler-version.json`](../infra/cloudflare/wrangler-version.json).
Every Worker's gate checks its installed Wrangler and all
four exact manifest pins against that file. To bump, update the shared version
and run `npm install --save-dev --save-exact wrangler@<version>` in each of
`infra/cloudflare/{control-plane,agent-email,agent-email-send,support-email-intake}`;
retain all four manifest and lockfile changes. Then run `npm ci`, `npm test`, and
`npm run bundle:check` in each directory, followed by `make check-infra` for the
remaining infrastructure gates. Dependabot groups Wrangler updates across these
four directories into one weekly PR; update the shared version file in that PR
before merging, because Dependabot does not update this custom JSON file.

Container SDK evaluation (2026-09-05): the control-plane manifest already uses
`@cloudflare/containers: ^0.3.7`, locked to `0.3.7`, after merged PR #349
(`e5a86ae`) upgraded it from `^0.0.28`; this Wrangler change leaves it untouched.
`npm view @cloudflare/containers version` reports `0.3.7` as latest. The
[upstream changelog](https://github.com/cloudflare/containers/blob/main/CHANGELOG.md)
highlights bridge-relevant changes since `0.0.28`: port-readiness and concurrent
cold-start fixes, response-body activity tracking and bodyless-response fixes,
subclass sleep timeout handling, and startup/stop-state race fixes through
`0.3.7`. The separate SDK follow-up must retain sandbox deployment evidence for
the existing upgrade and any later candidate: deploy the exact Worker/container
build to isolated Cloudflare resources; verify `/v1/version`, concurrent cold
requests through `getContainer(..., "singleton").fetch()` to port 8080,
streamed and bodyless responses with preserved status/headers, the ten-minute
idle sleep and wake cycle, and `destroy()` plus `startAndWaitForPorts()` applying
fresh allowlisted environment values before lifecycle activation succeeds.
Exercise startup failure/recovery and rollback to the prior build without
losing Durable Object state. Local mocked bridge tests and bundle checks do
not establish this runtime proof; no deployment was performed in this slice.

## Goals

- Build Witself as a Go project with one shared core, one public CLI/MCP
  binary, and one public backend API server binary.
- Treat v0 as a usable cloud-shaped slice with CLI, MCP stdio, `witself-server`,
  PostgreSQL-backed deterministic lexical memory, images, a Helm chart, the
  Pulumi-based cell provisioner, CI, and release automation.
- Start from the scaffold boundary in [docs/scaffold-readiness.md](scaffold-readiness.md).
- Keep the project on the latest stable Go release that is practical at the time
  implementation or release work happens.
- Ship the `witself` binary with CLI commands and `witself mcp serve`.
- Ship a separate `witself-server` binary from the same public repository once
  backend implementation starts.
- Support Homebrew, universal `curl | sh`, and native Windows PowerShell
  installation from the beginning.
- Make release artifacts verifiable with checksums.
- Sign checksum manifests and container-image manifests.
- Publish SBOMs and build provenance for release archives and container images
  from the first release.
- Keep the source repository, release artifacts, and packages public.
- Publish public container images from image definitions under `images/*`.
- Publish a public Helm chart for self-hosted Kubernetes deployments once
  `witself-server` exists.
- Treat Prometheus metrics, Kubernetes health probes, and chart-level
  observability support as release-gated server functionality.
- Ship the public Pulumi-based AWS, GCP, and Azure cell provisioner from the
  nested `infra/pulumi` module.
- Make managed and self-hosted backend deployment mechanics visible and
  reviewable from the public repo.
- Keep the tag-triggered release GitHub Action visible and reviewable with the
  product source.

## Go Baseline

Witself should use the latest stable Go release. As of October 20, 2026, the
current stable Go release is `go1.27.3`.

Current root-module settings:

```text
module github.com/witwave-ai/witself

go 1.27.0

toolchain go1.27.3
```

The nested `infra/pulumi` module declares `go 1.27.3`. CI and release jobs select
Go from the relevant module's `go.mod`; the control-plane source-build image uses
`golang:1.27.3`. The CLI and server runtime images copy GoReleaser-built binaries.

Refresh these baselines together before each release. If a new stable Go release
exists, update the toolchain baseline and rerun the full test and release smoke
path before publishing.

## Go Module Policy

- Use Go modules only.
- Start with one module at `github.com/witwave-ai/witself`.
- Keep CLI, MCP, backend API server, storage adapters, and shared core packages
  in the same module unless a real release boundary appears later. Client-side
  inference integrations are not backend dependencies.
- Commit `go.mod` and `go.sum`.
- Run `go mod tidy` after dependency changes.
- Run `go mod verify` in CI.
- Avoid vendoring dependencies by default.
- Keep dependencies current deliberately through reviewable updates.

To update the existing modules to the current baseline:

```sh
go mod edit -go=1.27.0
go mod edit -toolchain=go1.27.3
(cd infra/pulumi && go mod edit -go=1.27.3)
```

## Expected Checks

CI should be strong enough that regressions are caught before release tags.
Checks should run on pull requests and pushes to `main`; release-specific checks
should also run on version tags.

Required checks:

- Root docs checks for `README.md`, `SECURITY.md`, `CONTRIBUTING.md`, and the
  FSL-1.1-ALv2 `LICENSE`.
- `gofmt` cleanliness.
- `go build ./...`
- `go test ./...`
- `go test -race ./...` on Linux.
- `go vet ./...`
- `go mod tidy` cleanliness check.
- `go mod verify`
- Backend API route, auth, policy, audit, storage-adapter, narrative-memory, and
  migration tests once those packages exist.
- Goose migration ordering and apply tests against PostgreSQL, including
  canonical memory history, generated search documents, and lexical indexes.
- Server config validation tests with redacted error output.
- Server health endpoint tests for liveness, readiness, and startup behavior.
- Prometheus metrics registration, route-template labeling, and redaction tests.
- Server smoke test that `/metrics` returns Prometheus text format once the API
  server exists.
- Server smoke tests that API, health, and metrics listeners bind separately and
  that metrics can be disabled.
- Deterministic lexical recall tests and guards proving server config, health,
  images, and charts contain no backend model/provider dependency or credential.
- Capability tests proving optional client-supplied vector profiles are
  explicit, owner-scoped, client-authored data and never imply a backend model
  or pgvector deployment dependency.
- `golangci-lint`.
- `govulncheck`.
- Markdown lint or formatting checks for docs.
- `shellcheck` for install and release scripts.
- `actionlint` for GitHub Actions workflows.
- PSScriptAnalyzer for the native Windows installer and PowerShell smoke
  harness, with only explicitly reviewed style-only rules excluded.
- `hadolint` for Dockerfiles.
- Docker image build smoke tests for every Dockerfile under `images/*`,
  including CLI/MCP and backend server images.
- Helm chart linting for every chart under `charts/*`.
- Helm template rendering with representative production and development values.
- Helm template checks for liveness, readiness, startup, metrics,
  ServiceMonitor, PodMonitor, resource, autoscaling, disruption-budget,
  security-context, and network-policy paths where supported.
- Helm template checks that API, health, and metrics use separate named
  container ports and that metrics resources are omitted when disabled.
- Kubernetes manifest schema validation for rendered Helm templates.
- Nested `infra/pulumi` vet, build, tests, and golangci-lint.
- GoReleaser release-config checks.
- Release artifact signing checks.
- SBOM generation checks.
- Provenance or attestation generation checks.
- Release smoke tests for the Homebrew, curl installation, CLI/MCP image, and
  backend server image paths.

CI should use minimal permissions by default:

- Normal CI: `contents: read`.
- Package publishing: `contents: read`, `packages: write`.
- Release publishing: only the permissions needed to create releases, update the
  Homebrew tap, publish packages, sign artifacts, and publish provenance.
- Scheduled security scan: `contents: read`; its report job adds `issues: write`.

Use concurrency cancellation so superseded pushes do not leave stale CI running.

## Vulnerability Scans

`make security-scan` runs the on-demand Go, npm, and image vulnerability scans. It needs
network access and must not run beside a local gate. It is not part of `make
check` or `make check-infra`. Those gates and CI's required `helm` check run only
the offline contract tests in `scripts/test-security-scan.sh` and
`scripts/test-security-scan-images.sh`, plus the issue and workflow contracts in
`scripts/test-security-scan-issue.sh` and `scripts/test-security-scan-workflow.sh`.
The roll-state matrix also checks image
target coverage against the cell values produced by each roll state. The existing
required root-module govulncheck step in CI, its release counterpart, and
`make govulncheck` are unchanged.

The Go scanner discovers `go.mod` files throughout the repository, including
`infra/pulumi`, and runs govulncheck `v1.7.0` in each module. At a Git worktree
root it includes tracked and non-ignored untracked files without descending
into nested repositories; other roots use filesystem discovery. It excludes
`testdata`, `node_modules`, `.git`, and `.claude` path components. Only
vulnerabilities with a called function are findings; module-only and
package-only matches are informational. Standard-library results use the Go
toolchain that runs the scanner. This checks source reachability, not released
binaries or container images.

The npm scanner audits each `infra/cloudflare/*/package.json` directory from its
lockfile, without installing dependencies. Production dependencies fail at
moderate or above. A second full-tree audit fails at high or above, excluding
packages already reported by the production audit. The higher toolchain
threshold includes Wrangler and its dependencies because deployment tooling
runs with Cloudflare credentials. Lower-severity entries are informational.
Other npm projects, including the dashboard acceptance harness, are not scanned.
A Worker directory without a lockfile makes the scan incomplete.

The image scanner uses Grype to scan the latest released `witself-server` and
`witself-postgres-backup` images plus the live pins of `civo-sandbox-use1-serving`
and `civo-sandbox-use1-backup`. It reads the checked-in cell values; it does not
query live infrastructure. Pinned images use their digest when present, otherwise
their tag. Backup images may be an image object or a `repository:tag` string.
An enabled backup without an image makes the scan incomplete with
`unpinned backup image`; an invalid backup image keeps the cell's other targets in the scan.
Duplicate pinned refs are scanned once, and a release image is omitted
when its repository and tag already appear in a live pin. Grype reads registries
directly on `linux/amd64`, without Docker. Critical or High matches with a fix
available are findings; other matches are informational. In the server image,
`go-module` matches are also informational because govulncheck already judges
those source modules with reachability. This leaves the gap between a source fix
and a release; scanning the released binary with govulncheck is FU132-8. Go
modules in the backup image are still subject to the image threshold.

The image scan does not cover PostgreSQL mirrors (FU132-3), the control-plane
container built at deploy time (FU132-4), the dark `civo-prod-use1-serving` cell
until it is provisioned (FU132-5), or the released CLI/MCP `witself` image
(FU132-11).

Use the `targets` scope to print the planned images without running a scanner:

```sh
bash scripts/security-scan.sh targets --release-version 0.0.333
```

Its tab-separated output contains source, repository, tag, and a digest shortened
to 12 hex characters (or `-`). It lists live pins in the two-cell order above,
server before backup, followed by release server and backup. Omit
`--release-version` to resolve the latest release with `gh release view`;
`--repo OWNER/NAME` selects the release repository and defaults to
`witwave-ai/witself`. A supplied version makes target discovery offline.

Grype is checksum-pinned in `scripts/security-scan-grype.env`. To bump it, verify
the new release's signed checksums with this command (replace `<v>` with the
release version):

```sh
cosign verify-blob --bundle grype_<v>_checksums.txt.sigstore.json --certificate-identity https://github.com/anchore/grype/.github/workflows/release.yaml@refs/heads/main --certificate-oidc-issuer https://token.actions.githubusercontent.com grype_<v>_checksums.txt
```

The certificate identity is a branch ref (`refs/heads/main`). After verification,
write the version and the checksums for both `linux_amd64` and `darwin_arm64` tarballs to that file,
without printing the checksums. Its exact keys are `GRYPE_VERSION`,
`GRYPE_LINUX_AMD64_SHA256`, and `GRYPE_DARWIN_ARM64_SHA256`; the file is parsed as
data, never sourced. Each download is verified before extracting only the
`grype` member. There is no environment override for checksums. Other platforms
require `--grype PATH`, an executable whose version is marked caller-supplied in
the report. `GRYPE_DB_CACHE_DIR` can select a persistent database cache; otherwise
the database cache is removed with the scan's private temporary directory.

For a local report that keeps finding detail off the console, use:

```sh
bash scripts/security-scan.sh all --report ~/.witself/handoff/security-scan-local.md
```

The report's parent directory is created as needed. Without `--report`, the
Markdown report goes to standard output. Scanner output stays in a private
temporary directory that is removed on exit; standard error receives one
status-and-counts summary. Reports contain relative paths, at most 50 rows per
section, and at most 60,000 bytes. Their first line has this machine-readable
shape (counts include rows beyond the display cap):

```text
<!-- witself-security-scan v1 status=<clean|findings|incomplete> go=<n> npm_prod=<n> npm_toolchain=<n> images=<n> incomplete=<n> -->
```

Exit `0` means every planned target validated and no finding met a threshold;
`1` means findings; `2` means invalid usage; `3` means an incomplete scan, such
as a missing tool, missing lockfile, failed scanner, invalid output, or no
targets. Image failures also include a checksum mismatch, failed release lookup,
or missing cell values. Incomplete takes precedence over findings, and the report
retains findings from targets that completed. Successful `targets` output has no
summary on standard error; failure exits `3` with one reason-class line.

### Weekly scheduled run

`.github/workflows/security-scan.yml` runs every Monday at 13:17 UTC: 07:17 MDT
or 06:17 MST (starting November 1, 2026). It also supports an on-demand run:

```sh
gh workflow run security-scan.yml --ref main
```

The `scan` job uses the repository's Go toolchain and Node 22 on `ubuntu-24.04`,
with a 45-minute timeout and only `contents: read`. Only the release lookup step
receives the token in its environment as `GH_TOKEN`. Checkout (with
`persist-credentials: false`), setup-go, and setup-node receive the job's
read-only token through their default `token` input. No scanner step receives
a token in its environment or as an input.
The separate `report` job runs no scanner, has a 10-minute timeout, and adds
`issues: write`. It runs only for `main`, so dispatching on another branch runs
the scan without changing issues. A failed scan keeps the workflow red even
when reporting succeeds. This workflow has no pull-request or push trigger and
is not a required check. Existing PR-time govulncheck behavior is unchanged.

Workflow logs, summaries, artifacts, and issues are public. The workflow's
`SECURITY_SCAN_PUBLIC_DETAIL` switch defaults to `counts` pending the founder's
merge decision. The modes are:

- `counts`: publish the status header, report and section headings, time, commit,
  and finding, informational, scanned, and incomplete counts. Advisory IDs,
  package names, image names, and cell names stay in the local report.
- `no-cells`: publish the report's details with every `cell:<name>` token replaced
  by `pinned`.
- `full`: publish the complete bounded report, including live cell names.

Only the rendered public report is uploaded as `security-scan-report`, retained
for 30 days, appended to the job summary, and used as the issue body. Keep the
private-first reporting policy in `docs/security-policy.md` in mind before
changing the detail mode. Findings or an incomplete scan open or update one
`security` issue titled `Scheduled security scan findings`. An existing issue
qualifies only when its title and label match, its body starts with the scan
header, and its author is `app/github-actions` or `github-actions[bot]`. If
several qualify, only the lowest-numbered issue is updated; its comment names
the other matches. Human-authored issues are never changed. The workflow creates
the `security` label if it is missing and leaves an existing label unchanged.
A clean successful scan comments on and closes the selected tracking issue;
with no tracking issue it writes nothing.

A missing or invalid report, including a missing artifact after a timeout, is
incomplete and cannot close the issue. A clean header from an unsuccessful scan
job is also treated as incomplete. The scan failure uses GitHub's normal workflow
notifications; it adds no paging alert. The dispatcher obtains founder approval
for public detail, issue-write permission, and the scanner thresholds before
merging, using real reports rendered in all three modes.

Triage follows the affected component:

- Go: bump the toolchain or vulnerable module and ship a security release, as
  v0.0.331 did.
- npm production dependencies: merge a Dependabot PR or make a manual dependency
  bump, then deploy the affected Worker.
- npm toolchain: bump Wrangler through `infra/cloudflare/wrangler-version.json`
  using the Wrangler update procedure above.
- Images: ship a release and run the release train, or use a backup image re-pin
  slice when the live backup pin needs to advance.

## Hosted Runner Images And Required Check Names

Every hosted Linux job requests `ubuntu-24.04` or `ubuntu-24.04-arm`, spelled
exactly so. `ubuntu-latest` is not used as a runner label: GitHub's
[announced migration](https://github.com/actions/runner-images/issues/14748)
from Ubuntu 24.04 to Ubuntu 26.04 runs from 2026-10-19 to 2026-11-19, during which
the floating label can select either image. `scripts/test-workflow-runner-labels.sh`
enforces the pins in the `helm` check, release `verify`, and `make check-infra`.

Moving to a newer image is its own reviewed change. First prove the image
without merging anything: on a branch, change the label in a workflow that
already has a `workflow_dispatch` trigger and run it against that branch with
`gh workflow run <workflow> --ref <branch>`. The guard inspects every workflow
file, so a workflow that requests the new label cannot merge before the allowed
list changes. Then change the runner labels, the guard's allowed list and its
canaries, and the label assertions in
`scripts/test-memory-load-quality-workflow.sh`,
`scripts/test-provider-contract-workflow.sh`,
`scripts/test-security-scan-workflow.sh`, and
`scripts/test-mirror-postgresql-image.sh` together. The canaries name the
current label, and the `unreviewed new image` canary must name an image that is
still not allowed. macOS and Windows labels are unchanged by this rule.

These six required check names are contract strings, not runner labels:

- `go`
- `release-config`
- `homebrew-formula`
- `helm`
- `avatar-renderer-portability (ubuntu-latest)`
- `avatar-renderer-portability (ubuntu-24.04-arm)`

The x64 avatar check keeps its earlier name while running on `ubuntu-24.04`.
Renaming a required check needs three ordered changes:

1. Expand: CI reports both the old and the new name. The ruleset and release
   train floor still name the old one.
2. Switch: with no release train in flight, a repository administrator replaces
   the context in the ruleset. Older open pull requests must update their branch
   so they report the new name.
3. Contract: remove the old name from CI and change the release train floor,
   its test fixture, and the guard's required-name list. Run the train from a
   checkout containing this change.

The dashboard acceptance job name and artifact name carry a frozen target name
(`ubuntu-latest`, `macos-15`, or `windows-latest`), not the runner label. The Linux
target runs on `ubuntu-24.04`, so tools that match release job or artifact names
keep working. The load-quality hardware tiers `ubuntu-latest-pg16`,
`ubuntu-latest-pg17`, and `ubuntu-latest-pg18` also remain frozen evidence
identifiers for the standard GitHub-hosted Linux x64 runner class. Keeping them
preserves manifest validation across the image pin; the manifest does not record
the hosted runner image, and hardware tier identifiers must be dotless.

Workflow authors must extend the guard in the same pull request when a change
needs a form it cannot evaluate. It rejects a runner expression other than a
matrix reference, a runner group that is not self-hosted, and YAML aliases.
Where a runner label refers to the matrix, and for every matrix job in `ci.yml`,
it also rejects a matrix with `exclude`, a matrix mixing list keys with
`include`, and a whole-matrix expression. In `ci.yml` it also rejects a job name
that is not static after matrix substitution. A reusable workflow call is not
inspected; the called workflow is inspected when it lives in this repository.

## Release Action

The repository's current release workflow is `.github/workflows/release.yml`.

Workflow:

- Path: `.github/workflows/release.yml`
- Tag trigger: `v*`
- Manual trigger: `workflow_dispatch`
- Primary release tool: GoReleaser.
- Pinned GoReleaser binary: `v2.17.0`.
- Pinned GoReleaser action: `v7.2.3` by immutable commit SHA.

The implemented release action owns:

- Verifying the Go toolchain from `go.mod`.
- Running gofmt, vet, build, race tests, golangci-lint, the nested Pulumi module
  gates, and govulncheck against PostgreSQL-backed store tests.
- Running SHA-verified pinned ShellCheck and actionlint binaries plus pinned
  PSScriptAnalyzer on Windows, so release-script and workflow syntax are checked
  by the same versions as normal CI.
- Running native Windows x64 installer, platform-safety, managed-instruction,
  Codex-hook, Codex provider-contract, and credential-free Claude Code, Grok
  Build, Cursor, OpenClaw, Antigravity, and GitHub Copilot lifecycle gates
  before publication. Cursor's native-Windows cell skips because Windows is
  supported through WSL-as-Linux.
- Building release archives for macOS, Linux, and Windows x64.
- Building `witself`, `witself-server`, `witself-worker`, `witself-admin`,
  `witself-infra`, and the operator-only `witself-control-plane` evidence
  binary.
- Generating SHA256 checksums.
- Retaining the five native provider fixture reports and their validated
  aggregate, and including `provider-contract-evidence.json` in tagged release
  assets and the signed checksum manifest.
- Signing the checksum manifest into a keyless Sigstore bundle, while retaining
  the detached `.sig` and `.pem` assets required by older updaters.
- Generating archive SBOMs and container SBOM attestations.
- Publishing build-provenance attestations for archives and the chart.
- Publishing public GitHub Release assets.
- Publishing the public GHCR CLI/MCP image at
  `ghcr.io/witwave-ai/images/witself`.
- Publishing the public GHCR backend image at
  `ghcr.io/witwave-ai/images/witself-server`.
- Publishing the PostgreSQL backup job image at
  `ghcr.io/witwave-ai/images/witself-postgres-backup`.
- Publishing the public Helm chart at
  `ghcr.io/witwave-ai/charts/witself-server`.
- Signing the published GHCR images.
- Provenance-attesting the published Helm chart.
- Rendering the three public Homebrew formulae from the completed release
  archives and updating `witwave-ai/homebrew-tap` atomically.
- Reconciling the two existing GHCR `latest` tags from the tap's monotonic
  completed-release version in a separate serialized job.
- Re-running that reconciliation from the current default-branch workflow after
  any historical release workflow completes.

CI performs hermetic local-artifact installer smoke tests on native macOS,
Linux, and Windows x64 runners. The provider matrix derives a CLI-only build
from the GoReleaser configuration, installs that exact output through
`install.sh` or `install.ps1`, and uses the resulting executable for the
applicable Codex, Claude Code, Grok Build, Cursor, OpenClaw, Antigravity, and
GitHub Copilot lifecycle contracts. Codex additionally crosses the registered
MCP stdio boundary and invokes read-only `witself.self.show`; the other
providers stop at exact adapter/configuration lifecycle validation. This is
credential-free installer-to-provider contract evidence, not real-client or
authenticated model acceptance. Smoke tests against artifacts from an actual
published GitHub Release remain a separate post-publication check. See
[provider-integration-certification.md](provider-integration-certification.md).

Provider reports and aggregation are bound to one workflow run and attempt.
After a failure, rerun the entire workflow so all five reports belong to the
new attempt. The tagged JSON describes tested snapshot inputs and records the
publishing tag separately; it does not claim real-vendor/model acceptance or
execution of the final public archive bytes. Snapshot packaging omits the JSON
asset, while manual dispatch retains the aggregate as an Actions artifact.

Required workflow permissions:

- `contents: write` for creating or updating GitHub Releases.
- `packages: write` for publishing GHCR packages.
- `id-token: write` for keyless signing and provenance.
- `attestations: write` for GitHub build-provenance records.

Homebrew publication requires an existing `witwave-ai/homebrew-tap` repository
and the `HOMEBREW_TAP_TOKEN` organization secret. The built-in workflow token
cannot push formula updates across repositories.

Manual `workflow_dispatch` builds a snapshot and skips stable publication and
signing.

## Release Artifacts

Current releases include the tenant CLI/MCP binary `witself` (with `ws` as an
installed alias), the separate `witself-server`, the fleet-admin CLI
`witself-admin`, the Pulumi-based cell provisioner `witself-infra`, and the
operator-only `witself-control-plane`. The control-plane artifact is downloaded
directly for reviewed rollout ceremonies; it intentionally has no Homebrew
formula.

The primary repository should be public:

- Repository: `github.com/witwave-ai/witself`
- Visibility: public

All installation and package surfaces should also be public:

- GitHub Release assets.
- Homebrew tap repository and formula.
- GHCR container image packages.
- GHCR Helm chart packages.
- Any future package artifacts required for installation.

Public release paths must not depend on private base images, private package
registries, private chart repositories, or private tap repositories.

Target platforms:

- macOS ARM64.
- macOS x86-64.
- Linux ARM64.
- Linux x86-64.
- Windows x86-64 (`witself` CLI/MCP only).

Current release artifacts include:

- `witself` `.tar.gz` archives for macOS and Linux, plus
  `witself_<version>_windows_amd64.zip` for Windows x64.
- Separate `.tar.gz` archives for `witself-server`, `witself-worker`,
  `witself-admin`, `witself-infra`, and `witself-control-plane` on the macOS and
  Linux targets. Every archive contains exactly one root executable.
- SHA256 checksums.
- A keyless `checksums.txt.sigstore.json` bundle for the checksum manifest,
  plus transitional `checksums.txt.sig` and `checksums.txt.pem` compatibility
  assets for older `witself-admin` updaters.
- Per-archive SBOMs.
- `provider-contract-evidence.json`, containing the validated native fixture
  matrix for the tagged release workflow.
- Build-provenance attestations for release archives.

The stable release inventory is fail-closed at exactly 25 executable archives,
25 per-archive SPDX SBOMs, one provider contract JSON and four checksum/signing
assets (`checksums.txt`, its Sigstore bundle, compatibility certificate, and
detached signature): 55 nonempty GitHub Release assets total. `checksums.txt`
binds all 51 archive, SBOM and provider evidence payloads, and GitHub build
provenance continues to cover the 25 archives. The JSON is covered by the
checksum signature, not by an archive provenance attestation. The
`witself-control-plane` executable is the only command stamped with the full
40-hex release commit because billing inventory capture compares that exact
identity; the other commands retain their historical short-commit output.

Verify the preferred bundle form with:

```sh
TAG=v0.0.186
cosign verify-blob \
  --bundle checksums.txt.sigstore.json \
  --certificate-identity "https://github.com/witwave-ai/witself/.github/workflows/release.yml@refs/tags/${TAG}" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt
```

When `witself-admin` finds Cosign on `PATH`, self-upgrade signature checks fail
closed. Use Cosign v3, or patched Cosign v2.6.2 or newer; an older installed
Cosign that cannot verify the bundle blocks the upgrade and should be upgraded
before retrying.

Machine-readable release metadata and shell completions remain hardening
targets.

## Helm Chart

Witself publishes a public Helm chart for `witself-server` on every stable tag.

Initial chart:

- Chart path: `charts/witself-server`
- OCI package: `ghcr.io/witwave-ai/charts/witself-server`
- Primary API workload: `witself-server`
- Background workload: `witself-worker`

Install shape, where `VERSION` omits the tag's `v` prefix:

```sh
VERSION="${RELEASE_VERSION:?set RELEASE_VERSION}"
helm install witself oci://ghcr.io/witwave-ai/charts/witself-server \
  --version "$VERSION" \
  --namespace witself \
  --create-namespace \
  --values ./witself-values.yaml
```

Chart requirements:

- Production values should assume external PostgreSQL and optional external
  object/blob storage. No backend model/provider configuration is permitted.
  Agent secrets use client-held AVKs and client-side envelope encryption;
  they require no backend KMS configuration. The `sensitive` fact flag controls
  redaction, not secret-value encryption.
- Raw database passwords, KMS credentials, provider secrets, tokens,
  passphrases, private keys, and wallet credentials must not be placed directly
  in default values.
- Values should support existing Kubernetes Secret references and
  deployment-native identity such as service account annotations.
- The chart includes independent API and worker Deployments, Services,
  ServiceAccount, ConfigMaps, optional Ingress, and optional NetworkPolicy
  templates. It does not currently render a migration Job.
- The chart should include liveness, readiness, startup, metrics,
  ServiceMonitor, PodMonitor, resource, autoscaling, disruption-budget,
  security-context, and network-policy support where practical.
- When a database Secret is configured, `witself-server serve` applies embedded
  Goose migrations before serving. Failed migrations keep the pod from becoming
  Ready and must stop the rollout.
- Chart releases should include rendered-template smoke tests, schema
  validation, signing or provenance attestation, and public publication.

## API And Capability Contract

Release checks should validate that the CLI, MCP tools, and `witself-server`
agree on the public JSON and API contracts.

Required release checks once the API exists:

- OpenAPI generation or validation for `/v1`.
- Example request/response validation for the memory, fact, policy, group, and
  message resources and their colon-action subroutes (for example
  `/v1/messages/{message_id}:ack` and the mailbox `POST /v1/messages:listen`
  long-poll action).
- Health endpoint smoke tests for `/livez`, `/readyz`, and
  `/startupz`.
- Prometheus scrape smoke test for `/metrics`.
- Metrics-disabled smoke test proving the metrics listener and monitor resources
  are absent.
- `/v1/capabilities` smoke test for managed, self-hosted, and local development
  profiles where available.
- Capability-surface checks that `/v1/capabilities` reports the state of the
  memory, fact, policy, group, and message subsystems, the active lexical
  retrieval mode, and the implemented client-vector profile capability
  independently. It must not report a backend model/provider.
- Capability-surface checks for the post-v0 cross-realm collaboration surface:
  `/v1/capabilities` should report the `cross_realm_collaboration`,
  `federation`, and `agent_card` flags as capability-gated and off by default
  until cross-realm collaboration ships. Realm-local `messaging` is in v0; only
  the cross-realm surface is deferred.
- CLI `witself capabilities --json` smoke test.
- MCP `witself.capabilities` schema check.
- Deterministic `unsupported_operation` checks for unavailable backend
  features, including client-supplied vector profiles, cross-agent access
  policy, group-scoped shared identity data, messaging, and billing.
- `witself://` reference parse/resolve smoke tests for memory, fact, agent, and
  group reference forms.
- `witself://` reference parse/resolve smoke tests for the post-v0
  realm-qualified cross-realm agent form
  `witself://<realm-handle>/agent/<name>`, which parses but resolves only once
  cross-realm collaboration ships.

## Pulumi Infrastructure

Current cell infrastructure lives in the nested Go module under `infra/pulumi`.
The release verify job runs vet, build, tests, and golangci-lint inside that
module. GoReleaser packages its `witself-infra` command as separate macOS and
Linux archives, and the Homebrew renderer turns those archives into its
formula. The command drives Pulumi through the Automation API and keeps its
large provider graph outside the root CLI module.

Release artifacts must never include Pulumi state, backend credentials, cloud
credentials, customer identifiers, or secret configuration. A release makes
the provisioner binary available; it does not run an infrastructure update or
change a cell.

## Container Images

Witself publishes public Docker images from Dockerfiles under `images/*`. The
CLI/MCP image is:

- Dockerfile: `images/witself/Dockerfile`
- Image package: `ghcr.io/witwave-ai/images/witself`
- Platforms: `linux/amd64`, `linux/arm64`

The CLI/MCP image runs the same `witself` binary published in release archives.
Its entrypoint supports both ordinary CLI and MCP usage:

```sh
VERSION="${VERSION:?set VERSION to a published version without the v prefix}"
docker run --rm "ghcr.io/witwave-ai/images/witself:${VERSION}" version
docker run --rm -i "ghcr.io/witwave-ai/images/witself:${VERSION}" mcp serve
```

The backend image is:

- Dockerfile: `images/witself-server/Dockerfile`
- Image package: `ghcr.io/witwave-ai/images/witself-server`
- Platforms: `linux/amd64`, `linux/arm64`
- Default entrypoint: `witself-server`
- Additional executable: `/usr/local/bin/witself-worker`

Example:

```sh
VERSION="${VERSION:?set VERSION to a published version without the v prefix}"
docker run --rm "ghcr.io/witwave-ai/images/witself-server:${VERSION}" version
docker run --rm -p 8080:8080 -p 8081:8081 -p 9090:9090 \
  "ghcr.io/witwave-ai/images/witself-server:${VERSION}" serve
docker run --rm --entrypoint /usr/local/bin/witself-worker \
  -p 8081:8081 -p 9090:9090 \
  "ghcr.io/witwave-ai/images/witself-server:${VERSION}" serve
```

Implemented CLI/server image behavior:

- Build from the public distroless static base image.
- Run as the distroless non-root user.
- Include version, commit, and build date metadata labels.
- Publish immutable version tags without the Git tag's `v` prefix through
  GoReleaser. Preserve the existing moving `latest` channel through a separate,
  serialized reconciliation step that reads the tap's completed-release
  version. The default-branch guardian repairs writes from historical workflow
  reruns, so an older-tag retry cannot leave the channel rolled back.
- Support `linux/amd64` and `linux/arm64`.
- Do not include tokens, passphrases, store files, identity exports,
  model/provider credentials, or test fixtures in an image.
- Sign published images.
- Publish image SBOMs.
- Publish container SBOM attestations.

Published-image execution smoke tests remain release-hardening targets.

The scheduled PostgreSQL backup image is:

- Dockerfile: `images/witself-postgres-backup/Dockerfile`
- Image package: `ghcr.io/witwave-ai/images/witself-postgres-backup`
- Platforms: `linux/amd64`, `linux/arm64`

It pins the official PostgreSQL 18 Alpine base by its multi-architecture index
digest and installs exact `age` and `aws-cli` Alpine package versions at build
time. The PostgreSQL base supplies `pg_dump`, `psql`, Bash, and gzip. Updating
the base or package versions requires a reviewed Dockerfile change and another
release; unavailable package versions fail the build. The shared GoReleaser
Docker configuration publishes version tags, signs the resulting manifests
with keyless Cosign, and uses the same BuildKit provenance behavior as the
server image. The release workflow also generates and attests its Syft SBOM.
The backup image has no moving `latest` channel.

The backup-image job in `.github/workflows/ci.yml` is the pre-tag pin-drift
tripwire: it builds this Dockerfile for `linux/amd64` and `linux/arm64` without
pushing and runs `age --version`, `aws --version`, and `pg_dump --version`
inside the resulting image on each architecture. The install layer is rebuilt
without cache so an older cached layer cannot hide unavailable Alpine package
revisions before a release tag can begin publishing artifacts.
Run `make check-postgres-backup-image` locally
for the same check; `make check` includes it. Local runs print
`skipped: no docker` when a Docker daemon is unavailable, while CI requires
Docker and fails rather than skipping.

To bump the pins, edit `images/witself-postgres-backup/Dockerfile`: choose a
reviewed official PostgreSQL 18 multi-architecture base index digest and exact
package revisions available in that base's Alpine repositories. Record `age`
separately for `x86_64` and `aarch64` in the `case "$(apk --print-arch)"` block,
using `<arch>) age_version=<revision> ;;` for each architecture and an
`unsupported` default branch. Install `"age=$age_version"` and one exact
`aws-cli=<revision>` that is available on both `x86_64` and `aarch64`, with
`apk add --no-cache`. Alpine rebuild revisions may
differ by architecture; the current pins are `age=1.3.2-r1` on both architectures
and `aws-cli=2.34.63-r0`. Keep the base and package pins consistent and update
the Dockerfile's version comments. In the same change, append an entry to
`backupImageToolReleases` in `internal/gitopsvalues/backup_image_tools.go`, with
`FirstRelease` set to the first tag that will contain that distinct tool set.
The ledger is append-only and records both architecture-specific `age`
revisions, one `aws-cli` revision, and the base image. Its package history is:

| First release | age on x86_64 | age on aarch64 | aws-cli |
| --- | --- | --- | --- |
| 0.0.291 | 1.3.1-r6 | 1.3.1-r6 | 2.34.63-r0 |
| 0.0.330 | 1.3.2-r0 | 1.3.2-r0 | 2.34.63-r0 |
| 0.0.336 | 1.3.2-r0 | 1.3.2-r1 | 2.34.63-r0 |
| 0.0.337 | 1.3.2-r1 | 1.3.2-r1 | 2.34.63-r0 |

`TestBackupImageToolLedgerIsOrderedAndMatchesDockerfile` fails until that entry
is added. After tagging, confirm it with
`git show vFIRST:images/witself-postgres-backup/Dockerfile` and the tag before it.
Run `make check-postgres-backup-image`
and validate a two-platform build for `linux/amd64,linux/arm64` before the
reviewed change is released. The two-platform CI tripwire does not replace
the release's provenance and SBOM checks.

Cell activation is separate from publication. An explicit
`scripts/roll-cell.sh CELL VERSION --backup-image` plus the existing rollout
gate resolves both server and backup release manifests before one atomic
generated-values update. The optional backup pin is
`apps.civoPostgres.backup.image.{repository,tag,digest}`; the digest selects the
image while the tag records the release. Ordinary rolls and regeneration
preserve an existing backup pin; repeat `--backup-image` to update it. The
resolver accepts `--repository ghcr.io/witwave-ai/images/witself-postgres-backup`
and verifies registry manifest bytes before returning their index digest.
See **Keeping the backup image pin current** in the
[scheduled-backup runbook](backup-and-recovery.md#scheduled-postgresql-backups)
for the CI tool and release-distance rules and the re-pin and emergency rollback
procedures.

Until an operator opts in, the default empty `backup.image` object selects
`backup.legacyImage` (`postgres:18-alpine3.23`) and the runner still installs
packages at startup. Legacy scalar image values remain supported. A configured
image object requires a repository and tag, with an optional digest; it selects preinstalled
tools and never invokes `apk`. Missing tools fail the Job. See the
[scheduled-backup runbook](backup-and-recovery.md#scheduled-postgresql-backups)
for the first-release activation procedure. Publication alone changes neither
the backup schedule nor any cell's values.

The database PostgreSQL mirror is separate from the backup tool image:

- Image package: `ghcr.io/witwave-ai/images/postgresql`
- Approved digests and upstream source metadata: `images/postgresql/mirror.json`
- Tags: `VERSION-civo-sandbox-use1-serving` and `VERSION-civo-sandbox-use1-backup`

The descriptor records each pin's known original source tag as `latest` and
its `upstream_version` from the pinned image's config labels. The approved
digest determines exact content. Do not infer the PostgreSQL version from the
chart version or resolve today's mutable `latest` to describe either pin.

The `mirror-postgresql` release job checks GHCR for each approved digest first.
When present, it copies from GHCR to create the new release tag; only a digest
not yet mirrored is copied from Docker Hub. Every manifest and platform is
copied with digest preservation required, then the destination digest is
verified against the approved source. It does not rebuild PostgreSQL or alter
the manifest to attach metadata. Signatures, SPDX SBOM attestations, and
GitHub provenance are separate artifacts attached to the unchanged digest.
Each release signs and attests the digest again so a retry can repair an
incomplete publication; the image's presence in GHCR alone does not prove that
an earlier release completed its signatures and attestations.
An existing single-platform manifest stays single-platform: synthesizing a
multi-platform index would change its digest and requires a separate image
change. An existing index retains all its platforms.
Each cell retains its own existing PostgreSQL content; the cells' source
digests differ. Server release publication does not depend on this mirror job:
a mirror failure leaves ordinary server releases available, but prevents
`roll-cell.sh --postgres-image` from selecting an absent or inaccessible mirror
tag. Require both the published server release and successful mirror publication
before activation. Manual snapshot runs publish no mirror. The mirror has no
`latest` channel.

PR-time CI installs skopeo on `ubuntu-24.04` and runs
`make check-postgres-image-mirror`, which copies to temporary local image
directories with digest verification. This exercises the
runner package installation, required copy flags, and initial Docker Hub fetch
before a release tag is created; it does not publish a GHCR image.
The same target runs under `make check-infra`. Locally it reports
`skipped: no skopeo` when Skopeo is absent; CI requires Skopeo and cannot skip.
With the current two-cell descriptor, the tripwire anonymously pulls two
multi-architecture images from Docker Hub per PR. This rate-limit exposure is
accepted; add a Docker Hub login step if anonymous pull limits start failing
the job.

`scripts/roll-cell.sh CELL VERSION --postgres-image` selects that cell's release
tag only after verifying the mirror has the same digest as the cell's existing
pin. Regeneration and ordinary server rolls preserve the selected registry,
repository, and tag; an explicit `--write` propagates overlay digest changes.
See
[the PostgreSQL mirror runbook](backup-and-recovery.md#switch-the-postgresql-database-image-to-ghcr)
for the one-time public-visibility step, backup-first activation, and maintenance
window. The switch restarts PostgreSQL and requires fresh `--backup-evidence`
for both cells; `--no-schema-change` is not permitted for this operation.
Publishing this image changes no cell values or overlays.

Container publishing should use GitHub Packages / GHCR with public visibility.
Initial package paths should be:

- `ghcr.io/witwave-ai/images/witself`
- `ghcr.io/witwave-ai/images/witself-server`
- `ghcr.io/witwave-ai/images/witself-postgres-backup`
- `ghcr.io/witwave-ai/images/postgresql`

## Homebrew

Homebrew distribution must use the Witwave-owned tap repository:

- Repository: `github.com/witwave-ai/homebrew-tap`
- Visibility: public
- Tap name: `witwave-ai/tap`
- Formula paths: `Formula/witself.rb`, `Formula/witself-infra.rb`, and
  `Formula/witself-admin.rb`
- Install commands: `brew install witwave-ai/tap/witself`,
  `brew install witwave-ai/tap/witself-infra`, and
  `brew install witwave-ai/tap/witself-admin`

GoReleaser owns the archives but does not own formula rendering. Its legacy
`brews` pipe is deprecated, while moving unsigned macOS binaries into casks
would introduce Gatekeeper friction. The release workflow therefore:

1. runs the pinned GoReleaser and validates its configuration without
   deprecation warnings;
2. renders formulae from the exact archive paths and SHA-256 digests with
   `go run ./tools/homebrew-formula`;
3. syntax-checks every rendered Ruby file; and
4. uses `scripts/publish-homebrew-formulas.sh` to update all three formulae and
   the `Aliases/ws` symlink in one non-force tap commit. A bounded retry starts
   from the newest tap head after a concurrent push; an older release becomes a
   safe no-op instead of rolling the tap back.

The publisher uses `HOMEBREW_TAP_PUBLISH_TOKEN`. The legacy
`HOMEBREW_TAP_TOKEN` is intentionally unavailable to this repository so a
historical workflow definition cannot write an older formula. This credential
boundary and the default-branch channel guardian are both required parts of the
migration.

Normal CI renders fixture formulae and runs both `brew style` and
`brew audit --strict` in a temporary tap. Before a tag can publish, the release
gate also builds real snapshot archives through GoReleaser and repeats that
render-and-audit path against the actual archive naming contract. Tap promotion
runs after every immutable release artifact and attestation succeeds. The tap
then serves as the monotonic input to the serialized GHCR `latest` reconciler.
The formula-to-cask migration remains blocked on macOS signing and notarization,
tracked in GitHub issues
[`#1`](https://github.com/witwave-ai/witself/issues/1) and
[`#4`](https://github.com/witwave-ai/witself/issues/4).

Homebrew release smoke tests should verify:

- `brew tap witwave-ai/tap`
- `brew install witwave-ai/tap/witself`
- `witself version`
- `brew install witwave-ai/tap/witself-infra`
- `witself-infra help`
- `brew install witwave-ai/tap/witself-admin`
- `witself-admin version`

The default CLI install should stay lean. Operators who provision cells install
`witself-infra` explicitly; most `witself` users should not receive the
infrastructure provisioner or its Pulumi dependency.

## Native Binary Installers

The shell installer installs released binaries for macOS and Linux. The
PowerShell installer installs the native Windows x64 CLI.

Expected invocation:

```sh
curl -fsSL https://raw.githubusercontent.com/witwave-ai/witself/main/install.sh | sh
curl -fsSL https://raw.githubusercontent.com/witwave-ai/witself/main/install.sh | sh -s witself-infra
```

```powershell
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
irm https://raw.githubusercontent.com/witwave-ai/witself/main/install.ps1 | iex
```

Later, this can move to a product-owned domain such as `https://witself.dev` if
that becomes the canonical install surface.

Installer requirements:

- Detect OS and architecture.
- Download the matching GitHub Release artifact.
- Verify SHA256 checksums before installation.
- Inspect the complete archive before extraction and require exactly one regular
  root entry whose name matches the selected release binary.
- Verify signatures when the required signing metadata is available.
- Serialize updates with a destination-scoped install lock.
- Stage and self-test the candidate on the destination filesystem before
  replacing the live command.
- Preserve verified backups until both the installed command and its alias pass
  their post-commit self-tests; roll both paths back on failure and retain
  recovery state if rollback itself cannot complete safely.
- Install to `/usr/local/bin` when writable.
- Fall back to `$HOME/.local/bin` when a system install path is not writable.
- Install `witself` by default and create the `ws` alias beside it.
- Support selecting `witself`, `witself-infra`, `witself-server`, or
  `witself-admin` through the first positional argument, such as
  `sh -s witself-infra`.
- Keep `WITSELF_BINARY` as a compatibility alias for selecting the binary.
- Support selecting a version through `WS_VERSION`, or through the second
  positional argument when a binary is also supplied.
- Print the installed version and next-step PATH guidance.

The Windows installer has a deliberately narrower first slice:

- Require native Windows x64 and the exact
  `witself_<version>_windows_amd64.zip` release asset.
- Verify the matching SHA-256 entry before extraction.
- Install per-user to `%LOCALAPPDATA%\Witself\bin` by default, with no
  elevation or symlink requirement.
- Write both `witself.exe` and a real `ws.exe` alias, update the user PATH
  idempotently unless `-NoPathUpdate` is supplied, serialize pair mutation with
  a per-install-directory transaction lock, and restore the previous pair when
  an installed version self-test fails. If either installed target changes
  concurrently, refuse to overwrite it and retain the prior recovery backup
  instead.
- Support latest-release resolution and an explicit `-Version` when the
  script is downloaded and invoked as a file.
- Print `witself integrations` as the next client-integration discovery
  command.

macOS and Linux CI execute the shell installer against checksummed local release
fixtures. The successful install path uses the exact native tarball and
unmodified checksum set emitted by the CLI-only GoReleaser release pipeline;
negative fixtures cover reinstall/alias repair, foreign-alias and checksum
refusal, unexpected archive-member refusal, non-running staged-candidate
refusal, post-commit rollback, and live-lock contention. Windows x64 CI executes
the PowerShell installer against the exact native GoReleaser ZIP and checksum
set plus equivalent negative fixtures, including reinstall, checksum and
unexpected archive-member refusal, lock contention, and failed-upgrade/
concurrent-change recovery. The same five-cell native matrix
exercises the isolated Codex provider contract plus compiled-fake-CLI
install/reinstall/uninstall and sibling-preservation lifecycles for Claude Code,
Grok Build, Cursor, OpenClaw, Antigravity, and GitHub Copilot. Tag releases
repeat all five native cells; each cell builds the Witself CLI slice
mechanically derived from the main GoReleaser configuration, feeds the exact
native archive and unmodified checksum set through the platform installer, and
runs every applicable provider lifecycle with the resulting installed command.
Cursor's native-Windows lifecycle skips because its Windows support boundary is
WSL. Only Codex crosses the additional MCP stdio tool-invocation boundary in
this matrix. This evidence is contract coverage, not a claim that an
authenticated provider model completed an end-to-end MCP invocation; that
remains a separate real-client acceptance gate.

## Release Readiness Checklist

1. Start from a clean, green `main` commit and choose an unused semantic
   version.
2. Push `v${VERSION}` and wait for the tag-triggered `release` workflow to
   succeed.
3. Verify the GitHub release, CLI/server GHCR manifests, OCI Helm chart, and
   Homebrew formula updates all carry `${VERSION}` and the tagged commit.
4. Exercise the published CLI and server binaries before changing GitOps.
   For client-integration releases, also run `witself integrations --verify`
   in disposable provider profiles and distinguish that credential-free
   topology check from any separately recorded real-client or model acceptance.
5. Roll one intended canary cell by passing `${VERSION}` and either the
   verified backup-evidence directories or an explicit `--no-schema-change`
   attestation to `scripts/roll-cell.sh`; review, commit, and push only the
   two application version pins.
6. Verify Argo health, replacement-pod readiness, `/v1/version`, and the
   release-specific feature smoke in that provisioned cell.
7. Repeat the GitOps change and verification for each intended rollout wave.
8. Upgrade and, when managed client policy changed, reinstall the supported
   client integrations before declaring an end-to-end client feature
   operationally complete.
