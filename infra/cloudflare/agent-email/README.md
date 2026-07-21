# Cloudflare receive-only agent-email pilot

This directory contains the isolated Cloudflare Email Worker and route manager
for the capability-limited Witself pilot. It is not the Witself control-plane
Worker. It has no HTTP route, no control-plane Container binding, and no access
to the control-plane `DIRECTORY` KV namespace.

The pilot accepts one realm and exactly 5–10 literal recipient addresses. It
rejects messages larger than 5 MiB, signs the SMTP envelope plus raw-message
digest with Ed25519, and relays the raw message to the enrolled cell. Only a 2xx
response containing exactly `{"verdict":"accepted"}` counts as SMTP success.

## Safety boundary

- Keep the existing Email Routing catch-all unchanged.
- Use only the dedicated `witself-agent-email-pilot-directory` KV namespace.
- Keep `global_fetch_strictly_public` enabled so the Worker reaches the
  DNS-only cell ingress through its public hostname even though both hostnames
  are in the `witwave.ai` zone. Signed headers are never followed across a
  redirect.
- Do not put relay private-key material in the manifest, generated Wrangler
  configuration, Git, logs, or cell configuration.
- Treat `pilot.example.json` as a shape example, not deployable values.
- Run `status` and review the exact literal routes before activation.
- Do not activate until the destination cell is enabled and healthy.
- A failed operation attempts to disable the pilot gate and its managed rules;
  inspect Cloudflare state before retrying any reported incomplete rollback.

The route manager reads and fingerprints the catch-all before and after every
operation. Its API client contains no catch-all update operation. It also
refuses to replace an unmanaged rule for an enrolled literal address.

## Local verification

From this directory:

```sh
npm ci
npm test
npm run config
npx wrangler deploy --dry-run --config wrangler.generated.jsonc
```

`npm run config` requires `EMAIL_DIRECTORY_KV_ID` and `RELAY_KEY_ID`. It refuses
the KV ID bound to the adjacent control-plane Worker. The generated file is
local operator state and must not be committed.

## Staged managed rollout

Use a narrowly scoped Cloudflare token and set these environment variables in
the operator shell without printing their values:

- `CLOUDFLARE_API_TOKEN`
- `CLOUDFLARE_ACCOUNT_ID`
- `CLOUDFLARE_ZONE_ID`
- `EMAIL_DIRECTORY_KV_ID`
- `RELAY_KEY_ID`

The token needs Workers deployment/secret access plus Email Routing edit and KV
read/write for the isolated namespace. The route-manager scripts validate the
account, zone, namespace, manifest, and existing routes before mutation.

1. Create or locate the isolated KV namespace, then verify its exact title:

   ```sh
   npm run directory -- ensure
   npm run directory -- show
   ```

2. Render and deploy the unreachable email-only Worker first, then load the
   operator-provisioned PKCS#8 Ed25519 private key through Wrangler's secret
   prompt. The Worker has no HTTP or email route at this stage; putting the
   secret creates and deploys the secret-bearing version:

   ```sh
   npm run config
   npm run deploy
   npm run secret:put
   ```

3. Enable the matching cell configuration with only the public key, deploy the
   cell, and confirm its startup reconciliation and health checks.

4. Copy `pilot.example.json` outside the repository, replace every example
   value with the reviewed one-realm/5–10-agent enrollment, and prepare disabled
   literal routes:

   ```sh
   npm run routes -- prepare /absolute/path/to/pilot.json
   npm run routes -- status /absolute/path/to/pilot.json
   ```

5. Wait for directory propagation, review Cloudflare Email Routing and the
   unchanged catch-all, then activate and immediately recheck status:

   ```sh
   npm run routes -- activate /absolute/path/to/pilot.json
   npm run routes -- status /absolute/path/to/pilot.json
   ```

6. Send one synthetic message to the exact canary address. Confirm a committed
   mailbox row through the owner-only API before allowing expected low-risk
   verification-code workflows.

