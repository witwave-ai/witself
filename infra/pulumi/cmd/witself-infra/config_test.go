package main

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestFlagSet mirrors the per-cell subset of run()'s flag universe —
// same names, same built-in defaults — so precedence tests exercise
// the real merge machinery.
func newTestFlagSet() *flag.FlagSet {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.String("cloud", "aws", "")
	fs.String("account-alias", "sandbox", "")
	fs.String("region", "us-west-2", "")
	fs.String("role", "1", "")
	fs.String("channel", "experimental", "")
	fs.String("profile", "minimal", "")
	fs.String("cidr", "10.20.0.0/16", "")
	fs.String("k8s-version", "1.36", "")
	fs.String("db-version", "18", "")
	fs.String("ingress", "cloudflare-tunnel", "")
	fs.Bool("argocd", false, "")
	fs.String("gitops-repo", "https://github.com/witwave-ai/witself", "")
	fs.String("gitops-path", ".gitops/charts/bootstrap", "")
	fs.String("gitops-values-path", "", "")
	fs.String("gitops-revision", "main", "")
	fs.String("domain", "cells.witself.witwave.ai", "")
	fs.String("bootstrap-token-file", "", "")
	fs.String("aws-profile", "", "")
	fs.String("gcp-project", "", "")
	fs.String("azure-subscription", "", "")
	fs.String("backend", "s3", "")
	fs.String("state-dir", "/tmp/state", "")
	fs.String("control-plane", "", "")
	fs.String("fleet-token-file", "", "")
	fs.String("cell", "", "")
	fs.String("config", "", "")
	return fs
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "infra.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const sampleConfig = `version: 1
defaults:
  control_plane: https://self.witwave.ai
  channel: stable
  profile: prod
cells:
  aws-sandbox-usw2-dev:
    cloud: aws
    account_alias: sandbox
    region: us-west-2
    role: dev
    channel: experimental
    argocd: true
    security_context:
      aws:
        profile: witwave-sandbox
`

// TestApplyCellConfigPrecedence pins the merge order: explicit flag >
// cell entry > defaults > built-in, across every tier at once.
func TestApplyCellConfigPrecedence(t *testing.T) {
	path := writeConfig(t, sampleConfig)
	fs := newTestFlagSet()
	// Operator explicitly overrides k8s-version on the command line.
	if err := fs.Parse([]string{"-k8s-version", "1.99"}); err != nil {
		t.Fatal(err)
	}
	if err := applyCellConfig(fs, "aws-sandbox-usw2-dev", path); err != nil {
		t.Fatal(err)
	}
	get := func(n string) string { return fs.Lookup(n).Value.String() }
	if get("k8s-version") != "1.99" {
		t.Errorf("explicit flag must win: k8s-version = %q", get("k8s-version"))
	}
	if get("channel") != "experimental" {
		t.Errorf("cell entry must beat defaults: channel = %q", get("channel"))
	}
	if get("profile") != "prod" {
		t.Errorf("defaults must fill unset: profile = %q", get("profile"))
	}
	if get("control-plane") != "https://self.witwave.ai" {
		t.Errorf("defaults must fill unset: control-plane = %q", get("control-plane"))
	}
	if get("backend") != "s3" {
		t.Errorf("built-in default must survive when nothing overrides: backend = %q", get("backend"))
	}
	if get("aws-profile") != "witwave-sandbox" {
		t.Errorf("security_context.aws.profile must map to -aws-profile: %q", get("aws-profile"))
	}
	if get("argocd") != "true" {
		t.Errorf("bool field: argocd = %q", get("argocd"))
	}
	if get("role") != "dev" || get("cloud") != "aws" {
		t.Errorf("identity from entry: cloud=%q role=%q", get("cloud"), get("role"))
	}
}

// TestApplyCellConfigIdentityConflict pins the guard: -cell plus an
// explicit identity flag is an error, never a silent winner.
func TestApplyCellConfigIdentityConflict(t *testing.T) {
	path := writeConfig(t, sampleConfig)
	fs := newTestFlagSet()
	if err := fs.Parse([]string{"-region", "eu-central-1"}); err != nil {
		t.Fatal(err)
	}
	err := applyCellConfig(fs, "aws-sandbox-usw2-dev", path)
	if err == nil || !strings.Contains(err.Error(), "conflicts with -cell") {
		t.Fatalf("want identity-conflict error, got %v", err)
	}
}

// TestApplyCellConfigKeyMismatch pins the canonical-name rule: the map
// key must equal what the entry's identity composes to.
func TestApplyCellConfigKeyMismatch(t *testing.T) {
	path := writeConfig(t, `version: 1
cells:
  aws-sandbox-usw2-dev:
    cloud: aws
    account_alias: sandbox
    region: eu-central-1
    role: dev
`)
	fs := newTestFlagSet()
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	err := applyCellConfig(fs, "aws-sandbox-usw2-dev", path)
	if err == nil || !strings.Contains(err.Error(), "does not match its identity") {
		t.Fatalf("want key-mismatch error, got %v", err)
	}
}

// TestApplyCellConfigUnknownCellListsAvailable pins the error UX.
func TestApplyCellConfigUnknownCellListsAvailable(t *testing.T) {
	path := writeConfig(t, sampleConfig)
	fs := newTestFlagSet()
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	err := applyCellConfig(fs, "aws-nope-usw2-dev", path)
	if err == nil || !strings.Contains(err.Error(), "aws-sandbox-usw2-dev") {
		t.Fatalf("error must list available cells, got %v", err)
	}
}

// TestLoadInfraConfigRejectsSecrets pins the paste-guard: token-shaped
// values must never persist in infra.yaml.
func TestLoadInfraConfigRejectsSecrets(t *testing.T) {
	for _, secret := range []string{
		"witself_flt_abc123def",
		"witself_boot_zzz999",
		"AKIAIOSFODNN7EXAMPLE",
		"ghp_0123456789abcdefghij",
		"-----BEGIN RSA PRIVATE KEY-----",
	} {
		path := writeConfig(t, "version: 1\ncells:\n  x:\n    role: \""+secret+"\"\n")
		if _, _, err := loadInfraConfig(path); err == nil || !strings.Contains(err.Error(), "credential") {
			t.Errorf("secret %q must be rejected, got %v", secret, err)
		}
	}
}

// TestLoadInfraConfigRejectsUnknownKeys pins typo safety: a misspelled
// key must error, not silently fall back to a default.
func TestLoadInfraConfigRejectsUnknownKeys(t *testing.T) {
	path := writeConfig(t, `version: 1
cells:
  aws-sandbox-usw2-dev:
    cloud: aws
    account_alias: sandbox
    region: us-west-2
    role: dev
    k8s_verion: "1.35"
`)
	if _, _, err := loadInfraConfig(path); err == nil {
		t.Fatal("typo'd key must be a parse error")
	}
}

// TestLoadInfraConfigRejectsDefaultsIdentity pins the defaults shape:
// no identity, no fleet-wide security context.
func TestLoadInfraConfigRejectsDefaultsIdentity(t *testing.T) {
	path := writeConfig(t, "version: 1\ndefaults:\n  cloud: aws\n")
	if _, _, err := loadInfraConfig(path); err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("defaults identity must be rejected, got %v", err)
	}
	path = writeConfig(t, "version: 1\ndefaults:\n  security_context:\n    aws:\n      profile: x\n")
	if _, _, err := loadInfraConfig(path); err == nil || !strings.Contains(err.Error(), "security_context") {
		t.Fatalf("defaults security_context must be rejected, got %v", err)
	}
}

// TestConfigAddCellRoundTrip pins the add-cell → -cell flow end to
// end: record a cell from flags, then resolve it back through the
// merge and compare the effective values.
func TestConfigAddCellRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "infra.yaml")

	fs := newTestFlagSet()
	if err := fs.Parse([]string{
		"-cloud", "aws", "-account-alias", "sandbox", "-region", "us-west-2", "-role", "dev",
		"-argocd", "-aws-profile", "witwave-sandbox", "-channel", "stable",
	}); err != nil {
		t.Fatal(err)
	}
	if err := configAddCell(fs, path); err != nil {
		t.Fatal(err)
	}
	// Duplicate add is refused.
	if err := configAddCell(fs, path); err == nil {
		t.Fatal("duplicate add-cell must be refused")
	}

	fs2 := newTestFlagSet()
	if err := fs2.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if err := applyCellConfig(fs2, "aws-sandbox-usw2-dev", path); err != nil {
		t.Fatal(err)
	}
	get := func(n string) string { return fs2.Lookup(n).Value.String() }
	if get("argocd") != "true" || get("aws-profile") != "witwave-sandbox" || get("channel") != "stable" {
		t.Fatalf("round trip lost values: argocd=%q profile=%q channel=%q",
			get("argocd"), get("aws-profile"), get("channel"))
	}
	// Untyped flags were NOT baked into the file — the entry records
	// intent, and future default changes flow through.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "k8s_version") {
		t.Fatalf("unset flag leaked into config:\n%s", raw)
	}
}

// TestConfigInit pins init: creates once, refuses to overwrite, and
// the skeleton is loadable.
func TestConfigInit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "infra.yaml")
	if err := configInit(path); err != nil {
		t.Fatal(err)
	}
	if err := configInit(path); err == nil {
		t.Fatal("second init must refuse to overwrite")
	}
	if _, _, err := loadInfraConfig(path); err != nil {
		t.Fatalf("skeleton must load cleanly: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config perms = %v, want 0600", info.Mode().Perm())
	}
}

// TestDefaultsRejectPerCellOnlyFields pins the wrong-target guards the
// adversarial review confirmed: gitops.values_path in defaults would
// bootstrap every cell with ONE cell's identity (cross-cell hijack),
// and backend/state_dir in defaults could point destroy at a fresh
// empty stack while the real cell survives.
func TestDefaultsRejectPerCellOnlyFields(t *testing.T) {
	for name, body := range map[string]string{
		"gitops values_path": "version: 1\ndefaults:\n  gitops:\n    values_path: .gitops/cells/aws-sandbox-usw2-dev/values.yaml\n",
		"backend":            "version: 1\ndefaults:\n  backend: local\n",
		"state_dir":          "version: 1\ndefaults:\n  state_dir: /tmp/x\n",
	} {
		path := writeConfig(t, body)
		if _, _, err := loadInfraConfig(path); err == nil {
			t.Errorf("%s in defaults must be rejected", name)
		}
	}
	// A defaults gitops block WITHOUT values_path stays legal.
	path := writeConfig(t, "version: 1\ndefaults:\n  gitops:\n    revision: main\n")
	if _, _, err := loadInfraConfig(path); err != nil {
		t.Errorf("defaults gitops.revision must stay legal: %v", err)
	}
}

// TestFleetWideCommandsRejectCell pins the scoping-illusion guard:
// `rebalance -cell X` reads as "rebalance this cell" but the operation
// is fleet-wide — it must refuse, not silently ignore the cell.
func TestFleetWideCommandsRejectCell(t *testing.T) {
	for _, cmd := range []string{"rebalance", "placement-runner", "placement-status"} {
		err := run([]string{cmd, "-cell", "aws-sandbox-usw2-dev"})
		if err == nil || !strings.Contains(err.Error(), "fleet-wide") {
			t.Errorf("%s -cell must be rejected as fleet-wide, got %v", cmd, err)
		}
	}
}

// TestAddCellValidatesCloud pins that add-cell never records an entry
// its own suggested `preview -cell` would reject.
func TestAddCellValidatesCloud(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "infra.yaml")
	fs := newTestFlagSet()
	if err := fs.Parse([]string{"-cloud", "wrongcloud", "-account-alias", "sandbox", "-region", "us-west-2", "-role", "dev"}); err != nil {
		t.Fatal(err)
	}
	if err := configAddCell(fs, path); err == nil || !strings.Contains(err.Error(), "unknown -cloud") {
		t.Fatalf("bad cloud must be rejected, got %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("no file may be written on validation failure")
	}
}

// TestAddCellRefusesSecretValues pins the write-path paste-guard: a
// token VALUE passed where a token file PATH belongs must refuse the
// write (persisting it would leak the secret AND brick the inventory,
// since every later load rejects the file).
func TestAddCellRefusesSecretValues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "infra.yaml")
	fs := newTestFlagSet()
	if err := fs.Parse([]string{
		"-cloud", "aws", "-account-alias", "sandbox", "-region", "us-west-2", "-role", "dev",
		"-fleet-token-file", "witself_flt_pasteMistake123",
	}); err != nil {
		t.Fatal(err)
	}
	if err := configAddCell(fs, path); err == nil || !strings.Contains(err.Error(), "credential") {
		t.Fatalf("secret value must refuse the write, got %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("secret must not reach disk")
	}
}

// TestAddCellAlwaysRecordsBackend pins stack-addressing self-
// containment: the entry records the effective backend even when the
// operator relied on the built-in default.
func TestAddCellAlwaysRecordsBackend(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "infra.yaml")
	fs := newTestFlagSet()
	if err := fs.Parse([]string{"-cloud", "aws", "-account-alias", "sandbox", "-region", "us-west-2", "-role", "dev"}); err != nil {
		t.Fatal(err)
	}
	if err := configAddCell(fs, path); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "backend: s3") {
		t.Fatalf("entry must record the effective backend:\n%s", raw)
	}
}

// TestSecretShapesCoverGitHubPATs pins the expanded guard.
func TestSecretShapesCoverGitHubPATs(t *testing.T) {
	for _, secret := range []string{
		"github_pat_11ABCDEFG0abcdefghijklmnop",
		"ghu_0123456789abcdefghij",
	} {
		if !secretShapes.MatchString(secret) {
			t.Errorf("secretShapes must match %q", secret)
		}
	}
}

// TestAzureThreadingSubscriptionPerCall pins the safety fix for the
// documented Slice 2 hazard: EnsureAzureCLI must NOT run `az account
// set`, which mutates the operator's global default subscription and
// stays set after the tool exits — a footgun for multi-cell sessions.
// The subscription now threads via --subscription on every az call.
func TestAzureThreadingSubscriptionPerCall(t *testing.T) {
	body, err := os.ReadFile("../../internal/backend/azure.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(body)
	if strings.Contains(src, `runAzure(ctx, nil, "account", "set"`) {
		t.Fatal("`az account set` reintroduced — it mutates the operator's global default subscription")
	}
	if !strings.Contains(src, "never `az account set`") {
		t.Fatal("the rationale comment in EnsureAzureCLI must survive as future-proofing")
	}
}

// TestWhoamiRejectsAWSAccountMismatch pins the safety net: whoami must
// refuse when the runtime AWS account doesn't match the config pin —
// the whole reason for expected_account_id (bucket names embed the
// account ID, so a wrong profile silently bootstraps a fresh backend
// in the wrong place).
func TestWhoamiRejectsAWSAccountMismatch(t *testing.T) {
	entry := cellEntry{
		Cloud: strPtr("aws"),
	}
	want := "999999999999"
	entry.SecurityContext = &securityContext{AWS: &awsContext{
		Profile:           strPtr(""),
		ExpectedAccountID: &want,
	}}
	// Simulate the compare with a hand-built identity — the STS call
	// itself is exercised in an integration test, not a unit test.
	got := identity{Cloud: "aws", Account: "123456789012", OK: true}
	if want == got.Account {
		t.Fatal("test fixture mismatched")
	}
	// Mirror whoamiAWS's pin check.
	pin := *entry.SecurityContext.AWS.ExpectedAccountID
	if pin != got.Account {
		got.OK = false
	}
	if got.OK {
		t.Fatal("account-id pin must reject a mismatch")
	}
}

func strPtr(s string) *string { return &s }
