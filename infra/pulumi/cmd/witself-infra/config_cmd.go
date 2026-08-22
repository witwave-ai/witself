package main

// witself-infra config init|add-cell|show — manage infra.yaml without
// hand-editing YAML. add-cell translates the exact flags an operator
// already knows into a config entry, so migrating a cell is: rerun
// your usual `up` flags once with `config add-cell` in front.

import (
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const configSkeleton = `# witself-infra cell inventory.
# Precedence: explicit flag > cell entry > defaults > built-in default.
# References only — profile names, subscription/project IDs, token file
# PATHS. Never paste credential values here; the loader rejects them.
version: 1

defaults:
  # control_plane: https://self.witwave.ai
  # channel: stable
  # profile: minimal
  # gitops:
  #   repo: https://github.com/witwave-ai/witself
  #   revision: main

cells: {}
  # aws-sandbox-usw2-dev:
  #   cloud: aws
  #   account_alias: sandbox
  #   region: us-west-2
  #   role: dev
  #   argocd: true
  #   security_context:
  #     aws:
  #       profile: witwave-sandbox
  #
  # civo-sandbox-use1-dev:
  #   cloud: civo
  #   account_alias: sandbox
  #   region: nyc1
  #   role: dev
  #   backend: local
  #   civo_node_size: g4s.kube.medium
  #   civo_admin_cidr: 203.0.113.7/32
  #   backup_validation_target: true
  #   argocd: true
  #   security_context:
  #     civo:
  #       token_file: /secure/path/civo.token
  #       expected_account_id: 00000000-0000-0000-0000-000000000000
`

func runConfigCmd(sub string, fs *flag.FlagSet, configPath string) error {
	switch sub {
	case "init":
		return configInit(configPath)
	case "add-cell":
		return configAddCell(fs, configPath)
	case "show":
		return configShow(fs, configPath)
	default:
		return fmt.Errorf("unknown config subcommand %q (want init|add-cell|show)", sub)
	}
}

func configInit(configPath string) error {
	path, err := resolveConfigPath(configPath)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists — not overwriting", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(configSkeleton), 0o600); err != nil {
		return err
	}
	fmt.Println("wrote " + path)
	return nil
}

