# .gitops — GitOps source of truth

Argo CD (installed per cell by `witself-infra up -argocd`) watches this
directory. `witself-infra` creates a single root Argo `Application` named
`bootstrap` that points at [`bootstrap/`](bootstrap); everything else is declared
here and reconciled by Argo.

## Layout

- [`bootstrap/`](bootstrap) — the entrypoint the root app reads (recursively).
  Each file is an Argo `Application`. The first is the External Secrets Operator.
- _(later)_ `platform/`, `apps/`, `cells/<cell>/` — shared add-ons, the
  application tier, and per-cell overlays.

## Notes

- This repo is **public**, and the root app points at the `main` branch, so Argo
  needs **no credentials** to read `.gitops/`.
- **No secrets live here.** Application secrets (DB credentials, …) are delivered
  into the cluster by the External Secrets Operator from AWS Secrets Manager;
  `.gitops/` only references them by name.
