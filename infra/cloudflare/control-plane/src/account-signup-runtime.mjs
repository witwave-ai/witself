import {
  consumeSignupCounter,
  parseSignupLimit,
  SIGNUP_GLOBAL_SCOPE,
  signupIPScope,
} from "./signup-counters.mjs";
import {
  turnstileEnabled,
  verifyTurnstileToken,
} from "./signup-turnstile.mjs";

const ACCOUNT_ID = /^[A-Za-z0-9_-]{1,128}$/;
const CELL_NAME = /^[a-z0-9-]{1,64}$/;
// Dark ToS/privacy consent version labels mirror the cell store's bounds:
// 1..64 ASCII label characters, starting with an alphanumeric.
const CONSENT_VERSION = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/;
const INVITE_CODE = /^[a-z0-9][a-z0-9-]{2,63}$/;
const PROVISION_ID = /^[A-Za-z0-9_-]{1,128}$/;
const SIGNUP_IP_SCOPE = /^signup-counter:ip:[0-9a-f]{64}$/;
const STATE_KEY = "account-signup";
const INVITE_COUNT_KEY = "invite-count";
const INVITE_USE_PREFIX = "invite-use:";

const PHASE = {
  abuse_preflight: -1,
  initialized: 0,
  invite_reserved: 1,
  cell_selected: 2,
  protocol_verified: 3,
  target_reserved: 4,
  cell_acknowledged: 5,
  target_attached: 6,
  pending_projected: 7,
  route_projected: 8,
  resident_promoted: 9,
  completed: 10,
};

