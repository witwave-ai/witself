# WitSelf running locally

Active research and design for the **local cell**: the default new installation
provides a usable free local cell, replacing the hosted free plan. Required
synchronization with SaaS or self-managed services and an explicit lightweight
remote-only installation remain supported. Local cells are the selected direction,
explicitly subject to adversarial review and revision. Implementation has not
started in this effort.

The product direction is settled; rollout timing and the migration of existing
free hosted users still need a reviewed transition plan. These documents do not
change existing installations or accounts.

Upgrading to paid SaaS keeps the same local cell and client workflows, adding
continuous hosted synchronization without starting over or moving data manually.
Returning to local use preserves that continuity. Agent messaging is a local
capability and continues across the transition; email is the sole hosted-only
feature exception because of its service infrastructure.

The required [populated-account transition acceptance](implementation-plan.md#populated-account-transition-acceptance)
starts with data on both sides and proves continuity through interrupted sync,
subscription changes and return to local use, including secrets, installed clients
and active messaging. The plan also requires request-wide coordination, atomic
visibility of linked messaging operations, non-mutating imported request views
and authenticated offline account-console access.

## Start here

| Document | Purpose |
|---|---|
| [Fable handoff](fable-handoff.md) | One-link planning assignment, selected requirements and shared tracking instructions for Fable, Opus and Astra. |
| [Implementation plan](implementation-plan.md) | Draft milestones, dependencies, first implementation batch, acceptance criteria and open decisions for Claude to refine. |
| [Code assessment and rollout plan](local-mode-code-assessment.md) | Recommendation, source-backed findings, infra CLI reuse, native installation, effort, staged canaries and grounds to revise the decision. |
| [Product requirements and decisions](local-first-mode.md) | Discussion record, required behavior, authority, identity, capacity and open product questions. |
| [Storage and competitor research](local-storage-and-sync-patterns.md) | Database and packaging alternatives, retrieval quality, comparable products and synchronization patterns. |

## Review and collaboration

Reviewers should challenge assumptions with code or measured evidence. Preserve
required remote-only use, hosted email authority, safe local/connected messaging,
local data on capacity mismatch, nonblocking disclosed usage reporting and existing behavior
during incremental rollout. Native packaging should minimize dependencies and
avoid requiring Kubernetes or containers where possible.

Distinguish recorded decisions from proposed mechanisms, inspected code from
runtime proof, and canary readiness from complete product support. Keep the
requirements and assessment consistent when accepting a design revision. All
historical research observations use revision `adc6f186`; the implementation
plan identifies its newer source checkpoint. Refresh affected evidence as
implementation evolves.

[All WitSelf documentation](../../README.md)
