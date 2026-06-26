# Witself Release And Build Notes

Status: draft. This document captures implementation, module, and distribution
requirements before code exists.

## Goals

- Build Witself as a Go project with one shared core, one public CLI/MCP
  binary, and one public backend API server binary.
- Treat v0 as a usable cloud-shaped slice with CLI, MCP stdio, `witself-server
  serve --dev`, local development storage, Postgres with pgvector, an embedding
  provider, images, Helm skeleton, Terraform AWS skeleton, CI, and release
  automation.
- Start from the scaffold boundary in [docs/scaffold-readiness.md](scaffold-readiness.md).
- Keep the project on the latest stable Go release that is practical at the time
  implementation or release work happens.
- Ship the `witself` binary with CLI commands and `witself mcp serve`.
- Ship a separate `witself-server` binary from the same public repository once
  backend implementation starts.
- Support Homebrew and universal `curl | sh` installation from the beginning.
- Make release artifacts verifiable with checksums.
- Sign release archives, checksum manifests, and container images from the first
  release.
- Publish SBOMs and build provenance for release archives and container images
  from the first release.
- Keep the source repository, release artifacts, and packages public.
- Publish public container images from image definitions under `images/*`.
- Publish a public Helm chart for self-hosted Kubernetes deployments once
  `witself-server` exists.
- Treat Prometheus metrics, Kubernetes health probes, and chart-level
  observability support as release-gated server functionality.
- Include public Terraform modules and example stacks for AWS, GCP, and Azure
  under `infra/terraform`.
- Make managed and self-hosted backend deployment mechanics visible and
  reviewable from the public repo.
- Add a release GitHub Action immediately, even before the first product
  release, so release mechanics are visible and reviewable from day one.

## Go Baseline

Witself should use the latest stable Go release. As of June 26, 2026, the
current stable Go release is `go1.26.4`.

Initial module settings when code starts:

```text
module github.com/witwave-ai/witself

go 1.26

toolchain go1.26.4
```

Refresh this baseline before first implementation and before each release. If a
new stable Go release exists, update the toolchain baseline and rerun the full
test and release smoke path before publishing.

## Go Module Policy

- Use Go modules only.
- Start with one module at `github.com/witwave-ai/witself`.
- Keep CLI, MCP, backend API server, storage adapters, the embedding-provider
  abstraction, and shared core packages in the same module unless a real release
  boundary appears later.
- Commit `go.mod` and `go.sum`.
- Run `go mod tidy` after dependency changes.
- Run `go mod verify` in CI.
- Avoid vendoring dependencies by default.
- Keep dependencies current deliberately through reviewable updates.

The initial module bootstrap, once the first Go package exists, should look like:

```sh
go mod init github.com/witwave-ai/witself
go mod edit -go=1.26
go mod edit -toolchain=go1.26.4
go mod tidy
```

## Expected Checks

CI should be strong enough that regressions are caught before release tags.
Checks should run on pull requests and pushes to `main`; release-specific checks
should also run on version tags.

Required checks:

- Root docs checks for `README.md`, `SECURITY.md`, `CONTRIBUTING.md`, and the
  Apache-2.0 `LICENSE`.
- `gofmt` cleanliness.
- `go build ./...`
- `go test ./...`
- `go test -race ./...` on Linux.
- `go vet ./...`
- `go mod tidy` cleanliness check.
- `go mod verify`
- Backend API route, auth, policy, audit, storage-adapter, embedding-provider,
  and migration tests once those packages exist.
- Goose migration ordering and apply tests against Postgres, including pgvector
  extension and vector-column migrations, where practical.
- Server config validation tests with redacted error output.
- Server health endpoint tests for liveness, readiness, and startup behavior.
- Prometheus metrics registration, route-template labeling, and redaction tests.
- Server smoke test that `/metrics` returns Prometheus text format once the API
  server exists.
- Server smoke tests that API, health, and metrics listeners bind separately and
  that metrics can be disabled.
