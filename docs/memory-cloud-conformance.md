# Memory Cloud Conformance

Status: executable certification gate. The harness is implemented; AWS, GCP,
and Azure become certified only after this gate passes against the actual
managed PostgreSQL endpoints for a specific release. A local pass proves the
harness, not the identity of a cloud endpoint.

## Scope

`TestNarrativeMemoryManagedCloudConformance` runs one provider-neutral contract
across this directed matrix:

| Source | Destinations |
|---|---|
| AWS | AWS, GCP, Azure |
| GCP | AWS, GCP, Azure |
| Azure | AWS, GCP, Azure |

Every case creates a fresh schema on both endpoints, applies every migration,
builds a suspended source account, streams the logical account archive to a
separately migrated destination schema, resumes the imported account, verifies
it, and drops both schemas during test cleanup. It never provisions or destroys
cloud infrastructure and never calls an AI or embedding service.

The contract verifies:

- transcript-backed narrative capture, adjustment, history, forget, atomic
  one-to-many supersession, permanent-deletion tombstones, and retry shields;
- exact semantic-fact value/history portability and mutation idempotency;
- sensitive-capable lexical recall with identical source/destination results;
- migration-0032 immutable vector profile/row portability plus identical
  full-coverage hybrid recall and coverage metadata after import;
- agent and account isolation before and after import;
- applied curation audit state plus interruption of an in-flight source lease
  and advancement of the destination fencing generation;
- one-snapshot export from a suspended account, archive checksums, canonical
  row counts, and idempotent mutation retries after import; and
- exclusion of generated search documents and retrieval indexes from the
  archive, followed by proof that PostgreSQL generated columns and lexical
  indexes work on the destination.

Schema-32 archives always include the vector-profile and vector table streams,
even when empty. Existing client-authored rows participate in the conformance
fixture, while lexical recall remains the required zero-coverage baseline. The
backend performs no vector generation or other model inference.

## Required Endpoints

Every run supplies three PostgreSQL DSNs:

```text
WITSELF_MEMORY_AWS_DATABASE_URL
WITSELF_MEMORY_GCP_DATABASE_URL
WITSELF_MEMORY_AZURE_DATABASE_URL
```

Rehearsal mode is the default. Provider certification additionally sets
`WITSELF_MEMORY_CLOUD_CERTIFY=1` and supplies three protected canonical cloud
resource attestations:

```text
WITSELF_MEMORY_AWS_RESOURCE_ID
WITSELF_MEMORY_GCP_RESOURCE_ID
WITSELF_MEMORY_AZURE_RESOURCE_ID
```

Certification requires distinct configured host/port pairs and distinct
resource attestations. When permitted, it also rejects duplicate PostgreSQL
system identifiers. Live private address/port collisions are recorded but are
not a hard failure because isolated cloud networks can reuse RFC1918 addresses.
The gate logs only per-run salted endpoint fingerprints and PostgreSQL versions;
it never logs DSNs, users, passwords, hosts, database names, provider options,
or resource ids. Resource ids are runtime-only operator attestations used to
reject an aliased matrix; they are not retained in the downloadable artifact.
The fingerprint salt is intentionally discarded, so fingerprints are only
opaque run-local correlators for provider entries that already passed the
preflight's endpoint, resource-attestation, and system-identifier alias guards;
they are not independent proof of distinct resources and cannot correlate
resources across runs. If durable resource-identity audit is required, retain a separate
protected immutable mapping from workflow run URL and commit SHA to the three
resource ids; the current public artifact is deliberately insufficient for
that purpose.

In certification mode, any failure inside a directed archive round-trip is
reported only as the fixed provider pair and `archive round-trip failed; details
suppressed`. This boundary also covers schema setup, migrations, store calls,
assertions, and cleanup because native PostgreSQL connection errors can contain
user, database, host, and port metadata. Diagnose a failed protected run from
the managed-database and trusted-runner logs, or reproduce it in rehearsal mode;
do not weaken the certification reporter to expose the raw client error.

The database principals must be able to connect, create schemas, create all
objects used by the current migration set, and drop the schemas they create.
Use dedicated conformance databases or principals. The test schemas are named
`witself_migration_<pid>_<sequence>`; an abruptly killed process can leave one
behind for an operator to inspect and remove.

Keep DSNs in a secret store or the protected
`memory-cloud-conformance` GitHub environment. Do not pass them as command-line
arguments. Provider TLS, private networking, and any secure connector remain
deployment controls; this test deliberately does not guess provider identity
or transport security from a hostname.

