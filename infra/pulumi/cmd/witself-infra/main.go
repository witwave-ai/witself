// Command witself-infra provisions and manages Witself cells.
//
// It embeds the Pulumi engine via the Automation API (inline-program mode), so it
// is a self-contained provisioner. The cell definition lives in internal/cell and
// is compiled into this binary. State and credentials stay external — state in a
// backend (a local dir by default), cloud creds ambient (e.g. AWS_PROFILE / OIDC).
//
// The cell name is COMPOSED from components, never hand-typed:
//
//	<cloud>-<account-alias>-<region-code>-<role>   e.g. aws-sandbox-usw2-dev
//
// That composed name is the Pulumi stack name and the prefix the cell program
// threads into every resource (witself-<cell>-*). Functional inputs (cloud,
// region, profile) drive behavior; label inputs (account-alias, role) are free
// text used only in the name.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/pulumi/pulumi/sdk/v3/go/auto"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optdestroy"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optpreview"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optup"
	"github.com/pulumi/pulumi/sdk/v3/go/common/workspace"

	"github.com/witwave-ai/witself/infra/pulumi/internal/backend"
	"github.com/witwave-ai/witself/infra/pulumi/internal/cell"
	"github.com/witwave-ai/witself/infra/pulumi/internal/fleet"
	"github.com/witwave-ai/witself/infra/pulumi/internal/regions"
)

const projectName = "witself-infra"

// clouds are the functional provider selectors (also the name token).
var clouds = map[string]bool{"aws": true, "gcp": true, "azure": true}
var placementChannels = map[string]bool{"stable": true, "edge": true, "experimental": true}

// legacyRegionCodes maps real cloud regions outside the three-cloud placement
// catalog to the short token used in existing cell names and state backends.
// The placement catalog is authoritative for region_code registration; this
// fallback keeps older/single-provider regions from breaking while the catalog
// is deliberately limited to high-fidelity AWS/GCP/Azure mappings.
var legacyRegionCodes = map[string]string{
	"us-east-1": "use1", "us-east-2": "use2",
	"us-west-1": "usw1", "us-west-2": "usw2",
	"ca-central-1": "cac1",
	"eu-west-1":    "euw1", "eu-west-2": "euw2", "eu-west-3": "euw3",
	"eu-central-1": "euc1", "eu-north-1": "eun1", "eu-south-1": "eus1",
	"ap-south-1":     "aps1",
	"ap-southeast-1": "apse1", "ap-southeast-2": "apse2", "ap-southeast-3": "apse3",
	"ap-northeast-1": "apne1", "ap-northeast-2": "apne2", "ap-northeast-3": "apne3",
	"ap-east-1":  "ape1",
	"sa-east-1":  "sae1",
	"af-south-1": "afs1",
	"me-south-1": "mes1", "me-central-1": "mec1",

	// Google Cloud regions. These intentionally share the same short token as
	// the equivalent AWS geography where one exists; the cloud token in the cell
	// name keeps gcp-sandbox-usw2-dev distinct from aws-sandbox-usw2-dev.
	"us-central1": "usc1",
	"us-east1":    "use1", "us-east4": "use4", "us-east5": "use5",
	"us-west1": "usw1", "us-west2": "usw2", "us-west3": "usw3", "us-west4": "usw4",
	"northamerica-northeast1": "nane1", "northamerica-northeast2": "nane2",
	"southamerica-east1": "same1", "southamerica-west1": "samw1",
	"europe-central2":   "euc2",
	"europe-north1":     "eun1",
	"europe-southwest1": "eusw1",
	"europe-west1":      "euw1", "europe-west2": "euw2", "europe-west3": "euw3", "europe-west4": "euw4",
	"europe-west8": "euw8", "europe-west9": "euw9", "europe-west10": "euw10", "europe-west12": "euw12",
	"asia-east1":      "ase1",
	"asia-east2":      "ase2",
	"asia-northeast1": "asne1", "asia-northeast2": "asne2", "asia-northeast3": "asne3",
	"asia-south1": "ass1", "asia-south2": "ass2",
	"asia-southeast1": "asse1", "asia-southeast2": "asse2",
	"australia-southeast1": "ause1", "australia-southeast2": "ause2",
	"me-west1":      "mew1",
	"me-central1":   "mec1",
	"me-central2":   "mec2",
	"africa-south1": "afs1",

	// Azure regions. These also share geography tokens with AWS/GCP where
	// possible; the cloud token keeps azure-sandbox-use2-dev distinct.
	"eastus":  "use1",
	"eastus2": "use2",
}

// label is the safe form for the free-text account-alias and role tokens: they
// land in DNS-style resource names, so lowercase alphanumeric with internal hyphens.
var label = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
var domainName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)
var gcpProjectID = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)

