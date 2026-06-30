# cells/ — per-cell overlays

Per-cell customization lives here, one directory per cell named by its composed
cell name (e.g. `aws-sandbox-usw2-dev/`): values that differ between cells —
image tags, replica counts, hostnames, which secrets to sync.

Shared definitions (the `platform/` add-ons, the `apps/` charts) stay
cell-agnostic; a cell selects its overlay by name. Today the fleet is a single
cell, so this is scaffolding — the per-cell selection (the bootstrap Application
parameterized by the cell name `witself-infra` injects) is wired when a second
cell exists.
