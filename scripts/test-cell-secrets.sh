#!/usr/bin/env bash
# Real SOPS/age encryption with synthetic material; kubectl and 1Password are local stubs.
set -Eeuo pipefail
umask 077

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
for required in bash ruby sops age age-keygen; do
  command -v "$required" >/dev/null 2>&1 || {
    printf 'test-cell-secrets: required binary missing: %s\n' "$required" >&2
    exit 1
  }
done
work_dir="$(mktemp -d "${TMPDIR:-/tmp}/witself-cell-secrets-test.XXXXXX")"
trap 'rm -rf "$work_dir"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
work_dir="$(cd "$work_dir" && pwd -P)"
mkdir -p "$work_dir/bin" "$work_dir/fixtures" "$work_dir/cluster" "$work_dir/tool-tmp" "$work_dir/logs"
export WITSELF_HOME="$work_dir/witself-home" DSH_HOME="$work_dir/dsh-home"
REAL_SOPS="$(command -v sops)"
export REAL_SOPS
export CELL_SECRETS_TEST_ORIGINAL_PATH="$PATH"
export CELL_SECRETS_TEST_STATE="$work_dir"
export TMPDIR="$work_dir/tool-tmp"
export SOPS_AGE_KEY_FILE="$work_dir/fixtures/identity.txt"
unset SOPS_AGE_KEY SOPS_AGE_KEY_CMD
age-keygen -o "$SOPS_AGE_KEY_FILE" >/dev/null 2>&1
age-keygen -o "$work_dir/fixtures/other-identity.txt" >/dev/null 2>&1
test_recipient="$(age-keygen -y "$SOPS_AGE_KEY_FILE" 2>/dev/null)"
other_recipient="$(age-keygen -y "$work_dir/fixtures/other-identity.txt" 2>/dev/null)"
production_recipient=age10ck02we3wzes85qd0e0eylxqupw7ectqjvv7wlykzdtts8nsugwshkp8ue
cell=civo-sandbox-use1-serving
fixture_repo="$work_dir/repo"
secret_root="$fixture_repo/.gitops/secrets"
tool="$fixture_repo/scripts/cell-secrets.sh"
mkdir -p "$fixture_repo/scripts/lib" "$secret_root/$cell" "$secret_root/civo-sandbox-use1-backup"
cp "$repo_root/scripts/cell-secrets.sh" "$fixture_repo/scripts/cell-secrets.sh"
cp "$repo_root/scripts/lib/cell-secrets.rb" "$fixture_repo/scripts/lib/cell-secrets.rb"
printf 'creation_rules:\n  - path_regex: '\''^\\.gitops/secrets/.*\\.sops$'\''\n    age: %s,%s\n' \
  "$production_recipient" "$test_recipient" >"$secret_root/.sops.yaml"
: >"$secret_root/README.md"
: >"$secret_root/$cell/.gitkeep"
: >"$secret_root/civo-sandbox-use1-backup/.gitkeep"

cat >"$work_dir/bin/kubectl" <<'RUBY'
#!/usr/bin/env ruby
require 'json'
require 'digest'
state = ENV.fetch('CELL_SECRETS_TEST_STATE')
File.open("#{state}/logs/kubectl-invoked", 'a') { |f| f.puts('called') }
args = ARGV.dup
context = namespace = nil
dry = false
remaining = []
until args.empty?
  arg = args.shift
  case arg
  when '--context' then context = args.shift
  when '--namespace', '-n' then namespace = args.shift
  when '--dry-run=client' then dry = true
  else remaining << arg
  end
end
abort 'unexpected context or namespace' unless context == 'witself-civo-sandbox-use1-serving' && namespace == 'monitoring'
if remaining[0, 2] == ['get', 'secret']
  name = remaining[2]
  abort 'unexpected get arguments' unless remaining[3..-1] == ['--ignore-not-found', '-o', 'json']
  File.open("#{state}/logs/kubectl-get", 'a') { |f| f.puts(name) }
  if ENV['FAKE_GET_FAILURE'] == '1'
    warn 'synthetic-private-provider-output'
    exit 1
  end
  path = "#{state}/cluster/#{namespace}--#{name}.json"
  STDOUT.write(File.binread(path)) if File.file?(path)
