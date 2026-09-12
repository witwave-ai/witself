package gitopsvalues

import (
	"fmt"
	"strings"
)

// regionCodes maps provider-native regions used by the current fleet onto the
// short token in the composed cell name. Keep this aligned with
// witself-infra's regions catalog plus its legacyRegionCodes table.
var regionCodes = map[string]string{
	"us-east-1": "use1",
	"us-west-2": "usw2",
	"eastus2":   "use2",
	"westus2":   "usw2",
	"us-east1":  "use1",
	"us-west2":  "usw2",
	"nyc1":      "use1",
	"phx1":      "usw2",
}

func resolveCell(name string, cfg *Catalog) (templateData, error) {
	entry, ok := cfg.Cells[name]
	if !ok {
		return templateData{}, fmt.Errorf("cell %q is not in %s", name, catalogRelPath)
	}
	if entry.Cloud == "" {
		return templateData{}, fmt.Errorf("cell %q: cloud is required", name)
	}
	if entry.Region == "" {
		return templateData{}, fmt.Errorf("cell %q: region is required", name)
	}
	alias := entry.AccountAlias
	if alias == "" {
		alias = cfg.Defaults.AccountAlias
	}
	if alias == "" {
		return templateData{}, fmt.Errorf("cell %q: account_alias is required", name)
	}
	role := entry.Role
	if role == "" {
		role = cfg.Defaults.Role
	}
	if role == "" {
		return templateData{}, fmt.Errorf("cell %q: role is required", name)
	}
	code, ok := regionCodes[entry.Region]
	if !ok {
		return templateData{}, fmt.Errorf("cell %q: unknown region %q for composed-name check; add it to gitopsvalues.regionCodes", name, entry.Region)
	}
	composed := strings.Join([]string{entry.Cloud, alias, code, role}, "-")
	if composed != name {
		return templateData{}, fmt.Errorf("cell %q does not match identity (cloud/account_alias/region/role compose to %q)", name, composed)
	}
	parent := cfg.Defaults.DomainParent
	domain := entry.Domain
	if domain == "" {
		domain = name + "." + parent
	}
	apiHost := entry.APIHost
	if apiHost == "" {
		apiHost = "api." + name + "." + parent
	}
	return templateData{
		Name:                    name,
		Cloud:                   entry.Cloud,
		AccountAlias:            alias,
		Region:                  entry.Region,
		Role:                    role,
		Domain:                  domain,
		APIHost:                 apiHost,
		RepoURL:                 cfg.Gitops.RepoURL,
		TargetRevision:          cfg.Gitops.TargetRevision,
		GCPProject:              entry.GCPProject,
		ACMEEmail:               entry.ACMEEmail,
		AWSZoneType:             entry.Switches.AWSZoneType,
		GCPManagedHA:            entry.Switches.GCPManagedHA,
		GCPWorkerJobs:           entry.Switches.GCPWorkerJobs,
		GCPFactDeletion:         entry.Switches.GCPFactDeletion,
		GCPAvatarCompaction:     entry.Switches.GCPAvatarCompaction,
		GCPAgentEmailDark:       entry.Switches.GCPAgentEmailDark,
		DomainDocumentationOnly: entry.Switches.DomainDocumentationOnly,
		Monitoring:              entry.Switches.Monitoring,
		CollectorAlerts:         entry.Switches.CollectorAlerts,
		overlayName:             entry.Overlay,
	}, nil
}