const usage = `witself-infra — provision and manage Witself cells

usage:
  witself-infra <command> [flags]

commands:
  bootstrap initialize the state backend for the account/project/subscription
  up        create or update the cell
  preview   show what up would change
  destroy   tear the cell down
  refresh   reconcile state with the real cloud
  outputs   print the cell's stack outputs
  rebalance move live accounts to better eligible cells by placement policy
  placement-runner show, enable, disable, or trigger scheduled placement work
  placement-status show fleet placement, archive, and rebalance status
  whoami    resolve a cell's security context, call the cloud identity
              API, and refuse if it doesn't match the expected_account_id
              / tenant pin (also runs as a pre-flight before up/preview/
              destroy/refresh/bootstrap when -cell resolves the config)
  dashboard  fullscreen infra dashboard (alias: tui): inventory + fleet
              + per-cell identity, plus p/preview, u/up, D/destroy with
              typed-name and preview-first confirmation gates, and
              a/auth (interactive login for a cell in an auth-error state)
  version   print version information
  config    manage the local cell inventory (~/.witself/infra.yaml):
              config init                 write a skeleton file
              config add-cell [flags]     record a cell from the usual flags
              config show [-cell NAME]    list cells, or print one cell's
                                          effective merged configuration

The cell name is composed: <cloud>-<account-alias>-<region-code>-<role>

Cells can live in the config file instead of flags:
  witself-infra up -cell aws-sandbox-usw2-dev
Precedence: explicit flag > cell entry > defaults block > built-in.

flags:
  -cell           cell name from infra.yaml — fills every unset flag
                  from the config entry (identity flags then conflict)
  -config         config file path (default: $WITSELF_HOME/infra.yaml,
                  else ~/.witself/infra.yaml)
  -progress-json  with up/preview/destroy: emit NDJSON phase events on
                  stderr ({"ts","phase","state","cell","note"} lines)
  -cloud          provider (functional): aws|gcp|azure        (default "aws")
  -account-alias  free-text account label for the name        (default "sandbox")
  -region         real cloud region (functional)              (default "us-west-2")
  -role           role/ordinal label: dev, dev2, prod, ...    (default "1")
  -channel        placement channel: stable|edge|experimental (default "experimental")
  -profile        resource sizing (functional): minimal|prod  (default "minimal")
  -cidr           cell VPC CIDR (a /16)                        (default "10.20.0.0/16")
  -k8s-version    EKS Kubernetes version                       (default "1.36")
  -db-version     PostgreSQL major version                     (default "18")
  -ingress        cloudflare-tunnel|alb|none                   (default "cloudflare-tunnel")
  -argocd         install Argo CD (GitOps control plane)        (default false)
  -gitops-repo     GitOps repo Argo reconciles (with -argocd)   (default witwave-ai/witself)
  -gitops-path     path to the root bootstrap chart             (default ".gitops/charts/bootstrap")
  -gitops-values-path path to this cell's bootstrap values       (default ".gitops/cells/<cell>/values.yaml")
  -gitops-revision GitOps repo revision (branch/tag)            (default "main")
  -domain        parent domain for cell hostnames               (default "cells.witself.witwave.ai")
  -bootstrap-token-file first-operator bootstrap token file
                  (default: ~/.witself/tokens/<cell>/bootstrap.token when
                  present, else the shared ~/.witself/tokens/bootstrap.token)
  -aws-profile    AWS named profile for creds (default: ambient AWS chain / OIDC)
  -gcp-project    GCP project ID for GCP cells/state backend
  -azure-subscription Azure subscription name or ID for Azure cells/state backend
  -backend        state backend: s3|gcs|azblob|local           (default "s3")
  -bootstrap      with -backend s3/gcs/azblob, create backend if missing
  -state-dir      local Pulumi state backend dir (backend=local)
  -control-plane  fleet control plane URL, e.g. https://self.witwave.ai
                  up: registers the cell after provisioning
                  destroy: drains the cell, evacuates every account to
                  Cloudflare R2 archives, then removes the empty registry
                  entry. Omit = no fleet (self-host).
  -fleet-token-file  fleet token file (default: WITSELF_FLEET_TOKEN env,
                  then ~/.witself/tokens/fleet.token)
  -destroy-accounts  with destroy: SKIP evacuation and force-purge this
                  cell's accounts from the control-plane directory. The
                  data dies with the cell — sandbox/development override
  -restore-archives  with up: after the cell registers, restore every
                  archived account whose region matches this cell's region
                  from Cloudflare R2. Loops until none remain. Requires
                  -control-plane.
  -restore-any-region  with -restore-archives: intentionally restore all
                  archived accounts, ignoring their stored region. Use for
                  explicit cross-cloud/cross-region evacuation tests only.
  -batch          with rebalance: accounts to move per control-plane call (default 1)
  -dry-run        with rebalance: print selected moves without changing accounts
  -enable         with placement-runner: enable scheduled restore/rebalance
  -disable        with placement-runner: disable scheduled restore/rebalance
  -run            with placement-runner: trigger one manual restore/rebalance pass
  -limit          with placement-status: max sample rows per category (default 25)

example:
  witself-infra config add-cell -cloud aws -account-alias sandbox -region us-west-2 -role dev -aws-profile witwave-sandbox
  witself-infra up -cell aws-sandbox-usw2-dev
  # or the equivalent all-flags invocation (no config file needed):
  witself-infra up -cloud aws -account-alias sandbox -region us-west-2 -role dev -aws-profile witwave-sandbox
  # cell aws-sandbox-usw2-dev -> resources witself-aws-sandbox-usw2-dev-*
  # creds come from -aws-profile (or the ambient AWS_PROFILE/OIDC). The local-state
  # passphrase is managed for you — no PULUMI_CONFIG_PASSPHRASE needed.
`

// Injected by goreleaser ldflags; source builds report "dev". Closes
// the deployment-consistency gap where witself-infra was the only
// family binary with no version surface.
var (
	buildVersion = "dev"
	buildCommit  = ""
	buildDate    = ""
)

