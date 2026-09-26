# Transcript release: parked reference and re-implementation design

Status: design only; the reference is not approved for merge or release.
This document distinguishes observed reference behavior from proposed requirements.
It does not certify the reference, current main, or any installed runtime.

## Evidence and notation

`P` means the supplied local artifact
`~/.witself/handoff/tasks-wave1-2026-09-19/transcript-release-reference.patch`.
Every `P:path::function` citation identifies a target file and symbol in that patch;
these are reference-source locators, not claims that those files exist on main.
`C` below abbreviates `internal/transcriptcapture/`, and `CLI` abbreviates
`cmd/witself/`; both expand inside `P:` citations and the test inventory.
Declarations are named where a constant or persisted schema is the evidence.

The supplied patch has 41 file diffs, 6,055 added lines and 101 removed lines.
The task identifies commit `8de39628`, base `841bd9ee`, and a parking date of
2026-09-05. The backlog notes (comment dated 2026-09-05T16:56:47Z) called the
branch local-only and not pushed; it was pushed later as
`origin/claude/transcript-release` (commit 8de39628, parent 841bd9ee). The
reviewer confirmed locally, without contacting the remote, that the diff of
that ref against its parent is byte-identical to the patch artifact.
The two historical high findings below come from that comment and were checked
against the patch; the review rounds themselves were not independently verified.

Unchanged helpers absent from patch hunks were checked using local
`git show 841bd9ee:<path>` where needed, explicitly labeled **base supplement**.
Their behavior is **unverified from P alone**. Current checkout source is labeled
**current** and is not used to silently fill historical gaps.

## Purpose and command surface

The reference converts locally gated, unfenced turns into a privacy-restricted
operator completion. It preserves captured visible prompts/responses while
discarding tool payloads; this is neither proof of a runtime seal nor a purge.
The release command changes local eligibility; a subsequent ordinary flush sends
the entries. It does not itself append them to the server.
Evidence: `P:CLI/transcript_release.go::transcriptRelease`,
`P:C/release.go::releaseResidueTurn`, `redactOperatorReleasedEvent`;
`P:CLI/transcript_release_flush_test.go::TestTranscriptReleaseSubsequentFlushUploadsExactlyRedactedEvents`.

```text
witself transcript release --runtime RUNTIME
  [--session SESSION_ID | --all] [--older-than DURATION]
  [--dry-run] [--yes] [--force]
```

All following CLI rows cite `P:CLI/transcript_release.go::transcriptRelease`.

| Surface | Reference behavior |
| --- | --- |
| `--runtime` | Required in practice: empty/unsupported input fails normalization. |
| `--session` | Selects one local session; explicit blank or combination with `--all` is invalid. |
| `--all` | Explicit apply selection across the runtime; preview may omit both selectors. |
| `--older-than` | Go duration, default `24h`; negative values invalid. |
| `--yes` | Applies; without it the command lists residue. |
| `--dry-run` | Overrides `--yes`; no uploader probe, but see cleanup exception below. |
| `--force` | Overrides age only, not locks, ownership, binding or suppression checks. |
| Positionals / `--json` | Unsupported; parse/usage errors return 2. |
| Exit 0 | Preview or successful apply, including zero released turns. |
| Exit 1 | Listing, output, uploader verification or release failure; partial apply is possible. |
| Exit 2 | Invalid arguments, parse error or runtime normalization failure. |

Output is a tab-aligned table headed `SESSION`, `QUEUED EVENTS`,
`FIRST EVENT (UTC)`, `LAST EVENT (UTC)`, `TOOL.RESULT PAYLOAD`.
Rows contain quoted session IDs, counts, RFC3339Nano times and a boolean, not bodies.
Apply prints `released N turn(s)` even when `ReleaseResidue` returns partial failure.
Its stderr caveat says turns were not runtime-sealed, every tool result was
redacted, and prompts/assistant messages upload as captured. This caveat is too
broad for preserved attempted snapshots; see the invariants and findings.
Evidence: `P:CLI/transcript_release.go::transcriptRelease`, `transcriptReleasePrivacyCaveat`.

The private single-argument `--uploader-capability` returns 0 and prints
`witself.transcript-release.uploader.v1` before configuration access.
Evidence: `P:CLI/transcript_release.go::transcriptRelease`;
`P:CLI/transcript_release_uploaders.go::transcriptReleaseUploaderCapability`.

