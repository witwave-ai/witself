// Explicit subprocess acceptance adapter; ordinary Node tests do not start it.
// This closes fixture routes, but is not an OS network sandbox for the CLI.
import http from "node:http";
import { B2, makeSignupB2Fixture } from "./signup-b2-local.mjs";

const scenario = process.argv[2];
const MAX_BYTES = 64 * 1024;
const MAX_CONTROL_BYTES = 1024;
const MAX_SNAPSHOT_BYTES = 512 * 1024;
const LOST_ACK = "B2 simulated lost outer registration acknowledgement";
const sockets = new Set();
let fixture;
let origin;
let activeRequest;
let stopping;
let idle;
let firstManifest = true;
let dropped = false;
let connections = 0;
let requests = 0;
let lastCommandID = 0;
let controlCount = 0;
let controlBuffer = Buffer.alloc(0);
let controls = Promise.resolve();

function requireFixture(condition) {
  if (!condition) throw new Error("B2 loopback boundary refused");
}

function touch() {
  clearTimeout(idle);
  if (stopping) return;
  idle = setTimeout(() => stop(false), 30000);
}

async function line(value) {
  const text = JSON.stringify(value) + "\n";
  requireFixture(Buffer.byteLength(text) <= MAX_SNAPSHOT_BYTES);
  await new Promise((resolve, reject) => process.stdout.write(text, (error) => error ? reject(error) : resolve()));
}

const server = http.createServer({ maxHeaderSize: 8192 }, (incoming, outgoing) => {
  if (stopping || !fixture || activeRequest || ++requests > 16) {
    incoming.destroy();
    void stop(false);
    return;
  }
  touch();
  const task = serve(incoming, outgoing);
  activeRequest = task;
  task.then(() => {
    if (activeRequest === task) activeRequest = undefined;
    touch();
  }, () => {
    if (activeRequest === task) activeRequest = undefined;
    void stop(false);
  });
});
server.headersTimeout = 5000;
server.requestTimeout = 10000;
server.keepAliveTimeout = 1000;
server.maxHeadersCount = 32;
server.setTimeout(10000, () => stop(false));
server.on("connection", (socket) => {
  sockets.add(socket);
  socket.once("close", () => sockets.delete(socket));
  if (++connections > 32 || sockets.size > 4 || socket.remoteAddress !== "127.0.0.1" || stopping) {
    socket.destroy();
    void stop(false);
  }
});
for (const event of ["clientError", "upgrade", "connect", "checkContinue", "checkExpectation", "error"]) {
  server.on(event, () => stop(false));
}

async function responseBytes(response) {
  requireFixture(response instanceof Response && !response.redirected && !response.headers.has("Location"));
  if (!response.body) return Buffer.alloc(0);
  const reader = response.body.getReader();
  const chunks = [];
  let size = 0;
  try {
    while (true) {
      const { value, done } = await reader.read();
      if (done) return Buffer.concat(chunks, size);
      size += value.byteLength;
      requireFixture(size <= MAX_BYTES);
      chunks.push(Buffer.from(value));
    }
  } finally {
    await reader.cancel();
    reader.releaseLock();
  }
}