- Embedding-provider abstraction tests, including the `local-dev` provider for
  offline semantic recall and deterministic degradation to keyword/tag/kind/time
  ranking when no provider is available.
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
- Terraform formatting with `terraform fmt -check -recursive infra/terraform`.
- Terraform validation for modules and example stacks where practical.
- Terraform linting with `tflint`.
- Terraform static security checks with `checkov` or an equivalent tool.
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

The repository should include a release workflow as soon as the repository is
initialized, before the first product release.

Workflow:

- Path: `.github/workflows/release.yml`
- Tag trigger: `v*.*.*`
- Manual trigger: `workflow_dispatch`
- Primary release tool: GoReleaser.

The release action should own:

- Verifying the Go toolchain from `go.mod`.
- Running the required Go, module, docs, shell, and release-config checks.
- Building release archives for macOS and Linux.
- Building both `witself` and `witself-server` release archives once the server
  exists.
- Generating SHA256 checksums.
- Signing release archives and checksum manifests.
- Generating SBOMs for release archives and container images.
- Publishing build provenance or equivalent attestations.
- Publishing public GitHub Release assets.
- Publishing the public GHCR CLI/MCP image at
  `ghcr.io/witwave-ai/images/witself`.
- Publishing the public GHCR backend image at
  `ghcr.io/witwave-ai/images/witself-server` once the server exists.
- Publishing the public Helm chart at `ghcr.io/witwave-ai/charts/witself` once
  the server exists.
- Signing the published GHCR images.
- Signing or provenance-attesting the published Helm chart.
- Updating the public Homebrew tap formula in `witwave-ai/homebrew-tap`.
- Running Homebrew, curl installer, CLI/MCP image, backend image, and Helm chart
  smoke tests.

Required workflow permissions:

- `contents: write` for creating or updating GitHub Releases.
- `packages: write` for publishing GHCR packages.
- `id-token: write` for keyless signing and provenance, or an equivalent
  explicitly managed signing credential if keyless signing is not used.

Homebrew tap handling:

- The workflow must verify that `github.com/witwave-ai/homebrew-tap` exists
  before publishing the formula.
- If the tap repository does not exist, the workflow should create it as a
  public repository when configured with an org-level release token that has
  permission to create public repositories.
- If the workflow is not configured with that permission, it should fail with a
  clear setup error that names `witwave-ai/homebrew-tap` and the missing token
  requirement.

Manual `workflow_dispatch` should support a dry-run or snapshot path that builds
artifacts and runs smoke tests without publishing a stable release.

## Release Artifacts

The first primary release artifact is the `witself` binary. Once backend work
starts, releases should also include the public, separate `witself-server`
binary.

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

Release artifacts should include:

- Compressed archives for each target platform.
- Both `witself` and `witself-server` binaries once the server exists.
- SHA256 checksums.
- Signatures for archives and checksum manifests.
- SBOMs.
- Build provenance or equivalent attestations.
- Machine-readable release metadata where practical.
- Shell completions where practical.

## Helm Chart

Witself should publish a public Helm chart as the first self-hosting deployment
artifact once `witself-server` exists.

Initial chart:

- Chart path: `charts/witself`
- OCI package: `ghcr.io/witwave-ai/charts/witself`
- Primary workload: `witself-server`

Expected install shape:

```sh
helm install witself oci://ghcr.io/witwave-ai/charts/witself \
  --version 0.1.0 \
  --namespace witself \
  --create-namespace \
  --values ./witself-values.yaml
```

Chart requirements:

- Production values should assume external Postgres with the pgvector extension,
  a configured embedding provider, and optional external object/blob storage.
  KMS is optional and demoted: field-level encryption of `sensitive` facts is a
  capability, not a default chart dependency.
- Raw database passwords, embedding-provider credentials, KMS credentials,
  provider secrets, tokens, passphrases, private keys, and wallet credentials
  must not be placed directly in default values.