elsif remaining == ['apply', '-f', '-']
  payload = STDIN.read
  match = Digest::SHA256.hexdigest(payload) == Digest::SHA256.file(ENV.fetch('EXPECTED_MANIFEST')).hexdigest
  File.open("#{state}/logs/kubectl-apply", 'a') do |f|
    f.puts(JSON.generate({ 'same_bytes' => match, 'dry_run' => dry, 'context' => context, 'namespace' => namespace }))
  end
  if ENV['FAKE_APPLY_FAILURE'] == '1'
    warn 'synthetic-private-provider-output'
    exit 1
  end
  puts 'secret/synthetic-v1 configured'
else
  abort 'unexpected kubectl operation'
end
RUBY

cat >"$work_dir/bin/op" <<'RUBY'
#!/usr/bin/env ruby
state = ENV.fetch('CELL_SECRETS_TEST_STATE')
abort 'unexpected op invocation' unless ARGV == ['read', 'op://Private/witself-cell-secrets-age/credential']
File.open("#{state}/logs/op", 'a') { |f| f.puts('read') }
STDOUT.write(File.binread("#{state}/fixtures/identity.txt"))
if ENV['FAKE_OP_FAILURE'] == '1'
  warn 'synthetic-private-provider-output'
  exit 1
end
RUBY

cat >"$work_dir/bin/sops" <<'RUBY'
#!/usr/bin/env ruby
state = ENV.fetch('CELL_SECRETS_TEST_STATE')
if ARGV.include?('--decrypt') || ARGV.include?('-d')
  key_path = ENV['SOPS_AGE_KEY_FILE']
  if key_path && key_path.start_with?(ENV.fetch('TMPDIR') + '/')
    mode = File.stat(key_path).mode & 0777
    abort 'identity file mode must be 0600' unless mode == 0600
    File.open("#{state}/logs/temporary-identity", 'a') { |f| f.puts('mode-0600') }
  end
end
if ENV['FAKE_SOPS_FAILURE'] == '1'
  puts 'synthetic-private-provider-output'
  warn 'synthetic-private-provider-output'
  exit 1
end
# A mise/asdf shim may fall through to PATH. Restore the original search path so
# it cannot discover this test wrapper again and recurse indefinitely.
ENV['PATH'] = ENV.fetch('CELL_SECRETS_TEST_ORIGINAL_PATH')
exec(ENV.fetch('REAL_SOPS'), *ARGV)
RUBY
chmod 700 "$work_dir/bin/kubectl" "$work_dir/bin/op" "$work_dir/bin/sops"
export PATH="$work_dir/bin:$PATH"

fixture="$work_dir/fixtures/manifest.yaml"
export EXPECTED_MANIFEST="$fixture"
# Both decoded value boundaries matter. The document also deliberately has no final newline.
ruby -rbase64 -e '
  text = "apiVersion: v1\nkind: Secret\nimmutable: true\nmetadata:\n  name: synthetic-v1\n  namespace: monitoring\n  annotations:\n    witself.io/cell: civo-sandbox-use1-serving\ntype: Opaque\ndata:\n"
  text += "  one: #{Base64.strict_encode64("synthetic-no-final-newline")}\n"
  text += "  two: #{Base64.strict_encode64("synthetic-first\nsynthetic-second\n")}"
  File.binwrite(ARGV.fetch(0), text)
' "$fixture"

