package featurestatus

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/store"
)

func TestSecurityPolicyStatusIsCurrent(t *testing.T) {
	const policyFile = "docs/security-policy.md"
	policy := trustDocsRead(t, policyFile)
	preamble, _, _ := strings.Cut(policy, "\n## ")
	trustDocsRequire(t, policyFile, preamble, "Status: published.")
	trustDocsForbid(t, policyFile, preamble, "Status: draft", "before implementation")
	trustDocsForbid(t, policyFile, policy,
		"before the first public release",
		"releases once available",
		"- Published Terraform modules and examples.",
	)
	trustDocsRequire(t, policyFile, policy, "Terraform modules and examples, once published.")
	trustDocsRequire(t, policyFile, trustDocsNormalize(policy),
		"Report abuse of the service, such as spam sent through agent email, to `legal@witwave.ai`",
		"legal/acceptable-use.md",
	)

	const rootFile = "SECURITY.md"
	root := trustDocsNormalize(trustDocsRead(t, rootFile))
	trustDocsForbid(t, rootFile, strings.ToLower(root), "draft", "being designed", "not secret confidentiality")
	trustDocsRequire(t, rootFile, root,
		"docs/security-policy.md",
		"`security@witwave.ai`",
		"secret-value leakage or a reveal bypass",
	)
}