Selection groups by transcript/session/run/turn and skips missing identities,
operator-marker events, and pathless Codex events. Already runtime-fenced or
durably completed turns are excluded. Age is the last eligible event timestamp,
not evidence that a process exited. Explicit session selection refuses if any
residue turn is fresh; `--all` skips fresh turns and accumulates session errors.
The preview lists residue without applying the age filter.
Evidence: `P:C/release.go::residueTurns`, `ListResidue`, `ReleaseResidue`, `releaseResidueSession`.

The library listing is read-only, but CLI preview calls orphan cleanup first:
it can remove snapshots, acknowledged events and acknowledgement markers.
Therefore the flag's promise to list without changing anything is false.
Evidence: `P:C/release.go::ListResidue`, `P:CLI/transcript_release.go::transcriptRelease`,
`P:C/submission_cleanup.go::SweepOrphanSubmissions`.

The release projection uses `redacted: "released_without_fence"`,
`original_bytes` and `sha256` placeholders for result bodies and structured
payloads. Tool-call/error bodies keep tool identity only; other non-message
bodies become placeholders. Raw envelopes disappear, and recovered visible
message bodies remain with marker-only data. Existing bounded payload digests
are preserved rather than hashing the omission record. Readiness validates the
projection, not just its release flag.
Evidence: `P:C/release.go::{redactOperatorReleasedEvent,releasePayload,operatorReleasedEventSafe,releasedPayloadSafe}`.

## Required invariants and reference gaps

