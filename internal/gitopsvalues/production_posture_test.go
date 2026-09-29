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
		postureFatal(t, "catalog cell %s: missing", cellName)
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
		{name: "production off, names expected", cell: production, pagerduty: pagerV2, deadman: deadmanV2, wantError: "missing"},
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
