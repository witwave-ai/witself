# Signup legal admission and explicit re-consent

The release includes capable readers and clients. Enforcement is **off**: the
committed `CP_SIGNUP_LEGAL_ENFORCEMENT` value is exactly `false`. The renderer and
provider verifier both pin that value. Activation needs a separate reviewed
configuration change after the capable CLI and control plane have shipped. This
change does not publish or revise any legal document, change existing-account
consent, or implement a customer notification policy.

When enforcement is enabled, a fresh signup with nonempty consent reads the
canonical pair through the `LEGAL_DOCUMENTS` HTTP service binding to the existing
`witself-legal` Worker. It requests only `/legal/versions.json`, rejects redirects,
and permits at most 64 KiB with a 15-second total read deadline. Labels must be
strings matching the existing 1–64-character vocabulary; paths must be exactly
`/legal/terms` and `/legal/privacy`. Duplicate JSON keys, escaped aliases,
malformed UTF-8, incomplete bodies, and trailing values fail closed. No caller
can select the authority. The binding creates no additional Worker, container,
or reserved capacity.

A successful `initialized` write is admission. The final successful canonical
read immediately precedes that write; this is not an atomic transaction with a
separately published legal deployment. Exact initialized-or-later retries keep
their original request and consent, even when documents change or the legal
service is down. Invited consentless requests retain their existing behavior.
Open signup still requires both versions and the existing abuse controls.

Before admission, completed abuse evidence is saved in `legal_pending`.
Authority failure returns a fixed 503 and leaves that attempt pending; it is
never evidence of a durable refusal. A stale pair instead becomes
`legal_rejected`, with the original pair, a bounded refusal fence, required
pair, original request fingerprint, and a separate domain-separated core hash.
The original attempt remains terminal and replays the same 409 without another
legal, challenge, quota, invite, placement, cell, or projection call.

Durable records contain hashes and version labels, not email, display name,
invite text, raw IP, challenge tokens or credentials. The core hash covers
normalized email, display name and invite, excluding both the provision ID and
consent. The full original and candidate hashes retain the existing CP algorithm.
CP and local journal fingerprints are different algorithms and are never
compared as though they were the same identifier.

Fresh B1 attempts bound both incoming and CP-normalized email/display-name/invite
JSON to 32 KiB, using the CLI's Go-compatible HTML and line-separator escapes.
Lone-surrogate strings are invalid. IDs and version labels are excluded from
this invariant budget: their maximum protocol overhead is 1,463 bytes, so every
admissible core fits every bounded successor within 34,231 bytes, below the
64-KiB wire limit. Existing admitted and durable legal retries do not acquire
new input-size gates. Re-consent compares the CP-normalized core hash, preserving
the existing Go/JS normalization differences without changing either fingerprint.

## Read-only binding check

`GET /v1/signup-legal-readiness` requires the existing fleet bearer credential
and accepts no query arguments. It reads the configured `LEGAL_DOCUMENTS`
binding once, even while enforcement is off. Its private, no-store JSON reports
the effective enforcement boolean, accepted terms/privacy versions and SHA-256
of the actual manifest bytes. Authority failure returns a fixed 503 without
versions or a digest. The check does not consume signup or public rate limits,
access account state, dispatch a container, or change enforcement.

After deploying a release containing this route, use its verified CP origin and
compare the returned pair and digest with an independently validated public
manifest. Retain provider binding-target metadata as well: the response alone
does not prove which Worker owns the binding. A successful check proves that
invocation's binding read, not signup, re-consent, CLI durability or readiness to
enable enforcement. No legal publication is authorized by this result.

## One explicit successor

`POST /v1/account-signups/{provision_id}:reconsent` is a public, rate-limited
signup transition with private/no-store responses. Its exact request binds the
old refusal, one new candidate ID and pair, transition ID, and unchanged
normalized signup input. This is a signup fence, not authenticated ownership of
an existing account.

The predecessor serially persists one candidate. It releases its queue before
calling the internal candidate reservation endpoint, then verifies the exact
acknowledgement and records registration. The empty candidate becomes
`legal_reserved`. Occupied IDs, changed input, another candidate, another fence,
and self-targets conflict. Opposing transitions cannot hold each other's queues.
Ambiguous outcomes retain the same candidate; neither side generates another ID.
A collision requires a separately designed recovery, not journal deletion.

Registration does not fetch legal versions. If another publication occurs after
the client selects its candidate, the same candidate still registers and its own
admission may refuse again. Every refused invocation stops. There is no automatic
acceptance loop.

Only this trusted internal reservation carries completed abuse evidence:
original hashed IP scope, original counter root, Turnstile completion policy,
and each exact allowed daily counter disposition. A successor reuses that one
logical signup under the original limits. Public bypass flags and receipts grant
nothing. Missing or changed required policy evidence fails recoverably; it never
raises quotas or buys another signup allowance. Already admitted recovery is
unchanged. An unrecorded original challenge outcome retains the existing
challenge retry behavior; the new receipt only attests durable completed checks.

## Local recovery and rollout

The CLI saves a verified first refusal under its owner lock and full-record CAS,
then stops even if that invocation included `--accept-terms`. A later explicit
invocation with that flag first validates the original local input, fetches and
displays the current pair, and persists one candidate and transition ID before
network. A pending retry reuses that saved acceptance and re-establishes its
directory-sync boundary before registration, without another version lookup.
Only an exact complete acknowledgement promotes the candidate, after which the
ordinary signup path runs once.

The private v2 journal stays within the existing 16-KiB bound and contains at
most one refusal and one pending candidate. Promotion returns to an ordinary v1
current-attempt record; it never nests prior journals. Existing v1 pending and
credential journals remain readable. A concurrent credential winner prevents a
stale refusal or promotion and still recovers offline. Older CLIs reject v2
journals; they must not delete them to recover.

Release and deploy capable code with enforcement off, verify the service binding
and admitted-retry controls, then separately review activation. Existing B1
pending, reserved and rejected states remain binding even when enforcement is
later disabled. A reserved open-signup attempt still requires open signup to be
available.

CP v0.0.284/B0 is the minimum **state-reader safety** floor: it rejects the new
preadmission phases instead of accidentally provisioning them. It cannot resume
these lineages or enforce freshness for brand-new signups. After activation,
rollback must retain B1-capable code or suspend fresh admission while using B0
for emergency refusal handling. Never roll below that reader floor once B1
states exist; the separate aborted-export cleanup floor also remains in force.
No held legal publication or PR362 work is unblocked by this implementation.
