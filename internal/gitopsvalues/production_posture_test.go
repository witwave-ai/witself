package gitopsvalues

import (
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

type postureField struct {
	name string
	want any
}

// Only scalar comparisons belong in diagnostics, never a decoded values body.
func postureScalar(value any) string {
	switch value.(type) {
	case nil, bool, int, float64, string:
		return fmt.Sprint(value)
	default:
		return "<non-scalar>"
	}
}

func checkPostureFields(prefix string, block map[string]any, fields []postureField) error {
	allowed := make(map[string]bool, len(fields))
	for _, field := range fields {
		allowed[field.name] = true
		if got := block[field.name]; !reflect.DeepEqual(got, field.want) {
			return fmt.Errorf("%s.%s: got %s, want %s", prefix, field.name, postureScalar(got), postureScalar(field.want))
		}
	}
	var extra []string
	for key := range block {
		if !allowed[key] {
			extra = append(extra, key)
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		return fmt.Errorf("%s: unexpected key %s", prefix, extra[0])
	}
	return nil
}

func checkFirstSyncEmailRetention(values map[string]any) error {
	block, ok := productionEmailValue(values, "apps.witselfServer.worker.agentEmailRetention").(map[string]any)
	if !ok {
		return fmt.Errorf("agentEmailRetention: missing")
	}
	return checkPostureFields("agentEmailRetention", block, []postureField{
		{"enabled", true},
		{"mode", "preview"},
		{"batchSize", 100},
		{"interval", "1m"},
		{"batchTimeout", "2m"},
	})
}

func checkProductionServerResources(values map[string]any) error {
	resources, ok := productionEmailValue(values, "apps.witselfServer.resources").(map[string]any)
	if !ok || resources == nil {
		return fmt.Errorf("resources: missing")
	}
	var extra []string
	for key := range resources {
		if key != "requests" && key != "limits" {
			extra = append(extra, key)
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		return fmt.Errorf("resources: unexpected key %s", extra[0])
	}
	requests, _ := resources["requests"].(map[string]any)
	if err := checkPostureFields("resources.requests", requests, []postureField{
		{"cpu", "50m"},
		{"memory", "128Mi"},
	}); err != nil {
		return err
	}
	limits, _ := resources["limits"].(map[string]any)
	return checkPostureFields("resources.limits", limits, []postureField{
		{"memory", "512Mi"},
	})
}

func checkReceiverSecretNames(values map[string]any, pagerduty, deadman string) error {
	if (pagerduty == "") != (deadman == "") {
		return fmt.Errorf("receiver Secrets: both names must be set or empty")
	}
	monitoring, ok := productionEmailValue(values, "platform.monitoring").(map[string]any)
	if !ok {
		return fmt.Errorf("platform.monitoring: missing")
	}
	for _, receiver := range []struct {
		name   string
		fields []postureField
	}{
		{"receiver", []postureField{{"kind", "pagerduty"}, {"secretName", pagerduty}, {"secretKey", "routing_key"}}},
		{"receiverDeadman", []postureField{{"secretName", deadman}, {"secretKey", "url"}}},
	} {
		value, exists := monitoring[receiver.name]
		if pagerduty == "" {
			if exists {
				return fmt.Errorf("%s: must be absent", receiver.name)
			}
			continue
		}
		block, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: missing", receiver.name)
		}
		if err := checkPostureFields(receiver.name, block, receiver.fields); err != nil {
			return err
		}
	}
	return nil
}

var postureDigest = regexp.MustCompile(`[a-fA-F0-9]{64}`)

func postureFatal(t *testing.T, format string, args ...any) {
	t.Helper()
	t.Fatal(postureDigest.ReplaceAllString(fmt.Sprintf(format, args...), "<digest-redacted>"))
}

func generatedPostureValues(t *testing.T, cellName string, monitoring bool) map[string]any {
	t.Helper()
	root := repoRoot(t)
	catalog, err := loadCatalog(root)
	if err != nil {
		postureFatal(t, "%v", err)
	}
	cell, ok := catalog.Cells[cellName]
	if !ok {
		postureFatal(t, "catalog cell %s: missing; if the cell was retired, remove its cases from this table", cellName)
	}
	cell.Switches.Monitoring = monitoring
	catalog.Cells[cellName] = cell
	charts, err := loadChartPins(root)
	if err != nil {
		postureFatal(t, "%v", err)
	}
	body, err := generateCell(root, cellName, catalog, charts)
	if err != nil {
		postureFatal(t, "%v", err)
	}
	var values map[string]any
	if err := yaml.Unmarshal(body, &values); err != nil {
		postureFatal(t, "decode generated values: invalid YAML")
	}
	return values
}

func TestProductionAgentEmailRetentionStartsInPreview(t *testing.T) {
	const guidance = `civo-prod-use1-serving starts agent-email retention in preview; see docs/agent-email.md, "Agent email across cells and account moves"; a promotion to enforce updates this expectation, docs/agent-email.md and docs/data-retention-policy.md in the same reviewed change`
	for _, tc := range []struct {
		name       string
		monitoring bool
		field      string
		value      any
		missing    bool
		wantError  string
	}{
		{name: "generated, monitoring off"},
		{name: "generated, monitoring on", monitoring: true},
		{name: "mode enforce", field: "mode", value: "enforce", wantError: "agentEmailRetention.mode"},
		{name: "disabled", field: "enabled", value: false, wantError: "agentEmailRetention.enabled"},
		{name: "batch size", field: "batchSize", value: 25, wantError: "agentEmailRetention.batchSize"},
		{name: "interval", field: "interval", value: "5m", wantError: "agentEmailRetention.interval"},
		{name: "timeout", field: "batchTimeout", value: "5m", wantError: "agentEmailRetention.batchTimeout"},
		{name: "extra key", field: "extra", value: true, wantError: "unexpected key"},
		{name: "missing block", missing: true, wantError: "missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			values := generatedPostureValues(t, "civo-prod-use1-serving", tc.monitoring)
			if tc.field != "" {
				block, ok := productionEmailValue(values, "apps.witselfServer.worker.agentEmailRetention").(map[string]any)
				if !ok {
					postureFatal(t, "agentEmailRetention: missing")
				}
				block[tc.field] = tc.value
			}
			if tc.missing {
				worker, ok := productionEmailValue(values, "apps.witselfServer.worker").(map[string]any)
				if !ok {
					postureFatal(t, "worker: missing")
				}
				delete(worker, "agentEmailRetention")
			}
			err := checkFirstSyncEmailRetention(values)
			if tc.wantError == "" {
				if err != nil {
					postureFatal(t, "%v; %s", err, guidance)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				postureFatal(t, "got %v, want error containing %s", err, tc.wantError)
			}
		})
	}
}

func TestProductionServerResources(t *testing.T) {
	const guidance = "civo-prod-use1-serving runs witself-server with requests 50m CPU / 128Mi memory and a 512Mi memory limit (design 2026-10-09 S5); every key is set because overrides merge key by key with the server chart defaults; changing it is its own reviewed production change"
	for _, tc := range []struct {
		name       string
		monitoring bool
		block      string
		field      string
		value      any
		missing    bool
		wantError  string
	}{
		{name: "generated, monitoring off"},
		{name: "generated, monitoring on", monitoring: true},
		{name: "limits.memory deleted", block: "resources.limits", field: "memory", missing: true, wantError: "resources.limits.memory"},
		{name: "limits block deleted", block: "resources", field: "limits", missing: true, wantError: "resources.limits.memory"},
		{name: "requests.cpu deleted", block: "resources.requests", field: "cpu", missing: true, wantError: "resources.requests.cpu"},
		{name: "requests.memory 64Mi", block: "resources.requests", field: "memory", value: "64Mi", wantError: "resources.requests.memory"},
		{name: "limits.memory 256Mi", block: "resources.limits", field: "memory", value: "256Mi", wantError: "resources.limits.memory"},
		{name: "limits.cpu added", block: "resources.limits", field: "cpu", value: "500m", wantError: "resources.limits: unexpected key"},
		{name: "claims added", block: "resources", field: "claims", value: []any{}, wantError: "resources: unexpected key"},
		{name: "resources deleted", field: "resources", missing: true, wantError: "resources: missing"},
		{name: "resources not a map", field: "resources", value: "512Mi", wantError: "resources: missing"},
		{name: "requests block deleted", block: "resources", field: "requests", missing: true, wantError: "resources.requests.cpu: got <nil>, want 50m"},
		{name: "requests not a map", block: "resources", field: "requests", value: "128Mi", wantError: "resources.requests.cpu: got <nil>, want 50m"},
		{name: "limits not a map", block: "resources", field: "limits", value: "512Mi", wantError: "resources.limits.memory: got <nil>, want 512Mi"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			values := generatedPostureValues(t, "civo-prod-use1-serving", tc.monitoring)
			if tc.field != "" {
				path := "apps.witselfServer"
				if tc.block != "" {
					path += "." + tc.block
				}
				block, ok := productionEmailValue(values, path).(map[string]any)
				if !ok {
					postureFatal(t, "%s: missing", path)
				}
				if tc.missing {
					delete(block, tc.field)
				} else {
					block[tc.field] = tc.value
				}
			}
			err := checkProductionServerResources(values)
			if tc.wantError == "" {
				if err != nil {
					postureFatal(t, "%v; %s", err, guidance)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				postureFatal(t, "got %v, want error containing %s", err, tc.wantError)
			}
		})
	}

	t.Run("other cells", func(t *testing.T) {
		const canary = "civo-sandbox-use1-backup keeps the server chart's 256Mi limit as the OOM early-warning canary (design 2026-10-09 §3 rank 4); changing it is a separate founder decision"
		generated, err := generateAll(repoRoot(t))
		if err != nil {
			postureFatal(t, "%v", err)
		}
		for cell, body := range generated {
			if cell == "civo-prod-use1-serving" {
				continue
			}
			t.Run(cell, func(t *testing.T) {
				var values map[string]any
				if err := yaml.Unmarshal(body, &values); err != nil {
					postureFatal(t, "cell %s: decode generated values: invalid YAML", cell)
				}
				server, ok := productionEmailValue(values, "apps.witselfServer").(map[string]any)
				if !ok {
					postureFatal(t, "cell %s: apps.witselfServer: missing", cell)
				}
				if _, exists := server["resources"]; exists {
					if cell == "civo-sandbox-use1-backup" {
						postureFatal(t, "cell %s: apps.witselfServer.resources must be absent; %s", cell, canary)
					}
					postureFatal(t, "cell %s: apps.witselfServer.resources must be absent", cell)
				}
			})
		}
	})
}

func TestReceiverSecretNamesPerServingCell(t *testing.T) {
	const (
		production = "civo-prod-use1-serving"
		sandbox    = "civo-sandbox-use1-serving"
		pagerV2    = "witself-monitoring-pagerduty-v2"
		deadmanV2  = "witself-monitoring-deadman-v2"
		pagerV1    = "witself-monitoring-pagerduty-v1"
		deadmanV1  = "witself-monitoring-deadman-v1"
		guidance   = "civo-prod-use1-serving names the -v2 receiver Secrets and civo-sandbox-use1-serving names the -v1 receiver Secrets; the names are set per cell"
	)
	for _, tc := range []struct {
		name       string
		cell       string
		monitoring bool
		pagerduty  string
		deadman    string
		block      string
		field      string
		value      any
		wantError  string
	}{
		{name: "production on", cell: production, monitoring: true, pagerduty: pagerV2, deadman: deadmanV2},
		{name: "production off", cell: production},
		{name: "sandbox serving", cell: sandbox, monitoring: true, pagerduty: pagerV1, deadman: deadmanV1},
		{name: "production on, old names", cell: production, monitoring: true, pagerduty: pagerV1, deadman: deadmanV1, wantError: "receiver.secretName"},
		{name: "sandbox serving, new names", cell: sandbox, monitoring: true, pagerduty: pagerV2, deadman: deadmanV2, wantError: "receiver.secretName"},
		{name: "production on, dead-man", cell: production, monitoring: true, pagerduty: pagerV2, deadman: deadmanV2, block: "receiverDeadman", field: "secretName", value: deadmanV1, wantError: "receiverDeadman.secretName"},
		{name: "production off, names expected", cell: production, pagerduty: pagerV2, deadman: deadmanV2, wantError: "receiver: missing"},
		{name: "production on, none expected", cell: production, monitoring: true, wantError: "must be absent"},
		{name: "extra key", cell: production, monitoring: true, pagerduty: pagerV2, deadman: deadmanV2, block: "receiver", field: "extra", value: true, wantError: "unexpected key"},
		{name: "one name", cell: production, monitoring: true, pagerduty: pagerV2, wantError: "both names"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			values := generatedPostureValues(t, tc.cell, tc.monitoring)
			if tc.field != "" {
				block, ok := productionEmailValue(values, "platform.monitoring."+tc.block).(map[string]any)
				if !ok {
					postureFatal(t, "%s: missing", tc.block)
				}
				block[tc.field] = tc.value
			}
			err := checkReceiverSecretNames(values, tc.pagerduty, tc.deadman)
			if tc.wantError == "" {
				if err != nil {
					postureFatal(t, "%v; %s", err, guidance)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				postureFatal(t, "got %v, want error containing %s", err, tc.wantError)
			}
		})
	}
}
