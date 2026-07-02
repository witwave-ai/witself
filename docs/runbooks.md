# Runbooks

Hand-testing recipes for what is **actually built and running today** — unlike
the surrounding design docs (which specify the target system), every command
here has been executed and works. Copy-paste top to bottom; `expect:` comments
tell you what success looks like.

Conventions used throughout:

```sh
CP=https://self.witwave.ai                                        # control plane
EP=https://api.aws-sandbox-usw2-dev.cells.witself.witwave.ai      # the live cell
FT="Authorization: Bearer $(cat ~/.witself/tokens/fleet.token)"   # fleet auth
```

The fleet token lives at `~/.witself/tokens/fleet.token`; the shared bootstrap
token at `~/.witself/tokens/bootstrap.token`. Everything under
`~/.witself/tokens/` is secret.

---

## 1. Local loop — server + CLI on your machine

No cloud, no cost. Postgres in Compose; server + CLI run from source.

```sh
# terminal 1 — DB + server (mints a bootstrap token; enables dev provisioning)
make serve

# terminal 2 — claim the deployment's default account
make login                       # expect: "logged in as operator opr_…"
                                 #         token saved to .dev/operator.token
EP=http://localhost:8080
T=.dev/operator.token

# poke the meta endpoints
curl -s $EP/v1/version           # expect: version JSON (dev build)
curl -s $EP/v1/capabilities      # expect: backend self-hosted + account acc_…

# tenancy tree
ws() { go run ./cmd/ws "$@"; }   # or use the installed ws
ws realm create --endpoint $EP --token-file $T prod       # expect: realm_…  prod
RID=$(ws realm list --endpoint $EP --token-file $T | cut -f1)
ws agent create --endpoint $EP --token-file $T --realm $RID scott
AID=$(ws agent list --endpoint $EP --token-file $T --realm $RID | cut -f1)
ws token create --endpoint $EP --token-file $T --agent $AID --out .dev/agent.token
ws operator list --endpoint $EP --token-file $T           # expect: owner (root) + tokens

# inspect the database directly
make psql                        # \dt; select * from accounts; select kind, display_name from tokens;

# reset the world
make db-reset                    # wipes the volume; next `make serve` starts fresh
```

## 2. Local loop — account provisioning + close (the Cloud flow, minus the cloud)

`make serve` enables the provisioning endpoint with a fixed dev credential
(`witself_prv_dev-local-only`), so you can play control-plane by hand.

```sh
EP=http://localhost:8080
PRV="Authorization: Bearer witself_prv_dev-local-only"

# provision a second account (what the control plane does on signup)
curl -s -X POST $EP/v1/accounts -H "$PRV" \
  -d '{"email":"amy@co.com","display_name":"Amy"}' | python3 -m json.tool
# expect: 201 with account_id, operator_id, bootstrap_token

# claim it with the ordinary bootstrap exchange
# (paste the bootstrap_token from above into a file first)
echo "witself_boot_…" > .dev/amy-boot.token
go run ./cmd/ws auth login --endpoint $EP \
  --bootstrap-token-file .dev/amy-boot.token --out .dev/amy-opr.token
curl -s $EP/v1/whoami -H "Authorization: Bearer $(cat .dev/amy-opr.token)"
# expect: Amy's account_id — NOT the default account

# guards
curl -s -X POST $EP/v1/accounts -d '{"email":"x@y.z"}' -w ' [%{http_code}]'
# expect: 401 (no provision token)
curl -s -X POST $EP/v1/accounts -H "$PRV" -d '{"email":"AMY@co.com"}' -w ' [%{http_code}]'
# expect: 409 (duplicate email, case-insensitive)

# close Amy's account (owner-only, permanent tombstone)
ACC=$(curl -s $EP/v1/whoami -H "Authorization: Bearer $(cat .dev/amy-opr.token)" | python3 -c 'import sys,json;print(json.load(sys.stdin)["principal"]["account_id"])')
curl -s -X POST $EP/v1/account:close \
  -H "Authorization: Bearer $(cat .dev/amy-opr.token)" -d '{"reason":"testing"}'
# expect: {"status":"closed", …}
curl -s -o /dev/null -w '%{http_code}\n' $EP/v1/whoami \
  -H "Authorization: Bearer $(cat .dev/amy-opr.token)"
# expect: 401 — every credential died with the account

# guards: the deployment's default account refuses to close
curl -s -X POST $EP/v1/account:close \
  -H "Authorization: Bearer $(cat .dev/operator.token)" -d '{}' -w ' [%{http_code}]'
# expect: 403 "the deployment's default account cannot be closed"

# the tombstone, in the database
make psql   # select id, status, closed_reason, closed_at from accounts;
```

## 3. Cloud — the full customer lifecycle (create → use → close)

Needs: a registered cell (runbook 5) and a valid invite (runbook 4). Each
create consumes an invite use; the close at the end leaves nothing behind.