The governing targets are [Transcript Ledger](transcript-ledger.md#capture-and-delivery)
and [Sealed-Plane Acceptance](sealed-plane-acceptance.md#open-plane-and-transcript-non-leakage).
The latter requires queued sealed-turn content and later hooks to become
value-free before upload; a missing scan lane is not a passing result.
Its authorized plaintext test sinks do not authorize new production snapshots.

1. **Turn fencing:** retain exact runtime, transcript, session, run and turn
   boundaries; no timeout or force flag may manufacture runtime completion.
   The reference uses a distinct operator hold and completion kind and rechecks
   eligibility under flush/session locks. A companion fence cannot finish a
   partially rewritten operator release.
   Evidence: `P:C/release.go::ReleaseResidue`, `releaseResidueTurn`;
   `P:C/fence.go::EnqueueFence`; `P:C/capture.go::ReadinessIndex.UploadReady`.
2. **Sealed suppression:** reuse the shared rewrite path, including body,
   structured data, raw envelopes, recovered messages and turn-less hooks.
   The reference refuses release of an exact known-sensitive turn whose retained
   content still needs suppression, before installing its hold. However, shared
   redaction skips attempted envelopes and already-safe operator projections.
   Preserving a released prompt across a later sealed hook is reference behavior,
   not proof of the stronger sealed-plane target. Re-implementation must resolve
   this conflict explicitly; age and `--force` provide no liveness proof.
   Evidence: `P:C/release.go::releaseResidueSession`, `redactOperatorReleasedEvent`;
   `P:C/capture.go::redactPendingEvents`;
   `P:C/release_test.go::TestReleaseResidueLateHooksStayRedactedAndReleasedBytesStayStable`.
3. **No added plaintext persistence:** keep release bookkeeping value-free and
   add no content-bearing file beyond the existing capture representation.
   The reference fails this proposed requirement: attempted `.submissions`
   records contain both an `Event` and projected `Entries`, duplicating content.
   Prefer an immutable projection in the existing event file with value-free
   transaction metadata; prove safe migration and recovery before adopting it.
   Evidence: `P:C/submission.go::pendingSubmission`, `MarkPendingSubmitted`.
4. **Idempotent submission:** once dispatch may have occurred, retry exactly
   the same external IDs and bytes. Queued provenance does not prove an older
   uploader never sent the event. The reference creates stable replacement IDs
   for rewritten queued events and preserves attempted ones. This can coexist
   with previously accepted originals; it does not erase server-side content.
   Evidence: `P:C/submission.go::prepareOperatorReleaseSubmission`,
   `submissionProjection`, `releasedSubmissionEventID`, `loadPendingSubmission`;
   `P:CLI/transcript_release_lost_ack_test.go::TestTranscriptReleaseOldFlusherQueuedLostAckUsesFreshExternalIDs`.
5. **Acknowledge before cleanup:** successful delivery must precede local
   retirement. The reference persists value-free acknowledgement, then removes
   snapshot, event and acknowledgement in that order. Failed deletion retains
   recoverable evidence. Re-implementation must keep this ordering guarantee
   without adding a plaintext snapshot merely to make deletion possible.
   Evidence: `P:C/submission_acknowledgement.go::removePendingWithAcknowledgement`,
   `removeAcknowledgedPendingFiles`; `P:C/capture.go::RemovePending`.

## Persisted records and state machine

Paths below are relative to `WITSELF_HOME`. Outbox/state directory prefixes and
the completed-marker key formula are **base supplements**, verified in
`841bd9ee` at `C/capture.go::{outboxDir,sessionStatePath}` and
`C/fence.go::completedFencePath`, but **unverified from P alone**.

| Record | Contents, role and patch evidence |
| --- | --- |
| `capture/outbox/R/<timestamp>-<event-id>.json` | Capture event; release identity survives sidecar removal. `P:C/capture.go::writeOutboxEvent`; `P:C/submission.go::submissionProjection`. |
| Same directory, `.submissions/<event-file>.json` | `queued`: identity, optional release/superseded/reply IDs; `attempted`: frozen event plus entries. `P:C/submission.go::submissionPath`, `pendingSubmission`, `MarkPendingSubmitted`. |
| Same sidecar name plus `.ack` | Event/runtime/session/transcript identity only. `P:C/submission_acknowledgement.go::pendingAcknowledgement`, `pendingAcknowledgementPath`. |
| `capture/state/R/<session-hash>.json` | Mutable run/turn/prompt, sensitivity, pending fence and release mirror. `P:C/capture.go::sessionState`, `saveSessionState`. |
| `capture/operator-releases/R/<session-hash>.json` | Authoritative release hold: status, released turn IDs, since, fenced-at; survives old session writers. `P:C/release_state.go::operatorReleaseStatePath`; `P:C/capture.go::loadSessionState`. |
| `capture/state/R/completed/<session-hash>/<identity-hash>.json` | Identity hash covers transcript/run/turn; timestamp plus `operator_release` kind admits rewritten events. `P:C/release.go::releaseResidueTurn`; `P:C/fence.go::completedFence`, plus base helper above. |
| Outbox `.flush.lock` and existing session locks | Upgrade barrier checks legacy owner; release then acquires flush and session locks. `P:C/release_barrier.go::VerifyReleaseUpgradeBarrier`; `P:C/release.go::ReleaseResidue`, `releaseResidueSession`. |

There are two coupled state machines; `completed` release does not mean delivered.
The reference release transitions are reconstructed from
`P:C/release.go::{ReleaseResidue,releaseResidueSession,releaseResidueTurn,reconcileOperatorRelease}`:

| State | Transition and crash/retry behavior |
| --- | --- |
| Gated residue | Validate uploaders, upgrade barrier, provenance, bindings, age, sensitivity and pending runtime fence. Refusal leaves release unavailable; preliminary orphan cleanup may still run. |
| Session hold `pending` | Persist all requested turn IDs before rewriting any event. An interrupted mutable-state write leaves the separate hold authoritative. |
| Per-turn hold event | Publish `OperatorRelease` / `operator_release` with a fresh durable event ID; reuse it on retry. It is not upload-ready without completion. |
| Rewriting | Persist replacement identity before changing bytes; preserve prompt reply mapping; rewrite queued matching events and suppressed empty-turn session events. Partial failure retains the hold. |
| Turn complete | Write the completed marker only after rewriting succeeds. Update synthetic-fenced identity for the current turn without inventing a new prompt. |
| Session `completed` | After all selected turns succeed, persist completed status; after a crash, an explicit retry can reconcile existing safe events and markers without reuploading. |
| Suppression interval closed | Only a genuine, non-synthetic Stop for a fresh prompt-created, unreleased turn sets `FencedAt`; completed status alone does not end suppression. |

The final row and stale-snapshot rules are implemented by
`P:C/capture.go::enqueueHook` and `ReadinessIndex.UploadReady`, plus
`P:C/release.go::operatorReleaseState.suppresses`.
Released turn IDs remain protected. Empty-turn events remain suppressed while
the interval is active and historical events at/before `FencedAt` remain covered.
The genuine Stop is published before ending the interval; failure preserves retry
identity. A newly requested release can reopen the interval; retrying an old one
does not erase its genuine closing fence.
Evidence: `P:C/capture.go::enqueueHook`; `P:C/release.go::releaseResidueSession`.

Submission transitions are separate from release eligibility:

| State | Transition, persisted boundary and recovery |
| --- | --- |
| Absent sidecar / unknown history | Never infer unsent. Release refuses all such runtime events, including other sessions and already-redacted ones. Compatible normal flush may reconcile eligible events. |
| `queued` | Written before publishing a new event. A queued sidecar without an event is retained because publication may be in progress. |
| Queued replacement | Persist release ID, superseded IDs and only actual reply targets before rewriting; the first durable namespace wins across retries. |
| `attempted` | Freeze event and entries before append, after rechecking event bytes under session lock. This means dispatch is possible, not acknowledged or necessarily started. |
| Ambiguous dispatch / lost acknowledgement | Keep attempted snapshot; `PendingEvent.Entries` replays it and readiness/finalization bypass later holds. Original outbox edits cannot alter the attempted envelope. |
| Acknowledged | Write `.ack`, unlink snapshot, unlink event, unlink `.ack`; interrupted deletion resumes from identity-validated evidence. |
| Orphan snapshot | If the event is absent, sweep non-queued snapshots; retain queued publication records. Cleanup errors are nonfatal and retried later. |

Evidence by row: `P:C/release_barrier.go::VerifyReleaseSubmissionProvenance`;
`P:C/submission.go::{registerPendingQueued,prepareOperatorReleaseSubmission,MarkPendingSubmitted,loadPendingSubmission,PendingEvent.Entries}`;
`P:C/capture.go::{ReadinessIndex.UploadReady,finalizePendingWithin}`;
`P:C/submission_acknowledgement.go::{removePendingWithAcknowledgement,sweepPendingAcknowledgement}`;
`P:C/submission_cleanup.go::SweepOrphanSubmissions`.

Flush marks each event in a batch before issuing the append. A later event's
freeze failure aborts without undoing earlier freezes: finding H1 below.
Cleanup follows successful append of every chunk for that event and successful
activity handling. The chunk/activity control flow was checked by reconstructing
`CLI/integration.go` in memory from the local base plus patch; it is a **base
supplement**, not all visible in P's short hunks. Patch anchors are
`P:CLI/integration.go::{transcriptFlush,capturePendingAppendBatches,appendSingleCaptureEvent}`.
Power-loss durability/fsync guarantees and real server replay semantics remain
**unverified** by this source-only design review.

## Runtime and uploader matrix

| Runtime / uploader | Reference handling and evidence |
| --- | --- |
| `claude-code` | Common release projection and session suppression, with managed hook ownership/executable verification where configured. Synthetic core coverage: `P:C/release_test.go::TestReleaseResidueListsWithoutMutationAndRedactsEveryResult`; `P:C/release_uploaders_test.go::TestReleaseManagedUploaderRequiresExactExecutable`. |
| `codex` | Missing native transcript path excludes residue; run/turn companion fence must defer to pending release. `P:C/release.go::residueTurns`; `P:C/fence.go::EnqueueFence`; `P:CLI/transcript_release_fence_recovery_test.go::TestTranscriptReleaseInterruptedRewriteCompanionFenceRetryFlush`. |
| `grok-build` | Suppressed late Stop finalizes without native transcript rehydration. Core/HTTP fixtures exercise this, but valid user-hook apply is blocked by H2. `P:C/capture.go::finalizePendingWithin`; `P:C/release_grok_test.go::TestReleaseGrokLateStopFinalizesWithoutNativeTranscript`; `P:CLI/transcript_release_uploaders.go::verifyTranscriptReleaseHookExecutable`. |
| Cursor / user hooks | `VerifyOwnedHooks` branch exists but the preceding runner requirement makes valid user configurations fail; no positive real-install release proof. `P:CLI/transcript_release_uploaders.go::verifyTranscriptReleaseHookExecutable`; H2 includes validator evidence. |
| Legacy/mixed uploaders | Missing registration, mismatched version/capability, changed hook executable, live/unknown old flusher, or untracked outbox event causes refusal; force cannot override. `P:CLI/transcript_release_uploaders.go::{verifyTranscriptReleaseUploaders,probeTranscriptReleaseUploader}`; `P:C/release_barrier.go::{VerifyReleaseUpgradeBarrier,VerifyReleaseSubmissionProvenance}`. |
| Other runtimes / DSH | The loop checks every installed supported runtime, not only the selected one. Live coverage is **unverified**. DSH is absent from base `config.go::SupportedRuntimes` but present in current source; no DSH portability can be inferred from this patch. `P:CLI/transcript_release_uploaders.go::verifyTranscriptReleaseUploaders`, plus base/current supplements. |

Managed checks compare owned policy/runner and exact runner bytes targeting the
persisted executable. Each distinct executable is probed for exact version and
capability with a shared five-second timeout, 256-byte stdout bound, discarded
stderr, and temporary application/user/config homes. This is compatibility
evidence, not executable attestation or an OS sandbox. Dry-run avoids probes.
Evidence: `P:C/release_uploaders.go::VerifyManagedHookExecutable`;
`P:CLI/transcript_release_uploaders.go::{verifyTranscriptReleaseUploaders,probeTranscriptReleaseUploader,runTranscriptReleaseUploaderProbe,transcriptReleaseCapabilityOutput.Write}`;
`P:CLI/transcript_release.go::transcriptRelease`.

The reference suggests reinstalling the incompatible runtime and allowing a
foreground flush to finish. These are remediation messages, not guaranteed
recovery: a permanently unfenced legacy event can remain blocked by provenance.
Evidence: `P:CLI/transcript_release_uploaders.go::transcriptReleaseUploaderVerificationError.Error`;
`P:C/release_barrier.go::ReleaseUpgradeBarrierError.Error`, `VerifyReleaseSubmissionProvenance`;
`P:C/release.go::residueTurns`.

## Two open high findings and closure options

**H1 — preparation failure creates an unsafe immutable exemption.**
In `P:CLI/integration.go::transcriptFlush`, freeze event A successfully, then fail
`P:C/submission.go::MarkPendingSubmitted` for event B before the batch append.
A remains `attempted` even though that batch made no append request.
Later sealed redaction skips A in `P:C/capture.go::redactPendingEvents`;
`ReadinessIndex.UploadReady` and `finalizePendingWithin` admit the immutable copy.
A subsequent flush can upload the original prompt. Failure must be injected
after `Pending` has read the batch; an unreadable file at initial discovery only
fails earlier and does not reproduce this sequence. This is a source-verified
path, not an executed reproduction in this documentation slice.

- **Option A, narrow repair:** track exactly which events this batch newly froze.
  On known pre-dispatch failure, restore only those to their prior queued records
  under the same serialization and recheck their identity. Preserve all existing
  attempts, replacement mappings and events dispatched by earlier chunks.
  Advantage: small change. Cost: rollback can itself fail; crash recovery still
  needs durable evidence distinguishing preparation from possible dispatch.
- **Option B, preferred design:** use a value-free batch journal with explicit
  `preparing` and `dispatch_possible` phases. Validate/freeze the complete batch,
  persist the dispatch transition before network I/O, and recover incomplete
  preparation as mutable under locks. Once dispatch is possible, never roll back
  on timeout. Advantage: recoverable distinction; cost: transaction/lock design
  and proof that hooks cannot alter bytes between validation and dispatch.

Neither option may treat every failed HTTP call as unsubmitted. Proposed closure
tests: first freeze succeeds/second fails, then a
`mcp__witself__witself_secret_create` pre-tool hook and flush; assert zero canary
bytes and no append from the failed preparation. Add pre-existing attempts,
multi-chunk events, rollback failure and process interruption at each boundary.
The existing preservation test covers ambiguous dispatch, not failed preparation:
`P:CLI/transcript_release_submission_test.go::TestTranscriptReleasePreservesSubmittedEntries`.

**H2 — valid user hooks cannot satisfy uploader verification.**
`P:CLI/transcript_release_uploaders.go::verifyTranscriptReleaseHookExecutable`
requires `HookRunnerPath` for every non-none mode before reaching `HookModeUser`.
The unchanged `C/config.go::validateHookOwnershipFields` forbids that field for
user hooks: verified in local base `841bd9ee` and current source, but **unverified
from P alone** because config.go is not in the patch. The all-installed-runtime
loop therefore also blocks a managed Codex/Claude release in a mixed home.
The negative fixture entrenches this wrong requirement:
`P:CLI/transcript_release_uploader_legacy_regression_test.go::TestTranscriptReleaseUserRegistrationMissingRunnerRefusesWithoutMutation`.

- **Option A, preferred repair:** branch on hook mode first. Managed requires
  the runner and exact owned policy; user requires persisted executable/config
  path and `VerifyOwnedHooks`; none needs only its applicable executable checks.
  Keep capability/version probing and refuse genuinely incomplete registrations.
  Advantage: matches the validator; cost: fixtures must prove every user's exact
  owned handler, executable and arguments, not just a populated path.
- **Option B, broader redesign:** publish a versioned normalized uploader manifest
  at install time with hook kind, executable and ownership evidence; release
  verifies the manifest and actual installed files. Advantage: one explicit
  compatibility protocol. Cost: schema migration, installer work and legacy
  reconciliation; a manifest alone cannot establish live hook ownership.

Proposed closure tests use actual installer-generated fixtures in temporary homes
for Grok and Cursor, both alone and alongside managed Codex/Claude. Tamper each
owned handler and executable; retain refusal for incomplete legacy registrations,
version mismatch and active flushers. No real-home reinstall belongs in this gate.

## Reference test inventory

All 25 changed test files are listed below. A file cell is a `P:` source locator
using the prefixes defined above; function names identify the assertions read.
These are source-reviewed synthetic proofs, **not test results from this slice**.
Permission-dependent tests can skip on Windows or when Unix unlink permissions
are not enforced; historical aggregate, race and live-runtime results are **unverified**.

| Patch test file | Named tests and scope |
| --- | --- |
| `CLI/transcript_orphan_test.go` | `TestTranscriptFlushOrphanSubmissionAfterInterruptedAcknowledgement`, `TestTranscriptFlushOrphanSubmissionCleanupFailureIsNonfatal`, `TestTranscriptReleaseSweepsOrphanSubmissionSnapshot`: interrupted cleanup, retry/nonfatal error and preview/apply sweep, including unreadable orphan. |
| `CLI/transcript_release_acknowledgement_test.go` | `TestTranscriptReleaseFlushAfterInterruptedSubmittedAcknowledgement`: HTTP flush neither resends acknowledged plaintext nor strands the rest. |
| `CLI/transcript_release_body_test.go` | `TestTranscriptReleaseSessionRedactsSystemBodyCopiesHTTP`: system-body payload copies excluded while captured assistant text remains. |
| `CLI/transcript_release_fence_recovery_test.go` | `TestTranscriptReleaseInterruptedRewriteCompanionFenceRetryFlush`: no HTTP during unfinished rewrite; retry restores exact eligible entries. |
| `CLI/transcript_release_flush_test.go` | `TestTranscriptReleaseSubsequentFlushUploadsExactlyRedactedEvents`, `TestTranscriptReleaseGrokLateStopFlushesAfterReleaseUpload`: release is local, subsequent HTTP equals projected events, late Grok hooks stay suppressed. |
| `CLI/transcript_release_legacy_test.go` | `TestTranscriptReleaseLegacySessionEndLateResultFlush`: legacy session deletion cannot make unknown-history late plaintext uploadable. |
| `CLI/transcript_release_lost_ack_test.go` | `TestTranscriptReleaseOldFlusherQueuedLostAckUsesFreshExternalIDs`: old acceptance despite queued metadata requires fresh, retry-stable IDs and replies. |
| `CLI/transcript_release_session_test.go` | `TestTranscriptReleaseSessionPermissionRequestFlush`, `TestTranscriptReleaseSessionInterruptedStopLateResultFlush`, `TestTranscriptReleaseSessionNewFencedTurnFlushesNormally`: empty-turn suppression, interrupted release and no suppression of a genuinely new fenced turn. |
| `CLI/transcript_release_submission_test.go` | `TestTranscriptReleasePreservesSubmittedEntries`: snapshots precede HTTP; failed/lost response retries original bytes without conflicts. |
| `CLI/transcript_release_test.go` | `TestTranscriptReleaseDryRunListsResidueAndChangesNothing`, `TestTranscriptReleaseOlderThanRefusesFreshAndForceOverrides`, `TestTranscriptReleaseAllSkipsFreshAndFencedTurns`, `TestTranscriptReleaseSessionSelectionLeavesOtherResidueUntouched`, `TestTranscriptReleaseAllReportsFailureAndContinues`, `TestTranscriptReleaseInvalidArguments`: command contract on synthetic fixtures; dry-run fixture lacks cleanup residue. |
| `CLI/transcript_release_uploader_legacy_regression_test.go` | `TestTranscriptReleaseLegacy2b04c0dRegistrationRefusesWithoutMutation`, `TestTranscriptReleaseUserRegistrationMissingRunnerRefusesWithoutMutation`, `TestTranscriptReleaseCapabilityWithDifferentVersionRefusesWithoutMutation`: refusal before mutation/HTTP; user-runner expectation is wrong (H2). |
| `CLI/transcript_release_uploaders_test.go` | `TestTranscriptReleaseUploaderCapabilityIsReadOnly`, `TestTranscriptReleaseRejectsOlderInstalledUploaderBeforeMutation`, `TestTranscriptReleaseDryRunDoesNotProbeInstalledUploader`, `TestTranscriptReleaseAcceptsCompatibleInstalledUploader`, `TestTranscriptReleaseChecksOtherInstalledRuntimeUploaders`, `TestTranscriptReleaseRejectsHookExecutableDifferentFromRegistration`, `TestTranscriptReleaseUploaderProbeIsBoundedAndIsolated`: capability, mixed installs, binding, probe limits and temporary homes. |
| `C/release_barrier_test.go` | `TestReleaseUpgradeBarrierPreservesLiveOldFlusher`, `TestReleaseUpgradeBarrierRefusesEveryLegacyOutboxEvent`: active-owner and runtime-wide provenance refusal without force bypass. |
| `C/release_fence_recovery_test.go` | `TestReleaseResidueInterruptedRewriteSurvivesCompanionFence`: stable hold identity through failed rewrite and companion retry. |
| `C/release_grok_test.go` | `TestReleaseGrokLateStopFinalizesWithoutNativeTranscript`: persisted suppressed Stop finalization without native rehydration. |
| `C/release_legacy_test.go` | `TestReleaseHoldSurvivesLegacyHookSessionWrites`, `TestReleaseCorruptDurableHoldFailsClosedAfterLegacyRewrite`: separate hold survives overwrite/deletion; corrupt hold blocks capture/readiness. |
| `C/release_reply_metadata_test.go` | `TestReleaseReplyMetadataGrowsLinearly`, `TestReleaseReplyMetadataTargetsAndRetry`: only actual reply targets, recovered/cross-event chains and stable retry projection. |
| `C/release_sensitive_test.go` | `TestReleaseResidueRefusesFailedSensitiveRedactionBeforeHoldingTurn`: exact sensitive identity refuses before hold/completion; sealed-hook or fence recovery remains possible. |
| `C/release_session_test.go` | `TestReleaseSessionHoldSurvivesRewriteFailureAndStop`, `TestReleaseSessionSuppressionRequiresMatchingFreshStop`, `TestReleaseSessionReadinessRejectsStaleAndMutatedTurnlessSnapshots`, `TestReleasePermissionRequestPreservesOriginalPayloadDigest`, `TestReleaseSessionReconcilesCompletedMarkerWithoutRewriting`, `TestReleaseSessionRetryPreservesGenuineFreshFence`, `TestReleaseSessionFreshStopWriteFailurePreservesRetryIdentity`: session interval, stale data, bounded payload digest, marker reconciliation and fresh-Stop recovery. |
| `C/release_submission_identity_test.go` | `TestReleaseSubmissionIdentitySurvivesInterruptedAcknowledgement`: event retains replacement IDs after sidecar removal. |
| `C/release_test.go` | `TestReleaseResidueListsWithoutMutationAndRedactsEveryResult`, `TestReleaseResidueAgeForceAndFencedTurnsUntouched`, `TestReleaseResidueRetriesInterruptedCompletionAfterLateRuntimeFence`, `TestReleaseResidueLateHooksStayRedactedAndReleasedBytesStayStable`, `TestReleaseReadinessRejectsPayloadCopiesAndFlushContention`: three-runtime core, age/force, completion retry, stable projection and unsafe snapshot/lock rejection. |
| `C/release_uploaders_test.go` | `TestReleaseManagedUploaderRequiresExactExecutable`: owned managed runner must target the registered binary. |
| `C/submission_acknowledgement_test.go` | `TestSubmissionAcknowledgementSweepValidatesEvidenceAndPreservesCrashOrder`: mismatched/untrusted acknowledgement rejected; snapshot-first cleanup retains retry state. |
| `C/submission_cleanup_test.go` | `TestRemovePendingSubmissionCrashOrder`: failed event unlink does not retain the plaintext snapshot and retry finishes. |
| `C/submission_test.go` | `TestReleaseSubmissionProvenanceQueuedAttemptedAndLegacy`, `TestReleaseRefusesLegacyHistoryBeforeMutation`, `TestReleaseRefusesLegacyAlreadyRedactedNoop`, `TestReleaseSubmissionRejectsUnknownState`: identity/state validation, immutable attempts, no invented legacy provenance. |

## Proposed bounded slices on current main

These are proposed work scopes, not authorization or evidence of implementation.
Each slice gets `git diff --check`, `go build ./...`, `go vet ./...`,
`go mod tidy -diff` and the focused gate below. Home-touching tests use
`t.TempDir()` for `WITSELF_HOME` and, for DSH, `DSH_HOME`. No store test is needed
for the initial client work; any later store slice must name its own disposable
database and require it rather than silently skipping integration coverage.

| Slice | Files, tests and focused gate |
| --- | --- |
| 35A: settle contract | This design and baseline regression fixtures in `C/capture_test.go` / `C/fence_test.go`. Decide whole-turn suppression versus provably closed-run message preservation, true read-only preview and single plaintext representation before exposing a verb; pin existing unfenced and sealed-turn refusal behavior. Gate: `go test ./internal/transcriptcapture -run 'Fence|Redact|Readiness'`. |
| 35B: dispatch transaction | `C/submission*.go` (new), `C/capture.go`, `CLI/integration.go`, new `CLI/transcript_release_submission_test.go`. Implement H1 closure and immutable retry without duplicate plaintext; test every preparation/dispatch crash boundary, earlier chunks, lost acknowledgement and canary suppression. Gate: `go test -race ./internal/transcriptcapture ./cmd/witself -run 'Submission|Capture.*Flush|TranscriptFlush'`. |
| 35C: ownership compatibility | `CLI/transcript_release_uploaders.go` and its tests (new), `C/release_uploaders.go` and tests (new); consume existing `config.go` validation. Implement H2 option A with genuine user/managed fixture generation and mixed-home negatives. Gate: `go test ./internal/transcriptcapture ./cmd/witself -run 'Uploader|Hook.*Owned|Hook.*Ownership'`. |
| 35D: release transitions | `C/release*.go` (new), `C/capture.go`, `C/fence.go` and focused tests. Holds, exact identities, completion, empty-turn policy, interrupted rewrites and sensitive refusal; port only reviewed tests from the inventory. Gate: `go test -race ./internal/transcriptcapture -run 'Release|Fence|Redact|Readiness'`. |
| 35E: runtime and CLI | `CLI/transcript_release.go` (new), `CLI/main.go`, CLI release tests and current native finalization files as needed. Validate flags/output and zero-mutation preview; exercise Claude, Codex, Grok, Cursor and current DSH fencing/resume behavior. Gate: `go test ./internal/transcriptcapture ./cmd/witself`; explicitly record any sandbox lock-test exclusions for owner rerun. |
| 35F: recovery and acceptance | `C/submission_acknowledgement.go`, `C/submission_cleanup.go` and tests (new if retained), CLI HTTP recovery tests, acceptance documentation. Assert no content-bearing orphan, validated acknowledgement, stable retries, bounded metadata and no canaries in any new persistence path. Gate: both client packages with `-race`, plus the owner-run sealed-plane acceptance matrix. |

Main already has broader fencing/resume and kernel flush-lease contracts than
the reference. Reconcile with current `C/fence.go::EnqueueFence`,
`C/capture.go::AcquireFlushLock` and `CLI/integration.go::transcriptFlush`;
do not transplant the older PID-lock assumptions. See the current
[capture contract](transcript-ledger.md#capture-and-delivery).
The manager owns serialized full `make check`, any unfiltered integration gate,
PR/merge and later live acceptance. Passing client fixtures is not live certification.

## What to discard, simplify or resolve before coding

- Discard the universal runner-path predicate and the test that codifies it (H2).
  Evidence: `P:CLI/transcript_release_uploaders.go::verifyTranscriptReleaseHookExecutable`.
- Separate preparation from possible dispatch; a single `attempted` state is
  insufficient (H1). Evidence: `P:C/submission.go::MarkPendingSubmitted`.
- Remove cleanup from preview and correct the overly broad privacy caveat.
  Evidence: `P:CLI/transcript_release.go::transcriptRelease`;
  `P:C/capture.go::redactPendingEvents` preserves attempted envelopes.
- Replace duplicate plaintext snapshots with one durable content representation;
  do not drop immutable retry or acknowledgement recovery to achieve this.
  Evidence: `P:C/submission.go::pendingSubmission`;
  `P:C/submission_acknowledgement.go::removeAcknowledgedPendingFiles`.
- Resolve the late sealed-hook/preserved-prompt conflict before retaining
  `--force`. Evidence: `P:C/capture.go::redactPendingEvents`;
  `P:C/release_test.go::TestReleaseResidueLateHooksStayRedactedAndReleasedBytesStayStable`.
- Prefer explicit migration over indefinitely layering mirrored session state,
  separate holds, per-turn markers, sidecars and replacement maps. Separate
  holds do protect against old writers, so remove them only with a proven upgrade
  boundary. Evidence: `P:C/capture.go::loadSessionState`, `saveSessionState`;
  `P:C/release_state.go::loadOperatorReleaseState`; `P:C/release.go::releaseResidueTurn`.
- Do not copy a complete turn reply map into every event; retain the reference's
  corrected actual-target approach. Persisted growth is tested; CPU scalability
  and long-term hold retention remain **unverified**.
  Evidence: `P:C/submission.go::prepareOperatorReleaseSubmission`;
  `P:C/release_reply_metadata_test.go::TestReleaseReplyMetadataGrowsLinearly`.
- Preserve conservative legacy refusal, but design an actionable retirement or
  migration path for permanently unfenced unknown history. A foreground flush
  suggestion alone does not make it releasable.
  Evidence: `P:C/release_barrier.go::VerifyReleaseSubmissionProvenance`;
  `P:C/release.go::residueTurns`.
