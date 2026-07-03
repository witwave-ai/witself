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
	"net/http"
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
)

const projectName = "witself-infra"

// clouds are the functional provider selectors (also the name token).
var clouds = map[string]bool{"aws": true, "gcp": true, "azure": true}

// regionCodes maps a real cloud region to the short token used in the cell name.
// This is a lookup table, not an algorithm: naive abbreviation collides (e.g.
// ap-south-1 vs ap-southeast-1). Unknown regions are a hard error, not a guess.
var regionCodes = map[string]string{
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
}

// label is the safe form for the free-text account-alias and role tokens: they
// land in DNS-style resource names, so lowercase alphanumeric with internal hyphens.
var label = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
var domainName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)

const usage = `witself-infra — provision and manage Witself cells

usage:
  witself-infra <command> [flags]

commands:
  bootstrap initialize the state backend (S3 bucket + KMS key) for the account
  up        create or update the cell
  preview   show what up would change
  destroy   tear the cell down
  refresh   reconcile state with the real cloud
  outputs   print the cell's stack outputs

The cell name is composed: <cloud>-<account-alias>-<region-code>-<role>

flags:
  -cloud          provider (functional): aws|gcp|azure        (default "aws")
  -account-alias  free-text account label for the name        (default "sandbox")
  -region         real cloud region (functional)              (default "us-west-2")
  -role           role/ordinal label: dev, dev2, prod, ...    (default "1")
  -profile        resource sizing (functional): minimal|prod  (default "minimal")
  -cidr           cell VPC CIDR (a /16)                        (default "10.20.0.0/16")
  -k8s-version    EKS Kubernetes version                       (default "1.36")
  -db-version     RDS PostgreSQL major version                 (default "18")
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
  -backend        state backend: s3|local                      (default "s3")
  -bootstrap      with -backend s3, create the backend if missing
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

example:
  witself-infra up -cloud aws -account-alias sandbox -region us-west-2 -role dev -aws-profile witwave-sandbox
  # cell aws-sandbox-usw2-dev -> resources witself-aws-sandbox-usw2-dev-*
  # creds come from -aws-profile (or the ambient AWS_PROFILE/OIDC). The local-state
  # passphrase is managed for you — no PULUMI_CONFIG_PASSPHRASE needed.
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "witself-infra: "+err.Error())
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Print(usage)
		if len(args) == 0 {
			return fmt.Errorf("no command given")
		}
		return nil
	}
	cmd := args[0]

	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	cloud := fs.String("cloud", "aws", "provider (functional): aws|gcp|azure")
	accountAlias := fs.String("account-alias", "sandbox", "free-text account label for the cell name")
	region := fs.String("region", "us-west-2", "real cloud region (functional)")
	role := fs.String("role", "1", "role/ordinal label for the cell name: dev, dev2, prod")
	profile := fs.String("profile", "minimal", "resource sizing (functional): minimal|prod")
	cidr := fs.String("cidr", "10.20.0.0/16", "cell VPC CIDR (a /16)")
	k8sVersion := fs.String("k8s-version", "1.36", "EKS Kubernetes version")
	dbVersion := fs.String("db-version", "18", "RDS PostgreSQL major version")
	ingress := fs.String("ingress", "cloudflare-tunnel", "ingress mode: cloudflare-tunnel|alb|none")
	argocd := fs.Bool("argocd", false, "install Argo CD (GitOps control plane) into the cell cluster")
	gitopsRepo := fs.String("gitops-repo", cell.DefaultGitopsRepo, "GitOps repo URL Argo reconciles (with -argocd)")
	gitopsPath := fs.String("gitops-path", cell.DefaultGitopsPath, "path to the root bootstrap chart")
	gitopsValuesPath := fs.String("gitops-values-path", "", "path to this cell's bootstrap values (default: .gitops/cells/<cell>/values.yaml)")
	gitopsRevision := fs.String("gitops-revision", cell.DefaultGitopsRevision, "GitOps repo revision (branch/tag)")
	domain := fs.String("domain", cell.DefaultDomain, "parent domain for cell hostnames, e.g. cells.witself.witwave.ai")
	bootstrapTokenFile := fs.String("bootstrap-token-file", "", "first-operator bootstrap token file (default: per-cell ~/.witself/tokens/<cell>/bootstrap.token, else shared ~/.witself/tokens/bootstrap.token)")
	awsProfile := fs.String("aws-profile", "", "AWS named profile for credentials (default: ambient AWS chain / OIDC)")
	backendFlag := fs.String("backend", "s3", "state backend: s3|local (local is a dev opt-out)")
	bootstrap := fs.Bool("bootstrap", false, "with -backend s3: create the backend if it is missing")
	stateDir := fs.String("state-dir", defaultStateDir(), "local Pulumi state backend dir")
	controlPlane := fs.String("control-plane", "", "fleet control plane URL (up registers the cell; destroy drains+removes it)")
	fleetTokenFile := fs.String("fleet-token-file", "", "fleet token file (default: WITSELF_FLEET_TOKEN, then ~/.witself/tokens/fleet.token)")
	destroyAccounts := fs.Bool("destroy-accounts", false, "with destroy: SKIP evacuation and force-purge accounts (they die with the cell)")
	restoreArchives := fs.Bool("restore-archives", false, "with up: pull every archived account in this cell's region from R2 after registration")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	// Validate functional + label inputs before composing the name.
	if !clouds[*cloud] {
		return fmt.Errorf("unknown -cloud %q (want aws|gcp|azure)", *cloud)
	}
	regionCode, ok := regionCodes[*region]
	if !ok {
		return fmt.Errorf("unknown -region %q; add it to the region-code table", *region)
	}
	if !label.MatchString(*accountAlias) {
		return fmt.Errorf("-account-alias %q must be lowercase alphanumeric/hyphen", *accountAlias)
	}
	if !label.MatchString(*role) {
		return fmt.Errorf("-role %q must be lowercase alphanumeric/hyphen", *role)
	}
	*domain = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(*domain)), ".")
	if *domain != "" && !domainName.MatchString(*domain) {
		return fmt.Errorf("-domain %q must be a DNS domain name like cells.example.com", *domain)
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

	// Refresh an expired AWS SSO session up front (interactive only) so the
	// operation doesn't fail on credentials mid-flight.
	if *cloud == "aws" {
		backend.EnsureAWSSession(context.Background(), *awsProfile)
	}

	// bootstrap initializes the state backend (S3 + KMS) and returns; it is not a
	// cell op, so it skips the stack/passphrase machinery below.
	if cmd == "bootstrap" {
		return runBootstrap(*cloud, *region, regionCode, *awsProfile)
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
	default:
		return fmt.Errorf("unknown -backend %q (want local|s3)", *backendFlag)
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
		"witself:k8sVersion":       *k8sVersion,
		"witself:dbVersion":        *dbVersion,
		"witself:argocd":           fmt.Sprintf("%t", *argocd),
		"witself:gitopsRepo":       *gitopsRepo,
		"witself:gitopsPath":       *gitopsPath,
		"witself:gitopsValuesPath": *gitopsValuesPath,
		"witself:gitopsRevision":   *gitopsRevision,
		"witself:domain":           *domain,
		"witself:cloudflareDNS":    fmt.Sprintf("%t", cloudflareDelegation),
		"aws:region":               *region,
	} {
		if err := stack.SetConfig(ctx, k, auto.ConfigValue{Value: v}); err != nil {
			return fmt.Errorf("set config %s: %w", k, err)
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

	switch cmd {
	case "up":
		_, err = stack.Up(ctx, optup.ProgressStreams(os.Stdout))
		if err == nil && *controlPlane != "" {
			// Fleet registration is a post-step, deliberately outside the Pulumi
			// resource graph: membership is not a cloud resource.
			var apiHost string
			apiHost, err = registerCell(ctx, stack, *controlPlane, *fleetTokenFile, cellName, *cloud, *region)
			if err == nil && *restoreArchives {
				// Pulumi returning success doesn't mean the cell is
				// reachable — Argo has to reconcile, external-dns has to
				// publish the A record, the ALB target has to pass its
				// health check, and TLS has to deploy. Wait for the
				// public API to actually answer before firing restore.
				if err = waitForCellHealthy(ctx, apiHost, 20*time.Minute, 20*time.Second); err == nil {
					var cl *fleet.Client
					cl, err = fleet.NewClient(*controlPlane, *fleetTokenFile)
					if err == nil {
						err = restoreCell(ctx, cl, cellName)
					}
				}
			}
		}
	case "preview":
		_, err = stack.Preview(ctx, optpreview.ProgressStreams(os.Stdout))
	case "destroy":
		if *controlPlane != "" {
			// Fleet removal is a pre-step: drain first (placement stops), then
			// remove — refusing while accounts live on the cell unless the
			// operator explicitly acknowledges their destruction.
			if err := removeCell(ctx, *controlPlane, *fleetTokenFile, cellName, *destroyAccounts); err != nil {
				return err
			}
		}
		_, err = stack.Destroy(ctx, optdestroy.ProgressStreams(os.Stdout))
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
func registerCell(ctx context.Context, stack auto.Stack, controlPlane, fleetTokenFile, cellName, cloud, region string) (string, error) {
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
		ProvisionToken: provisionToken,
	}); err != nil {
		return "", err
	}
	fmt.Fprintf(os.Stderr, "cell %s registered with control plane %s\n", cellName, controlPlane)
	return host, nil
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
// httpClientFactory constructs the client used by waitForCellHealthy.
// Overridable so tests can substitute an httptest-served client without
// spinning up a real HTTPS listener.
var httpClientFactory = func() *http.Client {
	return &http.Client{Timeout: 10 * time.Second}
}

func waitForCellHealthy(ctx context.Context, host string, maxWait, pollEvery time.Duration) error {
	url := "https://" + host + "/v1/version"
	client := httpClientFactory()
	deadline := time.Now().Add(maxWait)
	started := time.Now()

	fmt.Fprintf(os.Stderr, "waiting for cell %s API endpoint to be reachable (up to %s)…\n", host, maxWait)

	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		var reason string
		if err == nil {
			if resp.StatusCode == http.StatusOK {
				_ = resp.Body.Close()
				fmt.Fprintf(os.Stderr, "cell %s endpoint healthy (took %s)\n", host, time.Since(started).Round(time.Second))
				return nil
			}
			_ = resp.Body.Close()
			reason = fmt.Sprintf("HTTP %d", resp.StatusCode)
		} else {
			// Truncate wordy net.OpError chains so the progress line stays readable.
			reason = err.Error()
			if len(reason) > 120 {
				reason = reason[:117] + "..."
			}
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("cell %s endpoint did not become healthy within %s (last: %s)", host, maxWait, reason)
		}

		fmt.Fprintf(os.Stderr, "  %s: %s (%s elapsed)\n", host, reason, time.Since(started).Round(time.Second))

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

// restoreCell loops the control-plane's :restore batches until every
// region-matched archived account has landed on the cell (remaining=0). The
// mirror of evacuateCell — same batch size, same stall guards. Fails fast on
// per-account failure so a partial restore never proceeds silently.
func restoreCell(ctx context.Context, cl *fleet.Client, cellName string) error {
	const batch = 4
	total := 0
	prevRemaining := -1
	for iter := 0; ; iter++ {
		res, err := cl.Restore(ctx, cellName, batch)
		if err != nil {
			if errors.Is(err, fleet.ErrCellDrained) {
				// The cell was just registered accepting=true, so this
				// should never happen — but if the operator concurrently
				// drained the cell, restore has no target.
				return fmt.Errorf("cell %s is drained — cannot restore into it", cellName)
			}
			return fmt.Errorf("restore cell: %w", err)
		}
		for _, a := range res.Restored {
			if !a.OK {
				return fmt.Errorf("restore failed for %s onto %s: %s", a.AccountID, cellName, a.Error)
			}
			total++
			fmt.Fprintf(os.Stderr, "restored %s onto %s\n", a.AccountID, cellName)
		}
		if res.Remaining == 0 {
			if total == 0 && iter == 0 {
				fmt.Fprintf(os.Stderr, "cell %s: no archived accounts awaiting placement in region %s\n", cellName, res.Region)
			} else {
				fmt.Fprintf(os.Stderr, "cell %s: %d accounts restored from Cloudflare R2\n", cellName, total)
			}
			return nil
		}
		if len(res.Restored) == 0 {
			// Remaining > 0 but the batch reported nothing — the control
			// plane sees archived accounts it will not hand us. Silent loop
			// would be wrong.
			return fmt.Errorf("restore stalled on cell %s with %d account(s) still awaiting placement in region %s", cellName, res.Remaining, res.Region)
		}
		if prevRemaining != -1 && res.Remaining >= prevRemaining {
			// A batch fired (per-account "ok") but the count didn't drop.
			// The acct: pointer never landed, or the archived: entry
			// wasn't retired — either way, spinning is the wrong answer.
			return fmt.Errorf("restore on cell %s is not making progress in region %s (%d accounts remaining after batch reported success)", cellName, res.Region, res.Remaining)
		}
		prevRemaining = res.Remaining
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

// runBootstrap initializes the state backend for the account+region (idempotent).
func runBootstrap(cloud, region, regionCode, profile string) error {
	if cloud != "aws" {
		return fmt.Errorf("bootstrap is only implemented for -cloud aws")
	}
	info, err := backend.BootstrapAWS(context.Background(), region, regionCode, profile, func(m string) {
		fmt.Fprintln(os.Stderr, "  "+m)
	})
	if err != nil {
		return err
	}
	fmt.Println("state backend ready:")
	fmt.Println("  bucket:           " + info.Bucket)
	fmt.Println("  backend (s3):     " + info.BackendURL)
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