7. Confirm the value-free edge outcome stream. The Worker writes one
   best-effort Analytics Engine point per final SMTP-facing outcome; metrics
   failure never changes message disposition. The dataset contains only the
   fixed schema, outcome, phase, count, latency, raw byte count, and numeric
   response status — never an address, realm, agent, sender, subject, message
   id, digest, signature, or content-derived value. Query the last hour with a
   token carrying `Account Analytics Read`:

   ```sh
   npm run metrics -- summary 60
   ```

   `accepted`, permanent-rejection, and tempfail outcomes must all be visible
   during acceptance and rollback drills. Built-in Worker invocation metrics
   remain the independent signal for runtime exceptions and resource failures.

## Continuous canary

`npm run canary` first arms one owner-generated opaque UUID through the
owner-only retry-canary endpoint, then sends the synthetic message through
Cloudflare Email Sending with `X-Witself-Canary-Retry`. The cell commits the
first matching attempt as a deliberate temporary result without storing a
message; the identical provider retry is accepted once. The runner requires
the cumulative tempfail-then-accepted checkpoint before it passively scans the
owner mailbox newest-first through bounded cursor pages, verifies the exact
subject and parsed synthetic code, claims, marks the code used, completes, and
acknowledges the message. A separate correlation nonce identifies the subject;
the retry challenge appears only in its dedicated MIME header, never the
subject or body. Its output is value-free and includes only
`provider_retry_proven:true`: no code, message content, challenge/message
identifier, address, or token is returned. A post-claim failure releases the
exact fence when possible; otherwise the bounded lease expires normally. A
retained tempfailed proof remains retryable for 24 hours but does not block the
next run from arming a fresh challenge.

The `agent-email-canary` GitHub Actions workflow runs every 15 minutes only
when repository variable `AGENT_EMAIL_CANARY_ENABLED` is exactly `true`.
Provision these repository variables:

- `CLOUDFLARE_ACCOUNT_ID`
- `WITSELF_EMAIL_CANARY_ENDPOINT`

Create the protected-main-only GitHub Environment named `agent-email-canary`
and place these environment secrets there (not repository variables or
repository-wide secrets):

- `AGENT_EMAIL_CANARY_CLOUDFLARE_TOKEN` (`Email Sending: Edit` only)
- `WITSELF_EMAIL_CANARY_TOKEN` (the dedicated enrolled canary agent only)
- `AGENT_EMAIL_CANARY_FROM`
- `AGENT_EMAIL_CANARY_TO`

Run one manual workflow dispatch and review both the value-free canary result
and Analytics Engine outcomes before setting `AGENT_EMAIL_CANARY_ENABLED=true`.
Keep the schedule false until that success. The
Cloudflare sender must already belong to an onboarded Email Sending domain.
The job has a 15-minute outer limit and a 600-second absolute canary deadline.

Do not arm or send during a mixed-version deployment. Deploy schema-61-capable
server code with `WITSELF_AGENT_EMAIL_RETRY_CANARY_AGENT_ID` empty, wait for
every pod to converge, then perform a config-only rollout selecting exactly one
enrolled agent and wait for every pod again. Only then run the manual canary.
For rollback, disable the schedule first and settle the unused arm or let its
15-minute TTL expire before unsetting the canary agent or deploying older code;
otherwise an old replica can ordinary-accept the first synthetic delivery.

Acknowledgement does not delete synthetic messages. A 15-minute schedule adds
about 96 retained messages per day until the ordinary mailbox retention/delete
contract is implemented. Keep the schedule default-off unless that accumulation
is explicitly accepted and monitored.

## Rollback

Disable first; this preserves the exact rules and directory rows for inspection:

```sh
npm run routes -- disable /absolute/path/to/pilot.json
npm run routes -- status /absolute/path/to/pilot.json
```

After the incident record and state review, `remove` deletes only the pilot's
managed literal rules and isolated directory rows. It does not modify the
catch-all:

```sh
npm run routes -- remove /absolute/path/to/pilot.json
```

Do not use `remove` as an automatic failure response; retaining disabled state
usually gives better forensic evidence.
