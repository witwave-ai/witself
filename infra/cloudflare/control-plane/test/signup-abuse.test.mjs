// Request-boundary abuse-control tests. Every public assertion enters through
// the Worker's real fetch export; the signup namespace below hosts the real
// AccountSignup Durable Object class in deterministic in-memory storage.
import assert from "node:assert/strict";
import { timingSafeEqual as nodeTimingSafeEqual } from "node:crypto";
import { register } from "node:module";
import test from "node:test";

register(new URL("./fixtures/cloudflare-containers-loader.mjs", import.meta.url));

const {
  AccountSignup,
  default: worker,
} = await import("../src/index.js");
const {
  signupIPScope,
  utcDayBucket,
} = await import("../src/signup-counters.mjs");

if (typeof crypto.subtle.timingSafeEqual !== "function") {
  Object.defineProperty(
    Object.getPrototypeOf(crypto.subtle),
    "timingSafeEqual",
    {
      configurable: true,
      value(left, right) {
        return nodeTimingSafeEqual(Buffer.from(left), Buffer.from(right));
      },
    },
  );
}

const ORIGIN = "https://cp.test.invalid";
const SOURCE_IP = "203.0.113.42";
const INVITE = "early-access";

class Storage {
  constructor() {
    this.values = new Map();
  }

  async get(key) {
    const value = this.values.get(key);
    return value === undefined ? undefined : structuredClone(value);
  }

  async put(key, value) {
    this.values.set(key, structuredClone(value));
  }

  async delete(key) {
    this.values.delete(key);
  }

  async list({ prefix = "" } = {}) {
    return new Map(
      [...this.values]
        .filter(([key]) => key.startsWith(prefix))
        .map(([key, value]) => [key, structuredClone(value)]),
    );
  }

  async transaction(callback) {
    const staged = new Map(
      [...this.values].map(([key, value]) => [key, structuredClone(value)]),
    );
    const transaction = {
      get: async (key) => {
        const value = staged.get(key);
        return value === undefined ? undefined : structuredClone(value);
      },
      put: async (key, value) => staged.set(key, structuredClone(value)),
      delete: async (key) => staged.delete(key),
      list: async ({ prefix = "" } = {}) =>
        new Map(
          [...staged]
            .filter(([key]) => key.startsWith(prefix))
            .map(([key, value]) => [key, structuredClone(value)]),
        ),
    };
    const result = await callback(transaction);
    this.values = staged;
    return result;
  }
}

class Directory {
  constructor() {
    this.values = new Map();
  }

  async get(key, options) {
    const value = this.values.get(key);
    if (value === undefined) return null;
    return options?.type === "json" ? JSON.parse(value) : value;
  }

  async put(key, value) {
    this.values.set(key, value);
  }
}

class SignupNamespace {
  constructor(env) {
    this.env = env;
    this.objects = new Map();
    this.storages = new Map();
    this.counterCalls = [];
    this.runBodies = [];
    this.loseNextCounterResponse = false;
  }

  idFromName(name) {
    return { name };
  }

  storage(name) {
    let storage = this.storages.get(name);
    if (!storage) {
      storage = new Storage();
      this.storages.set(name, storage);
    }
    return storage;
  }

  object(name) {
    let object = this.objects.get(name);
    if (!object) {
      object = new AccountSignup(
        { id: { name }, storage: this.storage(name) },
        this.env,
      );
      this.objects.set(name, object);
    }
    return object;
  }

  get(id) {
    return {
      fetch: async (request) => {
        const pathname = new URL(request.url).pathname;
        if (pathname === "/run") {
          this.runBodies.push(await request.clone().text());
        }
        const response = await this.object(id.name).fetch(request);
        if (pathname === "/counter/consume") {
          this.counterCalls.push({
            scope: id.name,
            body: await response.clone().json(),
          });
          if (this.loseNextCounterResponse) {
            this.loseNextCounterResponse = false;
            throw new Error("simulated lost committed counter response");
          }
        }
        return response;
      },
    };
  }
}

function signupEnv(overrides = {}) {
  const env = {
    DIRECTORY: new Directory(),
    // The template COMMITS these dark defaults; the invariance test must
    // run in the production shape.
    CP_SIGNUP_DAILY_LIMIT_PER_IP: "0",
    CP_SIGNUP_DAILY_LIMIT_GLOBAL: "0",
    ...overrides,
  };
  const namespace = new SignupNamespace(env);
  env.ACCOUNT_SIGNUP = namespace;
  return { env, namespace };
}

