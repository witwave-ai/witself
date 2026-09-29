#!/usr/bin/env bash
# Offline copy/compare contract checks; every kubectl invocation is a local fake.
set -Eeuo pipefail
umask 077
# Harness diagnostics are withheld; only fail writes to the saved stderr.
exec 3>&2
exec 2>/dev/null

case_id=setup
fail() {
  printf 'test-copy-cell-secrets: FAIL: %s: %s (captured output withheld)\n' "$case_id" "$1" >&3
  exit 1
}
trap 'fail "unexpected harness failure"' ERR
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
work_dir="$(mktemp -d "${TMPDIR:-/tmp}/witself-copy-cell-secrets.XXXXXX")"
trap 'rm -rf "$work_dir"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
work_dir="$(cd "$work_dir" && pwd -P)"
mkdir -p "$work_dir/bin" "$work_dir/fixtures" "$work_dir/clusters/a" "$work_dir/clusters/b" \
  "$work_dir/repo/scripts/lib" "$work_dir/cwd" "$work_dir/tool-tmp" "$work_dir/maps"
cp "$repo_root/scripts/copy-cell-secrets.sh" "$work_dir/repo/scripts/"
cp "$repo_root/scripts/lib/copy-cell-secrets.rb" "$repo_root/scripts/lib/cell-secrets.rb" "$work_dir/repo/scripts/lib/"
tool="$work_dir/repo/scripts/copy-cell-secrets.sh"
S=civo-sandbox-use1-serving
P=civo-prod-use1-serving
B=civo-sandbox-use1-backup
C=witself/witself-agent-email-receive-cohort-v1
K=witself/witself-agent-email-retry-canary-v1
reads='NS:S NS:P APP:S APP:P GET:S:C GET:S:K GET:P:C GET:P:K'
binding='NS:S NS:P APP:S APP:P'
source_c="$binding GET:S:C"
match_lines=$(printf '%s match\n%s match' "$C" "$K")
mismatch_lines=$(printf '%s mismatch\n%s match' "$C" "$K")
usage='usage: copy-cell-secrets.sh copy|compare SOURCE_CELL TARGET_CELL [--replace] NAMESPACE/NAME...'
allowed='only witself/witself-agent-email-receive-cohort-vN and witself/witself-agent-email-retry-canary-vN may be copied or compared'
refusal="${C} is referenced by a workload on ${P}; create a new -vN Secret instead of replacing it"
source_invalid="source Secret ${C} on ${S} is not an immutable Secret with data"
invalid_s="kubectl get failed or returned an invalid response on ${S}"
invalid_p="kubectl get failed or returned an invalid response on ${P}"

# Values occur only in this tracked harness and the excluded fixture storage.
ruby - "$work_dir" <<'RUBY' >"$work_dir/setup.out" 2>"$work_dir/setup.err"
root = ARGV.fetch(0)
values = {
  'VC' => 'acc_synthcohortaaaaa,acc_synthcohortbbbbb',
  'VK' => 'agent_synthcanaryaaaaa'
}
values['VN'] = values.fetch('VC') + "\n"
values.each do |key, value|
  File.binwrite("#{root}/fixtures/#{key}", value)
  File.binwrite("#{root}/fixtures/#{key}.b64", [value].pack('m0'))
end
snapshot = Dir.glob("#{root}/repo/**/*", File::FNM_DOTMATCH).select { |path| File.file?(path) }
  .map { |path| [path.sub("#{root}/repo/", ''), File.binread(path)] }.to_h
File.binwrite("#{root}/fixtures/repo-snapshot", Marshal.dump(snapshot))
RUBY

