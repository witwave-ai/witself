package main

// `witself-infra whoami -cell X` — the pre-flight identity check.
// Every cell operation runs as SOME identity in SOME cloud account,
// and a wrong one silently provisions to the wrong place. whoami
// resolves the security context, calls the cloud's identity API, and
// (when the config pins expected_account_id or tenant) refuses to
// return success unless the runtime identity matches.
//
// The provisioning verbs (up / preview / destroy / refresh /
// bootstrap) run the same check before touching any cloud state, so
// an operator can never sink 20 minutes of EKS creation into the
// wrong account.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/witwave-ai/witself/infra/pulumi/internal/backend"
)

// identity is what one cloud identity call returns.
type identity struct {
	Cloud   string // aws | gcp | azure
	Profile string // aws profile, gcp project, azure subscription id
	Account string // aws account id, gcp project, azure subscription id
	Tenant  string // azure only
	Actor   string // arn / gcloud active account / azure user
	OK      bool   // does the runtime identity match the config pin?
	Notes   []string
}

// whoamiCell resolves the security context for a cell and calls the
// cloud identity API. It returns the runtime identity and any
// mismatch against expected_account_id / tenant pins.
func whoamiCell(ctx context.Context, cellName, configPath string) (identity, error) {
	return whoamiCellCore(ctx, cellName, configPath, true)
}

// whoamiCellSilent skips the interactive SSO refresh — the dashboard's
// hot loop uses this so 60s auto-refresh doesn't spawn browser logins
// on top of the altscreen.
func whoamiCellSilent(ctx context.Context, cellName, configPath string) (identity, error) {
	return whoamiCellCore(ctx, cellName, configPath, false)
}

func whoamiCellCore(ctx context.Context, cellName, configPath string, bgSSO bool) (identity, error) {
	cfg, _, err := loadInfraConfig(configPath)
	if err != nil {
		return identity{}, err
	}
	entry, ok := cfg.Cells[cellName]
	if !ok {
		return identity{}, fmt.Errorf("cell %q not in config", cellName)
	}
	cloud := "aws"
	if entry.Cloud != nil {
		cloud = *entry.Cloud
	}
	switch cloud {
	case "aws":
		return whoamiAWSCore(ctx, entry, bgSSO)
	case "gcp":
		return whoamiGCP(ctx, entry)
	case "azure":
		return whoamiAzure(ctx, entry)
	}
	return identity{}, fmt.Errorf("unknown cloud %q", cloud)
}

func awsProfileFor(entry cellEntry) string {
	if entry.SecurityContext != nil && entry.SecurityContext.AWS != nil && entry.SecurityContext.AWS.Profile != nil {
		return *entry.SecurityContext.AWS.Profile
	}
	return ""
}

// whoamiAWSCore resolves the runtime AWS identity and compares
// against the expected_account_id pin. bgSSO reports whether to
// refresh a stale SSO session — the DASHBOARD path passes false
// because it loops over every cell every 60s and firing `aws sso
// login` inside a bubbletea altscreen would paint the browser's
// launch banner over the TUI and contend for stdin. The command-line
// whoami and the provisioning pre-flights pass true (interactive
// one-shot).
func whoamiAWSCore(ctx context.Context, entry cellEntry, bgSSO bool) (identity, error) {
	profile := awsProfileFor(entry)
	if bgSSO {
		backend.EnsureAWSSession(ctx, profile)
	}
	opts := []func(*awsconfig.LoadOptions) error{}
	if profile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(profile))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return identity{}, fmt.Errorf("load AWS config for profile %q: %w", profile, err)
	}
	out, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return identity{}, fmt.Errorf("aws sts:GetCallerIdentity: %w — pass -aws-profile or run `aws sso login`", err)
	}
	id := identity{Cloud: "aws", Profile: profile, Account: strDeref(out.Account), Actor: strDeref(out.Arn), OK: true}
	// Pin check: whoami's whole reason for existing. A wrong profile
	// that resolves to a different account must fail LOUDLY here, not
	// silently create a fresh backend elsewhere.
	if entry.SecurityContext != nil && entry.SecurityContext.AWS != nil && entry.SecurityContext.AWS.ExpectedAccountID != nil {
		want := strings.TrimSpace(*entry.SecurityContext.AWS.ExpectedAccountID)
		if want != id.Account {
			id.OK = false
			id.Notes = append(id.Notes, fmt.Sprintf("expected AWS account %s, but the resolved profile identifies as %s", want, id.Account))
		}
	}
	return id, nil
}