- Values should support existing Kubernetes Secret references and
  deployment-native identity such as service account annotations.
- The chart should include Deployment, Service, ServiceAccount, ConfigMap,
  optional Ingress, optional NetworkPolicy, and migration Job templates.
- The chart should include liveness, readiness, startup, metrics,
  ServiceMonitor, PodMonitor, resource, autoscaling, disruption-budget,
  security-context, and network-policy support where practical.
- Migration jobs should be explicit and opt-in for production upgrades, and
  should cover the pgvector extension and vector-column migrations.
- Chart releases should include rendered-template smoke tests, schema
  validation, signing or provenance attestation, and public publication.

## API And Capability Contract

Release checks should validate that the CLI, MCP tools, and `witself-server`
agree on the public JSON and API contracts.

Required release checks once the API exists:

- OpenAPI generation or validation for `/v1`.
- Example request/response validation for the memory, fact, policy, group, and
  message resources and their colon-action subroutes (for example
  `/v1/messages/{message_id}:ack`).
- Health endpoint smoke tests for `/v1/health/live`, `/v1/health/ready`, and
  `/v1/health/startup`.
- Prometheus scrape smoke test for `/metrics`.
- Metrics-disabled smoke test proving the metrics listener and monitor resources
  are absent.
- `/v1/capabilities` smoke test for managed, self-hosted, and local development
  profiles where available.
- Capability-surface checks that `/v1/capabilities` reports the state of the
  memory, fact, policy, group, and message subsystems, and the active embedding
  provider, model, and vector dimensionality, including the degraded-recall
  state when no embedding provider is available.
- CLI `witself capabilities --json` smoke test.
- MCP `witself.capabilities` schema check.
- Deterministic `unsupported_operation` checks for unavailable backend
  features, including capability-gated embeddings, cross-agent access policy,
  group-scoped shared identity data, messaging, and billing.
- `witself://` reference parse/resolve smoke tests for memory, fact, agent, and
  group reference forms.

## Terraform Infrastructure

Witself should keep public Terraform in `infra/terraform` for AWS, GCP, and
Azure self-hosted and managed-cloud substrate definitions.

Initial layout:

```text
infra/terraform/
  modules/
    aws/
    gcp/
    azure/
  stacks/
    self-hosted/
      aws/
      gcp/
      azure/
    witself-cloud/
      aws/
      gcp/
      azure/
```

Terraform modules are versioned with the repository release tags. Initial
consumption can use Git sources pinned to a tag:

```hcl
module "witself_aws" {
  source = "git::https://github.com/witwave-ai/witself.git//infra/terraform/modules/aws?ref=v0.1.0"
}
```

Modules should provision Postgres with the pgvector extension, object/blob
storage, Kubernetes, workload identity, and networking where practical. KMS
provisioning is optional and only required when field-level encryption of
`sensitive` facts is enabled.

Release checks should validate Terraform formatting, module validation, linting,
static security checks, and secret scanning. Real state files, backend
credentials, production `.tfvars`, customer identifiers, cloud credentials, and
secrets must not be release artifacts.

## Container Images

Witself should publish public Docker images from Dockerfiles under `images/*`.
The first CLI/MCP image should be:

- Dockerfile: `images/witself/Dockerfile`
- Image package: `ghcr.io/witwave-ai/images/witself`
- Platforms: `linux/amd64`, `linux/arm64`

The CLI/MCP image should run the same `witself` binary published in release
archives. The default entrypoint should be `witself`, allowing both ordinary CLI
usage and MCP usage:

```sh
docker run --rm ghcr.io/witwave-ai/images/witself:latest version
docker run --rm -i ghcr.io/witwave-ai/images/witself:latest mcp serve
```

Once the backend API server exists, the backend image should be:

- Dockerfile: `images/witself-server/Dockerfile`
- Image package: `ghcr.io/witwave-ai/images/witself-server`
- Platforms: `linux/amd64`, `linux/arm64`
- Entrypoint: `witself-server`

