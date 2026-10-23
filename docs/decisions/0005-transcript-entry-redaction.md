# ADR 0005: Owner-Authorized Transcript Entry Redaction

Status: accepted (2026-10-23). This amends the transcript immutability statement
in [ADR 0002](0002-client-side-narrative-memory.md).

## Context

Visible transcript entries can accidentally capture credentials or private
content. Removing the conversation would break sequence-based evidence and
discard unrelated interaction history. The owner needs a bounded way to remove
entry content while retaining transcript continuity and making the limits of
that removal explicit.

## Decision

The owner selects one transcript and an inclusive range of 1-100 entry sequences.
Preview returns value-free descriptors and impact counts. Apply requires the
preview's exact redaction-set revision and a fresh idempotency key. The CLI
requires `--yes`. Selection never accepts text, patterns, or the secret itself.

### Entry shape and continuity

Redaction replaces the live row's body with
`entry content redacted at the account owner's request`, replaces its payload
with the marker below, and empties its artifacts to `[]`. The optional `kind`
survives only when it belongs to the closed capture-kind vocabulary. No other
payload key survives.

```json
{
  "kind": "tool.result",
  "witself_redaction": {
    "schema": "witself.transcript-redaction.v1",
    "receipt_id": "trd_0123456789abcdef0123456789abcdef",
    "redacted_at": "2026-10-22T18:04:05.123456Z",
    "reason_code": "owner_request",
    "actor_kind": "operator"
  }
}
```

The server owns the reason and records a UTC timestamp at microsecond precision.
The entry's id, account, transcript, realm, recorder, sequence, external id,
role, model, reply link, and creation time stay unchanged. The conversation's
sequence and update time stay unchanged. Evidence references remain valid.
Redaction writes a value-free audit event and no usage event; retained storage
accounting follows the row update without rewriting historical usage.

### Guarded retries

The redaction-set revision is a SHA-256 of length-prefixed transcript id, range,
and each selected entry's id, sequence, and redacted state, under the
`witself.transcript-redact.set.v1` domain. It contains no entry content. Impact
counts are informational and do not change the revision.

The receipt id is `trd_` followed by the first 32 lowercase hexadecimal
characters of a SHA-256 over length-prefixed fields: the
`witself.transcript-redact.receipt.v1` domain, account id, transcript id, range,
expected revision, and the **trimmed** idempotency key. The raw key is never
stored. Only the receipt id is stored in each newly redacted entry's marker;
there is no separate retry-key record.

An exact retry with the same transcript, range, revision, and trimmed key replays
when its receipt is present and every selected entry is redacted. Reusing the
old revision with a different key returns 409. A fresh preview of an entirely
redacted range followed by apply returns 410. Key reuse for a different range is
not detected, so callers must choose a fresh key for each logical redaction.

### Owner authority and curation

Only the account owner's operator token (`root` or `account_owner` role) may
preview or apply on any transcript in its account. Other operators and agent
tokens receive 403 before transcript lookup. Unknown and foreign-account
transcripts return 404. Active and suspended accounts may redact; closing
revokes every credential, so a closed account cannot redact. An account move
returns 409.

The owner uses `witself transcript redact`. There is no MCP redaction tool,
direct-user authorization header, capture sealing, or runtime instruction
change for this operation. A process that has access to the owner's credential
can use it; the credential, rather than a claim about the caller, controls
authority.

Apply cancels every open or planned memory-curation run in the account whose
transcript inputs or transcript cursor overlap the selected range, in the same
transaction.
Preview reports `curation_runs_to_cancel`; apply reports
`cancelled_curation_run_count`. Applied runs remain untouched. A redacted entry
cannot become new fact evidence through `propose_from_transcript`.

## Consequences

Redaction is irreversible in the live ledger. Stable entry ids and sequences
keep paging, reply links, and evidence references usable. Retries of the original
capture return the marker when role, model, and reply target match, without
restoring the submitted content. Export and import preserve the marker. This
requires redaction-tolerant readers and append retry handling on every receiving
cell before the redaction route ships.

## Residual copies

Redaction changes only the live entry rows. Copies made before the redaction committed keep the original content until their own retention removes them:

- per-account archive snapshots in R2 (the initial lifecycle for the `accounts/` prefix is 90 days);
- cell-wide PostgreSQL dumps and pre-migration dumps, on their own retention;
- exports and evacuation archives already written or downloaded, including one whose snapshot began before the redaction committed;
- PostgreSQL's own dead row versions and write-ahead log, until vacuum and log recycling reuse that space.

Restoring or importing one of those copies brings the original content back without the redaction or its audit event, so redact again after any such restore. Redaction also does not reach copies held outside the entry: memories, memory evidence excerpts, facts and fact candidates written from the entry, curation plans, a client's local capture outbox, runtime-native session files, and anything already sent to a model provider.

Redaction does not revoke or rotate a leaked credential; rotate it separately.
Cell-wide PostgreSQL dumps and pre-migration dumps have no automatic retention; they remain until an operator deletes them.

The operational contract is in the Redaction section of
[the transcript ledger](../transcript-ledger.md#redaction).

## Follow-ups

- Update legal text in a separate slice.
- Add an out-of-cell receipt log for reapplying redactions after a restore.
- Add a dashboard redaction badge.
- Add local `witself transcript redact --pending`.
- Add threat-model entries for credential misuse and recapture.