cat >"$work_dir/bin/kubectl" <<'FAKE'
#!/usr/bin/env bash
set -Eeuo pipefail
work=${BASH_SOURCE[0]%/bin/kubectl}
printf '%s\0' "$#" >>"$work/argv-log"
if [[ $# -gt 0 ]]; then
  printf '%s\0' "$@" >>"$work/argv-log"
fi
[[ $# -ge 4 && $1 == --context && $3 == --request-timeout=20s ]] || exit 64
context=$2
shift 3
[[ -f "$work/maps/$context" ]] || exit 1
IFS= read -r cluster <"$work/maps/$context"
root="$work/clusters/$cluster"
op=unknown
name=
if [[ $# -eq 5 && $1 == get && $2 == namespace && $3 == kube-system && $4 == -o && $5 == json ]]; then
  op=get-namespace
elif [[ $# -eq 8 && $1 == --namespace && $2 == argocd && $3 == get && $4 == applications.argoproj.io && $5 == witself-server && $6 == --ignore-not-found && $7 == -o && $8 == json ]]; then
  op=get-application
elif [[ $# -eq 8 && $1 == --namespace && $2 == witself && $3 == get && $4 == secret && $6 == --ignore-not-found && $7 == -o && $8 == json ]]; then
  op=get-secret
  name=$5
elif [[ $# -eq 6 && $1 == --namespace && $2 == witself && $3 == get && $4 == deployments,statefulsets,daemonsets,jobs,cronjobs && $5 == -o && $6 == json ]]; then
  op=get-workloads
elif [[ $# -eq 6 && $1 == --namespace && $2 == witself && $3 == create && $4 == --save-config=false && $5 == -f && $6 == - ]]; then
  op=create
elif [[ $# -eq 5 && $1 == --namespace && $2 == witself && $3 == delete && $4 == secret ]]; then
  op=delete
  name=$5
else
  exit 64
fi
if [[ ${FAKE_KUBECTL_FAIL:-} == "$op@$context" ]]; then
  cat "$work"/fixtures/V* >&2
  exit 1
fi
if [[ $op == get-secret && ${FAKE_KUBECTL_FAIL:-} == "malformed-secret@$context" ]]; then
  ruby -rjson -e 'doc = JSON.parse(File.binread(ARGV.fetch(0))); STDOUT.write(JSON.generate(doc)[0...-2])' "$root/$name.json"
  exit 0
fi
case "$op" in
  get-namespace) cat "$root/namespace.json" ;;
  get-application) [[ ! -f "$root/application.json" ]] || cat "$root/application.json" ;;
  get-secret) [[ ! -f "$root/$name.json" ]] || cat "$root/$name.json" ;;
  get-workloads) cat "$root/workloads.json" ;;
  delete)
    [[ -f "$root/$name.json" ]] || exit 1
    rm "$root/$name.json"
    ;;
  create)
    ruby "$work/create.rb" "$root" "${FAKE_KUBECTL_FAIL:-}" "$context"
    ;;
esac
if [[ ${FAKE_KUBECTL_NOISE:-} == 1 ]]; then
  cat "$work"/fixtures/V* >&2
fi
FAKE
# NEW leaves its stdin available to Ruby, whose program is a separate file.
cat >"$work_dir/create.rb" <<'RUBY'
require 'json'
root, failure, context = ARGV
input = STDIN.read
doc = JSON.parse(input)
name = doc.fetch('metadata').fetch('name')
path = "#{root}/#{name}.json"
if File.exist?(path)
  STDERR.write(input)
  exit 1
end
if failure == "readback@#{context}"
  doc.fetch('data').transform_values! { |value| [value.unpack1('m0') + "\n"].pack('m0') }
  input = JSON.generate(doc) + "\n"
end
File.binwrite(path, input)
puts "secret/#{name} created"
RUBY
for stub in sops op age jq yq base64 cmp diff shasum sha256sum sha1sum md5 md5sum openssl xxd od hexdump tee curl wget pbcopy security gpg logger script; do
  cat >"$work_dir/bin/$stub" <<'STUB'
#!/usr/bin/env bash
work=${BASH_SOURCE[0]%/bin/*}
printf '%s\n' "${BASH_SOURCE[0]##*/}" >>"$work/tripwire-log"
exit 97
STUB
  chmod 700 "$work_dir/bin/$stub"
done
chmod 700 "$work_dir/bin/kubectl"
[[ $(env PATH="$work_dir/bin:$PATH" bash -c 'command -v kubectl') == "$work_dir/bin/kubectl" ]] || fail 'fake kubectl not first'

cat >"$work_dir/state.rb" <<'RUBY'
require 'json'
root, action, *args = ARGV
cells = { 'a' => 'civo-sandbox-use1-serving', 'b' => 'civo-prod-use1-serving' }
names = { 'C' => 'witself-agent-email-receive-cohort-v1', 'K' => 'witself-agent-email-retry-canary-v1' }
write = lambda { |path, doc| File.binwrite(path, JSON.generate(doc) + "\n") }
case action
when 'binding'
  cells.each do |cluster, cell|
    File.write("#{root}/maps/witself-#{cell}", "#{cluster}\n")
    write.call("#{root}/clusters/#{cluster}/namespace.json", {
      'apiVersion' => 'v1', 'kind' => 'Namespace', 'metadata' => { 'name' => 'kube-system', 'uid' => "uid-#{cluster}" }
    })
    write.call("#{root}/clusters/#{cluster}/application.json", {
      'apiVersion' => 'argoproj.io/v1alpha1', 'kind' => 'Application',
      'metadata' => { 'labels' => { 'witself.io/cell' => cell } }
    })
  end
when 'store'
  cluster, which, value, variant = args
  key = which == 'C' ? 'account_ids' : 'agent_id'
  name = names.fetch(which)
  data = { key => File.binread("#{root}/fixtures/#{value}.b64") }
  metadata = { 'name' => name, 'namespace' => 'witself', 'uid' => "stored-#{cluster}",
    'resourceVersion' => 'synthetic-version', 'creationTimestamp' => '2026-09-29T00:00:00Z',
    'managedFields' => [{ 'fieldsV1' => { 'f:data' => { "f:#{key}" => {} } } }] }
  doc = { 'apiVersion' => 'v1', 'kind' => 'Secret', 'metadata' => metadata,
    'type' => 'Opaque', 'immutable' => true, 'data' => data }
  annotation = JSON.generate(doc)
  metadata['annotations'] = { 'kubectl.kubernetes.io/last-applied-configuration' => annotation }
  case variant
  when 'restored'
    metadata['uid'] = 'restored-uid'
    metadata['annotations']['witself.io/cell'] = cells.fetch(cluster)
  when 'key' then data['account_id'] = data.delete('account_ids')
  when 'extra' then data['extra'] = data.fetch(key)
  when 'type' then doc['type'] = 'kubernetes.io/basic-auth'
  when 'mutable' then doc['immutable'] = false
  when 'no-immutable' then doc.delete('immutable')
  when 'empty' then doc['data'] = {}
  when 'stringData' then doc['stringData'] = { key => File.binread("#{root}/fixtures/#{value}") }
  when 'noncanonical' then data[key] = 'YWJjZA'
  end
  write.call("#{root}/clusters/#{cluster}/#{name}.json", doc)
when 'remove'
  cluster, which = args
  File.delete("#{root}/clusters/#{cluster}/#{names.fetch(which)}.json")
when 'empty'
  Dir.glob("#{root}/clusters/#{args.fetch(0)}/witself-agent-email-*.json").each { |path| File.delete(path) }
when 'workloads'
  form = args.fetch(0)
  name = form == 'unrelated' ? 'witself-db' : names.fetch('C')
  env = { 'name' => 'fixture', 'valueFrom' => { 'secretKeyRef' => { 'name' => name, 'key' => 'account_ids' } } }
  pod = { 'containers' => [{ 'name' => 'fixture' }] }
  kind = 'Deployment'
  case form
  when 'a', 'f', 'unrelated' then pod['containers'][0]['env'] = [env]
  when 'b' then pod['initContainers'] = [{ 'name' => 'fixture-init', 'env' => [env] }]
  when 'c' then pod['containers'][0]['envFrom'] = [{ 'secretRef' => { 'name' => name } }]
  when 'd'
    kind = 'StatefulSet'
    pod['volumes'] = [{ 'name' => 'fixture', 'secret' => { 'secretName' => name } }]
  when 'e'
    kind = 'DaemonSet'
    pod['volumes'] = [{ 'name' => 'fixture', 'projected' => { 'sources' => [{ 'secret' => { 'name' => name } }] } }]
  end
  item = { 'apiVersion' => 'apps/v1', 'kind' => kind, 'spec' => { 'template' => { 'spec' => pod } } }
  if form == 'f'
    item = { 'apiVersion' => 'batch/v1', 'kind' => 'CronJob', 'spec' => { 'jobTemplate' => { 'spec' => item.fetch('spec') } } }
  end
  items = form == 'empty' ? [] : [item]
  write.call("#{root}/clusters/b/workloads.json", { 'apiVersion' => 'v1', 'kind' => 'List', 'items' => items })
when 'map'
  cell, cluster = args
  File.write("#{root}/maps/witself-#{cell}", "#{cluster}\n")
when 'label'
  path = "#{root}/clusters/b/application.json"
  if args.fetch(0) == 'absent'
    File.delete(path)
  else
    write.call(path, { 'metadata' => { 'labels' => { 'witself.io/cell' => args.fetch(0) } } })
  end
when 'uid-empty'
  path = "#{root}/clusters/b/namespace.json"
  doc = JSON.parse(File.binread(path))
  doc.fetch('metadata')['uid'] = ''
  write.call(path, doc)
when 'save-target'
  File.binwrite("#{root}/fixtures/unchanged-target", File.binread("#{root}/clusters/b/#{names.fetch('C')}.json"))
end
RUBY
state() {
  ruby "$work_dir/state.rb" "$work_dir" "$@" >"$work_dir/setup.out" 2>"$work_dir/setup.err" || fail 'fixture setup'
}

cat >"$work_dir/audit.rb" <<'RUBY'
require 'json'
require 'digest'
root, actual_exit, expected_exit, shape_text, objects, unchanged = ARGV
check = lambda { |ok, status| exit(status) unless ok }
begin
  check.call(File.binread("#{root}/stdout") == File.binread("#{root}/expected-out"), 11)
  check.call(File.binread("#{root}/stderr") == File.binread("#{root}/expected-err"), 12)
  check.call(actual_exit == expected_exit, 13)
  cells = { 'S' => 'civo-sandbox-use1-serving', 'P' => 'civo-prod-use1-serving' }
  names = { 'C' => 'witself-agent-email-receive-cohort-v1', 'K' => 'witself-agent-email-retry-canary-v1' }
  expected = shape_text.split.map do |shape|
    op, cell, which = shape.split(':')
    prefix = ['--context', "witself-#{cells.fetch(cell)}", '--request-timeout=20s']
    rest = case op
    when 'NS' then %w[get namespace kube-system -o json]
    when 'APP' then %w[--namespace argocd get applications.argoproj.io witself-server --ignore-not-found -o json]
    when 'GET' then ['--namespace', 'witself', 'get', 'secret', names.fetch(which), '--ignore-not-found', '-o', 'json']
    when 'WL' then %w[--namespace witself get deployments,statefulsets,daemonsets,jobs,cronjobs -o json]
    when 'DEL' then ['--namespace', 'witself', 'delete', 'secret', names.fetch(which)]
    when 'NEW' then %w[--namespace witself create --save-config=false -f -]
    end
    prefix + rest
  end
  log = File.exist?("#{root}/argv-log") ? File.binread("#{root}/argv-log") : ''
  offset = File.exist?("#{root}/argv-offset") ? File.read("#{root}/argv-offset").to_i : 0
  fields = log.byteslice(offset..-1).split("\0", -1)
  fields.pop if fields.last == ''
  records = []
  cursor = 0
  while cursor < fields.length
    check.call(fields[cursor].match?(/\A[0-9]+\z/), 14)
    count = fields[cursor].to_i
    cursor += 1
    check.call(cursor + count <= fields.length, 14)
    records << fields[cursor, count]
    cursor += count
  end
  check.call(records == expected, 14)
  check.call(records.none? { |record| record[1] == "witself-#{cells.fetch('S')}" && (record.include?('create') || record.include?('delete')) }, 14)
  File.write("#{root}/argv-offset", log.bytesize.to_s)
  check.call(!File.exist?("#{root}/tripwire-log"), 15)
  check.call(Dir.children("#{root}/cwd").empty? && Dir.children("#{root}/tool-tmp").empty?, 16)
  snapshot = Dir.glob("#{root}/repo/**/*", File::FNM_DOTMATCH).select { |path| File.file?(path) }
    .map { |path| [path.sub("#{root}/repo/", ''), File.binread(path)] }.to_h
  check.call(snapshot == Marshal.load(File.binread("#{root}/fixtures/repo-snapshot")), 16)
  unless objects.empty?
    objects.split.each do |entry|
      which, fixture = entry.split(':')
      name = names.fetch(which)
      source = JSON.parse(File.binread("#{root}/clusters/a/#{name}.json"))
      bytes = File.binread("#{root}/clusters/b/#{name}.json")
      doc = JSON.parse(bytes)
      check.call(bytes.end_with?("\n") && !bytes.end_with?("\n\n"), 18)
      check.call(doc.keys.sort == %w[apiVersion kind metadata type immutable data].sort && doc['apiVersion'] == 'v1' && doc['kind'] == 'Secret', 18)
      metadata = doc.fetch('metadata')
      check.call(metadata.keys.sort == %w[name namespace annotations].sort && metadata['name'] == name && metadata['namespace'] == 'witself', 18)
      check.call(metadata['annotations'] == { 'witself.io/cell' => cells.fetch('P') }, 18)
      check.call(doc['data'] == source['data'] && doc.fetch('data').values.all? { |value| value.unpack1('m0') == File.binread("#{root}/fixtures/#{fixture}") }, 19)
      check.call(doc['immutable'] == true && doc['type'] == source['type'], 20)
    end
  end
  if unchanged == 'yes'
    check.call(File.binread("#{root}/clusters/b/#{names.fetch('C')}.json") == File.binread("#{root}/fixtures/unchanged-target"), 21)
  end
  values = %w[VC VK VN].map { |key| File.binread("#{root}/fixtures/#{key}") }
  needles = values.flat_map { |value| [value, [value].pack('m0'), Digest::SHA256.hexdigest(value), Digest::SHA1.hexdigest(value), Digest::MD5.hexdigest(value)] }
  # Derive the two marker strings from the fixture values without copying them.
  needles << values[0].split(',').first.split('_').last.sub(/a+\z/, '')
  needles << values[1].split('_').last.sub(/a+\z/, '')
  Dir.glob("#{root}/**/*", File::FNM_DOTMATCH).select { |path| File.file?(path) }.each do |path|
    relative = path.sub("#{root}/", '')
    next if relative.start_with?('fixtures/', 'clusters/')
    bytes = File.binread(path)
    if needles.any? { |needle| bytes.include?(needle) } || bytes.match?(/[a-fA-F0-9]{64}/)
      puts relative
      exit 17
    end
  end
  puts 'pass'
rescue StandardError
  exit 22
end
RUBY

cases=0
tool_runs=0
injection=
noise=
objects=
unchanged=
run_case() {
  case_id=$1
  local expected_status=$2 expected_stdout=$3 expected_stderr=$4 shapes=$5 actual_status=0 audit_status=0
  shift 5
  : >"$work_dir/expected-out"
  : >"$work_dir/expected-err"
  [[ -z $expected_stdout ]] || printf '%s\n' "$expected_stdout" >"$work_dir/expected-out"
  [[ -z $expected_stderr ]] || printf 'copy-cell-secrets: %s\n' "$expected_stderr" >"$work_dir/expected-err"
  if [[ $# -eq 0 ]]; then
    # Case 16a deliberately invokes a literal no-argument command (Bash 3.2).
    (cd "$work_dir/cwd" && env PATH="$work_dir/bin:$PATH" KUBECONFIG="$work_dir/no-kubeconfig" TMPDIR="$work_dir/tool-tmp" \
      FAKE_KUBECTL_FAIL="$injection" FAKE_KUBECTL_NOISE="$noise" bash "$tool") 3>&- >"$work_dir/stdout" 2>"$work_dir/stderr" || actual_status=$?
  else
    (cd "$work_dir/cwd" && env PATH="$work_dir/bin:$PATH" KUBECONFIG="$work_dir/no-kubeconfig" TMPDIR="$work_dir/tool-tmp" \
      FAKE_KUBECTL_FAIL="$injection" FAKE_KUBECTL_NOISE="$noise" bash "$tool" "$@") 3>&- >"$work_dir/stdout" 2>"$work_dir/stderr" || actual_status=$?
  fi
  ruby "$work_dir/audit.rb" "$work_dir" "$actual_status" "$expected_status" "$shapes" "$objects" "$unchanged" \
    >"$work_dir/audit.out" 2>"$work_dir/audit.err" || audit_status=$?
  case "$audit_status" in
    0) ;;
    11) fail 'A1 stdout' ;;
    12) fail 'A1 stderr' ;;
    13) fail 'A2 exit status' ;;
    14) fail 'A3 argv sequence and source protection' ;;
    15) fail 'A4 tripwire' ;;
    16) fail 'A5 filesystem' ;;
    17) fail 'A6 private bytes' ;;
    18) fail 'O1 created metadata and shape' ;;
    19) fail 'O2 created bytes' ;;
    20) fail 'O3 created immutability and type' ;;
    21) fail 'unchanged target' ;;
    *) fail 'audit operation' ;;
  esac
  cases=$((cases + 1))
  tool_runs=$((tool_runs + 1))
}