func versionLine() string {
	out := "witself-infra " + buildVersion
	if buildCommit != "" {
		out += " (commit " + buildCommit
		if buildDate != "" {
			out += ", built " + buildDate
		}
		out += ")"
	}
	return out
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "witself-infra: "+err.Error())
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) > 0 && (args[0] == "version" || args[0] == "--version" || args[0] == "-v") {
		fmt.Println(versionLine())
		return nil
	}
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Print(usage)
		if len(args) == 0 {
			return fmt.Errorf("no command given")
		}
		return nil
	}
	cmd := args[0]

	// `config <sub>` carries its subcommand between the command and the
	// flags — split it out before the shared parse.
	fsArgs := args[1:]
	configSub := ""
	if cmd == "config" {
		if len(args) < 2 {
			return fmt.Errorf("config needs a subcommand: init|add-cell|show")
		}
		configSub = args[1]
		fsArgs = args[2:]
	}

	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	cloud := fs.String("cloud", "aws", "provider (functional): aws|gcp|azure")
	accountAlias := fs.String("account-alias", "sandbox", "free-text account label for the cell name")
	region := fs.String("region", "us-west-2", "real cloud region (functional)")
	role := fs.String("role", "1", "role/ordinal label for the cell name: dev, dev2, prod")
	channel := fs.String("channel", "experimental", "placement channel: stable|edge|experimental")
	profile := fs.String("profile", "minimal", "resource sizing (functional): minimal|prod")
	cidr := fs.String("cidr", "10.20.0.0/16", "cell VPC CIDR (a /16)")
	k8sVersion := fs.String("k8s-version", "1.36", "EKS Kubernetes version")
	dbVersion := fs.String("db-version", "18", "PostgreSQL major version")
	ingress := fs.String("ingress", "cloudflare-tunnel", "ingress mode: cloudflare-tunnel|alb|none")
	argocd := fs.Bool("argocd", false, "install Argo CD (GitOps control plane) into the cell cluster")
	gitopsRepo := fs.String("gitops-repo", cell.DefaultGitopsRepo, "GitOps repo URL Argo reconciles (with -argocd)")
	gitopsPath := fs.String("gitops-path", cell.DefaultGitopsPath, "path to the root bootstrap chart")
	gitopsValuesPath := fs.String("gitops-values-path", "", "path to this cell's bootstrap values (default: .gitops/cells/<cell>/values.yaml)")
	gitopsRevision := fs.String("gitops-revision", cell.DefaultGitopsRevision, "GitOps repo revision (branch/tag)")
	domain := fs.String("domain", cell.DefaultDomain, "parent domain for cell hostnames, e.g. cells.witself.witwave.ai")
	bootstrapTokenFile := fs.String("bootstrap-token-file", "", "first-operator bootstrap token file (default: per-cell ~/.witself/tokens/<cell>/bootstrap.token, else shared ~/.witself/tokens/bootstrap.token)")
	awsProfile := fs.String("aws-profile", "", "AWS named profile for credentials (default: ambient AWS chain / OIDC)")
	gcpProject := fs.String("gcp-project", "", "GCP project ID for GCP cells/state backend")
	azureSubscription := fs.String("azure-subscription", "", "Azure subscription name or ID for Azure cells/state backend (default: az CLI current subscription)")
	backendFlag := fs.String("backend", "s3", "state backend: s3|gcs|azblob|local (local is a dev opt-out)")
	bootstrap := fs.Bool("bootstrap", false, "with -backend s3/gcs/azblob: create the backend if it is missing")
	stateDir := fs.String("state-dir", defaultStateDir(), "local Pulumi state backend dir")
	controlPlane := fs.String("control-plane", "", "fleet control plane URL (up registers the cell; destroy drains+removes it)")
	fleetTokenFile := fs.String("fleet-token-file", "", "fleet token file (default: WITSELF_FLEET_TOKEN, then ~/.witself/tokens/fleet.token)")
	destroyAccounts := fs.Bool("destroy-accounts", false, "with destroy: SKIP evacuation and force-purge accounts (they die with the cell)")
	restoreArchives := fs.Bool("restore-archives", false, "with up: pull every archived account in this cell's region from R2 after registration")
	restoreAnyRegion := fs.Bool("restore-any-region", false, "with -restore-archives: pull every archived account from R2, ignoring stored region")
	rebalanceBatch := fs.Int("batch", 1, "with rebalance: accounts to move per control-plane call")
	rebalanceDryRun := fs.Bool("dry-run", false, "with rebalance: print selected moves without changing accounts")
	placementRunnerEnable := fs.Bool("enable", false, "with placement-runner: enable scheduled restore/rebalance")
	placementRunnerDisable := fs.Bool("disable", false, "with placement-runner: disable scheduled restore/rebalance")
	placementRunnerRun := fs.Bool("run", false, "with placement-runner: trigger one manual restore/rebalance pass")
	placementStatusLimit := fs.Int("limit", 25, "with placement-status: max sample rows per category")
	cellSelector := fs.String("cell", "", "cell name from infra.yaml — fills every unset flag from the config entry")
	configPath := fs.String("config", "", "config file path (default: $WITSELF_HOME/infra.yaml, else ~/.witself/infra.yaml)")
	progressJSON := fs.Bool("progress-json", false, "emit NDJSON phase events on stderr alongside the plain-text output (dashboard-friendly)")
	if err := fs.Parse(fsArgs); err != nil {
		return err
	}

	if cmd == "config" {
		return runConfigCmd(configSub, fs, *configPath)
	}
	// -cell presets flags from the config file BEFORE any validation or
	// cloud work; an explicit flag still wins over the file. Fleet-wide
	// commands reject it: `rebalance -cell X` READS as "rebalance this
	// cell" but the operation moves live accounts across the whole
	// fleet — a scoping illusion this tool must not offer.
	if *cellSelector != "" {
		switch cmd {
		case "rebalance", "placement-runner", "placement-status":
			return fmt.Errorf("%s is fleet-wide — -cell does not scope it; pass -control-plane and -fleet-token-file directly", cmd)
		case "whoami":
			// whoami owns its own applyCellConfig call; running it here
			// too would set flags via fs.Set that flag.Visit then sees
			// as "explicit" on the second call, tripping the identity
			// conflict guard and breaking `whoami -cell X` end to end.
		case "dashboard", "tui":
			// The dashboard resolves every cell's config internally.
		default:
			if err := applyCellConfig(fs, *cellSelector, *configPath); err != nil {
				return err
			}
		}
	}

	if cmd == "whoami" {
		return runWhoami(fs)
	}
	if cmd == "dashboard" || cmd == "tui" {
		return runDashboard(fs)
	}
	// Identity pre-flight: any command that will TOUCH cloud state
	// runs whoami first when the cell has expected_account_id / tenant
	// pins in its security_context. A wrong profile that resolves to a
	// different account must fail HERE, before EnsureAWSSession or a
	// stack Upsert lets a fresh state backend appear in the wrong
	// account. Only fires for -cell (bare-flag invocations preserve
	// today's zero-safety-net behavior; the safety net is opt-in via
	// the config file).
	if *cellSelector != "" {
		switch cmd {
		case "up", "preview", "destroy", "refresh", "bootstrap":
			if err := requireIdentityMatch(context.Background(), *cellSelector, *configPath); err != nil {
				return err
			}
		}
	}
	if cmd == "rebalance" {
		if *controlPlane == "" {
			return fmt.Errorf("rebalance requires -control-plane")
		}
		return rebalanceFleet(context.Background(), *controlPlane, *fleetTokenFile, *rebalanceBatch, *rebalanceDryRun)
	}
	if cmd == "placement-runner" {
		if *controlPlane == "" {
			return fmt.Errorf("placement-runner requires -control-plane")
		}
		return placementRunnerFleet(context.Background(), *controlPlane, *fleetTokenFile, *placementRunnerEnable, *placementRunnerDisable, *placementRunnerRun)
	}
	if cmd == "placement-status" {
		if *controlPlane == "" {
			return fmt.Errorf("placement-status requires -control-plane")
		}
		return placementStatusFleet(context.Background(), *controlPlane, *fleetTokenFile, *placementStatusLimit)
	}

	// Validate functional + label inputs before composing the name.
	if !clouds[*cloud] {
		return fmt.Errorf("unknown -cloud %q (want aws|gcp|azure)", *cloud)
	}
	regionCode, placementRegionCode, ok := resolveRegionCode(*cloud, *region)
	if !ok {
		return fmt.Errorf("unknown -region %q for -cloud %s; add it to regions/catalog.json or the legacy region table", *region, *cloud)
	}
	if !label.MatchString(*accountAlias) {
		return fmt.Errorf("-account-alias %q must be lowercase alphanumeric/hyphen", *accountAlias)
	}
	if !label.MatchString(*role) {
		return fmt.Errorf("-role %q must be lowercase alphanumeric/hyphen", *role)
	}
	*channel = strings.ToLower(strings.TrimSpace(*channel))
	if !placementChannels[*channel] {
		return fmt.Errorf("unknown -channel %q (want stable|edge|experimental)", *channel)
	}
	*domain = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(*domain)), ".")
	if *domain != "" && !domainName.MatchString(*domain) {
		return fmt.Errorf("-domain %q must be a DNS domain name like cells.example.com", *domain)
	}
	if *gcpProject != "" && !gcpProjectID.MatchString(*gcpProject) {
		return fmt.Errorf("-gcp-project %q must be a Google Cloud project ID", *gcpProject)
	}
	if *cloud == "gcp" && *gcpProject == "" {
		return fmt.Errorf("-gcp-project is required with -cloud gcp")
	}
	// Cross-flag rejects come BEFORE any cloud work — an operator who typed
	// the wrong combination should learn it in milliseconds, not after
	// 20 minutes of EKS provisioning.
	if *restoreArchives && *controlPlane == "" {
		return fmt.Errorf("-restore-archives requires -control-plane")
	}
	if *restoreArchives && cmd != "up" {
		return fmt.Errorf("-restore-archives is only valid with `up`")
	}
	if *restoreAnyRegion && !*restoreArchives {
		return fmt.Errorf("-restore-any-region requires -restore-archives")
	}

	// Refresh an expired AWS SSO session up front (interactive only) so the
	// operation doesn't fail on credentials mid-flight.
	if *cloud == "aws" {
		backend.EnsureAWSSession(context.Background(), *awsProfile)
	}

	// bootstrap initializes the state backend (S3 + KMS) and returns; it is not a
	// cell op, so it skips the stack/passphrase machinery below.
	if cmd == "bootstrap" {
		return runBootstrap(*cloud, *region, regionCode, *awsProfile, *gcpProject, *azureSubscription)
	}

	// Compose the cell name: it is the Pulumi stack name and the resource prefix.
	cellName := strings.Join([]string{*cloud, *accountAlias, regionCode, *role}, "-")
	if *gitopsValuesPath == "" {
		*gitopsValuesPath = cell.DefaultGitopsValuesPath(cellName)
	}
	defaultBootstrapPath, err := defaultBootstrapTokenFile(cellName)
	if err != nil {
		return fmt.Errorf("resolve default bootstrap token path: %w", err)
	}

	ctx := context.Background()

	// A profile NAME is not a secret, so it is safe to pass as a flag; when
	// omitted, the AWS provider uses the ambient credential chain (AWS_PROFILE
	// env, SSO, OIDC), which is what CI / Witself Cloud rely on.
	env := map[string]string{}
	if *awsProfile != "" {
		env["AWS_PROFILE"] = *awsProfile
	}
	cloudflareDelegation := os.Getenv("CLOUDFLARE_API_TOKEN") != ""
	if cloudflareDelegation {
		env["CLOUDFLARE_API_TOKEN"] = os.Getenv("CLOUDFLARE_API_TOKEN")
	}

	var wsOpts []auto.LocalWorkspaceOption
	var secretsProvider string
	var azureSubscriptionID string
	switch *backendFlag {
	case "local":
		// Local file backend with a tool-managed passphrase (dev default).
		if err := os.MkdirAll(*stateDir, 0o755); err != nil {
			return fmt.Errorf("create state dir: %w", err)
		}
		passphrase, err := ensurePassphrase(*stateDir)
		if err != nil {
			return err
		}
		env["PULUMI_BACKEND_URL"] = "file://" + *stateDir
		env["PULUMI_CONFIG_PASSPHRASE"] = passphrase
	case "s3":
		// Shared S3 backend + KMS secrets provider (no passphrase). The backend is
		// created out-of-band by `bootstrap`; up uses it, or creates it only when
		// explicitly asked via -bootstrap.
		if *cloud != "aws" {
			return fmt.Errorf("-backend s3 is only implemented for -cloud aws")
		}
		info, exists, err := backend.ResolveAWS(ctx, *region, regionCode, *awsProfile)
		if err != nil {
			return err
		}
		if !exists {
			if !*bootstrap {
				return fmt.Errorf("state backend %s does not exist — run `witself-infra bootstrap -cloud aws -region %s` first (or pass -bootstrap)", info.Bucket, *region)
			}
			if _, err := backend.BootstrapAWS(ctx, *region, regionCode, *awsProfile, func(m string) {
				fmt.Fprintln(os.Stderr, "  "+m)
			}); err != nil {
				return err
			}
		}
		env["PULUMI_BACKEND_URL"] = info.BackendURL
		secretsProvider = info.SecretsProvider
		wsOpts = append(wsOpts, auto.SecretsProvider(secretsProvider))
	case "gcs":
		// Shared GCS backend + Cloud KMS secrets provider (no passphrase). The
		// bucket is project+region scoped, so one GCP project can hold many cell
		// stacks without making project == cell.
		if *cloud != "gcp" {
			return fmt.Errorf("-backend gcs is only implemented for -cloud gcp")
		}
		if *gcpProject == "" {
			return fmt.Errorf("-gcp-project is required with -cloud gcp -backend gcs")
		}
		if err := backend.EnsureGCPADC(ctx, *gcpProject); err != nil {
			return err
		}
		info, exists, err := backend.ResolveGCP(ctx, *gcpProject, *region, regionCode)
		if err != nil {
			return err
		}
		if !exists {
			if !*bootstrap {
				return fmt.Errorf("state backend %s does not exist — run `witself-infra bootstrap -cloud gcp -backend gcs -gcp-project %s -region %s` first (or pass -bootstrap)", info.Bucket, *gcpProject, *region)
			}
			if _, err := backend.BootstrapGCP(ctx, *gcpProject, *region, regionCode, func(m string) {
				fmt.Fprintln(os.Stderr, "  "+m)
			}); err != nil {
				return err
			}
		}
		env["PULUMI_BACKEND_URL"] = info.BackendURL
		secretsProvider = info.SecretsProvider
		wsOpts = append(wsOpts, auto.SecretsProvider(secretsProvider))
	case "azblob":
		// Shared Azure Blob backend + Key Vault secrets provider (no passphrase).
		// The storage account is subscription+region scoped and can hold many
		// cell stacks; cell identity stays in the stack name.
		if *cloud != "azure" {
			return fmt.Errorf("-backend azblob is only implemented for -cloud azure")
		}
		info, exists, err := backend.ResolveAzure(ctx, *azureSubscription, *region, regionCode)
		if err != nil {
			return err
		}
		if !exists {
			if !*bootstrap {
				return fmt.Errorf("state backend %s does not exist — run `witself-infra bootstrap -cloud azure -backend azblob -region %s` first (or pass -bootstrap)", info.Bucket, *region)
			}
			if _, err := backend.BootstrapAzure(ctx, *azureSubscription, *region, regionCode, func(m string) {
				fmt.Fprintln(os.Stderr, "  "+m)
			}); err != nil {
				return err
			}
			info, exists, err = backend.ResolveAzure(ctx, *azureSubscription, *region, regionCode)
			if err != nil {
				return err
			}
			if !exists {
				return fmt.Errorf("state backend %s was not visible after bootstrap", info.Bucket)
			}
		}
		env["PULUMI_BACKEND_URL"] = info.BackendURL
		env["AZURE_STORAGE_ACCOUNT"] = info.Bucket
		env["AZURE_STORAGE_KEY"] = info.StorageKey
		azureSubscriptionID = info.SubscriptionID
		secretsProvider = info.SecretsProvider
		wsOpts = append(wsOpts, auto.SecretsProvider(secretsProvider))
	default:
		return fmt.Errorf("unknown -backend %q (want local|s3|gcs|azblob)", *backendFlag)
	}
	if *cloud == "gcp" && cmd != "outputs" {
		if err := backend.EnsureGCPServices(ctx, *gcpProject, func(m string) {
			fmt.Fprintln(os.Stderr, "  "+m)
		}, "cloudresourcemanager.googleapis.com", "compute.googleapis.com", "dns.googleapis.com", "servicenetworking.googleapis.com", "container.googleapis.com", "sqladmin.googleapis.com", "secretmanager.googleapis.com", "iamcredentials.googleapis.com"); err != nil {
			return err
		}
	}
	if *cloud == "azure" && cmd != "outputs" {
		if err := backend.EnsureAzureProviders(ctx, func(m string) {
			fmt.Fprintln(os.Stderr, "  "+m)
		}, "Microsoft.Network", "Microsoft.NetworkFunction", "Microsoft.ServiceNetworking", "Microsoft.DBforPostgreSQL", "Microsoft.KeyVault", "Microsoft.ManagedIdentity", "Microsoft.Compute", "Microsoft.ContainerService"); err != nil {
			return err
		}
		if cmd == "up" && *argocd {
			if err := backend.EnsureAzureFeatures(ctx, func(m string) {
				fmt.Fprintln(os.Stderr, "  "+m)
			},
				backend.AzureFeature{Namespace: "Microsoft.ContainerService", Name: "ManagedGatewayAPIPreview"},
				backend.AzureFeature{Namespace: "Microsoft.ContainerService", Name: "ApplicationLoadBalancerPreview"},
			); err != nil {
				return err
			}
			if err := backend.EnsureAzureProviders(ctx, func(m string) {
				fmt.Fprintln(os.Stderr, "  "+m)
			}, "Microsoft.ContainerService"); err != nil {
				return err
			}
		}
	}

	wsOpts = append(wsOpts, auto.EnvVars(env))
	stack, err := auto.UpsertStackInlineSource(ctx, cellName, projectName, cell.Program, wsOpts...)
	if err != nil {
		return fmt.Errorf("create/select cell %q: %w", cellName, err)
	}

	// On the s3 backend, persist the KMS secrets provider into the (ephemeral)
	// workspace's stack settings. auto.SecretsProvider only applies on stack
	// CREATE; when a later run SELECTS an existing stack, config operations read
	// the local Pulumi.<stack>.yaml (not the checkpoint) and would otherwise fall
	// back to a passphrase. Re-saving it each run keeps select on KMS.
	if secretsProvider != "" {
		if err := stack.Workspace().SaveStackSettings(ctx, cellName, &workspace.ProjectStack{
			SecretsProvider: secretsProvider,
		}); err != nil {
			return fmt.Errorf("set secrets provider: %w", err)
		}
	}

	// Behavior config (cloud/profile/cidr/ingress) + the real region for the
	// provider. The name components are encoded in the cell/stack name itself.
	for k, v := range map[string]string{
		"witself:cloud":            *cloud,
		"witself:profile":          *profile,
		"witself:ingress":          *ingress,
		"witself:cidr":             *cidr,
		"witself:accountAlias":     *accountAlias,
		"witself:role":             *role,
		"witself:channel":          *channel,
		"witself:k8sVersion":       *k8sVersion,
		"witself:dbVersion":        *dbVersion,
		"witself:argocd":           fmt.Sprintf("%t", *argocd),
		"witself:gitopsRepo":       *gitopsRepo,
		"witself:gitopsPath":       *gitopsPath,
		"witself:gitopsValuesPath": *gitopsValuesPath,
		"witself:gitopsRevision":   *gitopsRevision,
		"witself:domain":           *domain,
		"witself:cloudflareDNS":    fmt.Sprintf("%t", cloudflareDelegation),
	} {
		if err := stack.SetConfig(ctx, k, auto.ConfigValue{Value: v}); err != nil {
			return fmt.Errorf("set config %s: %w", k, err)
		}
	}
	switch *cloud {
	case "aws":
		if err := stack.SetConfig(ctx, "aws:region", auto.ConfigValue{Value: *region}); err != nil {
			return fmt.Errorf("set config aws:region: %w", err)
		}
	case "gcp":
		for k, v := range map[string]string{
			"witself:gcpProject": *gcpProject,
			"gcp:project":        *gcpProject,
			"gcp:region":         *region,
		} {
			if err := stack.SetConfig(ctx, k, auto.ConfigValue{Value: v}); err != nil {
				return fmt.Errorf("set config %s: %w", k, err)
			}
		}
	case "azure":
		if err := stack.SetConfig(ctx, "azure-native:location", auto.ConfigValue{Value: *region}); err != nil {
			return fmt.Errorf("set config azure-native:location: %w", err)
		}
		if azureSubscriptionID != "" {
			if err := stack.SetConfig(ctx, "azure-native:subscriptionId", auto.ConfigValue{Value: azureSubscriptionID}); err != nil {
				return fmt.Errorf("set config azure-native:subscriptionId: %w", err)
			}
		}
	}
	if cmd == "up" || cmd == "preview" {
		path := *bootstrapTokenFile
		explicit := path != ""
		if path == "" {
			path = defaultBootstrapPath
		}
		tok, ok, err := readBootstrapTokenFile(path, explicit)
		if err != nil {
			return err
		}
		if ok {
			if err := stack.SetConfig(ctx, "witself:bootstrapToken", auto.ConfigValue{Value: tok, Secret: true}); err != nil {
				return fmt.Errorf("set bootstrap token config: %w", err)
			}
		} else {
			cfg, err := stack.GetAllConfig(ctx)
			if err != nil {
				return fmt.Errorf("read stack config: %w", err)
			}
			if _, exists := cfg["witself:bootstrapToken"]; exists {
				if err := stack.RemoveConfig(ctx, "witself:bootstrapToken"); err != nil {
					return fmt.Errorf("clear bootstrap token config: %w", err)
				}
			}
		}
	}

	sink := newProgressSink(*progressJSON)
	switch cmd {
	case "up":
		sink.start(cellName, "pulumi.up", "")
		_, err = stack.Up(ctx, optup.ProgressStreams(os.Stdout))
		if err != nil {
			sink.errPhase(cellName, "pulumi.up", err)
		} else {
			sink.end(cellName, "pulumi.up", "")
		}
		if err == nil && *controlPlane != "" {
			// Fleet registration is a post-step, deliberately outside the Pulumi
			// resource graph: membership is not a cloud resource.
			_, err = registerCell(ctx, stack, *controlPlane, *fleetTokenFile, cellName, *cloud, *region, placementRegionCode, *channel)
			if err == nil && *restoreArchives {
				// Pulumi returning success doesn't mean the cell is
				// reachable — Argo has to reconcile, external-dns has to
				// publish the A record, the ALB target has to pass its
				// health check, and TLS has to deploy. Poll the control
				// plane's :probe verb (which runs inside the Cloudflare
				// Worker, the client that will do the restore) until it
				// reports the cell as reachable.
				var cl *fleet.Client
				cl, err = fleet.NewClient(*controlPlane, *fleetTokenFile)
				if err == nil {
					if err = waitForCellHealthy(ctx, cl, cellName, 20*time.Minute, 20*time.Second); err == nil {
						err = restoreCell(ctx, cl, cellName, *restoreAnyRegion)
					}
				}
			}
		}
		if err == nil {
			err = waitForPostUpConvergence(ctx, stack, *cloud, *argocd, 15*time.Minute, 20*time.Second)
		}
	case "preview":
		sink.start(cellName, "pulumi.preview", "")
		_, err = stack.Preview(ctx, optpreview.ProgressStreams(os.Stdout))
		if err != nil {
			sink.errPhase(cellName, "pulumi.preview", err)
		} else {
			sink.end(cellName, "pulumi.preview", "")
		}
	case "destroy":
		if *controlPlane != "" {
			sink.start(cellName, "fleet.remove", "")
			// Fleet removal is a pre-step: drain first (placement stops), then
			// remove — refusing while accounts live on the cell unless the
			// operator explicitly acknowledges their destruction.
			if err := removeCell(ctx, *controlPlane, *fleetTokenFile, cellName, *destroyAccounts); err != nil {
				sink.errPhase(cellName, "fleet.remove", err)
				return err
			}
			sink.end(cellName, "fleet.remove", "")
		}
		sink.start(cellName, "pulumi.destroy", "")
		_, err = stack.Destroy(ctx, optdestroy.ProgressStreams(os.Stdout))
		if err != nil {
			sink.errPhase(cellName, "pulumi.destroy", err)
		} else {
			sink.end(cellName, "pulumi.destroy", "")
		}
		if err == nil {
			err = verifyPulumiDestroyEmpty(ctx, stack)
		}
	case "refresh":
		_, err = stack.Refresh(ctx)
	case "outputs":
		return printOutputs(ctx, stack)
	default:
		return fmt.Errorf("unknown command %q (see: witself-infra help)", cmd)
	}
	return err
}

