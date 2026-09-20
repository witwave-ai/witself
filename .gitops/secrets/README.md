# Cell operator Secrets

Only whole-document SOPS binary envelopes belong at
`<cell>/<namespace>/<secret-name>.sops`. The two supported cells are
`civo-sandbox-use1-serving` and `civo-sandbox-use1-backup`. They start empty;
an operator populates them after this tooling merges.

Run `scripts/cell-secrets.sh` from the repository. It requires Bash, Ruby,
SOPS and age; cluster operations also require kubectl. Tests require
`age-keygen`. Encrypt complete Kubernetes Secret manifests with
`apiVersion: v1`, `kind: Secret`, `immutable: true`, `metadata.name`,
`metadata.namespace`, `metadata.annotations["witself.io/cell"]` equal to
the selected cell, `type`, and base64 `data`. Never use `stringData`.
The encrypted cell annotation binds the manifest to its cell, while its
namespace and name bind the remaining path. All three are checked on
encryption and again before apply, preserving the input manifest bytes.

```sh
scripts/cell-secrets.sh encrypt civo-sandbox-use1-serving -
scripts/cell-secrets.sh export civo-sandbox-use1-serving monitoring/witself-monitoring-pagerduty-v1
scripts/cell-secrets.sh decrypt-apply civo-sandbox-use1-serving --diff-names
scripts/cell-secrets.sh decrypt-apply civo-sandbox-use1-serving --dry-run
scripts/cell-secrets.sh decrypt-apply civo-sandbox-use1-serving
make check-cell-secrets
```

Feed `encrypt ... -` through a private stdin pipeline; the tool writes only
ciphertext. `export` reads one existing Secret through the explicit
`witself-<cell>` kubectl context, requires it already be immutable, and
adds the cell annotation to the sanitized manifest before encrypting it
in memory. Apply decrypts to
kubectl stdin without a plaintext file. Inspection and dry run still read the
selected cluster to detect immutable Secret conflicts. Immutable Secrets with identical data and type
are no-ops; conflicting existing names are refused with value-free output.
Export strips server metadata, labels and prior annotations. Run restores
during an operator maintenance window: the preflight read does not fence
concurrent cluster changes.

The age private identity lives only in the 1Password item
`witself-cell-secrets-age`, never in Git, CI, or a cell. The public recipient
is pinned in `.sops.yaml`. Decrypt/apply and export use `SOPS_AGE_KEY_FILE`
when explicitly set; otherwise `op read
op://Private/witself-cell-secrets-age/credential` supplies an ephemeral
mode-0600 identity file that is removed on exit. Never pass an identity as
an argument or print it. Argo CD does not decrypt or apply this directory.

For rotation, encrypt a new `-vN` name with `--rotate`; existing files are
never overwritten. A predecessor must exist in the same cell and namespace,
the version must increase (an unversioned predecessor can begin at `-v1`), and the new name must be unused throughout that
cell. Prepare the values reference change in its source overlay and run
`make gitops-cell-values`; apply the new Secret before rolling that reference.
After verifying consumers use the new name, delete the old cluster Secret
and retire its ciphertext file. A suspected identity exposure requires a
new age identity and rotation of every Secret value: Git retains old
ciphertext forever, so re-encryption alone does not undo exposure.

This is a public repository. Paths and SOPS envelope metadata (including
the public recipient) remain public; the Kubernetes document, its data key
names and vendor details are encrypted as one value. Never add plaintext,
identity files, editor backups, or unreviewed metadata here. `check` needs
no private identity and rejects unexpected files or malformed envelopes;
it cannot authenticate or inspect encrypted plaintext without decryption.

See the [operator runbook](../../docs/runbooks.md#cell-operator-secrets-sops)
for custody, rebuild, rotation and the dated names-only inventory.