assert_clean() {
  ruby -e '
    entries = Dir.children(ARGV.fetch(0))
    abort "tool TMPDIR is not empty" unless entries.empty?
  ' "$TMPDIR" || fail "temporary artifacts remain"
}
assert_private_output() {
  ruby -e '
    text = File.binread(ARGV.fetch(0)) + File.binread(ARGV.fetch(1))
    forbidden = [/SOPS-AGE-SECRET-KEY-/, /AGE-SECRET-KEY-/, /synthetic-private-provider-output/,
      /synthetic-no-final-newline/, /synthetic-first/, /c3ludGhldGlj/, /\b[[:xdigit:]]{64}\b/]
    abort "private tool output escaped" if forbidden.any? { |pattern| text.match?(pattern) }
  ' "$work_dir/logs/stdout" "$work_dir/logs/stderr" || fail "tool output leaked private fixture content"
}
fail() { printf 'test-cell-secrets: FAIL: %s\n' "$*" >&2; exit 1; }
passes=0
expect_ok() {
  local label="$1"
  shift
  "$@" >"$work_dir/logs/stdout" 2>"$work_dir/logs/stderr" || fail "$label (unexpected refusal; captured output withheld)"
  assert_private_output
  assert_clean
  passes=$((passes + 1))
}
expect_fail() {
  local label="$1"
  shift
  if "$@" >"$work_dir/logs/stdout" 2>"$work_dir/logs/stderr"; then
    fail "$label (unexpected success)"
  fi
  assert_private_output
  assert_clean
  passes=$((passes + 1))
}
assert_applies() {
  ruby -rjson -e '
    path, count, dry = ARGV
    lines = File.exist?(path) ? File.readlines(path) : []
    abort "unexpected apply count" unless lines.length == count.to_i
    unless lines.empty?
      last = JSON.parse(lines.last)
      abort "manifest bytes changed" unless last.fetch("same_bytes")
      abort "unexpected dry-run mode" unless last.fetch("dry_run") == (dry == "true")
    end
  ' "$work_dir/logs/kubectl-apply" "$1" "${2:-false}" || fail "kubectl apply assertion"
}
kube_calls() {
  ruby -e 'puts(File.exist?(ARGV.fetch(0)) ? File.readlines(ARGV.fetch(0)).length : 0)' "$work_dir/logs/kubectl-invoked"
}
assert_status() {
  ruby -e '
    output, status = ARGV
    text = File.read(output)
    abort "missing value-free status" unless text.include?(status) && text.include?("monitoring/synthetic-v1")
  ' "$work_dir/logs/stdout" "$1" || fail "missing $1 status"
}
write_live() {
  ruby -rjson -ryaml -e '
    doc = YAML.safe_load(File.binread(ARGV.fetch(0)))
    case ARGV.fetch(2)
    when "data" then doc["data"]["one"] = "ZGlmZmVyZW50"
    when "type" then doc["type"] = "kubernetes.io/basic-auth"
    when "mutable" then doc["immutable"] = false
    when "name" then doc["metadata"]["name"] = "foreign-v1"
    when "namespace" then doc["metadata"]["namespace"] = "foreign"
    end
    doc["metadata"]["resourceVersion"] = "synthetic-rv"
    doc["metadata"]["uid"] = "synthetic-uid"
    doc["metadata"]["annotations"] = { "synthetic-private-provider-output" => "synthetic-private-provider-output" }
    File.write(ARGV.fetch(1), JSON.generate(doc))
  ' "$fixture" "$work_dir/cluster/monitoring--synthetic-v1.json" "$1"
}
mutate_manifest() {
  ruby -e 'File.binwrite(ARGV.fetch(1), File.binread(ARGV.fetch(0)).sub(ARGV.fetch(2), ARGV.fetch(3)))' \
    "$fixture" "$work_dir/fixtures/variant.yaml" "$1" "$2"
}
encrypted="$secret_root/$cell/monitoring/synthetic-v1.sops"

expect_ok "empty layout check" bash "$tool" check
expect_ok "encrypt file" bash "$tool" encrypt "$cell" "$fixture"
[[ -f "$encrypted" ]] || fail "encrypted artifact absent"
cp "$encrypted" "$work_dir/fixtures/good.sops"
expect_ok "encrypted fixture check without identity" env -u SOPS_AGE_KEY_FILE bash "$tool" check
expect_ok "byte-exact decrypt and apply" bash "$tool" decrypt-apply "$cell" monitoring/synthetic-v1
assert_applies 1
expect_ok "client dry run" bash "$tool" decrypt-apply "$cell" monitoring/synthetic-v1 --dry-run
assert_applies 2 true
expect_ok "diff identifies create" bash "$tool" decrypt-apply "$cell" --diff-names
assert_status created
assert_applies 2 true
write_live identical
expect_ok "identical immutable Secret is a no-op" bash "$tool" decrypt-apply "$cell"
assert_applies 2 true
expect_ok "diff identifies identical" bash "$tool" decrypt-apply "$cell" --diff-names
assert_status identical
for conflict in data type mutable; do
  write_live "$conflict"
  expect_fail "immutable conflict: $conflict" bash "$tool" decrypt-apply "$cell"
  assert_applies 2 true