// registerCell reports the freshly provisioned cell to the control plane. The
// endpoint comes from the cell's apiHost output (api.<cell>.<domain>). The
// hostname is returned so callers can chain a readiness poll before the
// next post-provision step (restore-archives).
func registerCell(ctx context.Context, stack auto.Stack, controlPlane, fleetTokenFile, cellName, cloud, region, regionCode, channel string) (string, error) {
	cl, err := fleet.NewClient(controlPlane, fleetTokenFile)
	if err != nil {
		return "", err
	}
	outs, err := stack.Outputs(ctx)
	if err != nil {
		return "", fmt.Errorf("read outputs for fleet registration: %w", err)
	}
	host, _ := outs["apiHost"].Value.(string)
	if host == "" {
		return "", fmt.Errorf("cell exports no apiHost (ingress/domain disabled) — cannot register with the control plane")
	}
	// The per-cell provisioning credential rides the registration payload; the
	// control plane stores it and presents it to this cell on each signup.
	provisionToken, _ := outs["provisionToken"].Value.(string)
	if err := cl.Register(ctx, fleet.Cell{
		Name:           cellName,
		Endpoint:       "https://" + host,
		Cloud:          cloud,
		Region:         region,
		RegionCode:     regionCode,
		Channel:        channel,
		ProvisionToken: provisionToken,
	}); err != nil {
		return "", err
	}
	fmt.Fprintf(os.Stderr, "cell %s registered with control plane %s\n", cellName, controlPlane)
	return host, nil
}