func TestAccountEmailIsTheRootOfTrust(t *testing.T) {
	const controlPlaneFile = "infra/cloudflare/control-plane/src/index.js"
	controlPlane := trustDocsRead(t, controlPlaneFile)
	// handleChangeEmail (index.js:2982) sets the revert record's actual TTL.
	// Parse only its whole-hours expression; an unrecognized or duplicate
	// declaration must fail closed instead of silently preserving stale prose.
	undoTTLs := regexp.MustCompile(`(?m)^[ \t]*const undoTtl = ([1-9][0-9]*) \* 3600;$`).FindAllStringSubmatch(controlPlane, -1)
	if len(undoTTLs) != 1 {
		t.Fatalf("%s: expected one whole-hours undoTtl declaration", controlPlaneFile)
	}
	undoHours, err := strconv.Atoi(undoTTLs[0][1])
	if err != nil {
		t.Fatalf("%s: undoTtl hours must fit an integer", controlPlaneFile)
	}

	const file = "docs/security-policy.md"
	policy := trustDocsRead(t, file)
	const heading = "## Account email is the root of trust"
	trustDocsHeadingOrder(t, file, policy, "## Expected Response", heading, "## Supported Surfaces")
	section := strings.ToLower(trustDocsNormalize(trustDocsSection(t, file, policy, heading)))
	trustDocsRequire(t, file, section,
		"witself account recover",
		"requesting a code does not change the account or its credentials; a new request replaces any earlier unused code.",
		"requests are rate-limited.",
		"nothing changes in the account or its credentials until someone redeems it.",
		"redeeming the code revokes every owner token",
		"agents and other operators keep their tokens",
		"recovery works only for an active account",
		"verification link",
		"a pending account cannot create or use agents or change account data until it is verified; its owner can check status, request a new verification link, export or close it",
		"a code sent to the new address",
		fmt.Sprintf("a link that reverts it for %d hours", undoHours),
		"does not unlock sealed-plane values",
		"in-scope vulnerability",
		"without the proof described below",
		"without the emailed verification link",
		"recover an account's owner credential, or mint a new one, without either the emailed recovery code or a live owner token",
		"provisioning credentials",
		"three paths are by design",
		fmt.Sprintf("the revert link sent to the old address, valid for %d hours after a committed change, which restores the old address", undoHours),
		"a change that has not yet been submitted cannot commit with the old token",
		"`witself operator list`",
		"`witself operator delete --yes operator`",
		"`witself token revoke --token token_id --yes`",
		"runbooks.md#recover-a-lost-owner-token",
	)
	trustDocsForbid(t, file, section,
		"at once: it revokes", "issues an owner credential, or changes",
		"requesting a code changes nothing", "a pending account can only check its status",
		"recover or rotate an account's owner credential without", "two paths are by design",
		"the change cannot commit with the old token",
		"without the matching inbox proof",
	)

	// handleRecover's request mode (index.js:2406-2455, 2546-2549)
	// rate-limits sends and replaces the one per-account unused code; only
	// redeem mode calls the cell's credential-rotation endpoint.
	trustDocsRequire(t, controlPlaneFile, trustDocsNormalize(controlPlane),
		`contact?.email && contact.status === "active" && env.EMAIL`,
		`if (!body.code && env.RECOVER_LIMITER) {`,
		`if ((rlPerIp.emails_sent ?? 0) >= 3) { return rateLimited(); }`,
		`if ((rl.emails_sent ?? 0) >= 10) { return rateLimited(); }`,
		`const rlKey = `+"`recover:${accountId}`;",
		`rl.code_hash = await sha256hex(code.replaceAll("-", ""));`,
		`await env.DIRECTORY.put(rlKey, JSON.stringify(rl), { expirationTtl: recoverStateTtlSeconds(rl) });`,
		`{ expirationTtl: undoTtl }`,
		`undo: true, expected_current: undo.new_email, new_email: undo.old_email,`,
	)
	// Pending-account enforcement is in the operator/domain status gates
	// (server.go:3347-3357, 3403-3426); create and rename use that gate.
	// Status and close deliberately use the any-status gate (:6408, :6441),
	// as does export_self.go:45. Resend verifies the pending status itself
	// (index.js:2244-2318). Guard code, not just the public sentence.
	const serverFile = "internal/server/server.go"
	server := trustDocsRead(t, serverFile)
	trustDocsRequire(t, serverFile, trustDocsGoFunction(t, serverFile, server, "requireOperator"),
		`if p.accountStatus != "active" { writeJSONError(w, http.StatusForbidden,`,
	)
	trustDocsRequire(t, serverFile, trustDocsGoFunction(t, serverFile, server, "requireDomainPrincipalAnyProfile"),
		`if p.AccountStatus != "active" { writeJSONError(w, http.StatusForbidden,`,
	)
	for _, name := range []string{"createAgentRequestHandler", "renameAccountHandler", "createOperatorTokenHandler"} {
		trustDocsRequire(t, serverFile, trustDocsGoFunction(t, serverFile, server, name),
			`return requireOperator(auth,`,
		)
	}
	for _, name := range []string{"getAccountHandler", "closeAccountHandler"} {
		trustDocsRequire(t, serverFile, trustDocsGoFunction(t, serverFile, server, name),
			`return requireOperatorAnyStatus(auth,`,
		)
	}
	const exportFile = "internal/server/export_self.go"
	trustDocsRequire(t, exportFile, trustDocsGoFunction(t, exportFile, trustDocsRead(t, exportFile), "accountSelfExportHandler"),
		`return requireOperatorAnyStatus(auth,`,
	)
	trustDocsRequire(t, controlPlaneFile, trustDocsNormalize(controlPlane),
		`if (account.status !== "pending") { return err(`+"`account is already ${account.status}`"+`, 409); }`,
		`emailSent = await sendVerificationEmail(`,
	)
	// A live owner token can mint its own replacement through the self-token
	// route (server.go:2598-2599, 6933-6982), without recovery email proof.
	trustDocsRequire(t, serverFile, server,
		`mux.HandleFunc("POST /v1/operators/self/tokens", createOperatorTokenHandler(cfg.Authenticate, cfg.CreateOperatorToken))`,
	)
	trustDocsRequire(t, serverFile, trustDocsGoFunction(t, serverFile, server, "createOperatorTokenHandler"),
		`create(r.Context(), p.accountID, p.operatorID, displayName, ttl)`,
	)
	const recoveryFile = "internal/store/recover.go"
	trustDocsRequire(t, recoveryFile, trustDocsNormalize(trustDocsRead(t, recoveryFile)),
		`UPDATE tokens SET consumed_at = now() WHERE account_id = $1 AND operator_id = $2 AND consumed_at IS NULL`,
	)

	const link = "security-policy.md#account-email-is-the-root-of-trust"
	const threatFile = "docs/threat-model.md"
	trustDocsRequire(t, threatFile,
		trustDocsSection(t, threatFile, trustDocsRead(t, threatFile), "## Core Assumptions"), link)
	for _, linkFile := range []string{"docs/operator-auth.md", "SECURITY.md"} {
		trustDocsRequire(t, linkFile, trustDocsRead(t, linkFile), link)
	}
	const runbookFile = "docs/runbooks.md"
	runbook := trustDocsRead(t, runbookFile)
	trustDocsRequire(t, runbookFile,
		trustDocsNormalize(trustDocsSection(t, runbookFile, runbook, "## Recover a lost owner token")),
		"Requesting a code does not change the account or its credentials; a new request replaces any earlier unused code.",
		"Requests are rate-limited.",
	)
	trustDocsRequire(t, runbookFile,
		trustDocsNormalize(trustDocsSection(t, runbookFile, runbook, "## Change the account email")),
		"request a code with `witself account recover`, then redeem it with `witself account recover --code CODE`",
		"a change that has not yet been submitted cannot commit with the old token",
	)
}