function signupRequest(provisionID, fields = {}, headers = {}) {
  return new Request(`${ORIGIN}/v1/accounts`, {
    method: "POST",
    headers: {
      "CF-Connecting-IP": SOURCE_IP,
      "Content-Type": "application/json",
      ...headers,
    },
    body: JSON.stringify({
      provision_id: provisionID,
      email: "person@example.test",
      display_name: "Person",
      invite: INVITE,
      ...fields,
    }),
  });
}

function run(request, env) {
  return worker.fetch(request, env, { waitUntil() {} });
}

function limiter(success) {
  const calls = [];
  return {
    calls,
    async limit(input) {
      calls.push(structuredClone(input));
      return { success };
    },
  };
}

test("dark defaults preserve signup response bytes and keep the challenge route dark", async () => {
  const { env, namespace } = signupEnv();
  const response = await run(signupRequest("prov-dark"), env);

  assert.equal(response.status, 403);
  assert.equal(
    await response.text(),
    '{"schema_version":"witself.v0","error":"invalid invite: unknown code"}',
  );
  assert.deepEqual(namespace.runBodies, [
    '{"provision_id":"prov-dark","email":"person@example.test",' +
      '"display_name":"Person","invite":"early-access",' +
      '"origin":"https://cp.test.invalid"}',
  ]);
  assert.deepEqual(
    [...namespace.storage("provision:prov-dark").values.keys()],
    ["account-signup"],
  );
  const state = await namespace.storage("provision:prov-dark").get(
    "account-signup",
  );
  assert.deepEqual(Object.keys(state), [
    "schema_version",
    "revision",
    "phase",
    "provision_id",
    "request_fingerprint",
    "cell",
    "account",
    "created_at",
    "email_attempted",
    "verification_email_sent",
  ]);
  assert.equal(state.phase, "initialized");

  const challenge = await run(
    new Request(`${ORIGIN}/signup/challenge`),
    env,
  );
  assert.equal(challenge.status, 404);
  assert.equal(await challenge.text(), "not found\n");
});

test("public and signup burst limiters return the shared 429", async (t) => {
  const logs = [];
  t.mock.method(console, "log", (...values) => logs.push(values.join(" ")));
  const publicLimiter = limiter(false);
  const publicResponse = await run(
    new Request(`${ORIGIN}/billing/success`, {
      headers: { "CF-Connecting-IP": SOURCE_IP },
    }),
    { PUBLIC_IP_LIMITER: publicLimiter },
  );
  assert.equal(publicResponse.status, 429);
  const limitedBody =
    '{"schema_version":"witself.v0","error":"too many requests — try again later"}';
  assert.equal(
    await publicResponse.text(),
    limitedBody,
  );
  assert.deepEqual(publicLimiter.calls, [{ key: SOURCE_IP }]);

  let durableCalls = 0;
  const signupLimiter = limiter(false);
  const signupResponse = await run(
    signupRequest("prov-burst"),
    {
      SIGNUP_IP_LIMITER: signupLimiter,
      ACCOUNT_SIGNUP: {
        idFromName: (name) => ({ name }),
        get: () => ({
          fetch: async () => {
            durableCalls += 1;
            return Response.json({ ok: true });
          },
        }),
      },
    },
  );
  assert.equal(signupResponse.status, 429);
  assert.equal(await signupResponse.text(), limitedBody);
  assert.deepEqual(signupLimiter.calls, [{ key: SOURCE_IP }]);
  assert.equal(durableCalls, 0);
  assert.equal(logs.length, 2);
  assert.match(logs[0], /^rate-limit denied scope=public-ip:[0-9a-f]{64}$/);
  assert.match(logs[1], /^rate-limit denied scope=signup-ip:[0-9a-f]{64}$/);
  assert.equal(logs.some((entry) => entry.includes(SOURCE_IP)), false);
});

test("the public limiter exempts internal bridge and support-email intake", async () => {
  for (const [path, expectedStatus] of [
    ["/v1/internal/not-a-route", 401],
    ["/v1/intake/support-email", 404],
  ]) {
    const publicLimiter = limiter(false);
    const response = await run(
      new Request(`${ORIGIN}${path}`, { method: "POST" }),
      { PUBLIC_IP_LIMITER: publicLimiter },
    );
    assert.equal(response.status, expectedStatus);
    assert.deepEqual(publicLimiter.calls, [], `${path} was edge limited`);
  }
});

