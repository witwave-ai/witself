package main

// infra.yaml — the local cell inventory. Every per-cell flag can live
// here instead of being retyped on each invocation:
//
//	witself-infra up -cell aws-sandbox-usw2-dev
//
// Precedence: explicit flag > cell entry > defaults block > built-in
// flag default. Explicitness is detected via flag.Visit, so an
// untouched flag never clobbers a configured value, and every
// existing all-flags invocation keeps working with no config file at
// all.
//
// The file holds POINTERS, never secrets: profile names, subscription
// and project IDs, token file PATHS. The loader hard-rejects anything
// shaped like a real credential so a paste mistake can't persist one.

import (
	"flag"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// witselfHome is the managed root shared with the other witself CLIs:
// $WITSELF_HOME, else ~/.witself (same resolution as the token dirs).
func witselfHome() (string, error) {
	if root := os.Getenv("WITSELF_HOME"); root != "" {
		return root, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".witself"), nil
}

func defaultConfigPath() (string, error) {
	root, err := witselfHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "infra.yaml"), nil
}

// gitopsEntry mirrors the -gitops-* flags.
type gitopsEntry struct {
	Repo       *string `yaml:"repo,omitempty"`
	Path       *string `yaml:"path,omitempty"`
	ValuesPath *string `yaml:"values_path,omitempty"`
	Revision   *string `yaml:"revision,omitempty"`
}

// securityContext names WHICH identity a cell's operations run as —
// references only (profile names, subscription/project IDs), never
// credential material. One shape per cloud.
type securityContext struct {
	AWS *struct {
		Profile *string `yaml:"profile,omitempty"`
	} `yaml:"aws,omitempty"`
	GCP *struct {
		Project *string `yaml:"project,omitempty"`
	} `yaml:"gcp,omitempty"`
	Azure *struct {
		Subscription *string `yaml:"subscription,omitempty"`
	} `yaml:"azure,omitempty"`
}

// cellEntry is one cell's configuration — pointer fields so "absent"
// and "zero value" stay distinguishable in the merge.
type cellEntry struct {
	Cloud        *string `yaml:"cloud,omitempty"`
	AccountAlias *string `yaml:"account_alias,omitempty"`
	Region       *string `yaml:"region,omitempty"`
	Role         *string `yaml:"role,omitempty"`

	Channel            *string          `yaml:"channel,omitempty"`
	Profile            *string          `yaml:"profile,omitempty"`
	CIDR               *string          `yaml:"cidr,omitempty"`
	K8sVersion         *string          `yaml:"k8s_version,omitempty"`
	DBVersion          *string          `yaml:"db_version,omitempty"`
	Ingress            *string          `yaml:"ingress,omitempty"`
	ArgoCD             *bool            `yaml:"argocd,omitempty"`
	Gitops             *gitopsEntry     `yaml:"gitops,omitempty"`
	Domain             *string          `yaml:"domain,omitempty"`
	BootstrapTokenFile *string          `yaml:"bootstrap_token_file,omitempty"`
	Backend            *string          `yaml:"backend,omitempty"`
	StateDir           *string          `yaml:"state_dir,omitempty"`
	ControlPlane       *string          `yaml:"control_plane,omitempty"`
	FleetTokenFile     *string          `yaml:"fleet_token_file,omitempty"`
	SecurityContext    *securityContext `yaml:"security_context,omitempty"`
}

// infraConfig is the whole file.
type infraConfig struct {
	Version  int                  `yaml:"version"`
	Defaults *cellEntry           `yaml:"defaults,omitempty"`
	Cells    map[string]cellEntry `yaml:"cells,omitempty"`
}

// flagValues maps set fields onto their CLI flag names — the single
// source of truth for config↔flag correspondence. Bools stringify for
// flag.Set.
func (e *cellEntry) flagValues() map[string]string {
	if e == nil {
		return nil
	}
	out := map[string]string{}
	set := func(name string, v *string) {
		if v != nil {
			out[name] = *v
		}
	}
	set("cloud", e.Cloud)
	set("account-alias", e.AccountAlias)
	set("region", e.Region)
	set("role", e.Role)
	set("channel", e.Channel)
	set("profile", e.Profile)
	set("cidr", e.CIDR)
	set("k8s-version", e.K8sVersion)
	set("db-version", e.DBVersion)
	set("ingress", e.Ingress)
	set("domain", e.Domain)
	set("bootstrap-token-file", e.BootstrapTokenFile)
	set("backend", e.Backend)
	set("state-dir", e.StateDir)
	set("control-plane", e.ControlPlane)
	set("fleet-token-file", e.FleetTokenFile)
	if e.ArgoCD != nil {
		out["argocd"] = strconv.FormatBool(*e.ArgoCD)
	}
	if g := e.Gitops; g != nil {
		set("gitops-repo", g.Repo)
		set("gitops-path", g.Path)
		set("gitops-values-path", g.ValuesPath)
		set("gitops-revision", g.Revision)
	}
	if sc := e.SecurityContext; sc != nil {
		if sc.AWS != nil {
			set("aws-profile", sc.AWS.Profile)
		}
		if sc.GCP != nil {
			set("gcp-project", sc.GCP.Project)
		}
		if sc.Azure != nil {
			set("azure-subscription", sc.Azure.Subscription)
		}
	}
	return out
}