```sh
EP=https://api.aws-sandbox-usw2-dev.cells.witself.witwave.ai
T=/tmp/runbook-operator.token

# create — one command from nothing to a working operator token
ws account create --email test-$(date +%s)@example.com \
  --invite friends-2026 --out $T
# expect: "account acc_… created on cell aws-sandbox-usw2-dev"
#         "logged in as operator opr_…"

# use it
ACC=$(curl -s $EP/v1/whoami -H "Authorization: Bearer $(cat $T)" | python3 -c 'import sys,json;print(json.load(sys.stdin)["principal"]["account_id"])')
ws realm create --endpoint $EP --token-file $T prod        # expect: realm_…  prod
curl -s $CP/v1/directory/$ACC                              # expect: routes to the cell

# close — permanent, demands --yes; every credential dies with it
ws account close --account $ACC --token-file $T --reason "runbook" --yes
curl -s -o /dev/null -w '%{http_code}\n' $EP/v1/whoami -H "Authorization: Bearer $(cat $T)"
# expect: 401 — and rm $T, it is dead
rm -f $T
```

Failure paths worth trying on purpose: a bogus invite (`--invite nope-nope` →
"invalid invite: unknown code"), the same email twice (409 per cell), and
`ws account close` without `--yes` (refuses with a warning).

For a real account you keep: use a stable `--out` path like
`~/.witself/tokens/cloud/operator.token` — and remember `--out` OVERWRITES, so
one file per account or you orphan the old account's key.

## 4. Cloud — fleet administration (invites, placement, registry)

All fleet-token authorized; all effective within ~a minute (KV propagation).

```sh
# invites — the signup gate and promo lever
curl -s -X POST $CP/v1/invites -H "$FT" \
  -d '{"code":"beta-2","expires_at":"2026-12-01T00:00:00Z","max_uses":10,"note":"batch 2"}'
curl -s -X POST $CP/v1/invites -H "$FT" -d '{"note":"auto code"}'      # auto-generates xxxx-xxxx-xxxx
curl -s $CP/v1/invites -H "$FT" | python3 -m json.tool                  # the table view, with validity
curl -s -X POST $CP/v1/invites -H "$FT" -d '{"code":"beta-2","enabled":false}'   # disable
curl -s -X DELETE $CP/v1/invites/beta-2 -H "$FT"                        # remove

# invite-level placement (top precedence)
curl -s -X POST $CP/v1/invites -H "$FT" -d '{"code":"acme-dedicated","cell":"aws-acme-use1-1"}'   # hard pin
curl -s -X POST $CP/v1/invites -H "$FT" -d '{"code":"eu-batch","region":"eu-central-1"}'          # hard region

# fleet-wide placement strategy
curl -s $CP/v1/placement -H "$FT"                                       # expect: weighted (default)
curl -s -X POST $CP/v1/placement -H "$FT" -d '{"strategy":"pinned","pinned_cell":"aws-sandbox-usw2-dev"}'
curl -s -X POST $CP/v1/placement -H "$FT" -d '{"strategy":"weighted"}'  # back to default

# cell registry
curl -s $CP/v1/cells -H "$FT" | python3 -m json.tool
# expect: has_provision_token true (the token itself is NEVER returned)
```

## 5. Cloud — cell provision / teardown

The canonical commands live in the root [README](../README.md#infrastructure-example);
run them verbatim. Verification after `up` (~14 min + a few for Argo):

```sh
curl -s $CP/v1/cells -H "$FT"        # expect: the cell, accepting, has_provision_token:true
curl -s $EP/v1/version               # expect: the release you shipped
# NOTE: /readyz is NOT public (health listener :8081, ALB-internal only).
# A public /readyz 404 is CORRECT. /v1/version is the public liveness probe.

# cluster view, if you want it
aws eks update-kubeconfig --name witself-aws-sandbox-usw2-dev --profile witwave-sandbox --region us-west-2
kubectl get applications -n argocd   # expect: all Synced/Healthy
kubectl get pods -n witself          # expect: witself-server Running
```

Teardown (drains, refuses while accounts live, purges only with the flag):

```sh
witself-infra destroy … -control-plane $CP …                  # expect: REFUSAL naming live accounts
witself-infra destroy … -control-plane $CP -destroy-accounts …  # purge + 10-15 min teardown
curl -s $CP/v1/cells -H "$FT"                                 # expect: {"cells":[]}
```

## 6. Cloud — shipping a server change to the live cell (GitOps rollout)

How code reaches the fleet — no kubectl, no SSH:

```sh
# 1. merge to main (CI green), tag the release
git tag -a v0.0.NN -m "…" && git push origin v0.0.NN
# 2. wait for the release workflow (builds images/witself-server:0.0.NN + chart)
# 3. bump the cell's pins and push — Argo does the rest
#    edit .gitops/cells/aws-sandbox-usw2-dev/values.yaml:
#      chartVersion: 0.0.NN
#      imageTag: 0.0.NN
git commit -am "gitops: roll sandbox cell to 0.0.NN" && git push
# 4. watch the roll (Argo poll + pod restart, usually < 10 min)
watch -n 20 "curl -s $EP/v1/version"

# control-plane (Worker) changes deploy separately, in seconds:
cd infra/cloudflare/control-plane && npx wrangler deploy
```
