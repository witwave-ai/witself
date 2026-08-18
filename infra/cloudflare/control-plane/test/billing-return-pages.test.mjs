import assert from "node:assert/strict";
import test from "node:test";

import {
  BILLING_RETURN_PATHS,
  billingReturnResponse,
} from "../src/billing-return-pages.mjs";

const origin = "https://self.witwave.ai";
const paths = Object.values(BILLING_RETURN_PATHS);

const request = (path, init = {}) =>
  new Request(`${origin}${path}`, init);

function assertSecurityHeaders(response) {
  assert.equal(response.headers.get("cache-control"), "no-store, max-age=0");
  assert.equal(response.headers.get("referrer-policy"), "no-referrer");
  assert.equal(response.headers.get("x-content-type-options"), "nosniff");
  assert.equal(response.headers.get("x-frame-options"), "DENY");
  assert.equal(response.headers.get("cross-origin-opener-policy"), "same-origin");
  assert.equal(response.headers.get("cross-origin-resource-policy"), "same-origin");
  assert.match(
    response.headers.get("content-security-policy") ?? "",
    /default-src 'none'/,
  );
  assert.match(
    response.headers.get("content-security-policy") ?? "",
    /form-action 'none'/,
  );
  assert.equal(response.headers.has("set-cookie"), false);
  assert.equal(response.headers.has("location"), false);
}

test("all billing returns are generic value-free GET pages", async () => {
  for (const path of paths) {
    const response = billingReturnResponse(request(
      `${path}?session_id=cs_test_must_not_echo&account=acct_must_not_echo`,
    ));
    assert.ok(response instanceof Response);
    assert.equal(response.status, 200);
    assert.equal(response.headers.get("content-type"), "text/html; charset=utf-8");
    assertSecurityHeaders(response);

    const body = await response.text();
    assert.match(body, /Return to the AI or terminal/);
    assert.match(body, /witself billing show/);
    assert.doesNotMatch(body, /cs_test_must_not_echo/);
    assert.doesNotMatch(body, /acct_must_not_echo/);
    assert.doesNotMatch(body, /<script|<form|href=/i);
  }
});

test("HEAD has the same safe boundary and no body", async () => {
  for (const path of paths) {
    const response = billingReturnResponse(request(path, { method: "HEAD" }));
    assert.ok(response instanceof Response);
    assert.equal(response.status, 200);
    assertSecurityHeaders(response);
    assert.equal(await response.text(), "");
  }
});

test("all other methods terminate at the return route with 405", async () => {
  for (const path of paths) {
    for (const method of ["POST", "PUT", "PATCH", "DELETE", "OPTIONS"]) {
      const response = billingReturnResponse(request(path, { method }));
      assert.ok(response instanceof Response);
      assert.equal(response.status, 405);
      assert.equal(response.headers.get("allow"), "GET, HEAD");
      assert.equal(response.headers.get("content-type"), "text/plain; charset=utf-8");
      assertSecurityHeaders(response);
      assert.equal(await response.text(), "method not allowed\n");
    }
  }
});

test("non-return paths preserve the existing Worker routing", () => {
  for (const path of [
    "/",
    "/billing",
    "/billing/success/",
    "/billing/cancel",
    "/v1/capabilities",
  ]) {
    assert.equal(billingReturnResponse(request(path)), null);
  }
});