## Run Locally Or From A Trusted Runner

After exporting all three DSNs into the trusted parent environment:

```sh
make test-memory-cloud-conformance
```

The manual `memory cloud conformance` workflow runs the same command in the
dedicated `witself-memory-conformance` runner group on a self-hosted Linux
runner labeled `witself-memory-conformance-ephemeral`. The runner needs private
network reachability to all three databases. Register it for one job only and
reimage or destroy it after completion; never allow this group or label to run
pull-request, fork, or other untrusted workflows. Restrict the runner group to
this workflow in repository/organization settings. Configure the three DSNs
and three resource attestations as protected environment secrets. The workflow
sets certification mode and retains the workflow URL, release tag, source
commit SHA, salted endpoint fingerprints, provider database versions, and
successful 3-by-3 subtest log as the sanitized certification record. This
database harness tests source code from the release tag; it does not exercise a
deployed `witself-server` binary or attest its runtime version.

The `memory-cloud-conformance` environment is a security boundary, not merely a
secret namespace. Require an environment reviewer and deployment-tag rule that
allows only exact semantic-version tags. Protect matching `v*` tags with a
repository ruleset that limits tag creation, update, and deletion to the trusted
release path; an ordinary repository writer must not be able to mint a tag that
can consume these credentials. The workflow validates the exact tag shape in a
credential-free job and again before any endpoint secret is referenced, but
those checks do not replace protected immutable tags and human/environment
approval of the release tag and commit SHA.

Every third-party action in the secret-bearing job is pinned to a reviewed full
commit SHA, with its major release retained only as a comment for automated
update review. Do not replace those pins with mutable major tags.

The workflow also uploads a 90-day `memory-cloud-conformance-<run-id>` artifact.
Its versioned JSON manifest contains only the release tag, commit, workflow URL,
timestamp, pass/fail outcome, per-provider salted fingerprint/PostgreSQL
`server_version_num`, and the nine directed outcomes. The companion
certification-mode test
log uses the same redacted reporter. Neither file contains a DSN, host, port,
database/user name, password, raw resource attestation, account/realm/agent id,
or memory/fact value. The manifest is written with mode `0600` on the trusted
runner before upload. GitHub artifact packaging and extraction do not preserve
that local Unix mode, so downloaded-artifact permissions are not a security
boundary. An incomplete or failed matrix can produce only a failed artifact,
never a passing one.

For a harmless harness rehearsal, all three DSN variables may point at one or
more disposable local PostgreSQL databases; leave certification mode unset.
Such a run validates matrix mechanics but must be labeled `local rehearsal`,
never `AWS/GCP/Azure certified`.

## Recorded Individual-Cloud Rehearsals

On 2026-07-17, temporary TTL-scoped Kubernetes Jobs in each live sandbox ran
`TestNarrativeMemoryArchiveCellMovePostgres` from exact release `v0.0.172`,
commit `67ec81d3f5485f1865f87e265ae9f33fa15c6988`, against that cell's managed
PostgreSQL service:

| Cell | Managed PostgreSQL | Result | Test duration |
|---|---:|---:|---:|
| AWS RDS | 18.3 | pass | 2.24 s |
| GCP Cloud SQL | 18.4 | pass | 5.25 s |
| Azure Flexible Server | 18.4 | pass | 3.43 s |

Each rehearsal applied the migrations and proved an isolated archive/import/
recall round trip within one provider. No secret value was logged. These three
passes establish individual-provider compatibility only: they do not exercise
any directed cross-cloud pair, do not produce the protected nine-case evidence
artifact, and do not close the 3-by-3 certification gate.

## Runtime Boundary

This is the backend and account-move gate. Runtime capture/recall conformance is
separate and currently covers Codex, Claude Code, Grok Build, and Cursor with
capability-accurate fallbacks. Gemini and GitHub Copilot are intentionally not
part of the current release gate.

The managed-cloud certification is tracked by
[issue #44](https://github.com/witwave-ai/witself/issues/44) under the canonical
[narrative-memory production-readiness checklist](narrative-memory-and-curation.md#production-readiness-checklist).
Related infrastructure issues #35 and #36 provide endpoint and network
prerequisites; this gate consumes reachable managed PostgreSQL endpoints and
does not provision or certify the broader cloud infrastructure.