func TestDataSubjectRunbookStatesImplementedPurgeTiming(t *testing.T) {
	cfg := store.DefaultAccountPurgeWorkerConfig()
	if cfg.Grace%time.Hour != 0 || cfg.Grace%(24*time.Hour) != 0 {
		t.Fatalf("internal/store/account_purge.go: default grace %s must be whole hours and days for the documented timing", cfg.Grace)
	}
	if cfg.Interval%time.Minute != 0 {
		t.Fatalf("internal/store/account_purge.go: default interval %s must be whole minutes for the documented timing", cfg.Interval)
	}
	h := int(cfg.Grace / time.Hour)
	m := int(cfg.Interval / time.Minute)
	const runbookFile = "docs/runbooks.md"
	section := trustDocsNormalize(trustDocsSection(t, runbookFile, trustDocsRead(t, runbookFile), "## Handle a data-subject request"))
	trustDocsRequire(t, runbookFile, section,
		fmt.Sprintf("%d hours (%d days)", h, h/24),
		fmt.Sprintf("every %d minutes", m),
		fmt.Sprintf("up to %d eligible accounts", cfg.BatchSize),
		fmt.Sprintf("earliest purge time, %d hours later", h),
	)

	const chartFile = "charts/witself-server/values.yaml"
	chartBlock := trustDocsIndentedBlock(t, chartFile, trustDocsRead(t, chartFile), "  accountPurge:")
	for _, fragment := range []string{
		fmt.Sprintf("grace: %dh", h),
		fmt.Sprintf("interval: %dm", m),
		fmt.Sprintf("batchSize: %d", cfg.BatchSize),
	} {
		trustDocsRequireLine(t, chartFile, chartBlock, fragment)
	}
	for _, cell := range trustDocsCivoCells() {
		file := ".gitops/cells/" + cell + "/values.yaml"
		block := trustDocsIndentedBlock(t, file, trustDocsRead(t, file), "      accountPurge:")
		if len(block) != 2 || block[0] != "enabled: true" || block[1] != "mode: enforce" {
			t.Errorf("%s: accountPurge block must contain exactly the lines %q and %q, without timing overrides", file, "enabled: true", "mode: enforce")
		}
	}
}

func TestLaunchReadinessErasureEntryIsCurrent(t *testing.T) {
	const file = "docs/launch-readiness.md"
	entry := trustDocsNormalize(trustDocsBullet(t, file, trustDocsRead(t, file), "- **Erasure posture"))
	trustDocsRequire(t, file, entry,
		"720 hours (30 days)",
		"civo-sandbox-use1-serving",
		"civo-sandbox-use1-backup",
		"civo-prod-use1-serving",
		"lost on 2026-09-19",
		"fleet registry name `civo-sandbox-usw2-dev`",
	)
	trustDocsForbid(t, file, entry, "on both production cells")
	for _, cell := range trustDocsCivoCells() {
		cellFile := ".gitops/cells/" + cell + "/values.yaml"
		block := trustDocsIndentedBlock(t, cellFile, trustDocsRead(t, cellFile), "      accountPurge:")
		trustDocsRequireLine(t, cellFile, block, "enabled: true")
		trustDocsRequireLine(t, cellFile, block, "mode: enforce")
	}
}