state binding
state workloads empty
state store a C VC
state store a K VK
objects='C:VC K:VK'
run_case 1 0 "$match_lines" '' "$reads NEW:P GET:P:C NEW:P GET:P:K" copy "$S" "$P" "$C" "$K"
objects=
run_case 2 0 "$match_lines" '' "$reads" copy "$S" "$P" "$C" "$K"
run_case 3 0 "$match_lines" '' "$reads" compare "$S" "$P" "$C" "$K"
noise=1
run_case 4 0 "$match_lines" '' "$reads" compare "$S" "$P" "$C" "$K"
noise=
state store b C VC restored
run_case 5 0 "$match_lines" '' "$reads" compare "$S" "$P" "$C" "$K"
state store b C VN
run_case 6 3 "$mismatch_lines" '' "$reads" compare "$S" "$P" "$C" "$K"
state save-target
unchanged=yes
run_case 7 3 "$mismatch_lines" '' "$reads" copy "$S" "$P" "$C" "$K"
unchanged=
state workloads unrelated
objects='C:VC K:VK'
run_case 8 0 "$match_lines" '' "$reads WL:P DEL:P:C NEW:P GET:P:C" copy "$S" "$P" "$C" "$K" --replace
objects=
state store a C VN
run_case 9 3 "$mismatch_lines" '' "$reads" compare "$S" "$P" "$C" "$K"
objects='C:VN K:VK'
run_case 10 0 "$match_lines" '' "$reads WL:P DEL:P:C NEW:P GET:P:C" copy "$S" "$P" "$C" "$K" --replace
objects=
state store a C VC
state store b C VC
for variant in a:key b:extra c:type d:mutable e:no-immutable f:empty g:stringData h:noncanonical; do
  state store b C VC "${variant#*:}"
  run_case "11${variant%%:*}" 3 "$mismatch_lines" '' "$reads" compare "$S" "$P" "$C" "$K"