async function serve(incoming, outgoing) {
  const controller = new AbortController();
  const deadline = setTimeout(() => { controller.abort(); void stop(false); }, 15000);
  let intentionalDrop = false;
  incoming.once("aborted", () => { controller.abort(); void stop(false); });
  outgoing.once("close", () => {
    if (!outgoing.writableFinished && !intentionalDrop && !stopping) {
      controller.abort();
      void stop(false);
    }
  });
  try {
    requireFixture(incoming.httpVersion === "1.1" && incoming.socket.remoteAddress === "127.0.0.1");
    const path = incoming.url;
    const manifest = path === "/legal/versions.json";
    const bootstrap = path === "/v1/auth/bootstrap";
    const signup = path === "/v1/accounts";
    const reconsent = /^\/v1\/account-signups\/[A-Za-z0-9_-]{1,128}:reconsent$/.test(path);
    requireFixture((manifest && incoming.method === "GET") ||
      ((bootstrap || signup || reconsent) && incoming.method === "POST"));
    const headers = new Headers();
    const seen = new Set();
    const allowed = new Set(["host", "content-type", "content-length", "accept", "accept-encoding", "user-agent", "connection"]);
    for (let i = 0; i < incoming.rawHeaders.length; i += 2) {
      const name = incoming.rawHeaders[i].toLowerCase();
      const value = incoming.rawHeaders[i + 1];
      requireFixture(allowed.has(name) && !seen.has(name));
      seen.add(name);
      if (name === "host") requireFixture(value === origin.slice("http://".length));
      else if (name === "connection") requireFixture(value.toLowerCase() === "close" || value.toLowerCase() === "keep-alive");
      else headers.set(name, value);
    }
    requireFixture(seen.has("host"));
    if (headers.has("content-length")) {
      const length = headers.get("content-length");
      requireFixture(/^(0|[1-9][0-9]{0,4})$/.test(length) && Number(length) <= MAX_BYTES);
    }
    if (!manifest) requireFixture(/^application\/json(?:;\s*charset=utf-8)?$/i.test(headers.get("content-type") ?? ""));
    const chunks = [];
    let size = 0;
    for await (const chunk of incoming) {
      size += chunk.length;
      requireFixture(size <= MAX_BYTES);
      chunks.push(chunk);
    }
    requireFixture(incoming.complete && (!manifest || size === 0));
    if (headers.has("content-length")) requireFixture(Number(headers.get("content-length")) === size);
    // No client-supplied CF or forwarded identity header is allowed above.
    headers.set("CF-Connecting-IP", B2.sourceIP);
    const request = new Request(origin + path, {
      method: incoming.method, headers, redirect: "error", signal: controller.signal,
      ...(!manifest ? { body: Buffer.concat(chunks, size) } : {}),
    });
    let response;
    if (manifest) {
      response = await fixture.dispatchCLILegalManifest(request, firstManifest && scenario === "lost-reconsent-ack");
      firstManifest = false;
    } else if (bootstrap) {
      response = await fixture.dispatchCLIBootstrap(request);
    } else {
      try {
        response = await fixture.dispatchPublic(request);
      } catch (error) {
        requireFixture(scenario === "lost-reconsent-ack" && reconsent && !dropped && error?.message === LOST_ACK);
        const snapshot = fixture.snapshotRelationships();
        const successor = snapshot.states.original?.legal_successor;
        requireFixture(snapshot.counts.droppedOuterAck === 1 && snapshot.counts.reserve === 1 &&
          snapshot.states.original?.phase === "legal_rejected" && successor?.registered === true &&
          snapshot.states.candidate?.phase === "legal_reserved" &&
          successor.candidate.provision_id === snapshot.cli.candidateID &&
          successor.transition_id === snapshot.cli.transitionID &&
          snapshot.states.candidate.request_fingerprint === successor.request_fingerprint &&
          snapshot.states.candidate.legal_reservation.transition_id === snapshot.cli.transitionID &&
          snapshot.logicalAccounts === 0 && snapshot.counts.provision === 0 && snapshot.cli.bootstrap === 0);
        fixture.assertNoUnexpectedCalls();
        requireFixture(!outgoing.headersSent && !controller.signal.aborted);
        dropped = true;
        intentionalDrop = true;
        outgoing.destroy();
        return;
      }
    }
    requireFixture((manifest || bootstrap || reconsent) ? response.status === 200 : [201, 409].includes(response.status));
    const bytes = await responseBytes(response);
    fixture.assertNoUnexpectedCalls();
    requireFixture(!controller.signal.aborted && !stopping);
    for (const [name, value] of response.headers) {
      requireFixture(!["connection", "transfer-encoding", "upgrade", "content-encoding"].includes(name));
      if (name !== "content-length") outgoing.setHeader(name, value);
    }
    outgoing.statusCode = response.status;
    outgoing.setHeader("Content-Length", String(bytes.length));
    await new Promise((resolve, reject) => outgoing.end(bytes, (error) => error ? reject(error) : resolve()));
  } finally {
    clearTimeout(deadline);
  }
}