// configAddCell turns the explicitly-passed per-cell flags into a
// config entry. Only flags the operator actually typed are recorded
// (so the file captures intent, not noise) — except identity and
// backend, which must be self-contained per entry. Round-tripping
// through the struct drops YAML comments; a note says so whenever an
// existing file is rewritten.
func configAddCell(fs *flag.FlagSet, configPath string) error {
	get := func(name string) string { return fs.Lookup(name).Value.String() }
	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })

	path, err := resolveConfigPath(configPath)
	if err != nil {
		return err
	}
	cfg := &infraConfig{Version: 1, Cells: map[string]cellEntry{}}
	existed := false
	if _, err := os.Stat(path); err == nil {
		existed = true
		cfg, _, err = loadInfraConfig(path)
		if err != nil {
			return err
		}
	}
	defaultValues := cfg.Defaults.flagValues()
	effective := func(name string) string {
		if !explicit[name] {
			if value, ok := defaultValues[name]; ok {
				return value
			}
		}
		return get(name)
	}

	// Same validations run() applies — add-cell must never record an
	// entry that its own suggested `preview -cell` command would reject.
	if !clouds[get("cloud")] {
		return fmt.Errorf("unknown -cloud %q (want aws|gcp|azure|civo)", get("cloud"))
	}
	regionCode, _, ok := resolveRegionCode(get("cloud"), get("region"))
	if !ok {
		return fmt.Errorf("unknown -region %q for -cloud %s", get("region"), get("cloud"))
	}
	if !label.MatchString(get("account-alias")) {
		return fmt.Errorf("-account-alias %q must be lowercase alphanumeric/hyphen", get("account-alias"))
	}
	if !label.MatchString(get("role")) {
		return fmt.Errorf("-role %q must be lowercase alphanumeric/hyphen", get("role"))
	}
	if get("cloud") == "civo" {
		for _, ignored := range []string{"cidr", "db-version", "domain"} {
			if explicit[ignored] {
				return fmt.Errorf("-%s does not apply to -cloud civo", ignored)
			}
		}
		if effective("backend") != "local" {
			return fmt.Errorf("-cloud civo requires -backend local")
		}
		if !civoProfiles[effective("profile")] {
			return fmt.Errorf("-cloud civo supports only -profile minimal or prod")
		}
		if effective("channel") != "experimental" {
			return fmt.Errorf("-cloud civo currently supports only -channel experimental")
		}
		if effective("control-plane") != "" && effective("argocd") != "true" {
			return fmt.Errorf("-cloud civo with -control-plane requires -argocd")
		}
		if strings.TrimSpace(effective("civo-admin-cidr")) == "" {
			return fmt.Errorf("-civo-admin-cidr is required with -cloud civo")
		}
		if _, _, err := net.ParseCIDR(effective("civo-admin-cidr")); err != nil {
			return fmt.Errorf("-civo-admin-cidr %q is not a valid CIDR", effective("civo-admin-cidr"))
		}
		if tokenFile := strings.TrimSpace(effective("civo-token-file")); tokenFile != "" {
			if _, err := resolveCivoToken(tokenFile); err != nil {
				return err
			}
		}
	}
	if effective("backup-validation-target") == "true" &&
		strings.TrimSpace(effective("control-plane")) == "" {
		return fmt.Errorf("-backup-validation-target requires -control-plane so isolation is enforced by the fleet registry")
	}
	cellName := strings.Join([]string{get("cloud"), get("account-alias"), regionCode, get("role")}, "-")

	entry := cellEntry{}
	str := func(name string, dst **string) {
		if explicit[name] {
			v := get(name)
			*dst = &v
		}
	}
	// Identity is always recorded — the entry must be self-contained.
	for _, pair := range []struct {
		name string
		dst  **string
	}{
		{"cloud", &entry.Cloud}, {"account-alias", &entry.AccountAlias},
		{"region", &entry.Region}, {"role", &entry.Role},
	} {
		v := get(pair.name)
		*pair.dst = &v
	}
	str("channel", &entry.Channel)
	str("profile", &entry.Profile)
	str("cidr", &entry.CIDR)
	str("k8s-version", &entry.K8sVersion)
	str("db-version", &entry.DBVersion)
	str("domain", &entry.Domain)
	str("bootstrap-token-file", &entry.BootstrapTokenFile)
	str("civo-node-size", &entry.CivoNodeSize)
	str("civo-admin-cidr", &entry.CivoAdminCIDR)
	// Backend is ALWAYS recorded, explicit or not: it addresses WHICH
	// stack state operations target, so an entry must be self-contained
	// — an implicit s3 falling back to some ambient default later could
	// point destroy at a different (empty) stack than the cell's real one.
	backendVal := effective("backend")
	entry.Backend = &backendVal
	str("state-dir", &entry.StateDir)
	str("control-plane", &entry.ControlPlane)
	str("fleet-token-file", &entry.FleetTokenFile)
	if explicit["argocd"] {
		v := get("argocd") == "true"
		entry.ArgoCD = &v
	}
	if explicit["backup-validation-target"] {
		v := get("backup-validation-target") == "true"
		entry.BackupValidationTarget = &v
	}
	if explicit["gitops-repo"] || explicit["gitops-path"] || explicit["gitops-values-path"] || explicit["gitops-revision"] {
		g := &gitopsEntry{}
		str("gitops-repo", &g.Repo)
		str("gitops-path", &g.Path)
		str("gitops-values-path", &g.ValuesPath)
		str("gitops-revision", &g.Revision)
		entry.Gitops = g
	}
	if explicit["aws-profile"] || explicit["gcp-project"] || explicit["azure-subscription"] || explicit["civo-token-file"] || explicit["civo-expected-account-id"] {
		sc := &securityContext{}
		if explicit["aws-profile"] {
			v := get("aws-profile")
			sc.AWS = &awsContext{Profile: &v}
		}
		if explicit["gcp-project"] {
			v := get("gcp-project")
			sc.GCP = &gcpContext{Project: &v}
		}
		if explicit["azure-subscription"] {
			v := get("azure-subscription")
			sc.Azure = &azureContext{Subscription: &v}
		}
		if explicit["civo-token-file"] || explicit["civo-expected-account-id"] {
			sc.Civo = &civoContext{}
			if explicit["civo-token-file"] {
				v := get("civo-token-file")
				sc.Civo.TokenFile = &v
			}
			if explicit["civo-expected-account-id"] {
				v := get("civo-expected-account-id")
				sc.Civo.ExpectedAccountID = &v
			}
		}
		entry.SecurityContext = sc
	}

	if cfg.Cells == nil {
		cfg.Cells = map[string]cellEntry{}
	}
	if _, exists := cfg.Cells[cellName]; exists {
		return fmt.Errorf("cell %q already in %s — edit the file to change it", cellName, path)
	}
	cfg.Cells[cellName] = entry
	out, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	// The same paste-guard the loader enforces, applied on the WRITE
	// path: refusing here keeps the credential off disk entirely and
	// the existing inventory loadable — persisting it would both leak
	// the secret and brick every config-touching command.
	for i, line := range strings.Split(string(out), "\n") {
		if secretShapes.MatchString(line) {
			return fmt.Errorf("refusing to write %s: line %d looks like a credential — pass a token file PATH, never a token value", path, i+1)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return err
	}
	fmt.Printf("added cell %s to %s\n", cellName, path)
	if existed {
		fmt.Println("note: add-cell rewrites the file — YAML comments are not preserved")
	}
	fmt.Printf("try: witself-infra preview -cell %s\n", cellName)
	return nil
}

