#!/usr/bin/env bash
# Offline contract tests. Every network/CLI operation is intercepted on PATH.
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
canary="${AVATAR_ACCEPTANCE_SCRIPT:-$repo_root/scripts/run-avatar-acceptance.sh}"
work_dir="$(mktemp -d "${TMPDIR:-/tmp}/witself-avatar-acceptance-test.XXXXXX")"
trap 'rm -rf "$work_dir"' EXIT
umask 077
chmod 700 "$work_dir"
mkdir "$work_dir/bin"

# Python is test-only: stdlib tarfile emits the shipped manifest/NDJSON/checksum
# layout, including unrelated account content that must never enter evidence.
cat >"$work_dir/bin/shim" <<'PY'
#!/usr/bin/env python3
import copy, datetime, hashlib, io, json, os, pathlib, sys, tarfile

root = pathlib.Path(os.environ['FAKE_AVATAR_STATE'])
scenario = os.environ.get('FAKE_AVATAR_SCENARIO', 'happy_path')
args = sys.argv[1:]
tool = pathlib.Path(sys.argv[0]).name
clock = root / 'clock'
now = float(clock.read_text()) if clock.exists() else int(datetime.datetime.now(datetime.timezone.utc).timestamp())
clock.write_text(str(now))
stamp = datetime.datetime.fromtimestamp(now, datetime.timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ')
account, realm, agent = 'acc_aaaaaaaaaaaaaaaa', 'realm_bbbbbbbbbbbbbbbb', 'agent_cccccccccccccccc'
backup = 'backup_20990101T000000Z'
svg = '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 256 256"><g data-layer="background"><rect width="256" height="256" fill="#ffffff"/></g><g data-layer="base-identity"><circle cx="128" cy="128" r="64" fill="#888888"/></g><g id="experience" data-layer="experience"></g></svg>'
identity = dict(account_id=account, realm_id=realm, agent_id=agent)
path = root / 'state.json'
state = json.loads(path.read_text()) if path.exists() else {
    'profile': dict(identity, status='generation_due', autonomy_policy='agent_self_managed',
        subject_form='human', style={'id':'witself-flat-portrait','version':1},
        lineage_generation=1, profile_revision=1, latest_avatar_version=0,
        active_avatar_version=0, proposed_avatar_version=0, retained_payload_count_limit=20,
        retained_payload_byte_limit=2097152, rollback_payload_floor=2, retained_payload_count=0,
        retained_payload_bytes=0, created_at=stamp, updated_at=stamp),
    'versions': [], 'activations': [], 'rejections': [], 'resets': [], 'keys': [], 'calls': []}
state['calls'].append({'tool':tool, 'args':args})
p = state['profile']
def save(): path.write_text(json.dumps(state))
def emit(value):
    save()
    dest=flag('--output', flag('-o')) if tool=='curl' else None
    if dest: pathlib.Path(dest).write_text(json.dumps(value))
    else: print(json.dumps(value))
    sys.exit(0)
def fail(message='avatar_conflict'):
    save()
    print('witself: HTTP 409: '+message, file=sys.stderr)
    sys.exit(1)
def flag(name, default=None):
    for n, arg in enumerate(args):
        if arg == name: return args[n+1]
        if arg.startswith(name+'='): return arg[len(name)+1:]
    return default
def version(n): return next(v for v in state['versions'] if v['version']==n)
def api_version(v):
    v=copy.deepcopy(v)
    if v.get('parent_version') is None: v.pop('parent_version',None)
    for field in ['svg','description','visual_spec']:
        if v.get(field) is None: v.pop(field,None)
    return v
def view():
    result = {'profile':copy.deepcopy(p)}
    for field in ['latest_avatar_version','active_avatar_version','proposed_avatar_version']:
        if not result['profile'][field]: result['profile'].pop(field)
    for field, pointer in [('active','active_avatar_version'),('proposed','proposed_avatar_version')]:
        if p[pointer]: result[field] = api_version(version(p[pointer]))
    return result
def receipt(op, n=0):
    p['profile_revision'] += 1
    p['retained_payload_count'] = sum(v['payload_state']=='full' for v in state['versions'])
    p['retained_payload_bytes'] = p['retained_payload_count']*1024
    for v in state['versions']:
        v['is_active'] = v['version']==p['active_avatar_version']
        v['is_proposed'] = v['version']==p['proposed_avatar_version']
        v['rollback_eligible'] = v['was_activated'] and not v['is_active'] and v['lineage_generation']==p['lineage_generation'] and v['payload_state']=='full'
    emit({'avatar':view(), 'receipt':{'operation':op, 'result_revision':p['profile_revision'],
        'result_version':n, 'result_lineage_generation':p['lineage_generation'],
        'request_hash':'f'*64, 'created_at':stamp}})

if tool == 'date':
    assert args[0]=='-u' and len(args)==2
    save()
    print(int(now) if args[1]=='+%s' else datetime.datetime.fromtimestamp(now, datetime.timezone.utc).strftime(args[1][1:]))
    sys.exit(0)
elif tool == 'sleep':
    delay=float(args[0])
    assert 0 < delay <= 60
    clock.write_text(str(now+delay))
    save()
    sys.exit(0)
elif tool == 'witself':
    if args == ['version']:
        save()
        commit = 'deadbee' if scenario=='release_pair_mismatch' else 'bd103ff'
        print('witself 0.0.275 (commit '+commit+', built 2026-09-06T00:21:06Z)')
        sys.exit(0)
    if args[:2] == ['account','list']:
        emit({'accounts':[{'name':'evac-a','id':account,'email':'private@example.invalid'}]})
    if args[:2] == ['agent','create']:
        assert flag('--realm')==realm and flag('--account')=='evac-a'
        save()
        print(agent+'\t'+args[-1])
        sys.exit(0)
    if args[:2] == ['token','create']:
        assert flag('--agent')==agent and flag('--account')=='evac-a'
        token = pathlib.Path(flag('--out'))
        token.write_text('witself_test_private_token')
        token.chmod(0o600)
        save()
        sys.exit(0)
    if args[0] == 'export':
        assert flag('--account')=='evac-a'
        profile = copy.deepcopy(p)
        profile['revision'] = profile.pop('profile_revision')
        for field in ['latest_avatar_version','active_avatar_version','proposed_avatar_version']:
            if not profile[field]: profile[field]=None
        profile.update(style_pack_id=profile['style']['id'],style_pack_version=profile['style']['version'])
        for field in ['style','retained_payload_count','retained_payload_bytes','rollback_payload_floor']: profile.pop(field,None)
        tables = {
            'avatar_style_packs': [], 'avatar_style_pack_versions': [], 'realm_avatar_styles': [],
            'avatar_style_rollout_jobs': [], 'agent_avatar_profiles':[profile],
            'agent_avatar_versions':copy.deepcopy(state['versions']),
            'agent_avatar_activations':state['activations'], 'agent_avatar_rejections':state['rejections'],
            'agent_avatar_resets':state['resets'], 'avatar_mutation_receipts':[],
            'facts':[dict(identity, agent_id='agent_unrelated', value='witself_private /Users/private <svg private')],
        }
        for row in tables['agent_avatar_versions']:
            row.update(style_pack_id=row['style']['id'],style_pack_version=row['style']['version'])
            row.update(proposed_by_kind=row['proposed_by']['kind'],proposed_by_id=row['proposed_by']['id'])
            for field in ['style','proposed_by','is_active','is_proposed','was_activated','rollback_eligible','rejected']: row.pop(field,None)
        if scenario=='archive_missing_version': tables['agent_avatar_versions'].pop(1)
        if scenario=='archive_hash_mismatch': tables['agent_avatar_versions'][0]['svg_sha256']='e'*64
        manifest = dict(format_version=1, schema_version=94, server_version='0.0.275',
            purpose='self', account_id=account, status='active', compression='gzip',
            exported_at=stamp, tables=list(tables))
        sums = {'chunks':[], 'table_rows':{t:len(rows) for t,rows in tables.items()}}
        with tarfile.open(flag('--out'), 'w:gz') as archive:
            def entry(name, data):
                info=tarfile.TarInfo(name); info.size=len(data); info.mode=0o600
                archive.addfile(info, io.BytesIO(data))
            entry('manifest.json', json.dumps(manifest).encode())
            for table, rows in tables.items():
                if not rows: continue
                data = b''.join(json.dumps(row).encode()+b'\n' for row in rows)
                name = table+'/000001.ndjson'
                sums['chunks'].append(dict(name=name, sha256=hashlib.sha256(data).hexdigest(), bytes=len(data), rows=len(rows)))
                entry(name, data)
            if scenario=='archive_checksum_mismatch': sums['chunks'][0]['sha256']='0'*64
            entry('checksums.json', json.dumps(sums).encode())
        pathlib.Path(flag('--out')).chmod(0o600)
        save()
        sys.exit(0)
    if args[0]=='avatar':
        operator = args[1]=='operator'
        op = args[2] if operator else args[1]
        if operator: assert flag('--account')=='evac-a' and flag('--agent-id')==agent
        else: assert pathlib.Path(flag('--token-file')).read_text()=='witself_test_private_token'
        assert flag('--endpoint')=='https://cell.invalid'
        if op=='show':
            if state.get('conflicted'): state['fresh_show_after_conflict']=True
            emit({'avatar':view()})
        if op=='style':
            emit({'style':{'realm_id':realm, 'style_revision':1, 'style_pack':{
                'id':'witself-flat-portrait','version':1,'references':[{'subject_form':'human','svg':svg,'sha256':hashlib.sha256(svg.encode()).hexdigest()}]}}})
        if op=='version': emit({'version':api_version(version(int(flag('--version'))))})
        if op=='history':
            limit=int(flag('--limit','20')); before=int(flag('--before-version','0'))
            rows=sorted((v for v in state['versions'] if not before or v['version']<before), key=lambda v:-v['version'])
            page=[api_version(v) for v in rows[:limit]]
            for v in page:
                for field in ['svg','visual_spec','description','provenance']: v.pop(field,None)
            emit({'schema_version':'witself.v0','versions':page,
                'next_before_version':page[-1]['version'] if len(rows)>limit else 0})
        key=flag('--idempotency-key')
        assert key and key not in state['keys'], 'mutation key reused'
        state['keys'].append(key)
        assert int(flag('--expected-profile-revision'))==p['profile_revision'], 'stale profile revision'
        n=int(flag('--version','0'))
        if op=='propose':
            if scenario=='pending_proposal' and p['latest_avatar_version']==1:
                p['proposed_avatar_version']=1
                fail()
            assert not p['proposed_avatar_version']
            parent=int(flag('--parent-version','0'))
            assert parent==p['active_avatar_version']
            n=p['latest_avatar_version']+1
            payload=pathlib.Path(flag('--svg-file')).read_text().strip()
            spec=json.loads(pathlib.Path(flag('--spec-file')).read_text())
            state['versions'].append(dict(identity, id='av_'+str(n), version=n, parent_version=parent or None,
                lineage_generation=p['lineage_generation'], subject_form='human', style=p['style'],
                description=flag('--description'), visual_spec=spec, svg=payload,
                svg_sha256=hashlib.sha256(payload.encode()).hexdigest(), locked_layers_sha256='d'*64,
                renderer_profile='perceptual-v1', payload_state='full', payload_bytes=1024,
                is_active=False, is_proposed=True, was_activated=False, rollback_eligible=False,
                rejected=False, proposed_at=stamp, proposed_by={'kind':'agent','id':agent}))
            p.update(latest_avatar_version=n, proposed_avatar_version=n, status='proposed')
        elif op in ['activate','rollback']:
            if op=='activate' and scenario=='revision_conflict' and not state.get('conflicted'):
                p['profile_revision']+=1; state['conflicted']=True; fail()
            if op=='activate' and state.get('conflicted'): assert state.get('fresh_show_after_conflict')
            if op=='rollback': assert version(n)['rollback_eligible']
            else: assert n==p['proposed_avatar_version']
            state['activations'].append(dict(identity, sequence=len(state['activations'])+1,
                avatar_version=n, prior_active_version=p['active_avatar_version'] or None,
                lineage_generation=p['lineage_generation'], action='rolled_back' if op=='rollback' else 'activated'))
            p.update(active_avatar_version=n, proposed_avatar_version=0, status='active')
            version(n)['was_activated']=True
        elif op=='reject':
            assert operator and n==p['proposed_avatar_version']
            if scenario!='rejection_not_recorded':
                version(n)['rejected']=True
                state['rejections'].append(dict(identity, avatar_version=n))
            p.update(proposed_avatar_version=0,
                status='active' if p['active_avatar_version'] else 'rejected')
            if scenario=='rejection_wrong_status': p['status']='rejected'
            if scenario=='rejection_wrong_active_version': p['active_avatar_version']=2
        elif op=='reset':
            assert p['active_avatar_version'] and p['autonomy_policy']=='agent_self_managed'
            state['resets'].append(dict(identity, retired_lineage_generation=p['lineage_generation'],
                new_lineage_generation=p['lineage_generation']+1, retired_active_version=p['active_avatar_version']))
            p.update(lineage_generation=p['lineage_generation']+1, active_avatar_version=0,
                proposed_avatar_version=0, status='generation_due')
        elif op=='quota':
            assert operator and int(flag('--retained-payload-count-limit'))==4
            assert int(flag('--retained-payload-byte-limit'))==p['retained_payload_byte_limit']
            p['retained_payload_count_limit']=4
            if scenario!='compaction_absent':
                version(1).update(payload_state='compacted', payload_compaction_reason='quota',
                    payload_compacted_at=stamp, svg=None, description=None, visual_spec=None,
                    continuity_fingerprint='\\x'+'46'*38092)
        else: raise AssertionError('unexpected avatar command')
        receipt(op,n)
elif tool=='curl':
    url=next(a for a in args if a.startswith('https://'))
    if '/v1/directory/' in url:
        emit({'account_id':account,'cell':{'cell':'civo-sandbox-usw2-dev','endpoint':'https://cell.invalid'},'status':'active','epoch':1})
    if url.endswith('/v1/version'):
        metadata = {'version':'0.0.275','commit':'bd103ff','date':'2026-09-06T00:21:06Z'}
        if url.startswith('https://cp.invalid/'):
            # Match shipped metadata: control-plane FullCommit and CommitDate,
            # independently varied to catch either release-matching regression.
            metadata.update(commit='bd103ff887606156ac9a097428df1c973faa76bb', date='2026-09-05T17:27:16-06:00')
            if scenario=='release_full_commit': metadata['date']='2026-09-06T00:21:06Z'
            if scenario=='release_offset_date': metadata['commit']='bd103ff'
            if scenario=='release_positive_offset': metadata['date']='2026-09-06T05:27:16.123+06:00'
            if scenario=='release_cp_commit_mismatch': metadata['commit']='deadbee887606156ac9a097428df1c973faa76bb'
            if scenario=='release_cp_version_mismatch': metadata['version']='0.0.274'
            if scenario=='release_cp_invalid_date': metadata['date']='2026-09-06T00:21:06+24:00'
        emit(metadata)
    if '/v1/backups' in url:
        if url.endswith(':run'):
            state['backup_requested']=True
            if scenario=='backup_reused_delayed_export':
                # The old snapshot finishes after the L10 manifest timestamp.
                clock.write_text(str(now+1))
            if scenario.startswith('backup_retry_'):
                state['backup_requested_at']=now
                state['backup_poll_times']=[]
                state['backup_retry_epoch']=now+(600 if scenario=='backup_retry_outside_deadline' else 60.25)
                state['backup_retry_at']=datetime.datetime.fromtimestamp(state['backup_retry_epoch'], datetime.timezone.utc).isoformat(timespec='milliseconds').replace('+00:00','Z')
                emit({'schema_version':'witself.v0','account_id':account,'backup_id':backup,
                    'status':'retrying','attempts':1,'retry_at':state['backup_retry_at']})
            result={'schema_version':'witself.v0','account_id':account,'backup_id':backup,'status':'committed'}
            if scenario=='backup_recovered_object': result['recovered_existing_object']=True
            emit(result)
        if '/status' in url:
            if not state.get('backup_requested'):
                # A post-lifecycle authoritative snapshot sees every durable job
                # that could already have acquired the database snapshot. Its
                # later exported_at is deliberately unhelpful for freshness.
                baseline={'schema_version':'witself.account-backup.v1','account_id':account,
                    'current_job':None, 'catalog':[]}
                if scenario=='happy_path' or scenario.startswith('backup_retry_'):
                    baseline['current_job']={'account_id':account,'backup_id':'backup_20981231T235900Z',
                        'scheduled_at':'2098-12-31T23:59:00.000Z','status':'committed'}
                    baseline['catalog']=[copy.deepcopy(baseline['current_job'])]
                if scenario=='backup_reused_delayed_export':
                    baseline['current_job']={'account_id':account,'backup_id':backup,
                        'scheduled_at':'2099-01-01T00:00:00Z','status':'running',
                        'created_at':datetime.datetime.fromtimestamp(now-120, datetime.timezone.utc).isoformat().replace('+00:00','Z')}
                if scenario=='backup_older_slot':
                    baseline['catalog']=[{'account_id':account,'backup_id':'backup_20990101T000100Z',
                        'scheduled_at':'2099-01-01T00:01:00Z'}]
                if scenario=='backup_baseline_missing_catalog': baseline.pop('catalog')
                if scenario=='backup_baseline_missing_current_job': baseline.pop('current_job')
                if scenario=='backup_baseline_wrong_account': baseline['account_id']='acc_unrelated'
                emit({'schema_version':'witself.v0','account':{'schema_version':'witself.v0',
                    'account_id':account,'backups':baseline}})
            if scenario.startswith('backup_retry_'):
                elapsed=now-state['backup_requested_at']
                state['backup_poll_times'].append(elapsed)
                assert float(flag('--max-time')) <= 420-elapsed, 'status request exceeds backup deadline'
                status='retrying' if now < state['backup_retry_epoch'] else 'running'
                if scenario=='backup_retry_failed' and status=='running': status='failed'
                # Exercise the scheduled retry plus nearly the full 300s export
                # allowance, so a short fixed poll count cannot accidentally pass.
                if scenario!='backup_retry_committed' or now < state['backup_retry_epoch']+299:
                    emit({'schema_version':'witself.v0','account':{'account_id':account,'backups':{
                        'catalog':[], 'current_job':{'account_id':account,'backup_id':backup,'status':status,
                            'retry_at':state['backup_retry_at']}}}})
            current_job={'account_id':account,'backup_id':backup,
                'scheduled_at':'2099-01-01T00:00:00Z','status':'committed'}
            if scenario=='backup_current_missing': current_job=None
            if scenario=='backup_current_mismatch': current_job['backup_id']='backup_20990101T000100Z'
            if scenario=='backup_current_wrong_account': current_job['account_id']='acc_unrelated'
            emit({'schema_version':'witself.v0','account':{'account_id':account,'backups':{
                'current_job':current_job, 'catalog':[
                {'backup_id':backup,'scheduled_at':'2099-01-01T00:00:00Z','status':'active',
                 'exported_at':stamp,'verified_at':stamp,'account_id':account,
                 'source_cell':'civo-sandbox-usw2-dev','archive_schema_version':94}]}}})
        if url.endswith(':restore-drill'):
            if scenario.startswith('backup_retry_'):
                assert scenario=='backup_retry_committed' and now >= state['backup_retry_epoch']+299, 'cannot drill an uncommitted backup'
            data=flag('--data',flag('--data-binary',flag('-d','{}')))
            if data.startswith('@'): data=pathlib.Path(data[1:]).read_text()
            body=json.loads(data)
            assert body=={'account_id':account,'backup_id':backup,'target_cell':'civo-sandbox-use1-backup'}
            result=dict(schema_version='witself.v0',account_id=account,backup_id=backup,
                target_cell='civo-sandbox-use1-backup',validated=True,validated_at=stamp,status='active',archive_schema_version=94)
            if scenario=='drill_false': result['validated']=False
            if scenario=='drill_bare_2xx': result.pop('validated_at')
            emit(result)
raise AssertionError('unexpected shim invocation: '+tool)
PY
chmod +x "$work_dir/bin/shim"
ln -s shim "$work_dir/bin/witself"
ln -s shim "$work_dir/bin/curl"
ln -s shim "$work_dir/bin/date"
ln -s shim "$work_dir/bin/sleep"
export PATH="$work_dir/bin:$PATH"
printf 'witself_test_fleet_token\n' >"$work_dir/fleet.token"

verify_backup_retry() {
  python3 - "$case_dir/state" "$FAKE_AVATAR_SCENARIO" <<'PY'
import json, pathlib, sys
root, scenario = pathlib.Path(sys.argv[1]), sys.argv[2]
state=json.loads((root/'state.json').read_text())
elapsed=float((root/'clock').read_text())-state['backup_requested_at']
polls=state['backup_poll_times']
assert polls and polls[0]==0 and all(0 <= at < 420 for at in polls), 'poll escaped deadline'
assert polls[1] >= 60, 'polled again before the scheduled retry wait'
run_calls=[call for call in state['calls'] if any(arg.endswith('/v1/backups:run') for arg in call['args'])]
drill_calls=[call for call in state['calls'] if any(arg.endswith('/v1/backups:restore-drill') for arg in call['args'])]
assert len(run_calls)==1, 'backup retry created another billed backup'
if scenario=='backup_retry_committed':
    assert 360 < elapsed < 420, 'did not wait for scheduled retry and export completion'
    assert len(drill_calls)==1, 'committed backup was not drilled exactly once'
else:
    assert not drill_calls, 'uncommitted backup reached restore drill'
    if scenario=='backup_retry_failed':
        assert 60.25 <= elapsed < 70, 'terminal backup failure was not rejected promptly'
    else:
        assert elapsed==420, 'retry_at moved or bypassed the overall deadline'
PY
}

verify_backup_freshness() {
  python3 - "$case_dir/state" "$FAKE_AVATAR_SCENARIO" <<'PY'
import json, pathlib, sys
root, scenario = pathlib.Path(sys.argv[1]), sys.argv[2]
state=json.loads((root/'state.json').read_text())
calls=state['calls']
export_at=next(i for i, call in enumerate(calls) if call['tool']=='witself' and call['args'][0]=='export')
status_at=next(i for i, call in enumerate(calls) if any('/v1/backups/status?' in arg for arg in call['args']))
runs=[i for i, call in enumerate(calls) if any(arg.endswith('/v1/backups:run') for arg in call['args'])]
drills=[call for call in calls if any(arg.endswith('/v1/backups:restore-drill') for arg in call['args'])]
assert status_at > export_at, 'freshness baseline was taken before the lifecycle/archive checks'
assert not drills, 'ambiguous backup reached the restore drill'
if scenario.startswith('backup_baseline_'):
    assert not runs, 'malformed baseline caused a billed backup request'
else:
    assert len(runs)==1 and runs[0] > status_at, 'backup request must follow the freshness baseline exactly once'
PY
}

failures=0
check() {
  local name="$1" expected="$2" actual
  shift 2
  set +e
  "$@" >"$case_dir/stdout" 2>"$case_dir/stderr"
  actual=$?
  set -e
  if [ "$actual" -ne "$expected" ]; then
    printf 'FAIL %s: exit %s, expected %s\n' "$name" "$actual" "$expected"
    if [ -f "$case_dir/record.json" ]; then
      jq -r '.legs[-1] // {} | "  last leg: \(.name // "none") passed=\(.passed // false)"' "$case_dir/record.json"
    fi
    failures=$((failures + 1))
    return 1
  fi
  printf 'PASS %s\n' "$name"
}
new_case() {
  case_dir="$work_dir/$1"
  mkdir -p "$case_dir/state" "$case_dir/work"
  export FAKE_AVATAR_STATE="$case_dir/state" FAKE_AVATAR_SCENARIO="$1"
}
run_harness() {
  local selected_drill=civo-sandbox-use1-backup
  if [ "$FAKE_AVATAR_SCENARIO" = usage_wrong_drill ]; then selected_drill=civo-sandbox-usw2-dev; fi
  bash "$canary" --account evac-a --realm-id realm_bbbbbbbbbbbbbbbb \
    --agent "avatar-acceptance-$FAKE_AVATAR_SCENARIO" --control-plane https://cp.invalid \
    --fleet-token-file "$work_dir/fleet.token" --drill-cell "$selected_drill" \
    --work "$case_dir/work" --out "$case_dir/record.json" --redact-check "$@"
}

scenarios="${AVATAR_ACCEPTANCE_TEST_SCENARIOS:-happy_path revision_conflict pending_proposal rejection_wrong_status rejection_wrong_active_version rejection_not_recorded archive_missing_version archive_hash_mismatch archive_checksum_mismatch compaction_absent drill_false drill_bare_2xx release_pair_mismatch release_full_commit release_offset_date release_positive_offset release_cp_commit_mismatch release_cp_version_mismatch release_cp_invalid_date backup_retry_committed backup_retry_deadline backup_retry_failed backup_retry_outside_deadline backup_reused_delayed_export backup_older_slot backup_recovered_object backup_current_missing backup_current_mismatch backup_current_wrong_account backup_baseline_missing_catalog backup_baseline_missing_current_job backup_baseline_wrong_account}"
for scenario in $scenarios; do
  new_case "$scenario"
  expected=1
  case "$scenario" in happy_path|revision_conflict|release_full_commit|release_offset_date|release_positive_offset|backup_retry_committed) expected=0 ;; esac
  if check "$scenario" "$expected" run_harness; then
    if [ "$expected" -eq 0 ]; then
      if ! jq -e '.status == "passed" and .certification_eligible == true and
          (.legs | length == 11) and all(.legs[]; .passed == true) and
          .archive.row_counts.agent_avatar_versions == 5 and .backup.validated_at != null' \
          "$case_dir/record.json" >/dev/null; then
        printf 'FAIL %s: incomplete evidence\n' "$scenario"; failures=$((failures+1))
      fi
    elif [[ $scenario != release_* ]]; then
      case "$scenario" in
        pending_proposal) failed_leg=L4_evolution ;;
        rejection_*) failed_leg=L6_rejection ;;
        archive_*) failed_leg=L10_archive ;;
        compaction_absent) failed_leg=L8_compaction ;;
        drill_*|backup_*) failed_leg=L11_restore_drill ;;
      esac
      if ! jq -e --arg leg "$failed_leg" '.status == "failed" and
          any(.legs[]; .name == $leg and .passed == false)' "$case_dir/record.json" >/dev/null; then
        printf 'FAIL %s: did not fail at expected leg %s\n' "$scenario" "$failed_leg"
        failures=$((failures+1))
      fi
    fi
    if [[ $scenario == release_* && $expected -eq 1 ]]; then
      if ! jq -e '.certification_eligible == false and .status == "rehearsal"' "$case_dir/record.json" >/dev/null; then
        printf 'FAIL %s: certification was not refused\n' "$scenario"; failures=$((failures+1))
      fi
    fi
    if [[ $scenario == backup_retry_* ]] && ! verify_backup_retry; then
      printf 'FAIL %s: backup retry timing or side effects\n' "$scenario"; failures=$((failures+1))
    fi
    if [[ $scenario == backup_* && $scenario != backup_retry_* ]] && ! verify_backup_freshness; then
      printf 'FAIL %s: backup freshness or side effects\n' "$scenario"; failures=$((failures+1))
    fi
    if [ -e "$case_dir/work/self.tar.gz" ] || [ -e "$case_dir/work/agent.token" ]; then
      printf 'FAIL %s: private archive or token survived\n' "$scenario"; failures=$((failures+1))
    fi
    if ! python3 - "$case_dir" <<'PY'