// identityFlags compose the cell name; with -cell they must come from
// the config entry, never the command line — a conflicting explicit
// flag is a hard error, not a silent winner.
var identityFlags = []string{"cloud", "account-alias", "region", "role"}

// secretShapes are credential patterns that must NEVER appear in
// infra.yaml — the file holds references (names, IDs, paths), and a
// pasted token would otherwise sit in plaintext on disk forever.
var secretShapes = regexp.MustCompile(
	`witself_(boot|opr|agt|flt|prv|adm)_[A-Za-z0-9]` +
		`|-----BEGIN( [A-Z]+)? PRIVATE KEY` +
		`|AKIA[0-9A-Z]{16}` +
		`|gh[posu]_[A-Za-z0-9]{20}` +
		`|github_pat_[A-Za-z0-9_]{20}`)

// loadInfraConfig reads + validates the config. Unknown YAML keys are
// errors (a typo like k8s_verion must not silently fall back to a
// default), and secret-shaped values are rejected with a line number.
func loadInfraConfig(path string) (*infraConfig, string, error) {
	if path == "" {
		var err error
		path, err = defaultConfigPath()
		if err != nil {
			return nil, "", err
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, path, fmt.Errorf("no config file at %s — run `witself-infra config init` to create one", path)
		}
		return nil, path, err
	}
	for i, line := range strings.Split(string(raw), "\n") {
		if secretShapes.MatchString(line) {
			return nil, path, fmt.Errorf("%s:%d looks like a credential — infra.yaml holds names, IDs, and token file PATHS, never secret values", path, i+1)
		}
	}
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	var cfg infraConfig
	if err := dec.Decode(&cfg); err != nil {
		return nil, path, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.Version != 1 {
		return nil, path, fmt.Errorf("%s: unsupported version %d (want 1)", path, cfg.Version)
	}
	if d := cfg.Defaults; d != nil {
		if d.Cloud != nil || d.AccountAlias != nil || d.Region != nil || d.Role != nil {
			return nil, path, fmt.Errorf("%s: defaults must not set cell identity (cloud/account_alias/region/role)", path)
		}
		if d.SecurityContext != nil {
			return nil, path, fmt.Errorf("%s: security_context is per-cell only — a fleet-wide default identity defeats per-cell isolation", path)
		}
		// values_path names ONE cell's values file; inherited, it would
		// bootstrap every cell with that cell's identity (name, hosts,
		// DNS owner) — a cross-cell hijack, not a default.
		if d.Gitops != nil && d.Gitops.ValuesPath != nil {
			return nil, path, fmt.Errorf("%s: defaults must not set gitops.values_path — it is inherently per-cell", path)
		}
		// backend/state_dir select WHICH stack state an operation
		// targets. Inherited, `destroy -cell` could run against a fresh
		// empty stack (reporting success) while the real cell survives.
		if d.Backend != nil || d.StateDir != nil {
			return nil, path, fmt.Errorf("%s: defaults must not set backend/state_dir — stack addressing is per-cell only", path)
		}
	}
	return &cfg, path, nil
}

// applyCellConfig resolves -cell: loads the file, guards against
// identity conflicts, and fills every flag the operator did NOT set
// explicitly from the cell entry (falling back to defaults). It then
// re-composes the cell name from the merged identity and requires it
// to match the entry key — the key IS the Pulumi stack name and fleet
// registry key, so drift here would silently target the wrong stack.
func applyCellConfig(fs *flag.FlagSet, cellName, configPath string) error {
	cfg, path, err := loadInfraConfig(configPath)
	if err != nil {
		return err
	}
	entry, ok := cfg.Cells[cellName]
	if !ok {
		names := make([]string, 0, len(cfg.Cells))
		for n := range cfg.Cells {
			names = append(names, n)
		}
		sort.Strings(names)
		return fmt.Errorf("cell %q not in %s (have: %s)", cellName, path, strings.Join(names, ", "))
	}

	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	for _, name := range identityFlags {
		if explicit[name] {
			return fmt.Errorf("-%s conflicts with -cell: identity comes from the config entry", name)
		}
	}

	merged := map[string]string{}
	maps.Copy(merged, cfg.Defaults.flagValues())
	maps.Copy(merged, entry.flagValues())
	for _, name := range identityFlags {
		if _, ok := merged[strings.ReplaceAll(name, "_", "-")]; !ok {
			return fmt.Errorf("cell %q in %s is missing %s — identity must be complete", cellName, path, name)
		}
	}
	for name, val := range merged {
		if explicit[name] {
			continue // an explicit flag always wins over the file
		}
		if err := fs.Set(name, val); err != nil {
			return fmt.Errorf("config value for -%s (%q): %w", name, val, err)
		}
	}

	// Key ↔ composition consistency.
	get := func(name string) string { return fs.Lookup(name).Value.String() }
	regionCode, _, ok := resolveRegionCode(get("cloud"), get("region"))
	if !ok {
		return fmt.Errorf("cell %q: unknown region %q for cloud %s", cellName, get("region"), get("cloud"))
	}
	composed := strings.Join([]string{get("cloud"), get("account-alias"), regionCode, get("role")}, "-")
	if composed != cellName {
		return fmt.Errorf("cell key %q does not match its identity (cloud/account_alias/region/role compose to %q) — the key is the stack + fleet name and must stay canonical", cellName, composed)
	}
	return nil
}