// configShow prints either the inventory summary or, with -cell, the
// effective merged flag values (flag > entry > defaults > built-in)
// exactly as an operation would see them.
func configShow(fs *flag.FlagSet, configPath string) error {
	cellName := fs.Lookup("cell").Value.String()
	if cellName == "" {
		cfg, path, err := loadInfraConfig(configPath)
		if err != nil {
			return err
		}
		fmt.Println("config: " + path)
		names := make([]string, 0, len(cfg.Cells))
		for n := range cfg.Cells {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			e := cfg.Cells[n]
			cloud := ""
			if e.Cloud != nil {
				cloud = *e.Cloud
			}
			fmt.Printf("  %s  (%s)\n", n, cloud)
		}
		if len(names) == 0 {
			fmt.Println("  (no cells — `witself-infra config add-cell ...` to add one)")
		}
		return nil
	}
	if err := applyCellConfig(fs, cellName, configPath); err != nil {
		return err
	}
	if fs.Lookup("k8s-version").Value.String() == "" {
		_ = fs.Set("k8s-version", defaultK8sVersion(fs.Lookup("cloud").Value.String()))
	}
	fmt.Println("effective configuration for " + cellName + ":")
	names := make([]string, 0, 24)
	fs.VisitAll(func(f *flag.Flag) {
		switch f.Name {
		case "cell", "config":
			return
		}
		names = append(names, f.Name)
	})
	sort.Strings(names)
	identity := map[string]bool{}
	for _, n := range identityFlags {
		identity[n] = true
	}
	for _, n := range names {
		f := fs.Lookup(n)
		// Identity always prints (it names the target); everything else
		// only when it differs from the built-in — the signal.
		if !identity[n] && f.Value.String() == f.DefValue {
			continue
		}
		fmt.Printf("  -%s %s\n", n, f.Value.String())
	}
	return nil
}
