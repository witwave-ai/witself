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