func resolveRegionCode(cloud, providerRegion string) (nameRegionCode, placementRegionCode string, ok bool) {
	if code, _, _, found := regions.LookupProviderRegion(cloud, providerRegion); found {
		return code, code, true
	}
	code, found := legacyRegionCodes[providerRegion]
	if !found {
		return "", "", false
	}
	return code, "", true
}

// waitForCellHealthy polls the cell's public API endpoint until GET
// /v1/version returns 200. Everything that must be true before -restore-
// archives can succeed — Argo CD reconciled, external-dns wrote the A
// record, ALB target-group health check passed, TLS certificate deployed,
// witself-server pod running with DB migrations applied — is transitively
// true when /v1/version answers 200 (the API listener refuses to accept
// requests until migrations complete, and the ALB won't route until the
// health check passes).
//
// On a fresh cell (Pulumi just finished, Argo hasn't reconciled) this
// typically takes 5–10 minutes; on a cell that was already up, it returns
// on the first poll. The default budget comfortably covers the worst
// realistic cold-start seen on the sandbox (~8 minutes) plus headroom.
// cellProber is the subset of fleet.Client the wait step needs. Declared
// locally so tests can substitute a fake Probe without spinning up a
// real HTTP server or reaching for a package-level override hook.
type cellProber interface {
	Probe(ctx context.Context, name string) (fleet.ProbeResult, error)
}