done
expect_fail "diff reports conflict and refuses" bash "$tool" decrypt-apply "$cell" --diff-names
assert_status conflict
write_live namespace
expect_fail "live namespace mismatch" bash "$tool" decrypt-apply "$cell"
write_live name
expect_fail "live name mismatch" bash "$tool" decrypt-apply "$cell"
rm "$work_dir/cluster/monitoring--synthetic-v1.json"
expect_fail "cluster read errors fail closed" env FAKE_GET_FAILURE=1 bash "$tool" decrypt-apply "$cell"
assert_applies 2 true
expect_fail "apply errors are value-free" env FAKE_APPLY_FAILURE=1 bash "$tool" decrypt-apply "$cell"
assert_applies 3
expect_fail "wrong identity" env SOPS_AGE_KEY_FILE="$work_dir/fixtures/other-identity.txt" bash "$tool" decrypt-apply "$cell"
assert_applies 3
expect_fail "missing configured identity does not fall back" env SOPS_AGE_KEY_FILE="$work_dir/fixtures/missing.txt" bash "$tool" decrypt-apply "$cell"
[[ ! -e "$work_dir/logs/op" ]] || fail "configured missing identity fell back to 1Password"
expect_ok "1Password fallback removes mode-0600 identity" env -u SOPS_AGE_KEY_FILE bash "$tool" decrypt-apply "$cell"
[[ -s "$work_dir/logs/temporary-identity" ]] || fail "temporary identity mode was not verified"
assert_applies 4
expect_fail "1Password partial-output failure cleans identity" env -u SOPS_AGE_KEY_FILE FAKE_OP_FAILURE=1 bash "$tool" decrypt-apply "$cell"
assert_applies 4

expect_fail "existing file cannot be overwritten" bash "$tool" encrypt "$cell" "$fixture"
expect_fail "rotate cannot overwrite existing file" bash "$tool" encrypt "$cell" "$fixture" --rotate
cmp -s "$encrypted" "$work_dir/fixtures/good.sops" || fail "overwrite refusal changed ciphertext"
mutate_manifest synthetic-v1 synthetic-v2
expect_ok "rotation adds a new increasing version" bash "$tool" encrypt "$cell" "$work_dir/fixtures/variant.yaml" --rotate
[[ -f "$secret_root/$cell/monitoring/synthetic-v2.sops" ]] || fail "rotation did not create v2"
ruby -rjson -ryaml -e '
  doc = YAML.safe_load(File.binread(ARGV.fetch(0)))
  doc["data"]["one"] = "ZGlmZmVyZW50"
  File.write(ARGV.fetch(1), JSON.generate(doc))