Example:

```sh
docker run --rm ghcr.io/witwave-ai/images/witself-server:latest version
docker run --rm -p 8080:8080 -p 8081:8081 -p 9090:9090 \
  ghcr.io/witwave-ai/images/witself-server:latest serve
```

Image requirements:

- Build from public base images.
- Run as a non-root user where practical.
- Include version, commit, and build date metadata labels.
- Publish immutable version tags such as `v0.1.0` and a moving `latest` tag.
- Support `linux/amd64` and `linux/arm64`.
- Avoid embedding tokens, passphrases, store files, identity exports,
  embedding-provider credentials, or test fixtures.
- Smoke test `witself version` in the CLI/MCP image before publishing.
- Smoke test `witself-server version` and `witself-server healthcheck` in the
  backend image before publishing.
- Sign published images.
- Publish image SBOMs.
- Publish image build provenance or equivalent attestations.

Container publishing should use GitHub Packages / GHCR with public visibility.
Initial package paths should be:

- `ghcr.io/witwave-ai/images/witself`
- `ghcr.io/witwave-ai/images/witself-server` once the server exists

## Homebrew

Homebrew distribution must use the Witwave-owned tap repository:

- Repository: `github.com/witwave-ai/homebrew-tap`
- Visibility: public
- Tap name: `witwave-ai/tap`
- Formula path: `Formula/witself.rb`
- Install command: `brew install witwave-ai/tap/witself`

Before the first Homebrew release, release automation should check whether
`witwave-ai/homebrew-tap` exists. If it does not exist, create it under the
`witwave-ai` organization before publishing the formula.

Early release automation may commit directly to the tap. Once branch protection
or review policy exists, the release process should open a pull request to the
tap instead.

Homebrew release smoke tests should verify:

- `brew tap witwave-ai/tap`
- `brew install witwave-ai/tap/witself`
- `witself version`
- `witself completion --help`

The Homebrew formula may install `witself-server` once the backend server is a
public release artifact. CLI installation should remain simple even if the
server binary is present.

## Universal Installer

The universal installer should install released binaries for macOS and Linux.

Expected invocation:

```sh
curl -fsSL https://raw.githubusercontent.com/witwave-ai/witself/main/install.sh | sh
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
- Support selecting a version through an environment variable such as
  `WITSELF_VERSION`.
- Print the installed version and next-step PATH guidance.

## First Release Flow

1. Confirm the Go baseline is still the latest stable release.
2. Add or verify `.github/workflows/release.yml`.
3. Run Go tests, vet, module checks, and docs checks.
4. Run Dockerfile lint and image build smoke tests.
5. Create or verify public `github.com/witwave-ai/homebrew-tap`.
6. Build release artifacts for macOS and Linux.
7. Generate checksums, signatures, SBOMs, and provenance.
8. Publish public GitHub Release artifacts, checksums, signatures, SBOMs, and
   provenance.
9. Publish the public signed CLI/MCP GHCR image at
   `ghcr.io/witwave-ai/images/witself`.
10. Publish the public signed backend GHCR image at
    `ghcr.io/witwave-ai/images/witself-server` once the server exists.
11. Publish the public Helm chart at `ghcr.io/witwave-ai/charts/witself` once
    the server exists.
12. Publish or update the Homebrew formula in `witwave-ai/homebrew-tap`.
13. Smoke test Homebrew installation.
14. Smoke test the universal installer, including checksum and signature
    verification where available.
15. Smoke test the published CLI/MCP Docker image.
16. Smoke test the published backend Docker image once the server exists.
17. Smoke test the published Helm chart with `helm show chart`, `helm template`,
    and a representative install or dry-run path.
18. Smoke test health and metrics behavior for the backend image and Helm chart
    when those artifacts exist.
19. Verify Terraform modules and example stacks are formatted, validated,
    linted, and secret-scanned.
20. Verify `witself version` and `witself-server version` report the expected
    version and build metadata.
