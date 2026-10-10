package plans

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	witself "github.com/witwave-ai/witself"
)

func TestCanonicalComingSoonFeatures(t *testing.T) {
	catalog, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"agent_email_custom_domain",
		"agent_email_realm_alias",
		"agent_email_receive",
		"agent_email_send",
	}
	if !slices.Equal(catalog.ComingSoonFeatures, want) {
		t.Errorf("coming_soon_features = %v; want %v", catalog.ComingSoonFeatures, want)
	}
	for _, tc := range []struct {
		id       string
		features []string
	}{
		{Free, nil},
		{"standard", []string{"agent_email_receive"}},
		{"team", want},
		{"enterprise", want},
	} {
		t.Run(tc.id, func(t *testing.T) {
			plan, ok := catalog.Get(tc.id)
			if !ok {
				t.Fatalf("missing plan %q", tc.id)
			}
			var got []string
			for _, feature := range want {
				if plan.HasFeature(feature) {
					got = append(got, feature)
				}
			}
			if !slices.Equal(got, tc.features) {
				t.Errorf("marked-feature entitlements = %v; want %v", got, tc.features)
			}
		})
	}
}

func TestParseComingSoonFeaturesValidation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		features []string
		absent   bool
		stale    bool
		wantErr  bool
	}{
		{name: "absent", absent: true},
		{name: "empty", features: []string{}},
		{name: "valid", features: []string{AgentEmailReceiveFeature, AgentEmailSendFeature}},
		{name: "unknown", features: []string{"unknown"}, wantErr: true},
		{name: "duplicate", features: []string{AgentEmailReceiveFeature, AgentEmailReceiveFeature}, wantErr: true},
		{name: "unsorted", features: []string{AgentEmailSendFeature, AgentEmailReceiveFeature}, wantErr: true},
		{name: "stale", features: []string{AgentEmailReceiveFeature}, stale: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var doc map[string]any
			if err := json.Unmarshal(witself.PlansJSON, &doc); err != nil {
				t.Fatal(err)
			}
			if tc.absent {
				delete(doc, "coming_soon_features")
			} else {
				doc["coming_soon_features"] = tc.features
			}
			if tc.stale {
				for _, entry := range doc["plans"].([]any) {
					plan := entry.(map[string]any)
					plan["features"] = slices.DeleteFunc(plan["features"].([]any), func(feature any) bool {
						return feature == AgentEmailReceiveFeature
					})
				}
			}
			raw, err := json.Marshal(doc)
			if err != nil {
				t.Fatal(err)
			}
			catalog, err := Parse(raw)
			if tc.wantErr {
				if err == nil || !strings.HasPrefix(err.Error(), "plan catalog: coming_soon_features:") {
					t.Fatalf("Parse error = %v; want coming_soon_features validation prefix", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(catalog.ComingSoonFeatures, tc.features) {
				t.Errorf("coming_soon_features = %v; want %v", catalog.ComingSoonFeatures, tc.features)
			}
			for _, feature := range SupportedFeatureKeys() {
				if got, want := catalog.FeatureComingSoon(feature), slices.Contains(tc.features, feature); got != want {
					t.Errorf("FeatureComingSoon(%q) = %t; want %t", feature, got, want)
				}
			}
			if catalog.FeatureComingSoon("unknown") {
				t.Error("unknown feature is coming soon")
			}
		})
	}
}

// This is a wording tripwire, not proof of delivery or a complete sales-language audit.
func TestPlanSummariesDoNotSellComingSoonFeatures(t *testing.T) {
	catalog, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, plan := range catalog.Plans {
		if !plan.Available || !slices.ContainsFunc(plan.Features, catalog.FeatureComingSoon) {
			continue
		}
		summary := strings.ToLower(plan.Summary)
		if (strings.Contains(summary, "email") || strings.Contains(summary, "alias") || strings.Contains(summary, "domain")) &&
			!strings.Contains(summary, "coming soon") {
			t.Errorf("available plan %q summary names a coming-soon capability without saying coming soon", plan.ID)
		}
	}
}

func TestDocsShowComingSoonFeatures(t *testing.T) {
	catalog, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	plansByName := make(map[string]Plan, len(catalog.Plans))
	for _, plan := range catalog.Plans {
		plansByName[plan.Name] = plan
	}
	checkCell := func(t *testing.T, plan Plan, label, gate, cell string) {
		t.Helper()
		cell = strings.TrimSpace(cell)
		if plan.HasFeature(gate) && catalog.FeatureComingSoon(gate) {
			if cell != "Coming soon" {
				t.Errorf("%s / %s = %q; want exactly Coming soon", plan.Name, label, cell)
			}
		} else if strings.Contains(strings.ToLower(cell), "coming soon") {
			t.Errorf("%s / %s = %q; must not say coming soon", plan.Name, label, cell)
		}
	}
	t.Run("billing_matrix", func(t *testing.T) {
		header, rows := comingSoonDocTable(t, "../../docs/billing-and-limits.md", "| Capability |")
		columns := make(map[int]Plan, len(catalog.Plans))
		seenPlans := make(map[string]bool, len(catalog.Plans))
		for column, cell := range header[1:] {
			name, _, _ := strings.Cut(cell, " — ")
			plan, ok := plansByName[name]
			if !ok || seenPlans[name] {
				t.Fatalf("unknown or duplicate billing plan column %q", name)
			}
			seenPlans[name] = true
			columns[column+1] = plan
		}
		if len(columns) != len(catalog.Plans) {
			t.Fatalf("billing matrix has %d plan columns; want %d", len(columns), len(catalog.Plans))
		}
		gates := map[string]string{
			"Receive agent email":                     AgentEmailReceiveFeature,
			"Raw MIME and attachment retention":       AgentEmailReceiveFeature,
			"Maximum raw email size":                  AgentEmailReceiveFeature,
			"Retained attachment storage per account": AgentEmailReceiveFeature,
			"Permanent Realm ID email address":        AgentEmailReceiveFeature,
			"Send agent email":                        AgentEmailSendFeature,
			"Realm email aliases":                     AgentEmailRealmAliasFeature,
			"Custom inbound email domains":            AgentEmailCustomDomainFeature,
		}
		seenRows := make(map[string]int, len(gates))
		for _, row := range rows {
			gate, ok := gates[row[0]]
			if !ok {
				continue
			}
			seenRows[row[0]]++
			for column, plan := range columns {
				checkCell(t, plan, row[0], gate, row[column])
			}
		}
		for label := range gates {
			if seenRows[label] != 1 {
				t.Errorf("billing row %q occurs %d times; want exactly once", label, seenRows[label])
			}
		}
	})
	t.Run("agent_email_table", func(t *testing.T) {
		header, rows := comingSoonDocTable(t, "../../docs/agent-email.md", "| Plan | Receive | Send |")
		if !slices.Equal(header, []string{"Plan", "Receive", "Send"}) {
			t.Fatalf("unexpected agent-email header %v", header)
		}
		seenPlans := make(map[string]int, len(catalog.Plans))
		for _, row := range rows {
			plan, ok := plansByName[row[0]]
			if !ok {
				t.Fatalf("unknown agent-email plan row %q", row[0])
			}
			seenPlans[plan.Name]++
			checkCell(t, plan, "Receive", AgentEmailReceiveFeature, row[1])
			checkCell(t, plan, "Send", AgentEmailSendFeature, row[2])
		}
		for name := range plansByName {
			if seenPlans[name] != 1 {
				t.Errorf("agent-email plan %q occurs %d times; want exactly once", name, seenPlans[name])
			}
		}
	})
}

func comingSoonDocTable(t *testing.T, path, headerPrefix string) ([]string, [][]string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(raw), "\n")
	headerIndex := -1
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), headerPrefix) {
			if headerIndex != -1 {
				t.Fatalf("%s: duplicate table header %q", path, headerPrefix)
			}
			headerIndex = i
		}
	}
	if headerIndex == -1 {
		t.Fatalf("%s: missing table header %q", path, headerPrefix)
	}
	parseRow := func(line string) []string {
		cells := strings.Split(strings.Trim(strings.TrimSpace(line), "|"), "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		return cells
	}
	header := parseRow(lines[headerIndex])
	var rows [][]string
	for _, line := range lines[headerIndex+1:] {
		if !strings.HasPrefix(strings.TrimSpace(line), "|") {
			break
		}
		row := parseRow(line)
		if len(row) != len(header) {
			t.Fatalf("%s: table row %q has %d cells; want %d", path, row[0], len(row), len(header))
		}
		if strings.Trim(row[0], "-:") == "" {
			continue
		}
		rows = append(rows, row)
	}
	return header, rows
}