import pathlib, stat, sys
root=pathlib.Path(sys.argv[1])
record=root/'record.json'
assert record.is_file() and stat.S_IMODE(record.stat().st_mode)==0o600
assert stat.S_IMODE((root/'work').stat().st_mode)==0o700
for path in [record,root/'stdout',root/'stderr']:
    text=path.read_text()
    assert not any(marker in text for marker in ['witself_test_', '/Users/private', '<svg', 'private@example.invalid'])
assert not any((root/'work').iterdir()), 'private raw artifacts survived cleanup'
PY
    then
      printf 'FAIL %s: private output contract\n' "$scenario"; failures=$((failures+1))
    fi
  fi
done

# Bounded local debugging still runs the same assertions; CI always runs all cases.
if [ -n "${AVATAR_ACCEPTANCE_TEST_SCENARIOS:-}" ]; then
  [ "$failures" -eq 0 ]
  exit
fi

new_case explicit_rehearsal
if check explicit_rehearsal 0 run_harness --rehearsal; then
  if ! jq -e '.certification_eligible == false and .status == "rehearsal"' "$case_dir/record.json" >/dev/null; then
    printf 'FAIL explicit_rehearsal: certification enabled\n'; failures=$((failures+1))
  fi
fi

new_case redaction_clean
check redaction_clean 0 bash "$canary" --redact-check --out "$work_dir/happy_path/record.json" || true

