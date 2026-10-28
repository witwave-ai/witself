import json, pathlib, re, subprocess, sys, time
root=pathlib.Path(__file__).resolve().parent.parent
scratch=root/'.lc-s7a-fix4-scratch'
for name in ['home','witself-home','dsh-home','lint-cache']:
    (scratch/name).mkdir(mode=0o700,exist_ok=True)
child_env={
    'PATH':'/Users/scott/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.27.3.darwin-arm64/bin:/Users/scott/.local/share/mise/installs/node/24.16.0/bin:/usr/bin:/bin:/usr/sbin:/sbin',
    'HOME':str(scratch/'home'),'WITSELF_HOME':str(scratch/'witself-home'),'DSH_HOME':str(scratch/'dsh-home'),
    'GOTOOLCHAIN':'local','GOPROXY':'off','GOSUMDB':'off',
    'GOMODCACHE':'/Users/scott/go/pkg/mod','GOCACHE':'/Users/scott/Library/Caches/go-build',
    'GOLANGCI_LINT_CACHE':str(scratch/'lint-cache'),'LANG':'C','LC_ALL':'C',
}
label=sys.argv[1]
args=sys.argv[2:]
if label=='lint': child_env['GOPROXY']='file:///Users/scott/go/pkg/mod/cache/download'
start=time.monotonic()
result=subprocess.run(args,cwd=root,env=child_env,stdout=subprocess.PIPE,stderr=subprocess.STDOUT,text=True)
wall=round(time.monotonic()-start,2)
# Only sanitized diagnostics are retained or displayed; do not emit environment values.
output=re.sub(r'(?i)[0-9a-f]{64}','<hex>',result.stdout)
(scratch/(label+'.log')).write_text(output)
record={'command':args,'exit':result.returncode,'wall_seconds':wall}
(scratch/(label+'.result.json')).write_text(json.dumps(record))
print(json.dumps(record))
if '-json' in args:
    events=[]
    for line in output.splitlines():
        try: events.append(json.loads(line))
        except json.JSONDecodeError: print(line)
    passed=[e for e in events if e.get('Action')=='pass' and 'Test' in e]
    print(json.dumps({'top_level_pass':sum('/' not in e['Test'] for e in passed),'subtest_pass':sum('/' in e['Test'] for e in passed),'skip_events':[{'package':e.get('Package'),'test':e.get('Test')} for e in events if e.get('Action')=='skip'],'package_results':[{'package':e.get('Package'),'action':e['Action'],'elapsed':e.get('Elapsed')} for e in events if e.get('Action') in ['pass','fail'] and 'Test' not in e]}))
    for e in events:
        if e.get('Action')=='output' and ('NOT_RUN' in e.get('Output','') or result.returncode): print(e.get('Output',''),end='')
elif label=='node':
    print('\n'.join(line for line in output.splitlines() if line.startswith('# ') or '# SKIP' in line or line.startswith('not ok')))
else: print(output,end='')
sys.exit(result.returncode)