done
# An absent target is a mismatch, never a match.
state remove b C
run_case 11i 3 "$mismatch_lines" '' "$reads" compare "$S" "$P" "$C" "$K"
state store b C VN
for form in a b c d e f; do
  state workloads "$form"
  run_case "12$form" 1 '' "$refusal" "$reads WL:P" copy "$S" "$P" "$C" "$K" --replace
done
state store b C VC
state map "$P" a
run_case 13a 1 '' "witself-${S} and witself-${P} reach the same cluster" 'NS:S NS:P' copy "$S" "$P" "$C" "$K" --replace
state map "$S" b
state map "$P" a
run_case 13b 1 '' "context witself-${S} does not identify cell ${S}" 'NS:S NS:P APP:S' copy "$S" "$P" "$C" "$K" --replace
state binding
state label absent
run_case 13c 1 '' "context witself-${P} does not identify cell ${P}" "$binding" compare "$S" "$P" "$C" "$K"
state label "$B"
run_case 13d 1 '' "context witself-${P} does not identify cell ${P}" "$binding" copy "$S" "$P" "$C" "$K"
state binding
state uid-empty
run_case 13e 1 '' "$invalid_p" 'NS:S NS:P' copy "$S" "$P" "$C" "$K"
state binding
state empty b
state remove a C
run_case 14a 1 '' "source Secret ${C} is absent on ${S}" "$source_c" copy "$S" "$P" "$C" "$K"
for variant in b:mutable c:empty d:noncanonical e:stringData; do
  state store a C VC "${variant#*:}"
  run_case "14${variant%%:*}" 1 '' "$source_invalid" "$source_c" copy "$S" "$P" "$C" "$K"
