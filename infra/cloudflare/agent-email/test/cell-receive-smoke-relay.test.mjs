import assert from "node:assert/strict";
import { createHash, generateKeyPairSync } from "node:crypto";
import { mkdtemp, readFile, rm, stat, writeFile } from "node:fs/promises";
import { createServer } from "node:http";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

import {
  runSignedProbe,
  SmokeRelayError,
} from "../scripts/cell-receive-smoke-relay.mjs";
import { canonicalSignatureInput } from "../src/relay.mjs";

function generatedKeyFiles(directory, label = "relay") {
  const { privateKey, publicKey } = generateKeyPairSync("ed25519");
  const privateDER = privateKey.export({ format: "der", type: "pkcs8" });
  const publicDER = publicKey.export({ format: "der", type: "spki" });
  return {
    privateKey,
    publicKey,
    privatePath: join(directory, `${label}-private`),
    publicPath: join(directory, `${label}-public.json`),
    privateValue: privateDER.toString("base64"),
    publicValue: publicDER.subarray(publicDER.length - 32).toString("base64"),
  };
}

async function fixture(t) {
  const directory = await mkdtemp(join(tmpdir(), "witself-cell-receive-smoke-"));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const key = generatedKeyFiles(directory);
  const rawFile = join(directory, "probe.eml");
  const targetFile = join(directory, "target.json");
  const probeFile = join(directory, "probe.json");
  const agentTokenFile = join(directory, "agent-token");
  const resultFile = join(directory, "result.json");
  const nonce = "0123456789abcdef";
  const target = {
    schema_version: 1,
    account_id: "acc_abcdefghijklmnop",
    realm_id: "realm_bcdefghijklmnopq",
    agent_id: "agent_cdefghijklmnopqr",
    recipient: "alpha.bcdefghijklmnopq@witmail.net",
    disabled_plan: "free",
    entitled_plan: "standard",
  };
  const taggedRecipient = `alpha.bcdefghijklmnopq+ws-${nonce}@witmail.net`;
  const raw = Buffer.from(
    "From: Witself smoke <witself-email-receive-smoke@smoke.invalid>\r\n" +
    `To: ${taggedRecipient}\r\n` +
    "Subject: Witself production receive smoke\r\n" +
    `Message-ID: <witself-receive-smoke-${nonce}@smoke.invalid>\r\n` +
    "X-Witself-Production-Receive-Smoke: 1\r\n" +
    "Content-Type: text/plain; charset=utf-8\r\n\r\n" +
    "Synthetic receive path proof.\r\n",
  );
  const probe = {
    nonce,
    tag: `ws-${nonce}`,
    recipient: taggedRecipient,
    mime_message_id: `<witself-receive-smoke-${nonce}@smoke.invalid>`,
    raw_sha256: createHash("sha256").update(raw).digest("hex"),
    raw_size: raw.length,
  };
  const agentToken = `witself_agt_${"A".repeat(43)}`;
  await Promise.all([
    writeFile(key.privatePath, `${key.privateValue}\n`, { mode: 0o600 }),
    writeFile(key.publicPath, `${JSON.stringify({ "relay-1": key.publicValue })}\n`, { mode: 0o600 }),
    writeFile(rawFile, raw, { mode: 0o600 }),
    writeFile(targetFile, `${JSON.stringify(target)}\n`, { mode: 0o600 }),
    writeFile(probeFile, `${JSON.stringify(probe)}\n`, { mode: 0o600 }),
    writeFile(agentTokenFile, `${agentToken}\n`, { mode: 0o600 }),
  ]);
  return {
    directory,
    key,
    raw,
    rawFile,
    target,
    targetFile,
    probeFile,
    agentToken,
    agentTokenFile,
    resultFile,
  };
}

async function listen(t, handler) {
  const server = createServer(handler);
  await new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", resolve);
  });
  t.after(() => new Promise((resolve) => server.close(resolve)));
  return server.address().port;
}

