# WitSelf running locally

Active research and design for the **local cell**: a free local WitSelf offering,
required synchronization with SaaS or self-managed services, and a lightweight
remote-only client. Local cells are the selected direction, explicitly subject
to adversarial review and revision. Implementation has not started in this effort.

## Start here

| Document | Purpose |
|---|---|
| [Code assessment and rollout plan](local-mode-code-assessment.md) | Recommendation, source-backed findings, infra CLI reuse, native installation, effort, staged canaries and grounds to revise the decision. |
| [Product requirements and decisions](local-first-mode.md) | Discussion record, required behavior, authority, identity, capacity and open product questions. |
| [Storage and competitor research](local-storage-and-sync-patterns.md) | Database and packaging alternatives, retrieval quality, comparable products and synchronization patterns. |

## Review and collaboration

Reviewers should challenge assumptions with code or measured evidence. Preserve
required remote-only use, remote authority for email/shared messaging, local data
on capacity mismatch, nonblocking disclosed usage reporting and existing behavior
during incremental rollout. Native packaging should minimize dependencies and
avoid requiring Kubernetes or containers where possible.

Distinguish recorded decisions from proposed mechanisms, inspected code from
runtime proof, and canary readiness from complete product support. Keep the
requirements and assessment consistent when accepting a design revision. All
source observations currently use revision `adc6f186`; refresh affected evidence
as implementation evolves.

[All WitSelf documentation](../../README.md)