// waitForCellHealthy polls the control plane's cell reachability probe
// until it reports OK=true. The probe runs INSIDE the Cloudflare Worker,
// which is the client that will do the actual restore, so its view of
// cell reachability is what actually matters — not the operator's local
// resolver, which on macOS can cache NXDOMAIN across destroy+up cycles
// well past a wait budget (issue #22).
//
// Everything that must be true before restore can succeed — Argo CD
// reconciled, external-dns wrote the A record, ALB target-group health
// check passed, TLS certificate deployed, witself-server pod running
// with DB migrations applied — is transitively true when the Worker's
// GET on <cell>/v1/version returns witself-server-shaped JSON.
func waitForCellHealthy(ctx context.Context, cl cellProber, cellName string, maxWait, pollEvery time.Duration) error {
	deadline := time.Now().Add(maxWait)
	started := time.Now()

	fmt.Fprintf(os.Stderr, "waiting for cell %s to be reachable via the control plane (up to %s)…\n", cellName, maxWait)

	for {
		result, err := cl.Probe(ctx, cellName)
		var fullReason string
		if err == nil && result.OK {
			verInfo := ""
			if result.CellVersion != "" {
				verInfo = fmt.Sprintf(" [witself-server %s]", result.CellVersion)
			}
			fmt.Fprintf(os.Stderr, "cell %s reachable%s (took %s)\n", cellName, verInfo, time.Since(started).Round(time.Second))
			return nil
		}
		if err != nil {
			// Probe-transport error (not the same as "the probe reported
			// the cell isn't ready"). ErrNotRegistered means someone
			// deregistered the cell — no amount of waiting fixes that.
			if errors.Is(err, fleet.ErrNotRegistered) {
				return fmt.Errorf("cell %s is not registered with the control plane — cannot probe", cellName)
			}
			fullReason = fmt.Sprintf("probe error: %s", err.Error())
		} else {
			// result.OK == false: the Worker reached the fleet but the
			// probe reports the cell is not yet reachable. Reason carries
			// the actual diagnostic (DNS lookup, TLS error, HTTP status).
			fullReason = result.Reason
			if fullReason == "" {
				fullReason = fmt.Sprintf("HTTP %d", result.CellStatus)
			}
		}
		shortReason := fullReason
		if len(shortReason) > 120 {
			shortReason = shortReason[:117] + "..."
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("cell %s did not become reachable within %s (last: %s)", cellName, maxWait, fullReason)
		}

		fmt.Fprintf(os.Stderr, "  %s: %s (%s elapsed)\n", cellName, shortReason, time.Since(started).Round(time.Second))

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollEvery):
		}
	}
}

// removeCell drains the cell and removes it from the fleet ahead of teardown.
// Default flow (polite): evacuate every account on the cell into an R2
// archive first, then remove the empty cell. With -destroy-accounts: skip
// preservation and force-purge every directory entry (accounts die with the
// cell — the sandbox / development override).
func removeCell(ctx context.Context, controlPlane, fleetTokenFile, cellName string, destroyAccounts bool) error {
	cl, err := fleet.NewClient(controlPlane, fleetTokenFile)
	if err != nil {
		return err
	}
	if err := cl.Drain(ctx, cellName); err != nil {
		if errors.Is(err, fleet.ErrNotRegistered) {
			fmt.Fprintf(os.Stderr, "cell %s is not registered with %s — skipping fleet removal\n", cellName, controlPlane)
			return nil
		}
		return fmt.Errorf("drain cell: %w", err)
	}
	if destroyAccounts {
		purged, err := cl.Purge(ctx, cellName)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "cell %s purged from fleet (%d account entries removed)\n", cellName, purged)
		return nil
	}
	// Evacuate any accounts still routing to this cell into R2 before we
	// try to remove the (empty) registry entry. Delete refuses while
	// accounts point at the cell, so the loop must reach remaining=0.
	if err := evacuateCell(ctx, cl, cellName); err != nil {
		return err
	}
	if err := cl.Delete(ctx, cellName); err != nil {
		if errors.Is(err, fleet.ErrAccountsLive) {
			// A signup landing on the cell between the last evacuate call
			// and Delete could produce this — although the cell was
			// drained, so it shouldn't. Retry the evacuation once so the
			// straggler catches up.
			fmt.Fprintln(os.Stderr, "a straggler slipped past the evacuation loop; re-running…")
			if err := evacuateCell(ctx, cl, cellName); err != nil {
				return err
			}
			if err := cl.Delete(ctx, cellName); err != nil {
				return fmt.Errorf("remove cell from fleet: %w", err)
			}
		} else {
			return fmt.Errorf("remove cell from fleet: %w", err)
		}
	}
	fmt.Fprintf(os.Stderr, "cell %s removed from fleet\n", cellName)
	return nil
}