function options(f, port, expectedVerdict = "accepted") {
  return {
    audience: "civo-sandbox-usw2-dev",
    agentTokenFile: f.agentTokenFile,
    expectedVerdict,
    expectedOwnerGate: expectedVerdict === "feature_disabled" ? "feature_disabled" : "address_available",
    keyId: "relay-1",
    privateKeyFile: f.key.privatePath,
    probeFile: f.probeFile,
    publicKeysFile: f.key.publicPath,
    rawFile: f.rawFile,
    resultFile: f.resultFile,
    targetFile: f.targetFile,
    url: `http://127.0.0.1:${port}/v1/internal/agent-email:ingest`,
  };
}

function respondWithOwnerAddress(response, f) {
  response.writeHead(200, {
    "Content-Type": "application/json",
    "Cache-Control": "private, no-store",
  });
  response.end(`${JSON.stringify({
    schema_version: "witself.v0",
    address: {
      account_id: f.target.account_id,
      realm_id: f.target.realm_id,
      owner_agent_id: f.target.agent_id,
      address: f.target.recipient,
      domain: "witmail.net",
      local_part: f.target.recipient.slice(0, -"@witmail.net".length),
      realm_label: f.target.realm_id.slice("realm_".length),
      receive_state: "enabled",
      agent_receive_state: "enabled",
      realm_receive_state: "enabled",
      addresses: [{ address: f.target.recipient, domain: "witmail.net", role: "primary" }],
      aliases: [],
    },
  })}\n`);
}

function respondWithDisabledOwner(response) {
  response.writeHead(403, {
    "Content-Type": "application/json",
    "Cache-Control": "private, no-store",
  });
  response.end(`${JSON.stringify({
    schema_version: "witself.v0",
    code: "feature_not_enabled",
    feature: "agent_email_receive",
    error: "Sorry, this feature is not enabled on this account.",
    retryable: false,
  })}\n`);
}

test("signed smoke probe reaches only loopback with the exact relay envelope", async (t) => {
  const f = await fixture(t);
  let ownerRequests = 0;
  let relayRequests = 0;
  const port = await listen(t, async (request, response) => {
    if (request.url === "/v1/email/address" && request.method === "GET") {
      ownerRequests += 1;
      assert.equal(request.headers.authorization, `Bearer ${f.agentToken}`);
      respondWithOwnerAddress(response, f);
      return;
    }
    const chunks = [];
    for await (const chunk of request) chunks.push(chunk);
    const body = Buffer.concat(chunks);
    assert.deepEqual(body, f.raw);
    assert.equal(request.method, "POST");
    assert.equal(request.headers["content-type"], "message/rfc822");
    assert.equal(request.headers["x-witself-email-version"], "witself-email-relay-pilot-v1");
    assert.equal(request.headers["x-witself-email-key-id"], "relay-1");
    assert.equal(request.headers["x-witself-email-audience"], "civo-sandbox-usw2-dev");
    assert.equal(request.headers["x-witself-email-raw-size"], String(f.raw.length));
    assert.match(request.headers["x-witself-email-raw-sha256"], /^sha256:[0-9a-f]{64}$/);

    const metadata = {
      timestamp: Number(request.headers["x-witself-email-timestamp"]),
      keyId: request.headers["x-witself-email-key-id"],
      envelopeFrom: "witself-email-receive-smoke@smoke.invalid",
      envelopeTo: "alpha.bcdefghijklmnopq+ws-0123456789abcdef@witmail.net",
      audience: request.headers["x-witself-email-audience"],
      rawSize: Number(request.headers["x-witself-email-raw-size"]),
      rawSHA256: request.headers["x-witself-email-raw-sha256"].slice(7),
    };
    assert.equal(
      await crypto.subtle.verify(
        "Ed25519",
        await crypto.subtle.importKey(
          "raw",
          Buffer.from(JSON.parse(await readFile(f.key.publicPath, "utf8"))["relay-1"], "base64"),
          { name: "Ed25519" },
          false,
          ["verify"],
        ),
        Buffer.from(request.headers["x-witself-email-signature"], "base64"),
        canonicalSignatureInput(metadata),
      ),
      true,
    );
    relayRequests += 1;
    response.writeHead(200, {
      "Content-Type": "application/json",
      "Cache-Control": "no-store",
    });
    response.end('{"verdict":"accepted"}\n');
  });

  assert.deepEqual(await runSignedProbe(options(f, port), { now: () => 1_800_000_000_000 }), {
    owner_gate: "address_available",
    http_status: 200,
    verdict: "accepted",
  });
  assert.equal(ownerRequests, 1);
  assert.equal(relayRequests, 1);
  assert.deepEqual(JSON.parse(await readFile(f.resultFile, "utf8")), {
    owner_gate: "address_available",
    http_status: 200,
    verdict: "accepted",
  });
  assert.equal((await stat(f.resultFile)).mode & 0o777, 0o600);
});

