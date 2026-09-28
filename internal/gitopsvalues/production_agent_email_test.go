package gitopsvalues

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func productionEmailValue(v map[string]any, path string) any {
	var current any = v
	for _, key := range strings.Split(path, ".") {
		object, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = object[key]
	}
	return current
}

func checkProductionEmail(v, serving map[string]any) error {
	prefix := "apps.witselfServer."
	expected := map[string]any{
		"agentEmail.receivePilot.enabled":                                    false,
		"agentEmail.receiveProduction.domain":                                "witmail.net",
		"agentEmail.receiveProduction.acceptedLegacyDomains":                 []any{"agent-mail.witwave.ai"},
		"agentEmail.receiveProduction.audience":                              "civo-prod-use1-serving",
		"agentEmail.receiveProduction.accountIDs":                            []any{},
		"agentEmail.receiveProduction.accountIDsExistingSecret.name":         "witself-agent-email-receive-cohort-v1",
		"agentEmail.receiveProduction.accountIDsExistingSecret.key":          "account_ids",
		"agentEmail.receiveProduction.retryCanaryAgentID":                    "",
		"agentEmail.receiveProduction.retryCanaryAgentIDExistingSecret.name": "witself-agent-email-retry-canary-v1",
		"agentEmail.receiveProduction.retryCanaryAgentIDExistingSecret.key":  "agent_id",
		"agentEmail.receiveProduction.relayReplayWindow":                     "5m",
		"agentEmail.providerEventTokenSecret.key":                            "token",
		"worker.agentEmailOutbound.dispatchEndpoint":                         "https://witself-agent-email-send.witwave.workers.dev/v1/dispatch",
		"worker.agentEmailOutbound.dispatchAudience":                         "witself-agent-email-send",
		"worker.agentEmailOutbound.dispatchKeyID":                            "civo-prod-use1-serving-2026-10",
		"worker.agentEmailOutbound.dispatchPrivateKeySecret.name":            "witself-agent-email-outbound-dispatch-v1",
		"worker.agentEmailOutbound.dispatchPrivateKeySecret.key":             "private-key",
	}
	for path, want := range expected {
		if !reflect.DeepEqual(productionEmailValue(v, prefix+path), want) {
			return fmt.Errorf("retained production email setting differs: %s", path)
		}
	}
	relayPath := prefix + "agentEmail.receiveProduction.relayPublicKeysJSON"
	if !reflect.DeepEqual(productionEmailValue(v, relayPath), productionEmailValue(serving, relayPath)) {
		return fmt.Errorf("fleet relay differs")
	}
	tokenPath := prefix + "agentEmail.providerEventTokenSecret.name"
	token := productionEmailValue(v, tokenPath)
	if productionEmailValue(v, prefix+"worker.agentEmailOutbound.enabled") == true && token == "" {
		return fmt.Errorf("outbound requires provider-event secret")
	}
	if token != "" && token != productionEmailValue(serving, tokenPath) {
		return fmt.Errorf("provider-event secret is not the serving versioned name")
	}
	return nil
}

func TestProductionAgentEmailRetainedConfiguration(t *testing.T) {
	root := repoRoot(t)
	catalog, err := loadCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	charts, err := loadChartPins(root)
	if err != nil {
		t.Fatal(err)
	}
	decode := func(cell string) map[string]any {
		t.Helper()
		raw, err := generateCell(root, cell, catalog, charts)
		if err != nil {
			t.Fatal(err)
		}
		var v map[string]any
		if err := yaml.Unmarshal(raw, &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	serving := decode("civo-sandbox-use1-serving")
	for _, state := range []string{"committed", "lit", "dark", "receive-first", "outbound-without-token", "wrong-audience"} {
		t.Run(state, func(t *testing.T) {
			v := decode("civo-prod-use1-serving")
			email := productionEmailValue(v, "apps.witselfServer.agentEmail").(map[string]any)
			receive := email["receiveProduction"].(map[string]any)
			token := email["providerEventTokenSecret"].(map[string]any)
			outbound := productionEmailValue(v, "apps.witselfServer.worker.agentEmailOutbound").(map[string]any)
			switch state {
			case "lit":
				receive["enabled"] = true
				outbound["enabled"] = true
				token["name"] = productionEmailValue(serving, "apps.witselfServer.agentEmail.providerEventTokenSecret.name")
			case "dark":
				receive["enabled"] = false
				outbound["enabled"] = false
				token["name"] = ""
			case "receive-first":
				receive["enabled"] = true
				outbound["enabled"] = false
				token["name"] = ""
			case "outbound-without-token":
				outbound["enabled"] = true
				token["name"] = ""
			case "wrong-audience":
				receive["audience"] = "wrong-cell"
			}
			err := checkProductionEmail(v, serving)
			negative := state == "outbound-without-token" || state == "wrong-audience"
			if (err != nil) != negative {
				t.Fatalf("configuration validation: %v", err)
			}
		})
	}
}
