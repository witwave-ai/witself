package gitopsvalues

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

const catalogSchema = "witself.gitops-cells.v1"

const (
	catalogRelPath = ".gitops/cells/catalog.yaml"
	cellsDirRel    = ".gitops/cells"
	valuesFileName = "values.yaml"
)

// Catalog is the checked-in fleet cell config used to generate per-cell
// GitOps values overlays.
type Catalog struct {
	Schema   string                `yaml:"schema"`
	Gitops   GitopsDefaults        `yaml:"gitops"`
	Defaults CatalogDefaults       `yaml:"defaults"`
	Cells    map[string]CellConfig `yaml:"cells"`
}

// GitopsDefaults are shared GitOps source pins for child Applications.
type GitopsDefaults struct {
	RepoURL        string `yaml:"repoURL"`
	TargetRevision string `yaml:"targetRevision"`
}

// CatalogDefaults apply when a cell omits identity fields.
type CatalogDefaults struct {
	AccountAlias string `yaml:"account_alias"`
	Role         string `yaml:"role"`
	DomainParent string `yaml:"domain_parent"`
}

// CellConfig is one cell's generation input.
type CellConfig struct {
	Cloud        string   `yaml:"cloud"`
	AccountAlias string   `yaml:"account_alias,omitempty"`
	Region       string   `yaml:"region"`
	Role         string   `yaml:"role,omitempty"`
	Domain       string   `yaml:"domain,omitempty"`
	APIHost      string   `yaml:"api_host,omitempty"`
	GCPProject   string   `yaml:"gcp_project,omitempty"`
	ACMEEmail    string   `yaml:"acme_email,omitempty"`
	Overlay      string   `yaml:"overlay,omitempty"`
	Switches     Switches `yaml:"switches"`
}

// Switches are per-cell enablement flags that select generated blocks.
// Large unique YAML still lives in overlay templates.
type Switches struct {
	AWSZoneType             string `yaml:"aws_zone_type,omitempty"`
	DomainDocumentationOnly bool   `yaml:"domain_documentation_only,omitempty"`
	GCPManagedHA            bool   `yaml:"gcp_managed_ha,omitempty"`
	GCPWorkerJobs           bool   `yaml:"gcp_worker_jobs,omitempty"`
	GCPFactDeletion         bool   `yaml:"gcp_fact_deletion,omitempty"`
	GCPAvatarCompaction     bool   `yaml:"gcp_avatar_compaction,omitempty"`
	GCPAgentEmailDark       bool   `yaml:"gcp_agent_email_dark,omitempty"`
	Monitoring              bool   `yaml:"monitoring,omitempty"`
	CollectorAlerts         bool   `yaml:"collector_alerts,omitempty"`
	SealedPlaneAlerts       bool   `yaml:"sealed_plane_alerts,omitempty"`
}

func loadCatalog(root string) (*Catalog, error) {
	path := filepath.Join(root, catalogRelPath)
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	var cfg Catalog
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.Schema != catalogSchema {
		return nil, fmt.Errorf("%s: unsupported schema %q (want %s)", path, cfg.Schema, catalogSchema)
	}
	if cfg.Gitops.RepoURL == "" || cfg.Gitops.TargetRevision == "" {
		return nil, fmt.Errorf("%s: gitops.repoURL and gitops.targetRevision are required", path)
	}
	if cfg.Defaults.DomainParent == "" {
		return nil, fmt.Errorf("%s: defaults.domain_parent is required", path)
	}
	if len(cfg.Cells) == 0 {
		return nil, fmt.Errorf("%s: cells is empty", path)
	}
	return &cfg, nil
}

func listCellDirs(root string) ([]string, error) {
	dir := filepath.Join(root, cellsDirRel)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if name == "overlays" {
			continue
		}
		values := filepath.Join(dir, name, valuesFileName)
		if _, err := os.Stat(values); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func reconcileCatalogAndDirs(cfg *Catalog, dirs []string) error {
	for _, name := range dirs {
		if _, ok := cfg.Cells[name]; !ok {
			return fmt.Errorf("cell directory %s has %s but is not in %s", name, valuesFileName, catalogRelPath)
		}
	}
	return nil
}