function control(text) {
  // Closed literal keys/values in either order also reject duplicate/aliased keys.
  const opFirst = /^\s*\{\s*"op"\s*:\s*"(snapshot|close)"\s*,\s*"id"\s*:\s*([1-9][0-9]{0,15})\s*\}\s*$/;
  const idFirst = /^\s*\{\s*"id"\s*:\s*([1-9][0-9]{0,15})\s*,\s*"op"\s*:\s*"(snapshot|close)"\s*\}\s*$/;
  requireFixture(opFirst.test(text) || idFirst.test(text));
  const command = JSON.parse(text);
  requireFixture(Number.isSafeInteger(command.id) && command.id > lastCommandID && ++controlCount <= 16);
  lastCommandID = command.id;
  return command;
}

async function handleControl(command) {
  requireFixture(!stopping && fixture);
  if (activeRequest) await activeRequest;
  fixture.assertNoUnexpectedCalls();
  touch();
  if (command.op === "snapshot") await line({ kind: "snapshot", id: command.id, value: fixture.snapshotRelationships() });
  else await stop(true, command.id);
}

async function stop(ok, id) {
  if (!ok) process.exitCode = 1;
  if (stopping) return stopping;
  stopping = (async () => {
    clearTimeout(idle);
    clearTimeout(overall);
    process.stdin.pause();
    // This adapter owns no child process. A hung fixture cleanup must fail,
    // rather than leave the listener or a blocked private pipe alive.
    const hardStop = setTimeout(() => process.exit(1), 3000);
    try {
      await new Promise((resolve) => {
        server.close(resolve);
        for (const socket of sockets) socket.destroy();
      });
      if (activeRequest) await activeRequest.catch(() => {});
      if (fixture) await fixture.close();
      if (ok && !process.exitCode) await line({ kind: "closed", id });
    } catch {
      process.exitCode = 1;
    } finally {
      clearTimeout(hardStop);
      process.stdin.destroy();
      if (process.exitCode) process.stderr.write("B2 loopback fixture failed\n");
    }
  })();
  return stopping;
}

const overall = setTimeout(() => stop(false), 180000);
process.stdin.on("data", (chunk) => {
  try {
    requireFixture(!stopping);
    controlBuffer = Buffer.concat([controlBuffer, chunk]);
    requireFixture(controlBuffer.length <= MAX_CONTROL_BYTES);
    let newline;
    while ((newline = controlBuffer.indexOf(10)) !== -1) {
      const text = new TextDecoder("utf-8", { fatal: true }).decode(controlBuffer.subarray(0, newline));
      controlBuffer = controlBuffer.subarray(newline + 1);
      const command = control(text);
      controls = controls.then(() => handleControl(command));
      controls.catch(() => stop(false));
    }
  } catch { void stop(false); }
});
process.stdin.on("end", () => { if (!stopping) void stop(false); });
process.stdin.on("error", () => stop(false));
process.stdout.on("error", () => stop(false));
for (const signal of ["SIGINT", "SIGTERM", "SIGHUP"]) process.on(signal, () => stop(false));
process.on("uncaughtException", () => stop(false));
process.on("unhandledRejection", () => stop(false));

try {
  requireFixture(/^22\./.test(process.versions.node) && process.argv.length === 3 &&
    ["current", "lost-reconsent-ack"].includes(scenario));
  await new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", resolve);
  });
  const address = server.address();
  requireFixture(address && typeof address === "object" && address.address === "127.0.0.1");
  origin = `http://127.0.0.1:${address.port}`;
  fixture = await makeSignupB2Fixture({ cliOrigin: origin });
  if (scenario === "lost-reconsent-ack") fixture.loseNextOuterAcknowledgement();
  requireFixture(!stopping);
  touch();
  await line({ kind: "ready", origin, pid: process.pid });
} catch { await stop(false); }
