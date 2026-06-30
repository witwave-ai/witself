# .gitops — GitOps source of truth

Argo CD (installed per cell by `witself-infra up -argocd`) watches this
directory. `witself-infra` creates a single root Argo `Application` named
`bootstrap` that points at [`bootstrap/`](bootstrap); everything else is declared
here and reconciled by Argo.

## Layout

```text
.gitops/
  bootstrap/    # Argo's entrypoint — the root app reads this (recurse)
    platform.yaml   # Application -> platform/  (sync-wave 0)
    apps.yaml       # Application -> apps/       (sync-wave 1)
  platform/     # shared cluster add-ons, one Application each
    external-secrets.yaml
    namespace-witself.yaml
    secrets-manager-store.yaml
    witself-db-secret.yaml
  apps/         # the application tier (witself-server, ...)
    witself-server.yaml
  cells/        # per-cell overlays, by cell name — fleet scaffolding
```

`witself-infra up -argocd` creates one root Argo `Application` (`bootstrap`)
pointing at [`bootstrap/`](bootstrap). It fans out to the `platform` and `apps`
tiers, which reconcile everything under [`platform/`](platform) and `apps/`. The
platform tier (sync-wave 0) comes up before the app tier (sync-wave 1).

The Application manifests here reference this public repo by URL; a self-hosted
fork (`-gitops-repo`) would adjust those URLs (or we templatize them via an
ApplicationSet later).

## Notes

- This repo is **public**, and the root app points at the `main` branch, so Argo
  needs **no credentials** to read `.gitops/`.
- **No secrets live here.** Application secrets (DB credentials, …) are delivered
  into the cluster by the External Secrets Operator from AWS Secrets Manager;
  `.gitops/` only references them by name.
