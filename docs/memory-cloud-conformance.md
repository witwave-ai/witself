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
or resource ids. Resource ids are operator attestations retained with the
release record, not cryptographic proof inferred from a hostname.

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

The manual `memory cloud conformance` workflow runs the same command on a
self-hosted Linux runner labeled `witself-memory-conformance`. The runner needs
private network reachability to all three databases. Configure the three DSNs
and three resource attestations as protected environment secrets. The workflow
sets certification mode and retains the workflow URL, commit SHA, released
server version, attested resource ids, salted endpoint fingerprints, provider
database versions, and successful 3-by-3 subtest log as the certification
record.

For a harmless harness rehearsal, all three DSN variables may point at one or
more disposable local PostgreSQL databases; leave certification mode unset.
Such a run validates matrix mechanics but must be labeled `local rehearsal`,
never `AWS/GCP/Azure certified`.

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