test("private key mismatch fails before any request", async (t) => {
  const f = await fixture(t);
  const other = generatedKeyFiles(f.directory, "other");
  await writeFile(other.privatePath, `${other.privateValue}\n`, { mode: 0o600 });
  let requests = 0;
  const port = await listen(t, (_request, response) => {
    requests += 1;
    response.writeHead(200, {
      "Content-Type": "application/json",
      "Cache-Control": "no-store",
    });
    response.end('{"verdict":"accepted"}\n');
  });
  await assert.rejects(
    runSignedProbe({ ...options(f, port), privateKeyFile: other.privatePath }),
    (error) => error instanceof SmokeRelayError && error.code === "key_mismatch",
  );
  assert.equal(requests, 0);
});

test("non-loopback endpoint and inexact verdict fail closed", async (t) => {
  const f = await fixture(t);
  await assert.rejects(
    runSignedProbe({ ...options(f, 80), url: "https://witmail.net/v1/internal/agent-email:ingest" }),
    (error) => error instanceof SmokeRelayError && error.code === "invalid_url",
  );

  const port = await listen(t, (request, response) => {
    if (request.url === "/v1/email/address") {
      respondWithOwnerAddress(response, f);
      return;
    }
    response.writeHead(200, {
      "Content-Type": "application/json",
      "Cache-Control": "no-store",
    });
    response.end('{"verdict":"accepted","detail":"leak"}\n');
  });
  await assert.rejects(
    runSignedProbe(options(f, port)),
    (error) => error instanceof SmokeRelayError && error.code === "unexpected_verdict",
  );
});

test("the disabled owner gate is exact and still permits one discard probe", async (t) => {
  const f = await fixture(t);
  let ownerRequests = 0;
  let relayRequests = 0;
  const port = await listen(t, (request, response) => {
    if (request.url === "/v1/email/address") {
      ownerRequests += 1;
      respondWithDisabledOwner(response);
      return;
    }
    relayRequests += 1;
    response.writeHead(200, {
      "Content-Type": "application/json",
      "Cache-Control": "no-store",
    });
    response.end('{"verdict":"feature_disabled"}\n');
  });
  assert.deepEqual(await runSignedProbe(options(f, port, "feature_disabled")), {
    owner_gate: "feature_disabled",
    http_status: 200,
    verdict: "feature_disabled",
  });
  assert.equal(ownerRequests, 1);
  assert.equal(relayRequests, 1);
});

test("an inexact owner response fails before relay ingest", async (t) => {
  const f = await fixture(t);
  let relayRequests = 0;
  const port = await listen(t, (request, response) => {
    if (request.url === "/v1/email/address") {
      response.writeHead(200, {
        "Content-Type": "application/json",
        "Cache-Control": "private, no-store",
      });
      response.end('{"schema_version":"witself.v0","address":{"account_id":"acc_wrong"}}\n');
      return;
    }
    relayRequests += 1;
    response.writeHead(500);
    response.end();
  });
  await assert.rejects(
    runSignedProbe(options(f, port)),
    (error) => error instanceof SmokeRelayError && error.code === "unexpected_owner_gate",
  );
  assert.equal(relayRequests, 0);
});