test("enabled Turnstile requires a challenge token and reports provider outage", async (t) => {
  const required = signupEnv({
    CP_SIGNUP_TURNSTILE_ENABLED: "true",
    CP_SIGNUP_TURNSTILE_SECRET_KEY: "turnstile-secret",
  });
  const missing = await run(signupRequest("prov-turnstile"), required.env);
  assert.equal(missing.status, 403);
  assert.deepEqual(await missing.json(), {
    schema_version: "witself.v0",
    error: "turnstile challenge required",
    challenge_url: `${ORIGIN}/signup/challenge`,
  });

  let verifies = 0;
  t.mock.method(globalThis, "fetch", async () => {
    verifies += 1;
    throw new Error("siteverify unavailable");
  });
  const outage = signupEnv({
    CP_SIGNUP_TURNSTILE_ENABLED: "true",
    CP_SIGNUP_TURNSTILE_SECRET_KEY: "turnstile-secret",
  });
  const unavailable = await run(
    signupRequest("prov-outage", { turnstile_token: "challenge-token" }),
    outage.env,
  );
  assert.equal(unavailable.status, 503);
  assert.deepEqual(await unavailable.json(), {
    schema_version: "witself.v0",
    error: "turnstile verification unavailable",
    retryable: true,
  });
  assert.equal(verifies, 1);
});

test("the enabled challenge page escapes its site key and pins Turnstile CSP", async () => {
  const siteKey = 'site\"><script>bad()</script>';
  const response = await run(
    new Request(`${ORIGIN}/signup/challenge`),
    {
      CP_SIGNUP_TURNSTILE_ENABLED: "true",
      CP_SIGNUP_TURNSTILE_SECRET_KEY: "turnstile-secret",
      CP_SIGNUP_TURNSTILE_SITE_KEY: siteKey,
    },
  );
  assert.equal(response.status, 200);
  const body = await response.text();
  assert.match(
    body,
    /data-sitekey="site&quot;&gt;&lt;script&gt;bad\(\)&lt;\/script&gt;"/,
  );
  assert.ok(!body.includes(siteKey));
  assert.match(body, /id="challenge-token"/);
  const csp = response.headers.get("Content-Security-Policy");
  assert.match(
    csp,
    /script-src .*https:\/\/challenges\.cloudflare\.com/,
  );
  assert.match(
    csp,
    /frame-src https:\/\/challenges\.cloudflare\.com/,
  );
});

test("a daily counter denial returns 429 without persisted provision state", async () => {
  const { env, namespace } = signupEnv({
    CP_SIGNUP_DAILY_LIMIT_PER_IP: "1",
    CP_SIGNUP_DAILY_LIMIT_GLOBAL: "0",
  });
  const scope = await signupIPScope(SOURCE_IP);
  const day = utcDayBucket();
  const counter = namespace.storage(scope);
  await counter.put("active-day", day);
  await counter.put(`count:${day}`, { day, count: 1 });

  const response = await run(signupRequest("prov-denied"), env);
  assert.equal(response.status, 429);
  assert.deepEqual(await response.json(), {
    schema_version: "witself.v0",
    error: "signup rate limit exceeded",
  });
  assert.equal(
    await namespace.storage("provision:prov-denied").get("account-signup"),
    undefined,
  );
});

test("an ambiguous counter response replays its marker without re-verifying Turnstile", async (t) => {
  let verifies = 0;
  t.mock.method(globalThis, "fetch", async () => {
    verifies += 1;
    return Response.json({ success: true });
  });
  const { env, namespace } = signupEnv({
    CP_SIGNUP_TURNSTILE_ENABLED: "true",
    CP_SIGNUP_TURNSTILE_SECRET_KEY: "turnstile-secret",
    CP_SIGNUP_DAILY_LIMIT_PER_IP: "1",
    CP_SIGNUP_DAILY_LIMIT_GLOBAL: "0",
  });
  namespace.loseNextCounterResponse = true;
  const request = (sourceIp = SOURCE_IP) =>
    signupRequest("prov-ambiguous", {
      turnstile_token: "challenge-token",
    }, {
      "CF-Connecting-IP": sourceIp,
    });

  const first = await run(request(), env);
  assert.equal(first.status, 502);
  const checkpoint = await namespace.storage("provision:prov-ambiguous").get(
    "account-signup",
  );
  assert.equal(checkpoint.phase, "abuse_preflight");
  assert.equal(checkpoint.turnstile_verified, true);

  const retry = await run(request("198.51.100.42"), env);
  assert.equal(retry.status, 403);
  assert.equal((await retry.json()).error, "invalid invite: unknown code");
  assert.equal(verifies, 1);
  assert.equal(namespace.counterCalls.length, 2);
  assert.equal(namespace.counterCalls[0].body.replayed, false);
  assert.equal(namespace.counterCalls[1].body.replayed, true);
  assert.equal(namespace.counterCalls[1].body.count, 1);
  assert.equal(namespace.counterCalls[1].scope, namespace.counterCalls[0].scope);
});