func TestDeploymentCellsPlacementNoteIsCurrent(t *testing.T) {
	const file = "docs/deployment-cells.md"
	doc := trustDocsNormalize(trustDocsRead(t, file))
	trustDocsForbid(t, file, doc,
		"schema-88 backup validation cell",
		"schema-91-capable",
		"invoke manual restore",
		"It is never a placement or move destination",
	)
	trustDocsRequire(t, file, doc,
		"runbooks.md#account-moves-in-a-chosen-order",
		"`config:placement_runner`",
		"backup_validation_target=true",
		"witself-admin settings placement-runner show --json",
		"unless it is deliberately reclassified",
	)
	const runbookFile = "docs/runbooks.md"
	trustDocsHeadingIndex(t, runbookFile, trustDocsRead(t, runbookFile), "## Account moves in a chosen order")
	const controlPlaneFile = "infra/cloudflare/control-plane/src/index.js"
	trustDocsRequire(t, controlPlaneFile, trustDocsRead(t, controlPlaneFile), `env.DIRECTORY.get("config:placement_runner"`)
	const adminFile = "docs/witself-admin.md"
	trustDocsRequire(t, adminFile, trustDocsRead(t, adminFile), "witself-admin settings placement-runner show --json")
}

func TestSupportPolicyLinksDataSubjectRunbook(t *testing.T) {
	const file = "docs/support-policy.md"
	entry := trustDocsNormalize(trustDocsBullet(t, file, trustDocsRead(t, file), "- **Data deletion**"))
	trustDocsRequire(t, file, entry, "runbooks.md#handle-a-data-subject-request")
	const runbookFile = "docs/runbooks.md"
	trustDocsHeadingIndex(t, runbookFile, trustDocsRead(t, runbookFile), "## Handle a data-subject request")
}

func TestDataSubjectRunbookCoversScopeAndBackups(t *testing.T) {
	const file = "docs/runbooks.md"
	doc := trustDocsRead(t, file)
	const heading = "## Handle a data-subject request"
	trustDocsHeadingOrder(t, file, doc, "## Close an account", heading, "## Close a realm without resurrecting its email route")
	section := trustDocsNormalize(trustDocsSection(t, file, doc, heading))
	trustDocsRequire(t, file, section,
		"No operator command exports, corrects or closes an account",
		"`recover` needs no credential; the code emailed to the account address is the proof.",
		"Take `--expected-version` and `--scrub-set-revision` from the preview.",
		"Choose a fresh `--idempotency-key`; reuse it only to retry the same deletion.",
		"runs as the agent that holds it",
		"Managed recovery works only for an active account",
		"controller",
		"Do not search customer content",
		"| Request | Owner runs | Notes |",
		"| Deletion of the account |",
		"evacuation archives",
		"durable retention",
		"Git does not record",
		"24 hours after its last write",
		"revert record keeps both for 48 hours",
		"within 30 days",
		"Do not confirm that the purge has finished",
		"security-policy.md#account-email-is-the-root-of-trust",
		"data-retention-policy.md#account-closure-and-erasure",
	)
	trustDocsForbid(t, file, section,
		"expire under their bucket lifecycle rules",
		"each step runs with the owner credential",
		"`recover`) use the owner's operator credential",
		"`close`, `recover`",
		"--idempotency-key KEY`, taken from the preview",
		"for at most 24 hours",
	)
	const controlPlaneFile = "infra/cloudflare/control-plane/src/index.js"
	trustDocsRequire(t, controlPlaneFile, trustDocsRead(t, controlPlaneFile),
		"await env.DIRECTORY.put(key, JSON.stringify(fresh), { expirationTtl: 24 * 3600 });",
		"{ expirationTtl: undoTtl }",
		`return Boolean(archived) && archived.status !== "closed";`,
	)
	const retentionFile = "docs/data-retention-policy.md"
	retention := trustDocsRead(t, retentionFile)
	trustDocsHeadingIndex(t, retentionFile, retention, "## Account closure and erasure")
	trustDocsRequire(t, retentionFile, retention, "`account.purged`")
}