func whoamiGCP(ctx context.Context, entry cellEntry) (identity, error) {
	project := ""
	credsFile := ""
	if sc := entry.SecurityContext; sc != nil && sc.GCP != nil {
		if sc.GCP.Project != nil {
			project = *sc.GCP.Project
		}
		if sc.GCP.CredentialsFile != nil {
			credsFile = *sc.GCP.CredentialsFile
		}
	}
	// Query gcloud with GOOGLE_APPLICATION_CREDENTIALS set for JUST
	// this call — never os.Setenv, which would leak across cells in
	// the dashboard's load loop and into every subprocess spawned by
	// startOp (they inherit the parent's env). Same footgun class the
	// slice explicitly closed for `az account set`.
	cmd := exec.CommandContext(ctx, "gcloud", "auth", "list", "--filter=status:ACTIVE", "--format=value(account)")
	if credsFile != "" {
		cmd.Env = append(os.Environ(), "GOOGLE_APPLICATION_CREDENTIALS="+credsFile)
	}
	out, err := cmd.Output()
	if err != nil {
		return identity{}, fmt.Errorf("gcloud auth list: %w — run `gcloud auth application-default login --project %s`", err, project)
	}
	actor := strings.TrimSpace(string(out))
	id := identity{Cloud: "gcp", Profile: project, Account: project, Actor: actor, OK: true}
	if actor == "" {
		id.OK = false
		id.Notes = append(id.Notes, "no active gcloud account — run `gcloud auth application-default login`")
	}
	if credsFile != "" {
		id.Notes = append(id.Notes, "using credentials from "+credsFile)
	}
	return id, nil
}

func whoamiAzure(ctx context.Context, entry cellEntry) (identity, error) {
	subscription := ""
	tenant := ""
	if sc := entry.SecurityContext; sc != nil && sc.Azure != nil {
		if sc.Azure.Subscription != nil {
			subscription = *sc.Azure.Subscription
		}
		if sc.Azure.Tenant != nil {
			tenant = *sc.Azure.Tenant
		}
	}
	// Use az directly (backend/azure.go's currentAzureAccount is
	// unexported by design — this call needs the raw tenant too).
	args := []string{"account", "show", "-o", "json"}
	if subscription != "" {
		args = []string{"account", "show", "--subscription", subscription, "-o", "json"}
	}
	raw, err := exec.CommandContext(ctx, "az", args...).Output()
	if err != nil {
		return identity{}, fmt.Errorf("az %s: %w — run `az login --tenant %s`", strings.Join(args, " "), err, tenant)
	}
	var acct struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		TenantID string `json:"tenantId"`
		User     struct {
			Name string `json:"name"`
		} `json:"user"`
	}
	if err := json.Unmarshal(raw, &acct); err != nil {
		return identity{}, fmt.Errorf("parse az account show: %w", err)
	}
	id := identity{Cloud: "azure", Profile: subscription, Account: acct.ID, Tenant: acct.TenantID, Actor: acct.User.Name, OK: true}
	if tenant != "" && !strings.EqualFold(tenant, acct.TenantID) {
		id.OK = false
		id.Notes = append(id.Notes, fmt.Sprintf("expected Azure tenant %s, got %s — run `az login --tenant %s`", tenant, acct.TenantID, tenant))
	}
	return id, nil
}

// runWhoami is the `whoami` subcommand.
func runWhoami(fs *flag.FlagSet) error {
	cellName := fs.Lookup("cell").Value.String()
	configPath := fs.Lookup("config").Value.String()
	if cellName == "" {
		return fmt.Errorf("whoami requires -cell")
	}
	// whoamiCell reloads the config itself; this pass exists for its
	// VALIDATION side effects — unknown-cell listing, identity-flag
	// conflicts, and the key↔composition check — so whoami reports
	// config problems the same way the provisioning verbs would.
	if err := applyCellConfig(fs, cellName, configPath); err != nil {
		return err
	}
	id, err := whoamiCell(context.Background(), cellName, configPath)
	if err != nil {
		return err
	}
	fmt.Println("cell:    " + cellName)
	fmt.Println("cloud:   " + id.Cloud)
	if id.Profile != "" {
		fmt.Println("profile: " + id.Profile)
	}
	fmt.Println("account: " + id.Account)
	if id.Tenant != "" {
		fmt.Println("tenant:  " + id.Tenant)
	}
	if id.Actor != "" {
		fmt.Println("actor:   " + id.Actor)
	}
	for _, n := range id.Notes {
		fmt.Fprintln(os.Stderr, "note:    "+n)
	}
	if !id.OK {
		return fmt.Errorf("identity does not match the config pin — see notes above")
	}
	fmt.Println("ok")
	return nil
}

// requireIdentityMatch is the pre-flight for provisioning verbs. It
// runs whoami and refuses to proceed on a mismatch. Only called when a
// cell is resolved from config (bare-flag invocations preserve the old
// zero-safety-net behavior for parity — the safety net is opt-in via
// the config file, and the config is the surface Slice 1 introduced).
func requireIdentityMatch(ctx context.Context, cellName, configPath string) error {
	id, err := whoamiCell(ctx, cellName, configPath)
	if err != nil {
		return err
	}
	if !id.OK {
		return fmt.Errorf("refusing to run: cell %q identity check failed — %s", cellName, strings.Join(id.Notes, "; "))
	}
	return nil
}

func strDeref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
