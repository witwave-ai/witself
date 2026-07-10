package main

// witself-infra config init|add-cell|show — manage infra.yaml without
// hand-editing YAML. add-cell translates the exact flags an operator
// already knows into a config entry, so migrating a cell is: rerun
// your usual `up` flags once with `config add-cell` in front.

import (
	"flag"
	"fmt"
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
	// Same validations run() applies — add-cell must never record an
	// entry that its own suggested `preview -cell` command would reject.
	if !clouds[get("cloud")] {
		return fmt.Errorf("unknown -cloud %q (want aws|gcp|azure)", get("cloud"))
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
	cellName := strings.Join([]string{get("cloud"), get("account-alias"), regionCode, get("role")}, "-")

	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })

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
	str("ingress", &entry.Ingress)
	str("domain", &entry.Domain)
	str("bootstrap-token-file", &entry.BootstrapTokenFile)
	// Backend is ALWAYS recorded, explicit or not: it addresses WHICH
	// stack state operations target, so an entry must be self-contained
	// — an implicit s3 falling back to some ambient default later could
	// point destroy at a different (empty) stack than the cell's real one.
	backendVal := get("backend")
	entry.Backend = &backendVal
	str("state-dir", &entry.StateDir)
	str("control-plane", &entry.ControlPlane)
	str("fleet-token-file", &entry.FleetTokenFile)
	if explicit["argocd"] {
		v := get("argocd") == "true"
		entry.ArgoCD = &v
	}
	if explicit["gitops-repo"] || explicit["gitops-path"] || explicit["gitops-values-path"] || explicit["gitops-revision"] {
		g := &gitopsEntry{}
		str("gitops-repo", &g.Repo)
		str("gitops-path", &g.Path)
		str("gitops-values-path", &g.ValuesPath)
		str("gitops-revision", &g.Revision)
		entry.Gitops = g
	}
	if explicit["aws-profile"] || explicit["gcp-project"] || explicit["azure-subscription"] {
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
		entry.SecurityContext = sc
	}

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
