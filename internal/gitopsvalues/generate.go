package gitopsvalues

import (
	"bytes"
	"embed"
	"fmt"
	"path/filepath"
	"sort"
	"text/template"
)

//go:embed templates/*.tmpl overlays/*.tmpl
var embedded embed.FS

type templateData struct {
	Name                        string
	Cloud                       string
	AccountAlias                string
	Region                      string
	Role                        string
	Domain                      string
	APIHost                     string
	RepoURL                     string
	TargetRevision              string
	ChartVersion                string
	ImageTag                    string
	CertManagerChartVersion     string
	ExternalDNSChartVersion     string
	ExternalSecretsChartVersion string
	KEDAChartVersion            string
	MetricsServerChartVersion   string
	CivoPostgresChartVersion    string
	GCPProject                  string
	ACMEEmail                   string
	AWSZoneType                 string
	GCPManagedHA                bool
	GCPWorkerJobs               bool
	GCPFactDeletion             bool
	GCPAvatarCompaction         bool
	GCPAgentEmailDark           bool
	DomainDocumentationOnly     bool
	Monitoring                  bool
	CollectorAlerts             bool
	Overlay                     string
	overlayName                 string
}

var templates = mustParseTemplates()

func mustParseTemplates() *template.Template {
	t := template.Must(template.New("gitops-cell-values").ParseFS(embedded, "templates/*.tmpl", "overlays/*.tmpl"))
	t.Option("missingkey=error")
	return t
}

func generateCell(root string, name string, cfg *Catalog, charts chartPins) ([]byte, error) {
	data, err := resolveCell(name, cfg)
	if err != nil {
		return nil, err
	}
	valuesPath := filepath.Join(root, cellsDirRel, name, valuesFileName)
	data.ChartVersion, data.ImageTag, err = readServerPins(valuesPath, charts)
	if err != nil {
		return nil, err
	}
	data.CertManagerChartVersion = charts.CertManager
	data.ExternalDNSChartVersion = charts.ExternalDNS
	data.ExternalSecretsChartVersion = charts.ExternalSecrets
	data.KEDAChartVersion = charts.KEDA
	data.MetricsServerChartVersion = charts.MetricsServer
	data.CivoPostgresChartVersion = charts.CivoPostgres

	family := data.Cloud
	switch family {
	case "aws", "azure", "gcp", "civo":
	default:
		return nil, fmt.Errorf("cell %q: unsupported cloud %q", name, family)
	}

	if data.overlayName != "" {
		overlayTmpl := templates.Lookup(data.overlayName)
		if overlayTmpl == nil {
			return nil, fmt.Errorf("cell %q: overlay template %q not found", name, data.overlayName)
		}
		var overlay bytes.Buffer
		if err := overlayTmpl.Execute(&overlay, data); err != nil {
			return nil, fmt.Errorf("cell %q overlay: %w", name, err)
		}
		data.Overlay = overlay.String()
	}

	tmpl := templates.Lookup(family + ".yaml.tmpl")
	if tmpl == nil {
		return nil, fmt.Errorf("cell %q: missing %s family template", name, family)
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, data); err != nil {
		return nil, fmt.Errorf("cell %q: %w", name, err)
	}
	return out.Bytes(), nil
}

func generateAll(root string) (map[string][]byte, error) {
	cfg, err := loadCatalog(root)
	if err != nil {
		return nil, err
	}
	dirs, err := listCellDirs(root)
	if err != nil {
		return nil, err
	}
	if err := reconcileCatalogAndDirs(cfg, dirs); err != nil {
		return nil, err
	}
	charts, err := loadChartPins(root)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(cfg.Cells))
	for name := range cfg.Cells {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make(map[string][]byte, len(names))
	for _, name := range names {
		body, err := generateCell(root, name, cfg, charts)
		if err != nil {
			return nil, err
		}
		out[name] = body
	}
	return out, nil
}

func valuesRel(cell string) string {
	return filepath.ToSlash(filepath.Join(cellsDirRel, cell, valuesFileName))
}
