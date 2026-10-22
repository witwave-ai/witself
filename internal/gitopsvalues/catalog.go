package gitopsvalues

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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
	Cloud        string `yaml:"cloud"`
	AccountAlias string `yaml:"account_alias,omitempty"`
	Region       string `yaml:"region"`
	Role         string `yaml:"role,omitempty"`
	Domain       string `yaml:"domain,omitempty"`
	APIHost      string `yaml:"api_host,omitempty"`
	GCPProject   string `yaml:"gcp_project,omitempty"`
	ACMEEmail    string `yaml:"acme_email,omitempty"`
	Overlay      string `yaml:"overlay,omitempty"`
	// Unprovisioned records an onboarded cell that has never been provisioned.
	// Only such a cell may take scripts/roll-cell.sh --first-sync. It is not
	// rendered into values; absent means provisioned.
	Unprovisioned bool     `yaml:"unprovisioned,omitempty"`
	Switches      Switches `yaml:"switches"`
}

// RequestBudgetSwitch presence turns the server's per-account request budget on
// for this cell. Remove the block to turn it off. Values are per replica. The
// five top-level values apply to every plan without its own entry under plans.
type RequestBudgetSwitch struct {
	ReadPerMinute      int                          `yaml:"read_per_minute"`
	ReadBurst          int                          `yaml:"read_burst"`
	WritePerMinute     int                          `yaml:"write_per_minute"`
	WriteBurst         int                          `yaml:"write_burst"`
	MaxTrackedAccounts int                          `yaml:"max_tracked_accounts"`
	Plans              map[string]RequestBudgetTier `yaml:"plans,omitempty"`
}

// RequestBudgetTier overrides the four rate values for one plan.
type RequestBudgetTier struct {
	ReadPerMinute  int `yaml:"read_per_minute"`
	ReadBurst      int `yaml:"read_burst"`
	WritePerMinute int `yaml:"write_per_minute"`
	WriteBurst     int `yaml:"write_burst"`
}