done
state store a C VC
injection="get-secret@witself-$S"
run_case 15a 1 '' "$invalid_s" "$source_c" copy "$S" "$P" "$C" "$K"
state store b C VC
state store b K VK
injection="malformed-secret@witself-$P"
run_case 15b 1 '' "$invalid_p" "$source_c GET:S:K GET:P:C" compare "$S" "$P" "$C" "$K"
state remove b C
injection="create@witself-$P"
run_case 15c 1 '' "kubectl create failed on ${P} for ${C}" "$reads NEW:P" copy "$S" "$P" "$C" "$K"
injection="readback@witself-$P"
run_case 15d 1 '' "${C} did not read back identical on ${P} after the write" "$reads NEW:P GET:P:C" copy "$S" "$P" "$C" "$K"
state workloads empty
injection="delete@witself-$P"
run_case 15e 1 '' "kubectl delete failed on ${P} for ${C}; the Secret may now be absent there, re-run copy" "$reads WL:P DEL:P:C" copy "$S" "$P" "$C" "$K" --replace
injection="get-workloads@witself-$P"
run_case 15f 1 '' "$invalid_p" "$reads WL:P" copy "$S" "$P" "$C" "$K" --replace
injection="get-namespace@witself-$P"
run_case 15g 1 '' "$invalid_p" 'NS:S NS:P' compare "$S" "$P" "$C" "$K"
injection=
run_case 16a 1 '' "$usage" ''
run_case 16b 1 '' "$usage" '' apply "$S" "$P" "$C"
run_case 16c 1 '' "$usage" '' copy "$S" "$P"
run_case 16d 1 '' "$usage" '' copy "$S" "$P" "$C" --force
run_case 16e 1 '' "$usage" '' copy "$S" "$P" "$C" --replace --replace
run_case 16f 1 '' '--replace is only valid with copy' '' compare "$S" "$P" "$C" --replace
run_case 16g 1 '' 'unknown cell' '' copy civo-sandbox-usw2-dev "$P" "$C"
run_case 16h 1 '' 'source and target cells must differ' '' copy "$S" "$S" "$C"
for rejected in \
  witself/witself-agent-email-provider-event-v2 \
  witself/witself-agent-email-outbound-dispatch-v1 \
  witself/witself-db \
  monitoring/witself-agent-email-receive-cohort-v1 \
  witself/witself-agent-email-receive-cohort-v0 \
  witself/witself-agent-email-receive-cohort \
  witself/witself-agent-email-receive-cohort-v1/x \
  witself/witself-agent-email-receive-cohort-v1000; do
  run_case 16i 1 '' "$allowed" '' copy "$S" "$P" "$rejected"
