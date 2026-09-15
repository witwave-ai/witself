#!/usr/bin/env bash
set -euo pipefail

source_root=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
generator="$source_root/scripts/gitops-cell-values.sh"
work_dir=$(mktemp -d "${TMPDIR:-/tmp}/witself-gitops-cell-values.XXXXXX")
cleanup() {
  find "$work_dir" -depth -mindepth 1 -delete 2>/dev/null || true
  rmdir "$work_dir" 2>/dev/null || true
}
trap cleanup EXIT INT TERM

fail() {
  printf 'gitops cell values test: FAIL: %s\n' "$1" >&2
  exit 1
}

# (a) --check passes on the committed nine files.
check_out="$work_dir/check.out"
if ! bash "$generator" --check --root "$source_root" >"$check_out" 2>&1; then
  cat "$check_out" >&2
  fail "--check did not pass on the committed cell values"
fi
grep -Fq 'gitops cell values: 9 files match' "$check_out" \
  || fail "--check passed without the nine-file match verdict"

# Fixture tree: copy catalog, chart pins, and cell values. Templates are
# compiled into the generator from this checkout.
fixture="$work_dir/repo"
mkdir -p "$fixture/.gitops/charts"
cp -R "$source_root/.gitops/cells" "$fixture/.gitops/cells"
cp -R "$source_root/.gitops/charts/platform" "$fixture/.gitops/charts/platform"
cp -R "$source_root/.gitops/charts/apps" "$fixture/.gitops/charts/apps"

azure_values="$fixture/.gitops/cells/azure-sandbox-use2-dev/values.yaml"
aws_values="$fixture/.gitops/cells/aws-sandbox-usw2-dev/values.yaml"
original_azure="$work_dir/azure.original"
original_aws="$work_dir/aws.original"
cp "$azure_values" "$original_azure"
cp "$aws_values" "$original_aws"

# (b) synthetic drift: --check fails and prints a unified diff.
awk '
  !done && $0 == "  role: dev" { print "  role: drift"; done=1; next }
  { print }
' "$aws_values" >"$work_dir/aws.drifted"
if cmp -s "$aws_values" "$work_dir/aws.drifted"; then
  fail "fixture missing role: dev to drift"
fi
mv "$work_dir/aws.drifted" "$aws_values"
drift_out="$work_dir/drift.out"
status=0
bash "$generator" --check --root "$fixture" >"$drift_out" 2>&1 || status=$?
[ "$status" -eq 1 ] || fail "--check on drifted files exited $status, want 1"
grep -Eq '^\-  role: drift$' "$drift_out" || fail "--check diff did not show the drifted role"
grep -Eq '^\+  role: dev$' "$drift_out" || fail "--check diff did not show the generated role"
grep -Fq 'aws-sandbox-usw2-dev/values.yaml' "$drift_out" \
  || fail "--check diff did not name the drifted cell file"

# (c) --write on the temp copy restores the exact committed bytes.
restore_out="$work_dir/restore.out"
if ! bash "$generator" --write --root "$fixture" >"$restore_out" 2>&1; then
  cat "$restore_out" >&2
  fail "--write did not restore the drifted fixture"
fi
grep -Fq 'wrote 1 files, skipped 8 unchanged' "$restore_out" \
  || fail "--write did not report restoring exactly one drifted file"
cmp -s "$original_aws" "$aws_values" \
  || fail "--write did not restore aws-sandbox-usw2-dev/values.yaml byte-for-byte"
if ! bash "$generator" --check --root "$fixture" >"$work_dir/recheck.out" 2>&1; then
  cat "$work_dir/recheck.out" >&2
  fail "--check failed after --write restored the fixture"
fi

# (d) comment-only drift: --check fails, --write restores the Azure comment.
if ! grep -Fq 'Pulumi enables this at runtime after enabling the AKS-managed ALB' "$azure_values"; then
  fail "fixture lost the Azure gateway comment before comment-only drift"
fi
awk '
  !done && $0 == "      # Pulumi enables this at runtime after enabling the AKS-managed ALB" {
    print "      # DRIFT Pulumi enables this at runtime after enabling the AKS-managed ALB"
    done=1
    next
  }
  { print }