// evacuateCell loops the control-plane's :evacuate batches until every
// account on the cell is archived (remaining=0). Fails fast if any account
// batch reports a failure — a stuck evacuation must not silently proceed to
// destroying the cell's database.
func evacuateCell(ctx context.Context, cl *fleet.Client, cellName string) error {
	const batch = 4
	total := 0
	// Track the LOWEST Remaining we've seen so we can detect the far more
	// insidious stall: batches that report per-account success but the
	// server's Remaining doesn't strictly decrease (a control-plane bug
	// leaving acct: pointers behind would spin this loop forever otherwise).
	prevRemaining := -1
	for iter := 0; ; iter++ {
		res, err := cl.Evacuate(ctx, cellName, batch)
		if err != nil {
			if errors.Is(err, fleet.ErrNotDrained) {
				return fmt.Errorf("cell %s is not drained — cannot evacuate", cellName)
			}
			return fmt.Errorf("evacuate cell: %w", err)
		}
		for _, a := range res.Evacuated {
			if !a.OK {
				return fmt.Errorf("evacuation failed for %s on %s: %s", a.AccountID, cellName, a.Error)
			}
			total++
			if a.Reaped {
				fmt.Fprintf(os.Stderr, "reaped pending %s on %s (no archive)\n", a.AccountID, cellName)
			} else {
				fmt.Fprintf(os.Stderr, "evacuated %s from %s\n", a.AccountID, cellName)
			}
		}
		if res.Remaining == 0 {
			if total == 0 && iter == 0 {
				fmt.Fprintf(os.Stderr, "cell %s has no accounts to evacuate\n", cellName)
			} else {
				fmt.Fprintf(os.Stderr, "cell %s: %d accounts evacuated to Cloudflare R2\n", cellName, total)
			}
			return nil
		}
		if len(res.Evacuated) == 0 {
			// The call reported remaining > 0 but did nothing — either
			// nothing on the cell is currently evacuable, or something's
			// stuck. Either way, silently looping is wrong.
			return fmt.Errorf("evacuation stalled on cell %s with %d account(s) still routing", cellName, res.Remaining)
		}
		if prevRemaining != -1 && res.Remaining >= prevRemaining {
			// A batch fired (accounts reported "ok") but the fleet's count
			// didn't decrease. The archived: entry landed but the acct:
			// pointer didn't retire, or the same accounts are being re-
			// listed — a control-plane bug that would otherwise burn all
			// day. Fail loudly; Pulumi destroy stays parked.
			return fmt.Errorf("evacuation on cell %s is not making progress (%d accounts remaining after batch reported success)", cellName, res.Remaining)
		}
		prevRemaining = res.Remaining
	}
}

// restoreCell loops the control-plane placement runner until every currently
// placeable archived account has landed on its best eligible cell
// (remaining=0). The cellName argument is the just-registered cell that
// triggered the run and is used for compatibility fallback and status text.
func restoreCell(ctx context.Context, cl *fleet.Client, cellName string, allRegions bool) error {
	const batch = 4
	total := 0
	prevRemaining := -1
	for iter := 0; ; iter++ {
		res, err := cl.RestorePlacement(ctx, batch, allRegions)
		if err != nil {
			// Older control planes only know the per-cell restore endpoint.
			if strings.Contains(err.Error(), "HTTP 404") {
				res, err = cl.Restore(ctx, cellName, batch, allRegions)
			}
			if err != nil {
				if errors.Is(err, fleet.ErrCellDrained) {
					return fmt.Errorf("cell %s is drained — cannot restore into it", cellName)
				}
				return fmt.Errorf("restore archives: %w", err)
			}
		}
		for _, a := range res.Restored {
			dest := a.Cell
			if dest == "" {
				dest = cellName
			}
			if !a.OK {
				return fmt.Errorf("restore failed for %s onto %s: %s", a.AccountID, dest, a.Error)
			}
			total++
			fmt.Fprintf(os.Stderr, "restored %s onto %s\n", a.AccountID, dest)
		}
		if res.Remaining == 0 {
			if res.Unplaced > 0 {
				fmt.Fprintf(os.Stderr, "%d archived account(s) remain unplaced (no eligible accepting cell)\n", res.Unplaced)
			}
			if total == 0 && iter == 0 {
				fmt.Fprintln(os.Stderr, "no archived accounts currently eligible for placement")
			} else {
				fmt.Fprintf(os.Stderr, "%d accounts restored from Cloudflare R2\n", total)
			}
			return nil
		}
		if len(res.Restored) == 0 {
			// Remaining > 0 but the batch reported nothing — the control
			// plane sees archived accounts it will not hand us. Silent loop
			// would be wrong.
			return fmt.Errorf("placement restore stalled with %d eligible archived account(s) still awaiting placement", res.Remaining)
		}
		if prevRemaining != -1 && res.Remaining >= prevRemaining {
			// A batch fired (per-account "ok") but the count didn't drop.
			// The acct: pointer never landed, or the archived: entry
			// wasn't retired — either way, spinning is the wrong answer.
			return fmt.Errorf("placement restore is not making progress (%d eligible account(s) remaining after batch reported success)", res.Remaining)
		}
		prevRemaining = res.Remaining
	}
}

func rebalanceFleet(ctx context.Context, controlPlane, fleetTokenFile string, batch int, dryRun bool) error {
	if batch < 1 {
		return fmt.Errorf("-batch must be at least 1")
	}
	if batch > 5 {
		batch = 5
	}
	cl, err := fleet.NewClient(controlPlane, fleetTokenFile)
	if err != nil {
		return err
	}
	total := 0
	prevRemaining := -1
	for iter := 0; ; iter++ {
		res, err := cl.Rebalance(ctx, batch, dryRun)
		if err != nil {
			return err
		}
		for _, a := range res.Rebalanced {
			if !a.OK {
				return fmt.Errorf("rebalance failed for %s from %s to %s: %s", a.AccountID, a.FromCell, a.ToCell, a.Error)
			}
			total++
			if dryRun || a.DryRun {
				fmt.Fprintf(os.Stderr, "would move %s from %s to %s", a.AccountID, a.FromCell, a.ToCell)
			} else {
				fmt.Fprintf(os.Stderr, "moved %s from %s to %s", a.AccountID, a.FromCell, a.ToCell)
			}
			if a.Reason != "" {
				fmt.Fprintf(os.Stderr, " (%s)", a.Reason)
			}
			fmt.Fprintln(os.Stderr)
		}
		if len(res.Skipped) > 0 && iter == 0 {
			fmt.Fprintf(os.Stderr, "%d account(s) skipped by the control plane\n", len(res.Skipped))
		}
		if dryRun {
			fmt.Fprintf(os.Stderr, "%d account(s) would be moved in this batch; %d move candidate(s) currently visible\n", total, res.Remaining)
			return nil
		}
		if res.Remaining == 0 {
			if total == 0 && iter == 0 {
				fmt.Fprintln(os.Stderr, "no live accounts currently need rebalancing")
			} else {
				fmt.Fprintf(os.Stderr, "%d account(s) rebalanced\n", total)
			}
			return nil
		}
		if len(res.Rebalanced) == 0 {
			return fmt.Errorf("rebalance stalled with %d account(s) still eligible to move", res.Remaining)
		}
		if prevRemaining != -1 && res.Remaining >= prevRemaining {
			return fmt.Errorf("rebalance is not making progress (%d eligible account(s) remaining after a successful batch)", res.Remaining)
		}
		prevRemaining = res.Remaining
	}
}

func placementRunnerFleet(ctx context.Context, controlPlane, fleetTokenFile string, enable, disable, runOnce bool) error {
	if enable && disable {
		return fmt.Errorf("choose only one of -enable or -disable")
	}
	cl, err := fleet.NewClient(controlPlane, fleetTokenFile)
	if err != nil {
		return err
	}
	cfg, err := cl.GetPlacementRunner(ctx)
	if err != nil {
		return err
	}
	if enable || disable {
		cfg.Enabled = enable
		cfg, err = cl.SetPlacementRunner(ctx, cfg)
		if err != nil {
			return err
		}
	}
	printPlacementRunnerConfig(cfg)
	if !runOnce {
		return nil
	}
	res, err := cl.RunPlacementRunner(ctx, cfg)
	if err != nil {
		return err
	}
	printPlacementRunnerResult(res)
	if res.RestoreError != nil {
		return fmt.Errorf("placement runner restore failed: HTTP %d: %s", res.RestoreError.Status, string(res.RestoreError.Body))
	}
	if res.RebalanceError != nil {
		return fmt.Errorf("placement runner rebalance failed: HTTP %d: %s", res.RebalanceError.Status, string(res.RebalanceError.Body))
	}
	return nil
}

