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
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/auto"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optdestroy"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optpreview"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optup"

	"github.com/witwave-ai/witself/infra/pulumi/internal/cell"
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

const usage = `witself-infra — provision and manage Witself cells

usage:
  witself-infra <command> [flags]

commands:
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
  -ingress        cloudflare-tunnel|alb|none                   (default "cloudflare-tunnel")
  -aws-profile    AWS named profile for creds (default: ambient AWS chain / OIDC)
  -state-dir      local Pulumi state backend dir

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
	ingress := fs.String("ingress", "cloudflare-tunnel", "ingress mode: cloudflare-tunnel|alb|none")
	awsProfile := fs.String("aws-profile", "", "AWS named profile for credentials (default: ambient AWS chain / OIDC)")
	stateDir := fs.String("state-dir", defaultStateDir(), "local Pulumi state backend dir")
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

	// Compose the cell name: it is the Pulumi stack name and the resource prefix.
	cellName := strings.Join([]string{*cloud, *accountAlias, regionCode, *role}, "-")

	ctx := context.Background()
	if err := os.MkdirAll(*stateDir, 0o755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}

	passphrase, err := ensurePassphrase(*stateDir)
	if err != nil {
		return err
	}
	env := map[string]string{
		"PULUMI_BACKEND_URL":       "file://" + *stateDir,
		"PULUMI_CONFIG_PASSPHRASE": passphrase,
	}
	// A profile NAME is not a secret, so it is safe to pass as a flag; when
	// omitted, the AWS provider uses the ambient credential chain (AWS_PROFILE
	// env, SSO, OIDC), which is what CI / Witself Cloud rely on.
	if *awsProfile != "" {
		env["AWS_PROFILE"] = *awsProfile
	}

	stack, err := auto.UpsertStackInlineSource(ctx, cellName, projectName, cell.Program,
		auto.EnvVars(env),
	)
	if err != nil {
		return fmt.Errorf("create/select cell %q: %w", cellName, err)
	}

	// Behavior config (cloud/profile/cidr/ingress) + the real region for the
	// provider. The name components are encoded in the cell/stack name itself.
	for k, v := range map[string]string{
		"witself:cloud":        *cloud,
		"witself:profile":      *profile,
		"witself:ingress":      *ingress,
		"witself:cidr":         *cidr,
		"witself:accountAlias": *accountAlias,
		"witself:role":         *role,
		"aws:region":           *region,
	} {
		if err := stack.SetConfig(ctx, k, auto.ConfigValue{Value: v}); err != nil {
			return fmt.Errorf("set config %s: %w", k, err)
		}
	}

	switch cmd {
	case "up":
		_, err = stack.Up(ctx, optup.ProgressStreams(os.Stdout))
	case "preview":
		_, err = stack.Preview(ctx, optpreview.ProgressStreams(os.Stdout))
	case "destroy":
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

func defaultStateDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".witself-infra-state"
	}
	return filepath.Join(home, ".witself-infra", "state")
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
