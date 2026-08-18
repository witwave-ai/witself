# Hosted Billing Return Pages

Status: implemented on the public `self.witwave.ai` control-plane Worker. These
pages are navigation endpoints, not billing authority and not a hosted Agent
Console.

Configure Stripe with these exact canonical HTTPS URLs:

| Flow | Worker binding | Container environment | URL |
|---|---|---|---|
| Checkout success | `CP_STRIPE_SUCCESS_URL` | `WITSELF_CP_STRIPE_SUCCESS_URL` | `https://self.witwave.ai/billing/success` |
| Checkout cancel | `CP_STRIPE_CANCEL_URL` | `WITSELF_CP_STRIPE_CANCEL_URL` | `https://self.witwave.ai/billing/cancelled` |
| Portal return | `CP_STRIPE_PORTAL_RETURN_URL` | `WITSELF_CP_STRIPE_PORTAL_RETURN_URL` | `https://self.witwave.ai/billing/portal-return` |

All three routes accept only `GET` and `HEAD`. They are unauthenticated because
Stripe returns a normal browser without a Witself browser session. Their static
responses contain no account, provider-session, payment, or plan data; never
fetch billing state; never claim that payment or a plan change completed; set no
cookie; and perform no mutation. Query parameters are ignored and never
rendered. Responses are non-cacheable and carry restrictive browser-security
headers.

The local Agent Console remains loopback-only and content-read-only. It may not
be running when a hosted flow returns, and its per-process browser session must
not be exposed to a public site. The AI or CLI that opened Stripe owns workflow
resumption and reads authenticated state with `witself billing show`. All
Witself-side billing mutations continue through the guarded AI/CLI API surface.

Before a billing cohort is enabled, verify the deployed release serves all
three exact URLs and retains the static response contract. A successful landing
page is never provider or subscription evidence; the signed webhook and
authenticated billing read remain authoritative.