func printPlacementRunnerConfig(cfg fleet.PlacementRunnerConfig) {
	fmt.Printf("enabled\t%v\n", cfg.Enabled)
	fmt.Printf("restore archives\t%v\n", cfg.RestoreArchives)
	fmt.Printf("restore batch\t%d\n", cfg.RestoreBatch)
	fmt.Printf("restore any region\t%v\n", cfg.RestoreAnyRegion)
	fmt.Printf("rebalance\t%v\n", cfg.Rebalance)
	fmt.Printf("rebalance batch\t%d\n", cfg.RebalanceBatch)
}

func printPlacementRunnerResult(res fleet.PlacementRunnerResult) {
	if res.Restore != nil {
		ok := 0
		for _, a := range res.Restore.Restored {
			if a.OK {
				ok++
			}
		}
		fmt.Printf("restore\t%d restored, %d remaining, %d unplaced\n", ok, res.Restore.Remaining, res.Restore.Unplaced)
	}
	if res.Rebalance != nil {
		ok := 0
		for _, a := range res.Rebalance.Rebalanced {
			if a.OK {
				ok++
			}
		}
		fmt.Printf("rebalance\t%d moved, %d remaining\n", ok, res.Rebalance.Remaining)
	}
}

func placementStatusFleet(ctx context.Context, controlPlane, fleetTokenFile string, limit int) error {
	if limit < 1 {
		return fmt.Errorf("-limit must be at least 1")
	}
	cl, err := fleet.NewClient(controlPlane, fleetTokenFile)
	if err != nil {
		return err
	}
	status, err := cl.GetPlacementStatus(ctx, limit)
	if err != nil {
		return err
	}
	printPlacementStatus(status)
	return nil
}

func printPlacementStatus(status fleet.PlacementStatus) {
	fmt.Printf("runner\tenabled=%v restore=%v rebalance=%v\n",
		status.PlacementRunner.Enabled,
		status.PlacementRunner.RestoreArchives,
		status.PlacementRunner.Rebalance)
	fmt.Printf("archived\ttotal=%d placeable=%d unplaced=%d\n",
		status.Archived.Total,
		status.Archived.Placeable,
		status.Archived.Unplaced)
	fmt.Printf("live\ttotal=%d movable=%d skipped=%d\n",
		status.Live.Total,
		status.Live.Movable,
		status.Live.Skipped)

	cells := append([]fleet.PlacementStatusCell(nil), status.Cells...)
	sort.Slice(cells, func(i, j int) bool { return cells[i].Name < cells[j].Name })
	for _, c := range cells {
		fmt.Printf("cell\t%s\tcloud=%s region_code=%s channel=%s accepting=%v live=%d archived=%d\n",
			c.Name,
			c.Cloud,
			c.RegionCode,
			c.Channel,
			c.Accepting,
			c.AccountCount,
			c.ArchivedCount)
	}
	for _, a := range status.Archived.Blocked {
		fmt.Printf("blocked\t%s\tfrom=%s region_code=%s reason=%s\n",
			a.AccountID,
			a.FromCell,
			a.RegionCode,
			a.Reason)
	}
	for _, a := range status.Live.MovableAccounts {
		fmt.Printf("movable\t%s\tfrom=%s to=%s reason=%s\n",
			a.AccountID,
			a.FromCell,
			a.ToCell,
			a.Reason)
	}
	for _, a := range status.Live.SkippedAccounts {
		fmt.Printf("skipped\t%s\tcell=%s reason=%s\n",
			a.AccountID,
			a.Cell,
			a.Reason)
	}
}

func printOutputs(ctx context.Context, stack auto.Stack) error {
	outs, err := stack.Outputs(ctx)
	if err != nil {
		return err
	}
	keys := make([]string, 0, len(outs))
	for k := range outs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if outs[k].Secret {
			fmt.Printf("%s = [secret]\n", k) // never print secret values
			continue
		}
		fmt.Printf("%s = %v\n", k, outs[k].Value)
	}
	return nil
}

// runBootstrap initializes the state backend for the account/project+region
// (idempotent).
func runBootstrap(cloud, region, regionCode, awsProfile, gcpProject, azureSubscription string) error {
	ctx := context.Background()
	var (
		info *backend.Info
		err  error
	)
	switch cloud {
	case "aws":
		info, err = backend.BootstrapAWS(ctx, region, regionCode, awsProfile, func(m string) {
			fmt.Fprintln(os.Stderr, "  "+m)
		})
	case "gcp":
		if gcpProject == "" {
			return fmt.Errorf("-gcp-project is required with -cloud gcp bootstrap")
		}
		info, err = backend.BootstrapGCP(ctx, gcpProject, region, regionCode, func(m string) {
			fmt.Fprintln(os.Stderr, "  "+m)
		})
	case "azure":
		info, err = backend.BootstrapAzure(ctx, azureSubscription, region, regionCode, func(m string) {
			fmt.Fprintln(os.Stderr, "  "+m)
		})
	default:
		return fmt.Errorf("bootstrap is not implemented for -cloud %s", cloud)
	}
	if err != nil {
		return err
	}
	fmt.Println("state backend ready:")
	fmt.Println("  bucket:           " + info.Bucket)
	fmt.Println("  backend:          " + info.BackendURL)
	fmt.Println("  secrets provider: " + info.SecretsProvider)
	return nil
}

func defaultStateDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".witself-infra-state"
	}
	return filepath.Join(home, ".witself-infra", "state")
}

func defaultBootstrapTokenFile(cellName string) (string, error) {
	root := os.Getenv("WITSELF_HOME")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		root = filepath.Join(home, ".witself")
	}
	// All credentials live under <root>/tokens. Most specific wins: a per-cell
	// token, else the shared tokens/bootstrap.token used for all cells. Legacy
	// pre-consolidation locations are honored one generation back.
	candidates := []string{
		filepath.Join(root, "tokens", cellName, "bootstrap.token"),
		filepath.Join(root, "bootstrap", cellName, "bootstrap.token"), // legacy per-cell
		filepath.Join(root, "bootstrap", cellName, "bootstrap-token"), // legacy name
		filepath.Join(root, "bootstrap.token"),                        // legacy shared
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return filepath.Join(root, "tokens", "bootstrap.token"), nil
}

func readBootstrapTokenFile(path string, required bool) (token string, ok bool, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if !required && os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read bootstrap token file %s: %w", path, err)
	}
	token = strings.TrimSpace(string(b))
	if token == "" {
		return "", false, fmt.Errorf("bootstrap token file %s is empty", path)
	}
	return token, true, nil
}

// ensurePassphrase returns the passphrase that encrypts secrets in the local
// state. A passphrase is a secret, so it is never a CLI flag. It respects an
// explicit PULUMI_CONFIG_PASSPHRASE if the user set one; otherwise it manages one
// for them — generating a random passphrase on first use and persisting it 0600
// alongside the state — so secret outputs work with nothing to export or type.
func ensurePassphrase(stateDir string) (string, error) {
	if p := os.Getenv("PULUMI_CONFIG_PASSPHRASE"); p != "" {
		return p, nil
	}
	path := filepath.Join(stateDir, "passphrase")
	if b, err := os.ReadFile(path); err == nil {
		return strings.TrimSpace(string(b)), nil
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate state passphrase: %w", err)
	}
	p := base64.RawURLEncoding.EncodeToString(buf)
	if err := os.WriteFile(path, []byte(p+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("persist state passphrase: %w", err)
	}
	return p, nil
}