' "$work_dir/fixtures/variant.yaml" "$work_dir/cluster/monitoring--synthetic-v2.json"
expect_fail "all selected conflicts preflight before any apply" bash "$tool" decrypt-apply "$cell"
assert_applies 4
rm "$work_dir/cluster/monitoring--synthetic-v2.json"
cp "$encrypted" "$secret_root/$cell/monitoring/zzz-invalid-v1.sops"
binding_calls="$(kube_calls)"
expect_fail "all selected bindings preflight before cluster access" bash "$tool" decrypt-apply "$cell"
[[ "$(kube_calls)" == "$binding_calls" ]] || fail "selection binding preflight reached kubectl"
assert_applies 4
rm "$secret_root/$cell/monitoring/zzz-invalid-v1.sops"
expect_fail "same rotated version cannot overwrite" bash "$tool" encrypt "$cell" "$work_dir/fixtures/variant.yaml" --rotate
mutate_manifest synthetic-v1 synthetic-v0
expect_fail "rotation cannot decrease version" bash "$tool" encrypt "$cell" "$work_dir/fixtures/variant.yaml" --rotate
mutate_manifest synthetic-v1 unrelated-v2
expect_fail "rotation needs an existing predecessor" bash "$tool" encrypt "$cell" "$work_dir/fixtures/variant.yaml" --rotate
mutate_manifest synthetic-v1 synthetic
expect_fail "rotation requires versioned name" bash "$tool" encrypt "$cell" "$work_dir/fixtures/variant.yaml" --rotate
mkdir -p "$secret_root/$cell/other"
cp "$encrypted" "$secret_root/$cell/other/synthetic-v3.sops"
mutate_manifest synthetic-v1 synthetic-v3
expect_fail "rotation names are unique across cell namespaces" bash "$tool" encrypt "$cell" "$work_dir/fixtures/variant.yaml" --rotate
rm -r "$secret_root/$cell/other" "$secret_root/$cell/monitoring/synthetic-v2.sops"
mutate_manifest synthetic-v1 legacy
expect_ok "initial unversioned Secret" bash "$tool" encrypt "$cell" "$work_dir/fixtures/variant.yaml"
mutate_manifest synthetic-v1 legacy-v1
expect_ok "rotate unversioned Secret to v1" bash "$tool" encrypt "$cell" "$work_dir/fixtures/variant.yaml" --rotate
rm "$secret_root/$cell/monitoring/legacy.sops" "$secret_root/$cell/monitoring/legacy-v1.sops"

binding_calls="$(kube_calls)"
mv "$encrypted" "$secret_root/$cell/monitoring/renamed-v1.sops"
expect_fail "encrypted name must match path" bash "$tool" decrypt-apply "$cell" monitoring/renamed-v1
mv "$secret_root/$cell/monitoring/renamed-v1.sops" "$encrypted"
mkdir -p "$secret_root/$cell/foreign"
mv "$encrypted" "$secret_root/$cell/foreign/synthetic-v1.sops"
expect_fail "encrypted namespace must match path" bash "$tool" decrypt-apply "$cell" foreign/synthetic-v1
mv "$secret_root/$cell/foreign/synthetic-v1.sops" "$encrypted"
rmdir "$secret_root/$cell/foreign"
mkdir -p "$secret_root/civo-sandbox-use1-backup/monitoring"
cp "$encrypted" "$secret_root/civo-sandbox-use1-backup/monitoring/synthetic-v1.sops"
expect_fail "ciphertext copied between cells refuses" bash "$tool" decrypt-apply civo-sandbox-use1-backup monitoring/synthetic-v1
[[ "$(kube_calls)" == "$binding_calls" ]] || fail "binding mismatch reached kubectl"
rm -r "$secret_root/civo-sandbox-use1-backup/monitoring"
expect_fail "cell path traversal" bash "$tool" encrypt ../outside "$fixture"
expect_fail "selector traversal" bash "$tool" decrypt-apply "$cell" ../synthetic-v1
expect_fail "unconfigured cell" bash "$tool" encrypt imaginary-cell "$fixture"
expect_fail "unknown option" bash "$tool" decrypt-apply "$cell" --unexpected
expect_fail "conflicting view modes" bash "$tool" decrypt-apply "$cell" --dry-run --diff-names

for mutation in immutable stringdata base64 namespace name type kind api duplicate cell missing_cell; do
  ruby -e '
    text = File.binread(ARGV.fetch(0)).sub("synthetic-v1", "invalid-v1")
    text = case ARGV.fetch(2)
      when "immutable" then text.sub("immutable: true", "immutable: false")
      when "stringdata" then text + "\nstringData: { forbidden: synthetic }\n"
      when "base64" then text.sub(/one: .*/, "one: not!base64")
      when "namespace" then text.sub("namespace: monitoring", "namespace: ../outside")
      when "name" then text.sub("name: invalid-v1", "name: ../outside")
      when "type" then text.sub("type: Opaque", "type: \"\"")
      when "kind" then text.sub("kind: Secret", "kind: ConfigMap")
      when "api" then text.sub("apiVersion: v1", "apiVersion: example/v1")
      when "duplicate" then text + "\nimmutable: true\n"
      when "cell" then text.sub("witself.io/cell: civo-sandbox-use1-serving", "witself.io/cell: civo-sandbox-use1-backup")
      when "missing_cell" then text.sub("  annotations:\n    witself.io/cell: civo-sandbox-use1-serving\n", "")
      end
    File.binwrite(ARGV.fetch(1), text)
  ' "$fixture" "$work_dir/fixtures/invalid.yaml" "$mutation"
  expect_fail "invalid manifest: $mutation" bash "$tool" encrypt "$cell" "$work_dir/fixtures/invalid.yaml"
