# Witself Release And Build Notes

Status: implemented release path with additional hardening targets. The current
automation is defined by `.github/workflows/release.yml`, `.goreleaser.yaml`,
the Homebrew renderer and publisher, the latest-image reconciler, the Helm
chart, and `install.sh`; those executable sources win if this document drifts.

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
Pulumi-module, and vulnerability gates before publishing. GoReleaser then
publishes the macOS/Linux archives, checksum Sigstore bundle and transitional
detached-signature compatibility assets,
archive SBOMs, GitHub release, multi-architecture CLI and server images,
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

The current Helm chart does not render a migration Job. With a database DSN,
`witself-server serve` applies its embedded Goose migrations before opening the
service; a migration failure prevents that process from serving. A rollout is
therefore not complete until the replacement pods are Ready and report the
expected build from `/v1/version`. This startup behavior must be considered
before any future explicit migration Job is introduced.

## Goals

- Build Witself as a Go project with one shared core, one public CLI/MCP
  binary, and one public backend API server binary.
- Treat v0 as a usable cloud-shaped slice with CLI, MCP stdio, `witself-server`,
  PostgreSQL-backed deterministic lexical memory, images, a Helm chart, the
  Pulumi-based cell provisioner, CI, and release automation.
- Start from the scaffold boundary in [docs/scaffold-readiness.md](scaffold-readiness.md).
- Keep the project on the latest stable Go release that is practical at the time
  implementation or release work happens.
- Ship the `ws` binary with CLI commands and `witself mcp serve`.
- Ship a separate `witself-server` binary from the same public repository once
  backend implementation starts.
- Support Homebrew and universal `curl | sh` installation from the beginning.
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

Witself should use the latest stable Go release. As of July 10, 2026, the
current stable Go release is `go1.26.5`.

Initial module settings when code starts:

```text
module github.com/witwave-ai/witself

go 1.26

toolchain go1.26.5
```

Refresh this baseline before first implementation and before each release. If a
new stable Go release exists, update the toolchain baseline and rerun the full
test and release smoke path before publishing.

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

The initial module bootstrap, once the first Go package exists, should look like:

```sh
go mod init github.com/witwave-ai/witself
go mod edit -go=1.26
go mod edit -toolchain=go1.26.5
go mod tidy
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

Use concurrency cancellation so superseded pushes do not leave stale CI running.

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
- Building release archives for macOS and Linux.
- Building `witself`, `witself-server`, `witself-admin`, and `witself-infra`.
- Generating SHA256 checksums.
- Signing the checksum manifest into a keyless Sigstore bundle, while retaining
  the detached `.sig` and `.pem` assets required by older updaters.
- Generating archive SBOMs and container SBOM attestations.
- Publishing build-provenance attestations for archives and the chart.
- Publishing public GitHub Release assets.
- Publishing the public GHCR CLI/MCP image at
  `ghcr.io/witwave-ai/images/witself`.
- Publishing the public GHCR backend image at
  `ghcr.io/witwave-ai/images/witself-server`.
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

Broader published-artifact installation smoke tests remain release-hardening
targets; they are not silently implied by the current workflow.

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
`witself-admin`, and the Pulumi-based cell provisioner `witself-infra`.

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

Current release artifacts include:

- Separate compressed archives for `witself`, `witself-server`,
  `witself-admin`, and `witself-infra` on each target platform.
- SHA256 checksums.
- A keyless `checksums.txt.sigstore.json` bundle for the checksum manifest,
  plus transitional `checksums.txt.sig` and `checksums.txt.pem` compatibility
  assets for older `witself-admin` updaters.
- Per-archive SBOMs.
- Build-provenance attestations for release archives.

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
- Primary workload: `witself-server`

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
  KMS is optional and demoted: field-level encryption of `sensitive` facts is a
  capability, not a default chart dependency.
- Raw database passwords, KMS credentials, provider secrets, tokens,
  passphrases, private keys, and wallet credentials must not be placed directly
  in default values.
- Values should support existing Kubernetes Secret references and
  deployment-native identity such as service account annotations.
- The chart includes Deployment, Service, ServiceAccount, ConfigMap, optional
  Ingress, and optional NetworkPolicy templates. It does not currently render a
  migration Job.
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
- Entrypoint: `witself-server`

Example:

```sh
VERSION="${VERSION:?set VERSION to a published version without the v prefix}"
docker run --rm "ghcr.io/witwave-ai/images/witself-server:${VERSION}" version
docker run --rm -p 8080:8080 -p 8081:8081 -p 9090:9090 \
  "ghcr.io/witwave-ai/images/witself-server:${VERSION}" serve
```

Implemented image behavior:

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

Container publishing should use GitHub Packages / GHCR with public visibility.
Initial package paths should be:

- `ghcr.io/witwave-ai/images/witself`
- `ghcr.io/witwave-ai/images/witself-server`

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
`witself-infra` explicitly; most `ws` users should not receive the infrastructure
provisioner or its Pulumi dependency.

## Universal Installer

The universal installer should install released binaries for macOS and Linux.

Expected invocation:

```sh
curl -fsSL https://raw.githubusercontent.com/witwave-ai/witself/main/install.sh | sh
curl -fsSL https://raw.githubusercontent.com/witwave-ai/witself/main/install.sh | sh -s witself-infra
```

Later, this can move to a product-owned domain such as `https://witself.dev` if
that becomes the canonical install surface.

Installer requirements:

- Detect OS and architecture.
- Download the matching GitHub Release artifact.
- Verify SHA256 checksums before installation.
- Verify signatures when the required signing metadata is available.
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

## Release Readiness Checklist

1. Start from a clean, green `main` commit and choose an unused semantic
   version.
2. Push `v${VERSION}` and wait for the tag-triggered `release` workflow to
   succeed.
3. Verify the GitHub release, CLI/server GHCR manifests, OCI Helm chart, and
   Homebrew formula updates all carry `${VERSION}` and the tagged commit.
4. Exercise the published CLI and server binaries before changing GitOps.
5. Roll one intended canary cell by passing `${VERSION}` to
   `scripts/roll-cell.sh`; review, commit, and push only the two application
   version pins.
6. Verify Argo health, replacement-pod readiness, `/v1/version`, and the
   release-specific feature smoke in that provisioned cell.
7. Repeat the GitOps change and verification for each intended rollout wave.
8. Upgrade and, when managed client policy changed, reinstall the supported
   client integrations before declaring an end-to-end client feature
   operationally complete.
