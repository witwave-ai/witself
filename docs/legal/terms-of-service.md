# Witself Terms of Service

**Version 2026-08-31 · Effective 2026-08-31**

**Provider:** Witwave LLC ("Witwave", "we"). **Service:** Witself, the
managed agent durable-state platform served at self.witwave.ai, together
with its APIs, CLIs, and MCP adapters. Formal notices to Witwave:
legal@witwave.ai. We send notices to your account email.

## 1. The agreement

By creating an account or using the Service you agree to these Terms, the
[Acceptable Use Policy](acceptable-use.md), and the
[Privacy Policy](privacy-policy.md). If you use the Service for an
organization, you represent that you may bind it. You must be at least 18
years old, or the age of majority where you live, to create an account.

These Terms are governed by the laws of the State of Colorado, USA, without
regard to conflict-of-laws rules. Disputes belong exclusively to the state
or federal courts located in Colorado, and both sides consent to their
jurisdiction.

## 2. The service

Witself stores and serves agent state: an open plane (memories, facts,
transcripts, avatars, messaging, agent email) and a sealed plane (secrets, TOTP) that is
client-encrypted — we hold ciphertext and cannot read sealed values. Feature
availability varies by plan; the plan catalog in the product is
authoritative. The Service's source code is separately available under the
FSL-1.1-ALv2 license; these Terms govern only the managed service. Running
the software yourself is governed by the source license, not these Terms.

## 3. Accounts

You are responsible for your operators' actions, for credential custody
(including client-held vault keys, which we cannot recover), and for keeping
your contact email current. Seats are caps on live operators, not billed
quantities.

## 4. Fees, auto-renewal, and billing

Paid plans are **automatically renewing monthly subscriptions** billed
through Stripe at the flat monthly price shown in the plan catalog at
purchase, plus tax where applicable. Your plan renews and your payment
method is charged each month on your billing date **until you cancel**.

**Cancel any time**: run `witself plan downgrade free` (or use the billing
portal). Cancellation takes effect at the end of the paid period already
billed; you keep paid features until then, and no further charges occur.
There is no partial-month proration. We will notify you by email at least 30
days before any price increase takes effect; the increase applies from your
next renewal after the notice period.

An existing subscription stays at the price you subscribed at; a price
change applies to you only after the 30-day notice above. The free plan is
free indefinitely and never converts to a paid plan automatically; we offer
no free trials, and charges begin only when you explicitly purchase an
upgrade and complete checkout.

Failed renewals are retried; if payment ultimately fails the subscription is
cancelled and the account continues on the free plan — content is retained
per the [data-retention policy](https://github.com/witwave-ai/witself/blob/main/docs/data-retention-policy.md), subject to the
free plan's retention windows, not deleted for non-payment itself. Refunds:
see [Refund & Cancellation](refund-cancellation.md).

## 5. Your data

You own the content you store. You grant us only the rights needed to
operate the Service: storing, replicating, backing up, serving, and
transmitting your content as you direct (for example, agent email you
send), and abuse protection. Value-free operational records (audit events
and usage counters) contain no content and survive content deletion. We do not train models on your content and we do not sell it.
Retention and deletion behavior is the published
[data-retention policy](https://github.com/witwave-ai/witself/blob/main/docs/data-retention-policy.md). You may request a full
export of your open-plane data at any time by contacting
legal@witwave.ai; sealed-plane data exports only as ciphertext. A
self-service export command is on the roadmap.

## 6. Acceptable use and suspension

The [AUP](acceptable-use.md) is part of these Terms. We may suspend or close
accounts for material breach, non-payment, legal requirement, or genuine
risk to the Service or others; where practicable we notify first. Closure
follows the [Privacy Policy](privacy-policy.md)'s closure and erasure terms.

## 7. Support and availability

Support is provided per the published
[support policy](https://github.com/witwave-ai/witself/blob/main/docs/support-policy.md). The Service is provided without an
uptime SLA at launch. Incidents affecting the managed service are published
as GitHub issues labeled `incident` on the public repository.

## 8. Disclaimers and liability

THE SERVICE IS PROVIDED "AS IS" AND "AS AVAILABLE", WITHOUT WARRANTIES OF
ANY KIND, EXPRESS OR IMPLIED, INCLUDING MERCHANTABILITY, FITNESS FOR A
PARTICULAR PURPOSE, AND NON-INFRINGEMENT. TO THE MAXIMUM EXTENT PERMITTED BY
LAW, WITWAVE'S TOTAL AGGREGATE LIABILITY ARISING OUT OF OR RELATING TO THE
SERVICE IS LIMITED TO THE FEES YOU PAID US IN THE TWELVE MONTHS BEFORE THE
EVENT GIVING RISE TO LIABILITY (OR US $50 IF YOU HAVE PAID NO FEES), AND
NEITHER SIDE IS LIABLE FOR INDIRECT, INCIDENTAL, SPECIAL, CONSEQUENTIAL, OR
PUNITIVE DAMAGES, OR LOST PROFITS, DATA, OR GOODWILL, EVEN IF ADVISED OF THE
POSSIBILITY. Nothing in these Terms excludes liability that cannot be
excluded by law. You will defend and indemnify Witwave against third-party
claims arising from your content or your breach of these Terms; we will
defend and indemnify you against third-party claims that the managed Service
itself infringes their intellectual property.

## 9. Changes and termination

We may update these Terms with notice (email or in-product) at least 14 days
before material changes take effect; continued use after the effective date
is acceptance. If you do not agree, close your account before the effective
date. You may close your account at any time (request an export first —
closure is irreversible; see the Privacy Policy).

## 10. Miscellany

Witwave retains all rights in the Service and its software not expressly
granted; feedback you choose to send may be used without obligation; no
trademark rights are granted by these Terms. Neither side is liable for
delay or failure caused by events beyond its
reasonable control. You may not assign these Terms without our consent; we
may assign them in connection with a merger, acquisition, or sale of assets.
If a provision is unenforceable, the rest remains in effect. These Terms,
the policies they incorporate, and your order details are the entire
agreement. You agree to comply with applicable US export-control and
sanctions laws. Notices to Witwave: legal@witwave.ai; notices to you: your
account email.