for marker in svg token home metadata; do
  new_case "redaction_$marker"
  case "$marker" in svg) value='<svg xmlns="test"/>' ;; token) value=witself_private ;; home) value=/Users/private/.witself/secret ;; metadata) value='Private Project Alpha' ;; esac
  if [ -f "$work_dir/happy_path/record.json" ]; then
    if [ "$marker" = metadata ]; then
      jq --arg value "$value" '.witself.version=$value' "$work_dir/happy_path/record.json" >"$case_dir/record.json"
    else
      jq --arg value "$value" '.legs[0].detail=$value' "$work_dir/happy_path/record.json" >"$case_dir/record.json"
    fi
  else
    jq -n --arg value "$value" '{schema_version:"witself.avatar-acceptance.v1",detail:$value}' >"$case_dir/record.json"
  fi
  check "redaction_$marker" 1 bash "$canary" --redact-check --out "$case_dir/record.json" || true
done

for scenario in usage_missing usage_unknown usage_missing_value usage_wrong_drill; do
  new_case "$scenario"
  case "$scenario" in
    usage_missing) check "$scenario" 2 bash "$canary" || true ;;
    usage_unknown) check "$scenario" 2 bash "$canary" --surprise || true ;;
    usage_missing_value) check "$scenario" 2 bash "$canary" --account || true ;;
    usage_wrong_drill) check "$scenario" 2 run_harness || true ;;
  esac
  if [ -f "$case_dir/state/state.json" ]; then
    printf 'FAIL %s: invalid usage invoked a shim\n' "$scenario"; failures=$((failures+1))
  fi
done

[ "$failures" -eq 0 ] || exit 1
printf 'avatar acceptance script tests passed (offline shims only)\n'