var requestBudgetPlanID = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// Switches are per-cell enablement flags that select generated blocks.
// Large unique YAML still lives in overlay templates.
type Switches struct {
	AWSZoneType             string               `yaml:"aws_zone_type,omitempty"`
	DomainDocumentationOnly bool                 `yaml:"domain_documentation_only,omitempty"`
	GCPManagedHA            bool                 `yaml:"gcp_managed_ha,omitempty"`
	GCPWorkerJobs           bool                 `yaml:"gcp_worker_jobs,omitempty"`
	GCPFactDeletion         bool                 `yaml:"gcp_fact_deletion,omitempty"`
	GCPAvatarCompaction     bool                 `yaml:"gcp_avatar_compaction,omitempty"`
	GCPAgentEmailDark       bool                 `yaml:"gcp_agent_email_dark,omitempty"`
	Monitoring              bool                 `yaml:"monitoring,omitempty"`
	CollectorAlerts         bool                 `yaml:"collector_alerts,omitempty"`
	SealedPlaneAlerts       bool                 `yaml:"sealed_plane_alerts,omitempty"`
	MemoryAlerts            bool                 `yaml:"memory_alerts,omitempty"`
	ControlPlaneAlerts      bool                 `yaml:"control_plane_alerts,omitempty"`
	PostgresBackup          bool                 `yaml:"postgres_backup,omitempty"`
	RequestBudget           *RequestBudgetSwitch `yaml:"request_budget,omitempty"`
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
	var probe struct {
		Cells map[string]struct {
			Switches map[string]any `yaml:"switches"`
		} `yaml:"cells"`
	}
	if err := yaml.Unmarshal(raw, &probe); err != nil {
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
	names := make([]string, 0, len(cfg.Cells))
	for name := range cfg.Cells {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		cell := cfg.Cells[name]
		if cell.Unprovisioned && cell.Switches.DomainDocumentationOnly {
			return nil, fmt.Errorf("%s: cell %q sets unprovisioned with domain_documentation_only; an unprovisioned cell needs an authoritative API host", path, name)
		}
		budget := cell.Switches.RequestBudget
		if _, present := probe.Cells[name].Switches["request_budget"]; present && budget == nil {
			return nil, fmt.Errorf("%s: cell %q switches.request_budget must be a mapping of five integers; remove the key to turn it off", path, name)
		}
		if budget == nil {
			continue
		}
		if cell.Overlay == "" {
			return nil, fmt.Errorf("%s: cell %q sets switches.request_budget without an overlay; only overlay templates render it", path, name)
		}
		// yaml.v3 converts float scalars to int by truncation, even with
		// KnownFields enabled. The untyped probe preserves their scalar type.
		budgetProbe, _ := probe.Cells[name].Switches["request_budget"].(map[string]any)
		for _, rate := range []struct {
			key   string
			value int
		}{
			{"read_per_minute", budget.ReadPerMinute},
			{"read_burst", budget.ReadBurst},
			{"write_per_minute", budget.WritePerMinute},
			{"write_burst", budget.WriteBurst},
		} {
			if _, nonInteger := budgetProbe[rate.key].(float64); nonInteger {
				return nil, fmt.Errorf("parse %s: cannot unmarshal !!float into int for cell %q switches.request_budget.%s", path, name, rate.key)
			}
			if rate.value < 1 || rate.value > 1000000 {
				return nil, fmt.Errorf("%s: cell %q switches.request_budget.%s must be an integer from 1 to 1000000", path, name, rate.key)
			}
		}
		if _, nonInteger := budgetProbe["max_tracked_accounts"].(float64); nonInteger {
			return nil, fmt.Errorf("parse %s: cannot unmarshal !!float into int for cell %q switches.request_budget.max_tracked_accounts", path, name)
		}
		if budget.MaxTrackedAccounts < 100 || budget.MaxTrackedAccounts > 1000000 {
			return nil, fmt.Errorf("%s: cell %q switches.request_budget.max_tracked_accounts must be an integer from 100 to 1000000", path, name)
		}
	}
	if err := validateRequestBudgetPlans(path, raw); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// validateRequestBudgetPlans validates raw plan nodes after strict decoding and
// the existing catalog checks. Scalar tags retain floats truncated by the typed
// decoder and non-string plan ids that decoding into a string-keyed map can omit.
func validateRequestBudgetPlans(path string, raw []byte) error {
	var probe struct {
		Cells map[string]struct {
			Switches map[string]yaml.Node `yaml:"switches"`
		} `yaml:"cells"`
	}
	// The branches below that return an error instead of skipping are
	// unreachable after the strict decode in loadCatalog; they fail closed so
	// a future decoder change can never silently skip plan validation.
	if err := yaml.Unmarshal(raw, &probe); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	names := make([]string, 0, len(probe.Cells))
	for name := range probe.Cells {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		budgetNode := probe.Cells[name].Switches["request_budget"]
		var budget map[string]yaml.Node
		if err := budgetNode.Decode(&budget); err != nil {
			continue
		}
		plansNode, present := budget["plans"]
		if !present {
			continue
		}
		for plansNode.Kind == yaml.AliasNode {
			plansNode = *plansNode.Alias
		}
		if count := len(plansNode.Content) / 2; count == 0 || count > 16 {
			return fmt.Errorf("%s: cell %q switches.request_budget.plans must map 1 to 16 plan ids to budgets; remove the key to use the cell-wide values for every plan", path, name)
		}
		ids := make([]int, 0, len(plansNode.Content)/2)
		for i := 0; i < len(plansNode.Content); i += 2 {
			ids = append(ids, i)
		}
		sort.Slice(ids, func(i, j int) bool {
			return plansNode.Content[ids[i]].Value < plansNode.Content[ids[j]].Value
		})
		for _, i := range ids {
			idNode := plansNode.Content[i]
			id := idNode.Value
			if idNode.Kind == yaml.AliasNode {
				id = "*" + id
			}
			if idNode.Kind != yaml.ScalarNode || idNode.ShortTag() != "!!str" || !requestBudgetPlanID.MatchString(id) {
				return fmt.Errorf("%s: cell %q switches.request_budget.plans has plan id %q; plan ids are 1-32 lowercase letters, digits or underscores starting with a letter", path, name, id)
			}
			tierNode := plansNode.Content[i+1]
			var tier map[string]yaml.Node
			if err := tierNode.Decode(&tier); err != nil {
				return fmt.Errorf("%s: cell %q switches.request_budget.plans.%s must map read_per_minute, read_burst, write_per_minute and write_burst to integers", path, name, id)
			}
			for key := range tier {
				switch key {
				case "read_per_minute", "read_burst", "write_per_minute", "write_burst":
				default:
					return fmt.Errorf("%s: cell %q switches.request_budget.plans.%s has unknown key %q", path, name, id, key)
				}
			}
			for _, key := range []string{"read_per_minute", "read_burst", "write_per_minute", "write_burst"} {
				rate, present := tier[key]
				if rate.ShortTag() == "!!float" {
					return fmt.Errorf("parse %s: cannot unmarshal !!float into int for cell %q switches.request_budget.plans.%s.%s", path, name, id, key)
				}
				var value int
				if present {
					if err := rate.Decode(&value); err != nil && rate.ShortTag() != "!!int" {
						return fmt.Errorf("%s: cell %q switches.request_budget.plans.%s.%s must be an integer from 1 to 1000000", path, name, id, key)
					}
				}
				if value < 1 || value > 1000000 {
					return fmt.Errorf("%s: cell %q switches.request_budget.plans.%s.%s must be an integer from 1 to 1000000", path, name, id, key)
				}
			}
		}
	}
	return nil
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