' "$azure_values" >"$work_dir/azure.drifted"
if cmp -s "$azure_values" "$work_dir/azure.drifted"; then
  fail "fixture missing the Azure gateway comment to drift"
fi
mv "$work_dir/azure.drifted" "$azure_values"
comment_out="$work_dir/comment-drift.out"
status=0
bash "$generator" --check --root "$fixture" >"$comment_out" 2>&1 || status=$?
[ "$status" -eq 1 ] || fail "--check on comment-only drift exited $status, want 1"
grep -Fq 'DRIFT Pulumi enables this at runtime' "$comment_out" \
  || fail "--check diff did not show the drifted Azure comment"
grep -Fq 'azure-sandbox-use2-dev/values.yaml' "$comment_out" \
  || fail "--check diff did not name the comment-drifted Azure file"
comment_restore="$work_dir/comment-restore.out"
if ! bash "$generator" --write --root "$fixture" >"$comment_restore" 2>&1; then
  cat "$comment_restore" >&2
  fail "--write did not restore the comment-drifted Azure file"
fi
cmp -s "$original_azure" "$azure_values" \
  || fail "--write did not restore the Azure comment bytes"
grep -Fq 'Pulumi enables this at runtime after enabling the AKS-managed ALB' "$azure_values" \
  || fail "--write dropped the Azure gateway comment"
if grep -Fq 'DRIFT Pulumi' "$azure_values"; then
  fail "--write left the drifted Azure comment in place"
fi

# (e) --write bootstraps a catalog cell with no values.yaml using apps-chart pins.
new_cell="aws-sandbox-usw2-canary"
printf '%s\n' \
  "  ${new_cell}:" \
  "    cloud: aws" \
  "    region: us-west-2" \
  "    role: canary" \
  >>"$fixture/.gitops/cells/catalog.yaml"
new_values="$fixture/.gitops/cells/${new_cell}/values.yaml"
[ ! -e "$new_values" ] || fail "bootstrap fixture already had ${new_cell}/values.yaml"
missing_out="$work_dir/missing.out"
status=0
bash "$generator" --check --root "$fixture" >"$missing_out" 2>&1 || status=$?
[ "$status" -eq 1 ] || fail "--check on a catalog cell without values.yaml exited $status, want 1"
grep -Fq "gitops cell values: missing .gitops/cells/${new_cell}/values.yaml" "$missing_out" \
  || fail "--check did not report the missing catalog cell"
bootstrap_out="$work_dir/bootstrap.out"
if ! bash "$generator" --write --root "$fixture" >"$bootstrap_out" 2>&1; then
  cat "$bootstrap_out" >&2
  fail "--write did not bootstrap the missing catalog cell"
fi
[ -f "$new_values" ] || fail "--write did not create ${new_cell}/values.yaml"
grep -Fq "gitops cell values: wrote .gitops/cells/${new_cell}/values.yaml" "$bootstrap_out" \
  || fail "--write did not report creating the new cell overlay"
default_chart=$(awk '
  $0 == "  witselfServer:" { in_server=1; next }
  in_server && $1 == "chartVersion:" { print $2; exit }
' "$fixture/.gitops/charts/apps/values.yaml")
default_tag=$(awk '
  $0 == "  witselfServer:" { in_server=1; next }
  in_server && $1 == "imageTag:" { print $2; exit }
' "$fixture/.gitops/charts/apps/values.yaml")
[ -n "$default_chart" ] && [ -n "$default_tag" ] \
  || fail "could not read apps chart default pins"
grep -Fq "    chartVersion: ${default_chart}" "$new_values" \
  || fail "bootstrapped overlay did not use the apps chart chartVersion default"
grep -Fq "    imageTag: ${default_tag}" "$new_values" \
  || fail "bootstrapped overlay did not use the apps chart imageTag default"
if ! bash "$generator" --check --root "$fixture" >"$work_dir/bootstrap-check.out" 2>&1; then
  cat "$work_dir/bootstrap-check.out" >&2
  fail "--check failed after bootstrapping the new catalog cell"
fi

printf 'gitops cell values tests passed\n'