done
ruby -e 'File.binwrite(ARGV.fetch(1), File.binread(ARGV.fetch(0)).sub("synthetic-v1", "invalid-v1"))' "$fixture" "$work_dir/fixtures/invalid.yaml"
printf '\n---\nkind: Secret\n' >>"$work_dir/fixtures/invalid.yaml"
expect_fail "multiple YAML documents" bash "$tool" encrypt "$cell" "$work_dir/fixtures/invalid.yaml"
mutate_manifest 'one: ' 'one: &shared '
ruby -e 'p=ARGV.fetch(0); File.binwrite(p, File.binread(p).sub("synthetic-v1", "invalid-v1"))' "$work_dir/fixtures/variant.yaml"
printf '\n  three: *shared\n' >>"$work_dir/fixtures/variant.yaml"
expect_fail "YAML aliases" bash "$tool" encrypt "$cell" "$work_dir/fixtures/variant.yaml"

cp "$fixture" "$secret_root/plain.yaml"
expect_fail "check rejects plaintext manifest" bash "$tool" check
rm "$secret_root/plain.yaml"
printf 'synthetic-private-provider-output\n' >"$secret_root/$cell/.gitkeep"
expect_fail "check rejects nonempty placeholder" bash "$tool" check
: >"$secret_root/$cell/.gitkeep"
cp "$encrypted" "$secret_root/renamed.yaml"
expect_fail "check rejects encrypted YAML extension" bash "$tool" check
rm "$secret_root/renamed.yaml"
printf '{"data":' >"$encrypted"
expect_fail "check rejects truncated envelope" bash "$tool" check
cp "$work_dir/fixtures/good.sops" "$encrypted"
ruby -rjson -e 'doc = JSON.parse(File.read(ARGV.fetch(0))); doc["kind"] = "Secret"; File.write(ARGV.fetch(0), JSON.generate(doc))' "$encrypted"
expect_fail "check rejects plaintext envelope fields" bash "$tool" check
cp "$work_dir/fixtures/good.sops" "$encrypted"
ruby -rjson -e 'doc = JSON.parse(File.read(ARGV.fetch(0))); doc["data"] = {"one" => "ENC[not-whole-document]"}; File.write(ARGV.fetch(0), JSON.generate(doc))' "$encrypted"
expect_fail "check rejects structured SOPS encryption" bash "$tool" check
cp "$work_dir/fixtures/good.sops" "$encrypted"
ruby -rjson -e 'doc = JSON.parse(File.read(ARGV.fetch(0))); doc["sops"].delete("mac"); File.write(ARGV.fetch(0), JSON.generate(doc))' "$encrypted"
expect_fail "check rejects incomplete SOPS metadata" bash "$tool" check
cp "$work_dir/fixtures/good.sops" "$encrypted"
ruby -rjson -e 'raw = File.read(ARGV.fetch(0)); doc = JSON.parse(raw); File.write(ARGV.fetch(0), raw.sub(/\{/, "{\"data\":" + JSON.generate(doc["data"]) + ","))' "$encrypted"
expect_fail "check rejects duplicate envelope keys" bash "$tool" check
cp "$work_dir/fixtures/good.sops" "$encrypted"
"$REAL_SOPS" --encrypt --input-type binary --output-type binary --age "$other_recipient" "$fixture" >"$encrypted" 2>"$work_dir/logs/direct-sops"
expect_fail "check rejects different recipient" bash "$tool" check
cp "$work_dir/fixtures/good.sops" "$encrypted"
cp "$fixture" "$secret_root/$cell/monitoring/plain.sops"
expect_fail "check rejects plaintext disguised as SOPS" bash "$tool" check
rm "$secret_root/$cell/monitoring/plain.sops"
ln -s "$work_dir/fixtures/good.sops" "$secret_root/$cell/monitoring/link.sops"
expect_fail "check rejects symlink artifacts" bash "$tool" check
expect_fail "apply rejects symlink artifacts" bash "$tool" decrypt-apply "$cell" monitoring/link
rm "$secret_root/$cell/monitoring/link.sops"
ln "$encrypted" "$secret_root/$cell/monitoring/hardlink.sops"
expect_fail "check rejects multiply linked artifacts" bash "$tool" check
rm "$secret_root/$cell/monitoring/hardlink.sops"
mkdir "$work_dir/fixtures/outside"
ln -s "$work_dir/fixtures/outside" "$secret_root/$cell/linked"
mutate_manifest 'namespace: monitoring' 'namespace: linked'
expect_fail "encrypt rejects symlink namespace" bash "$tool" encrypt "$cell" "$work_dir/fixtures/variant.yaml"
rm "$secret_root/$cell/linked"
expect_ok "restored encrypted layout passes" bash "$tool" check