function json(value, status = 200) {
  return new Response(JSON.stringify(value), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function errorResponse(message, status, fields = {}) {
  return json({
    schema_version: "witself.v0",
    error: message,
    ...fields,
  }, status);
}

function isObject(value) {
  return value !== null && typeof value === "object" &&
    !Array.isArray(value);
}

class SignupError extends Error {
  constructor(message, status = 500, options = {}) {
    super(message, options);
    this.name = "SignupError";
    this.status = status;
    this.responseFields = options.responseFields ?? {};
  }
}

function fail(message, status = 500, options = {}) {
  throw new SignupError(message, status, options);
}

function phaseAtLeast(state, phase) {
  return PHASE[state.phase] >= PHASE[phase];
}

function normalizedRequest(input) {
  if (!isObject(input)) {
    fail("invalid signup request", 400);
  }
  const provisionID = typeof input.provision_id === "string"
    ? input.provision_id.trim()
    : "";
  const email = typeof input.email === "string"
    ? input.email.trim().toLowerCase()
    : "";
  const displayName = input.display_name == null
    ? ""
    : typeof input.display_name === "string"
    ? input.display_name.trim()
    : null;
  const invite = typeof input.invite === "string" ? input.invite : "";
  const sourceIP = input.source_ip == null
    ? ""
    : typeof input.source_ip === "string"
    ? input.source_ip
    : null;
  const turnstileToken = input.turnstile_token == null
    ? ""
    : typeof input.turnstile_token === "string"
    ? input.turnstile_token
    : null;
  const consentTermsVersion = input.consent_terms_version == null
    ? ""
    : typeof input.consent_terms_version === "string"
    ? input.consent_terms_version
    : null;
  const consentPrivacyVersion = input.consent_privacy_version == null
    ? ""
    : typeof input.consent_privacy_version === "string"
    ? input.consent_privacy_version
    : null;
  if (!PROVISION_ID.test(provisionID)) {
    fail(
      "provision_id is required and must be a nonempty opaque identifier",
      400,
    );
  }
  if (!email || !email.includes("@")) {
    fail("valid email required", 400);
  }
  if (displayName === null || displayName.length > 200) {
    fail("display_name must be a string of at most 200 characters", 400);
  }
  if (!INVITE_CODE.test(invite)) {
    fail("invite code required", 403);
  }
  if (sourceIP === null) {
    fail("source_ip must be a string", 400);
  }
  if (turnstileToken === null) {
    fail("turnstile_token must be a string", 400);
  }
  if (consentTermsVersion === null || consentPrivacyVersion === null) {
    fail("consent versions must be strings", 400);
  }
  if ((consentTermsVersion === "") !== (consentPrivacyVersion === "")) {
    fail(
      "consent_terms_version and consent_privacy_version must be provided together",
      400,
    );
  }
  if (
    consentTermsVersion !== "" &&
    (
      !CONSENT_VERSION.test(consentTermsVersion) ||
      !CONSENT_VERSION.test(consentPrivacyVersion)
    )
  ) {
    fail(
      "consent versions must be 1 to 64 characters, starting with an alphanumeric and containing only alphanumerics, dots, underscores, or hyphens",
      400,
    );
  }
  return {
    provision_id: provisionID,
    email,
    display_name: displayName,
    invite,
    source_ip: sourceIP,
    turnstile_token: turnstileToken,
    consent_terms_version: consentTermsVersion,
    consent_privacy_version: consentPrivacyVersion,
  };
}

function signupChallengeURL(origin) {
  if (typeof origin !== "string" || origin.length < 1) {
    return "/signup/challenge";
  }
  try {
    const parsed = new URL(origin);
    if (parsed.protocol !== "https:" && parsed.protocol !== "http:") {
      return "/signup/challenge";
    }
    return `${parsed.origin}/signup/challenge`;
  } catch {
    return "/signup/challenge";
  }
}

async function sha256Hex(value) {
  const bytes = new TextEncoder().encode(value);
  const digest = await globalThis.crypto.subtle.digest("SHA-256", bytes);
  return [...new Uint8Array(digest)]
    .map((byte) => byte.toString(16).padStart(2, "0"))
    .join("");
}

function requestCanonical(input) {
  const canonical = [
    input.provision_id,
    input.email,
    input.display_name,
    input.invite,
  ];
  // Dark consent capture: a consent-less request keeps the exact historical
  // canonical bytes, so its durable fingerprint never changes across deploys.
  // Recorded consent appends a domain-separated consent/v1 block, making a
  // retry with drifted consent a different signup request.
  if (input.consent_terms_version !== "") {
    canonical.push(
      "consent/v1",
      input.consent_terms_version,
      input.consent_privacy_version,
    );
  }
  return JSON.stringify(canonical);
}

function inviteVerdict(invite, uses, nowMs) {
  if (!invite) return { valid: false, reason: "unknown code" };
  if (invite.enabled === false) {
    return { valid: false, reason: "disabled" };
  }
  if (invite.not_before && nowMs < Date.parse(invite.not_before)) {
    return { valid: false, reason: "not yet valid" };
  }
  if (invite.expires_at && nowMs >= Date.parse(invite.expires_at)) {
    return { valid: false, reason: "expired" };
  }
  if (
    Number.isFinite(invite.max_uses) &&
    uses >= invite.max_uses
  ) {
    return { valid: false, reason: "fully used" };
  }
  return { valid: true };
}

function inviteSnapshot(invite) {
  return {
    ...(typeof invite.cell === "string" ? { cell: invite.cell } : {}),
    ...(typeof invite.region === "string" ? { region: invite.region } : {}),
  };
}

async function responseBody(response) {
  const text = await response.text().catch(() => "");
  let body = null;
  try {
    body = JSON.parse(text);
  } catch {
    // External acknowledgements are accepted only as exact JSON.
  }
  return { text, body };
}

/**
 * One instance named provision:<provision_id> serializes a public signup.
 * Instances named invite:<code> serialize exact invite consumption, and
 * signup-counter:<scope> instances serialize daily abuse counters. No role
 * ever persists the bootstrap token returned by a cell.
 */
export class DurableAccountSignup {
  constructor(ctx, env, dependencies = {}) {
    this.ctx = ctx;
    this.storage = ctx.storage;
    this.env = env;
    this.objectName = ctx.id?.name ?? "";
    // Cloudflare's native fetch must retain globalThis as its receiver. A
    // direct assignment followed by this.fetchImpl(...) binds the Durable
    // Object instance instead and fails with an illegal invocation.
    this.fetchImpl =
      dependencies.fetch ?? ((...args) => globalThis.fetch(...args));
    this.placeAccount = dependencies.placeAccount;
    this.sendVerification = dependencies.sendVerification ?? (() => false);
    this.logVerification = dependencies.logVerification ?? (() => {});
    this.now = dependencies.now ?? (() => new Date());
    this.hash = dependencies.hash ?? sha256Hex;
    this.verifyTurnstile =
      dependencies.verifyTurnstile ??
      ((input) => verifyTurnstileToken({
        ...input,
        fetchImpl: this.fetchImpl,
      }));
    this.consumeCounterImpl =
      dependencies.consumeCounter ??
      ((input) => this.requestCounterAuthority(input));
    this.reserveInviteImpl =
      dependencies.reserveInvite ??
      ((input) => this.requestInviteAuthority(input));
    this.targetRequestImpl =
      dependencies.targetRequest ??
      ((cellName, path, payload) =>
        this.requestTargetCoordinator(cellName, path, payload));
    this.queue = Promise.resolve();
  }

  fetch(request) {
    return this.serial(() => this.handleFetch(request));
  }

  serial(work) {
    const result = this.queue.then(work, work);
    this.queue = result.catch(() => {});
    return result;
  }

  async handleFetch(request) {
    const url = new URL(request.url);
    if (request.method !== "POST") {
      return errorResponse("account signup endpoint not found", 404);
    }
    let input;
    try {
      input = await request.json();
    } catch {
      return errorResponse("invalid JSON body", 400);
    }
    try {
      if (url.pathname === "/run") {
        return await this.run(input);
      }
      if (url.pathname === "/invite/reserve") {
        return await this.reserveInvite(input);
      }
      if (url.pathname === "/counter/consume") {
        return await this.consumeCounter(input);
      }
      return errorResponse("account signup endpoint not found", 404);
    } catch (error) {
      return errorResponse(
        String(error?.message ?? error),
        error instanceof SignupError ? error.status : 500,
        error instanceof SignupError ? error.responseFields : {},
      );
    }
  }

  async run(input) {
    const request = normalizedRequest(input);
    if (this.objectName !== `provision:${request.provision_id}`) {
      fail("signup Durable Object identity mismatch", 400);
    }
    const fingerprint = await this.hash(requestCanonical(request));
    let state = await this.storage.get(STATE_KEY);
    if (state) {
      if (
        state.provision_id !== request.provision_id ||
        state.request_fingerprint !== fingerprint
      ) {
        fail(
          "provision_id was already used for a different signup request",
          409,
        );
      }
      if (state.phase === "abuse_preflight") {
        if (
          !SIGNUP_IP_SCOPE.test(state.signup_ip_scope ?? "") ||
          (
            Object.hasOwn(state, "turnstile_verified") &&
            state.turnstile_verified !== true
          )
        ) {
          fail("signup abuse checkpoint is invalid", 500);
        }
      }
    }

    const perIPLimit = parseSignupLimit(
      this.env.CP_SIGNUP_DAILY_LIMIT_PER_IP,
    );
    const globalLimit = parseSignupLimit(
      this.env.CP_SIGNUP_DAILY_LIMIT_GLOBAL,
    );
    let turnstileVerified = state?.turnstile_verified === true;
    if (!state) {
      if (turnstileEnabled(this.env)) {
        const verdict = await this.verifyTurnstile({
          secretKey: this.env.CP_SIGNUP_TURNSTILE_SECRET_KEY,
          token: request.turnstile_token,
          remoteIp: request.source_ip,
        });
        if (verdict?.ok !== true) {
          if (verdict?.reason === "invalid") {
            fail("turnstile challenge required", 403, {
              responseFields: {
                challenge_url: signupChallengeURL(input.origin),
              },
            });
          }
          fail("turnstile verification unavailable", 503, {
            responseFields: { retryable: true },
          });
        }
        turnstileVerified = true;
      }

      // Persist the selected hashed IP scope before any counter self-call.
      // A retry can then replay the exact marker even if the caller's network
      // changes after a committed response is lost. Successful Turnstile
      // verification shares this preflight so it is never repeated either.
      if (perIPLimit > 0 || globalLimit > 0) {
        state = {
          schema_version: "witself.signup.v1",
          revision: 0,
          phase: "abuse_preflight",
          provision_id: request.provision_id,
          request_fingerprint: fingerprint,
          cell: null,
          account: null,
          created_at: this.now().toISOString(),
          email_attempted: false,
          verification_email_sent: false,
          signup_ip_scope: await signupIPScope(
            request.source_ip,
            this.hash,
          ),
          ...(turnstileVerified ? { turnstile_verified: true } : {}),
        };
        await this.storage.put(STATE_KEY, state);
      }
    }

    if (!state || state.phase === "abuse_preflight") {
      if (perIPLimit > 0) {
        const scope = state?.signup_ip_scope ??
          await signupIPScope(request.source_ip, this.hash);
        const verdict = await this.consumeCounterImpl({
          scope,
          provision_id: request.provision_id,
          limit: perIPLimit,
        });
        if (typeof verdict?.allowed !== "boolean") {
          fail("signup counter returned an invalid verdict", 502);
        }
        if (!verdict.allowed) {
          console.log(`signup: daily counter denied scope ${scope}`);
          if (state?.phase === "abuse_preflight") {
            await this.storage.delete(STATE_KEY);
          }
          fail("signup rate limit exceeded", 429);
        }
      }

      if (globalLimit > 0) {
        const verdict = await this.consumeCounterImpl({
          scope: SIGNUP_GLOBAL_SCOPE,
          provision_id: request.provision_id,
          limit: globalLimit,
        });
        if (typeof verdict?.allowed !== "boolean") {
          fail("signup counter returned an invalid verdict", 502);
        }
        if (!verdict.allowed) {
          console.log(
            `signup: daily counter denied scope ${SIGNUP_GLOBAL_SCOPE}`,
          );
          if (state?.phase === "abuse_preflight") {
            await this.storage.delete(STATE_KEY);
          }
          fail("signup rate limit exceeded", 429);
        }
      }

      if (state?.phase === "abuse_preflight") {
        const { signup_ip_scope: _signupIPScope, ...initialized } = state;
        state = await this.save({
          ...initialized,
          phase: "initialized",
        });
      } else {
        state = {
          schema_version: "witself.signup.v1",
          revision: 0,
          phase: "initialized",
          provision_id: request.provision_id,
          request_fingerprint: fingerprint,
          cell: null,
          account: null,
          created_at: this.now().toISOString(),
          email_attempted: false,
          verification_email_sent: false,
          ...(turnstileVerified ? { turnstile_verified: true } : {}),
        };
        await this.storage.put(STATE_KEY, state);
      }
    }

    if (!phaseAtLeast(state, "invite_reserved")) {
      const invite = await this.reserveInviteImpl({
        invite: request.invite,
        provision_id: state.provision_id,
        request_fingerprint: state.request_fingerprint,
      });
      state = await this.advance(state, "invite_reserved", {
        placement: invite.snapshot,
      });
    }

    if (!phaseAtLeast(state, "cell_selected")) {
      if (typeof this.placeAccount !== "function") {
        fail("signup placement authority is unavailable", 503);
      }
      const placed = await this.placeAccount(state.placement);
      if (placed?.fail instanceof Response) {
        return placed.fail;
      }
      const cell = placed?.cell;
      const registrationID =
        cell?.registration_id ?? cell?.registered_at ?? null;
      if (
        !cell ||
        !CELL_NAME.test(cell.name ?? "") ||
        !registrationID
      ) {
        fail("no eligible cell registration is available", 503);
      }
      state = await this.advance(state, "cell_selected", {
        cell: {
          name: cell.name,
          registration_id: registrationID,
        },
      });
    }

    let cell = await this.exactCell(state);
    if (!phaseAtLeast(state, "protocol_verified")) {
      await this.requireProvisionProtocol(cell);
      state = await this.advance(state, "protocol_verified");
    }

    if (!phaseAtLeast(state, "target_reserved")) {
      const begun = await this.targetRequestImpl(
        state.cell.name,
        "/provision/begin",
        {
          provision_id: state.provision_id,
          registration_id: state.cell.registration_id,
        },
      );
      if (
        begun?.provision_id !== state.provision_id ||
        begun?.registration_id !== state.cell.registration_id
      ) {
        fail("target cell returned an invalid provisioning reservation", 502);
      }
      state = await this.advance(state, "target_reserved");
    }

    // Always replay the same cell operation, including after completion. The
    // cell reissues a fresh bootstrap token only while its exact receipt still
    // points at a pending, unclaimed account. The token stays on this stack.
    cell = await this.exactCell(state);
    const cellReceipt = await this.provisionAtCell(state, cell, request);
    const account = cellReceipt.account;
    if (state.account) {
      for (
        const field of ["account_id", "operator_id", "status"]
      ) {
        if (state.account[field] !== account[field]) {
          fail("cell provisioning receipt changed across replay", 409);
        }
      }
    } else {
      state = await this.advance(state, "cell_acknowledged", {
        account: {
          account_id: account.account_id,
          operator_id: account.operator_id,
          status: account.status,
        },
      });
    }

    if (!phaseAtLeast(state, "target_attached")) {
      const attached = await this.targetRequestImpl(
        state.cell.name,
        "/provision/attach",
        {
          provision_id: state.provision_id,
          registration_id: state.cell.registration_id,
          account_id: state.account.account_id,
          route_epoch: 0,
        },
      );
      if (
        attached?.provision_id !== state.provision_id ||
        attached?.account_id !== state.account.account_id ||
        attached?.attached !== true
      ) {
        fail("target cell returned an invalid provisioning attachment", 502);
      }
      state = await this.advance(state, "target_attached");
    }

    const pendingKey = `pending:${state.account.account_id}`;
    const pending = {
      cell: state.cell.name,
      created_at: state.created_at,
      route_epoch: 0,
      provision_id: state.provision_id,
    };
    if (!phaseAtLeast(state, "pending_projected")) {
      await this.env.DIRECTORY.put(pendingKey, JSON.stringify(pending));
      state = await this.advance(state, "pending_projected");
    }

    const routeKey = `acct:${state.account.account_id}`;
    const route = {
      cell: state.cell.name,
      endpoint: cell.endpoint,
      region: cell.region ?? null,
      region_code: cell.region_code ?? null,
      cell_registration_id: state.cell.registration_id,
      epoch: 0,
    };
    if (!phaseAtLeast(state, "route_projected")) {
      const current = await this.env.DIRECTORY.get(routeKey, {
        type: "json",
      });
      if (
        current &&
        (
          current.cell !== route.cell ||
          current.cell_registration_id !== route.cell_registration_id ||
          (Number.isSafeInteger(current.epoch) ? current.epoch : 0) !== 0
        )
      ) {
        fail("account route changed before signup projection", 409);
      }
      await this.env.DIRECTORY.put(routeKey, JSON.stringify(route));
      state = await this.advance(state, "route_projected");
    }

    if (!phaseAtLeast(state, "resident_promoted")) {
      const promoted = await this.targetRequestImpl(
        state.cell.name,
        "/provision/promote",
        {
          provision_id: state.provision_id,
          registration_id: state.cell.registration_id,
          account_id: state.account.account_id,
          route_epoch: 0,
        },
      );
      if (
        promoted?.provision_id !== state.provision_id ||
        promoted?.account_id !== state.account.account_id ||
        promoted?.resident !== true
      ) {
        fail("target cell returned an invalid resident promotion", 502);
      }
      state = await this.advance(state, "resident_promoted");
    }

    if (!phaseAtLeast(state, "completed")) {
      state = await this.advance(state, "completed");
    }

    if (!state.email_attempted) {
      // Email is best-effort at-most-once. Persist intent before the external
      // send; if the Worker dies after this checkpoint, the explicit resend
      // endpoint is the recovery path rather than duplicate delivery.
      state = await this.save({
        ...state,
        email_attempted: true,
        verification_email_sent: false,
      });
      let emailSent = false;
      try {
        emailSent = await this.sendVerification({
          origin: typeof input.origin === "string" ? input.origin : "",
          email: request.email,
          account_id: state.account.account_id,
          cell_name: state.cell.name,
        });
        if (emailSent) {
          const emailedPending = {
            ...pending,
            emails_sent: 1,
            last_email_at: this.now().toISOString(),
          };
          await this.env.DIRECTORY.put(
            pendingKey,
            JSON.stringify(emailedPending),
          );
          await this.logVerification({
            cell,
            account_id: state.account.account_id,
            email: request.email,
          });
        }
      } catch (error) {
        console.log(
          `signup: verification email for ${state.account.account_id} failed: ${error}`,
        );
      }
      if (emailSent) {
        state = await this.save({
          ...state,
          verification_email_sent: true,
        });
      }
    }

    return json({
      schema_version: "witself.v0",
      provision_id: state.provision_id,
      replayed: cellReceipt.replayed,
      account_id: account.account_id,
      operator_id: account.operator_id,
      email: account.email,
      status: account.status,
      verification_email_sent: state.verification_email_sent,
      cell: { name: state.cell.name, endpoint: cell.endpoint },
      bootstrap_token: account.bootstrap_token,
    }, 201);
  }

  async reserveInvite(input) {
    const inviteCode = input?.invite;
    const provisionID = input?.provision_id;
    const fingerprint = input?.request_fingerprint;
    if (
      !INVITE_CODE.test(inviteCode ?? "") ||
      !PROVISION_ID.test(provisionID ?? "") ||
      !/^[0-9a-f]{64}$/.test(fingerprint ?? "") ||
      this.objectName !== `invite:${inviteCode}`
    ) {
      fail("invalid invite reservation", 400);
    }
    const inviteKey = `invite:${inviteCode}`;
    const invite = await this.env.DIRECTORY.get(inviteKey, {
      type: "json",
    });
    const generation = invite?.created_at ?? "legacy";
    const nowMs = this.now().getTime();
    if (typeof this.storage.transaction !== "function") {
      fail("invite reservation transaction is unavailable", 503);
    }
    const outcome = await this.storage.transaction(async (txn) => {
      const useKey = `${INVITE_USE_PREFIX}${provisionID}`;
      const existing = await txn.get(useKey);
      if (existing) {
        if (existing.request_fingerprint !== fingerprint) {
          fail(
            "provision_id conflicts with an existing invite reservation",
            409,
          );
        }
        const countState = await txn.get(INVITE_COUNT_KEY);
        return {
          count: countState?.count ?? 0,
          snapshot: existing.snapshot,
        };
      }

      let countState = await txn.get(INVITE_COUNT_KEY);
      if (!countState) {
        countState = {
          generation,
          count:
            Number.isSafeInteger(invite?.uses) && invite.uses >= 0
              ? invite.uses
              : 0,
        };
      } else if (countState.generation !== generation) {
        fail(
          "invite generation changed; use a new provision_id",
          409,
        );
      }
      const verdict = inviteVerdict(invite, countState.count, nowMs);
      if (!verdict.valid) {
        fail(`invalid invite: ${verdict.reason}`, 403);
      }
      const snapshot = inviteSnapshot(invite);
      const nextCount = countState.count + 1;
      await txn.put(useKey, {
        provision_id: provisionID,
        request_fingerprint: fingerprint,
        snapshot,
        reserved_at: this.now().toISOString(),
      });
      await txn.put(INVITE_COUNT_KEY, {
        generation,
        count: nextCount,
      });
      return { count: nextCount, snapshot };
    });

    // KV remains an admin/read projection. A retry repairs an interrupted
    // projection from the exact durable count without consuming twice.
    const fresh = await this.env.DIRECTORY.get(inviteKey, {
      type: "json",
    });
    if (fresh && (fresh.created_at ?? "legacy") === generation) {
      await this.env.DIRECTORY.put(
        inviteKey,
        JSON.stringify({ ...fresh, uses: outcome.count }),
      );
    }
    return json({
      ok: true,
      provision_id: provisionID,
      invite: inviteCode,
      snapshot: outcome.snapshot,
      uses: outcome.count,
    });
  }

  async consumeCounter(input) {
    const provisionID = input?.provision_id;
    const limit = input?.limit;
    if (
      !this.objectName.startsWith("signup-counter:") ||
      !PROVISION_ID.test(provisionID ?? "") ||
      !Number.isSafeInteger(limit) ||
      limit < 1
    ) {
      fail("invalid signup counter consumption", 400);
    }
    if (typeof this.storage.transaction !== "function") {
      fail("signup counter transaction is unavailable", 503);
    }
    const verdict = await this.storage.transaction((transaction) =>
      consumeSignupCounter(transaction, {
        provisionId: provisionID,
        limit,
        now: this.now(),
      })
    );
    return json({
      ok: true,
      scope: this.objectName,
      provision_id: provisionID,
      ...verdict,
    });
  }

  async requestInviteAuthority(input) {
    if (!this.env.ACCOUNT_SIGNUP) {
      fail("account signup Durable Object is unavailable", 503);
    }
    const id = this.env.ACCOUNT_SIGNUP.idFromName(
      `invite:${input.invite}`,
    );
    let response;
    try {
      response = await this.env.ACCOUNT_SIGNUP.get(id).fetch(
        new Request("https://account-signup.internal/invite/reserve", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(input),
        }),
      );
    } catch (error) {
      fail(
        `invite reservation outcome is ambiguous: ${String(error?.message ?? error)}`,
        502,
      );
    }
    const { text, body } = await responseBody(response);
    if (!response.ok || body?.ok !== true) {
      fail(
        body?.error ??
          `invite reservation returned HTTP ${response.status}: ${text.slice(0, 120)}`,
        response.ok ? 502 : response.status,
      );
    }
    return body;
  }

  async requestCounterAuthority(input) {
    if (!this.env.ACCOUNT_SIGNUP) {
      fail("account signup Durable Object is unavailable", 503);
    }
    const id = this.env.ACCOUNT_SIGNUP.idFromName(input.scope);
    let response;
    try {
      response = await this.env.ACCOUNT_SIGNUP.get(id).fetch(
        new Request("https://account-signup.internal/counter/consume", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            provision_id: input.provision_id,
            limit: input.limit,
          }),
        }),
      );
    } catch (error) {
      fail(
        `signup counter outcome is ambiguous: ${String(error?.message ?? error)}`,
        502,
      );
    }
    const { text, body } = await responseBody(response);
    if (
      !response.ok ||
      body?.ok !== true ||
      typeof body?.allowed !== "boolean"
    ) {
      fail(
        body?.error ??
          `signup counter returned HTTP ${response.status}: ${text.slice(0, 120)}`,
        response.ok ? 502 : response.status,
      );
    }
    return body;
  }

  async requestTargetCoordinator(cellName, path, payload) {
    if (!this.env.CELL_COORDINATOR) {
      fail("target cell coordinator Durable Object is unavailable", 503);
    }
    const id = this.env.CELL_COORDINATOR.idFromName(cellName);
    let response;
    try {
      response = await this.env.CELL_COORDINATOR.get(id).fetch(
        new Request(`https://target-cell.internal${path}`, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            cell_name: cellName,
            ...payload,
          }),
        }),
      );
    } catch (error) {
      fail(
        `target cell coordinator outcome is ambiguous: ${String(error?.message ?? error)}`,
        502,
      );
    }
    const { text, body } = await responseBody(response);
    if (!response.ok || body?.ok !== true) {
      fail(
        body?.error ??
          `target cell coordinator returned HTTP ${response.status}: ${text.slice(0, 120)}`,
        response.ok ? 502 : response.status,
      );
    }
    return body;
  }

  async exactCell(state) {
    const cell = await this.env.DIRECTORY.get(
      `cell:${state.cell.name}`,
      { type: "json" },
    );
    if (
      !cell?.endpoint ||
      !cell?.provision_token ||
      (cell.registration_id ?? cell.registered_at) !==
        state.cell.registration_id
    ) {
      fail(
        `selected cell ${state.cell.name} registration is unavailable`,
        503,
      );
    }
    return { ...cell, name: state.cell.name };
  }

  async requireProvisionProtocol(cell) {
    let response;
    try {
      response = await this.fetchImpl(`${cell.endpoint}/v1/version`, {
        method: "GET",
        signal: AbortSignal.timeout(10_000),
      });
    } catch (error) {
      fail(
        `cell ${cell.name} provision protocol probe failed: ${String(error?.message ?? error)}`,
        502,
      );
    }
    const { text, body } = await responseBody(response);
    if (
      !response.ok ||
      !isObject(body) ||
      body.account_provision_protocol !== 1
    ) {
      fail(
        `cell ${cell.name} does not attest account provision protocol 1: HTTP ${response.status} ${text.slice(0, 120)}`,
        409,
      );
    }
  }

  async provisionAtCell(state, cell, request) {
    let response;
    try {
      response = await this.fetchImpl(
        `${cell.endpoint}/v1/accounts:provision-exact`,
        {
          method: "POST",
          headers: {
            "Content-Type": "application/json",
            Authorization: `Bearer ${cell.provision_token}`,
          },
          body: JSON.stringify({
            email: request.email,
            display_name: request.display_name,
            provision_id: state.provision_id,
            ...(request.consent_terms_version !== ""
              ? {
                consent_terms_version: request.consent_terms_version,
                consent_privacy_version: request.consent_privacy_version,
              }
              : {}),
          }),
          signal: AbortSignal.timeout(15_000),
        },
      );
    } catch (error) {
      fail(
        `cell ${cell.name} provisioning outcome is ambiguous: ${String(error?.message ?? error)}`,
        502,
      );
    }
    const { text, body } = await responseBody(response);
    if (!response.ok) {
      if (response.status === 404 || response.status === 405) {
        fail(
          `cell ${cell.name} exact provision route is not available yet`,
          502,
        );
      }
      fail(
        body?.error ??
          `cell provisioning failed (${cell.name}): HTTP ${response.status}`,
        response.status >= 400 && response.status < 500
          ? response.status
          : 502,
      );
    }
    const account = body?.account;
    if (
      response.status !== 201 ||
      body?.schema_version !== "witself.v0" ||
      body?.provision_id !== state.provision_id ||
      typeof body?.replayed !== "boolean" ||
      !isObject(account) ||
      !ACCOUNT_ID.test(account.account_id ?? "") ||
      typeof account.operator_id !== "string" ||
      account.operator_id.length < 1 ||
      account.email !== request.email ||
      account.status !== "pending" ||
      typeof account.bootstrap_token !== "string" ||
      account.bootstrap_token.length < 1
    ) {
      fail(
        `cell ${cell.name} returned an invalid provision receipt: ${text.slice(0, 120)}`,
        502,
      );
    }
    if (
      request.consent_terms_version !== "" &&
      (
        body.recorded_consent_terms_version !==
          request.consent_terms_version ||
        body.recorded_consent_privacy_version !==
          request.consent_privacy_version
      )
    ) {
      fail(
        `cell ${cell.name} did not confirm the requested consent versions`,
        502,
      );
    }
    return { replayed: body.replayed, account };
  }

  async advance(state, phase, patch = {}) {
    if (PHASE[phase] < PHASE[state.phase]) {
      fail("account signup phase regressed", 500);
    }
    return this.save({ ...state, ...patch, phase });
  }

  async save(state) {
    const saved = { ...state, revision: state.revision + 1 };
    await this.storage.put(STATE_KEY, saved);
    return saved;
  }
}