done
cases=$((cases - 7))
VC=$(cat "$work_dir/fixtures/VC")
run_case 16j 1 '' "$allowed" '' copy "$S" "$P" "$VC"
unset VC
run_case 16k 1 '' 'each Secret may be named once' '' copy "$S" "$P" "$C" "$C"
run_case 16l 1 '' "$usage" '' copy "$S" "$P" \
  "$C" witself/witself-agent-email-receive-cohort-v2 witself/witself-agent-email-receive-cohort-v3 \
  witself/witself-agent-email-receive-cohort-v4 witself/witself-agent-email-receive-cohort-v5 \
  witself/witself-agent-email-receive-cohort-v6 witself/witself-agent-email-receive-cohort-v7 \
  witself/witself-agent-email-receive-cohort-v8 witself/witself-agent-email-receive-cohort-v9
run_case 16m 1 '' 'civo-sandbox-use1-backup runs no agent email' '' copy "$S" "$B" "$C"
run_case 16n 1 '' 'civo-sandbox-use1-backup runs no agent email' '' compare "$B" "$P" "$C"

case_id=17
ruby - "$work_dir/repo" <<'RUBY' >"$work_dir/static.out" 2>"$work_dir/static.err" || fail 'static contract'
root = ARGV.fetch(0)
helper = File.binread("#{root}/scripts/lib/copy-cell-secrets.rb")
wrapper = File.binread("#{root}/scripts/copy-cell-secrets.sh")
original = File.binread("#{root}/scripts/lib/cell-secrets.rb")
raise unless helper.scan('Open3.capture3').length == 1
raise unless helper.scan(/Open3\.[A-Za-z0-9_]+/) == ['Open3.capture3']
raise unless helper.scan('kube-system').length == 1
reduced = helper.sub('kube-system', '')
[
  /\b(system|spawn|popen|popen2|popen3|fork|exec|syscall)\b/,
  /`|%x/,
  /\b(File|Dir|Tempfile|IO|STDOUT|STDERR|Kernel|ENV)\b|getenv|\$std(out|err)/,
  /\b(print|printf|pp|p|putc|display|syswrite|write_nonblock)\b/
].each { |pattern| raise if reduced.match?(pattern) }
raise unless helper.scan(/\bputs\b/).length == 1 && helper.scan(/\bwarn\b/).length == 1
['set +x', 'set -Eeuo pipefail', 'umask 077', 'ulimit -c 0',
 'exec env -u RUBYOPT -u RUBYLIB ruby "$repo_root/scripts/lib/copy-cell-secrets.rb" "$@"'].each do |line|
  raise unless wrapper.lines.map(&:chomp).include?(line)
end
cells = /CELLS = %w\[([^\]]+)\]/
raise unless helper.match(cells)[1].split == original.match(cells)[1].split
RUBY
ruby "$work_dir/audit.rb" "$work_dir" 1 1 '' '' '' \
  >"$work_dir/audit.out" 2>"$work_dir/audit.err" || fail 'A1-A6 after static checks'
cases=$((cases + 1))
[[ $cases -eq 57 && $tool_runs -eq 63 ]] || fail 'case and invocation counts'
printf 'test-copy-cell-secrets: PASS (%s cases; fake kubectl, tripwire sops and op)\n' "$cases"