func trustDocsRead(t *testing.T, file string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), filepath.FromSlash(file)))
	if err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	return string(raw)
}

func trustDocsNormalize(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

func trustDocsGoFunction(t *testing.T, file, source, name string) string {
	t.Helper()
	_, body, ok := strings.Cut(source, "\nfunc "+name+"(")
	if !ok {
		t.Fatalf("%s: missing function %s", file, name)
	}
	body, _, _ = strings.Cut(body, "\nfunc ")
	return trustDocsNormalize(body)
}

func trustDocsRequire(t *testing.T, file, text string, fragments ...string) {
	t.Helper()
	for _, fragment := range fragments {
		if !strings.Contains(text, fragment) {
			t.Errorf("%s: missing required fragment %q", file, fragment)
		}
	}
}

func trustDocsForbid(t *testing.T, file, text string, fragments ...string) {
	t.Helper()
	for _, fragment := range fragments {
		if strings.Contains(text, fragment) {
			t.Errorf("%s: contains stale fragment %q", file, fragment)
		}
	}
}

func trustDocsHeadingIndex(t *testing.T, file, text, heading string) int {
	t.Helper()
	for i, line := range strings.Split(text, "\n") {
		if line == heading {
			return i
		}
	}
	t.Fatalf("%s: missing heading line %q", file, heading)
	return -1
}

func trustDocsHeadingOrder(t *testing.T, file, text, before, heading, after string) {
	t.Helper()
	beforeIndex := trustDocsHeadingIndex(t, file, text, before)
	headingIndex := trustDocsHeadingIndex(t, file, text, heading)
	afterIndex := trustDocsHeadingIndex(t, file, text, after)
	if headingIndex <= beforeIndex || headingIndex >= afterIndex {
		t.Errorf("%s: heading %q must follow %q and precede %q", file, heading, before, after)
	}
}

func trustDocsSection(t *testing.T, file, text, heading string) string {
	t.Helper()
	start := trustDocsHeadingIndex(t, file, text, heading)
	lines := strings.Split(text, "\n")
	end := start + 1
	for end < len(lines) && !strings.HasPrefix(lines[end], "## ") {
		end++
	}
	return strings.Join(lines[start:end], "\n")
}

func trustDocsBullet(t *testing.T, file, text, prefix string) string {
	t.Helper()
	lines := strings.Split(text, "\n")
	for start, line := range lines {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		end := start + 1
		for end < len(lines) && !strings.HasPrefix(lines[end], "- ") && !strings.HasPrefix(lines[end], "## ") {
			end++
		}
		return strings.Join(lines[start:end], "\n")
	}
	t.Fatalf("%s: missing bullet beginning %q", file, prefix)
	return ""
}

func trustDocsIndentedBlock(t *testing.T, file, text, heading string) []string {
	t.Helper()
	start := trustDocsHeadingIndex(t, file, text, heading)
	indent := len(heading) - len(strings.TrimLeft(heading, " "))
	lines := strings.Split(text, "\n")
	var block []string
	for _, line := range lines[start+1:] {
		if len(line)-len(strings.TrimLeft(line, " ")) <= indent {
			break
		}
		block = append(block, strings.TrimSpace(line))
	}
	return block
}

func trustDocsRequireLine(t *testing.T, file string, lines []string, fragment string) {
	t.Helper()
	for _, line := range lines {
		if line == fragment {
			return
		}
	}
	t.Errorf("%s: accountPurge block is missing line %q", file, fragment)
}

func trustDocsCivoCells() []string {
	return []string{"civo-sandbox-use1-serving", "civo-sandbox-use1-backup", "civo-prod-use1-serving"}
}