rm "$encrypted"
expect_fail "SOPS encryption errors leave no artifact" env FAKE_SOPS_FAILURE=1 bash "$tool" encrypt "$cell" "$fixture"
[[ ! -e "$encrypted" ]] || fail "failed encryption left artifact"
expect_ok "stdin encryption" bash "$tool" encrypt "$cell" - <"$fixture"
expect_ok "stdin encryption round trip" bash "$tool" decrypt-apply "$cell" monitoring/synthetic-v1
assert_applies 5
expect_fail "SOPS decryption errors cannot reach kubectl" env FAKE_SOPS_FAILURE=1 bash "$tool" decrypt-apply "$cell"
assert_applies 5

rm "$encrypted"
write_live identical
expect_ok "export immutable live Secret" bash "$tool" export "$cell" monitoring/synthetic-v1
expect_ok "export output satisfies CI check" bash "$tool" check
"$REAL_SOPS" --decrypt --input-type binary --output-type binary "$encrypted" 2>"$work_dir/logs/direct-sops" |
  ruby -rjson -ryaml -e '
    doc = YAML.safe_load(STDIN.read)
    expected = YAML.safe_load(File.binread(ARGV.fetch(0)))
    abort "export changed Secret or retained server metadata" unless doc == expected
  ' "$fixture" || fail "export manifest contract"
expect_fail "export cannot overwrite ciphertext" bash "$tool" export "$cell" monitoring/synthetic-v1
rm "$encrypted"
write_live mutable
expect_fail "export rejects mutable Secret" bash "$tool" export "$cell" monitoring/synthetic-v1
rm "$work_dir/cluster/monitoring--synthetic-v1.json"
expect_fail "export missing Secret" bash "$tool" export "$cell" monitoring/synthetic-v1
expect_fail "export cluster failure" env FAKE_GET_FAILURE=1 bash "$tool" export "$cell" monitoring/synthetic-v1
[[ ! -e "$encrypted" ]] || fail "failed export left artifact"

mkdir -p "$work_dir/no-sops" "$work_dir/no-age"
ln -s "$(command -v age)" "$work_dir/no-sops/age"
ln -s "$REAL_SOPS" "$work_dir/no-age/sops"
expect_fail "missing sops fails closed" env PATH="$work_dir/no-sops:/usr/bin:/bin" bash "$tool" check
expect_fail "missing age fails closed" env PATH="$work_dir/no-age:/usr/bin:/bin" bash "$tool" check
expect_ok "final empty layout check" bash "$tool" check
assert_clean
printf 'test-cell-secrets: tool TMPDIR entries: []\n'
printf 'test-cell-secrets: PASS (%s cases; real SOPS/age, local stubs only)\n' "$passes"
