package server

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestSupportAlertRulesAreValueFree(t *testing.T) {
	rulesPath, err := filepath.Abs("../../.gitops/charts/platform/files/founder-open-plane.rules.yaml")
	if err != nil {
		t.Fatal(err)
	}
	rulesData, err := os.ReadFile(rulesPath)
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Groups []struct {
			Rules []struct {
				Alert       string            `yaml:"alert"`
				Expr        string            `yaml:"expr"`
				Labels      map[string]string `yaml:"labels"`
				Annotations map[string]string `yaml:"annotations"`
			} `yaml:"rules"`
		} `yaml:"groups"`
	}
	if err := yaml.Unmarshal(rulesData, &document); err != nil {
		t.Fatal(err)
	}
	runbookPath, err := filepath.Abs("../../docs/runbooks.md")
	if err != nil {
		t.Fatal(err)
	}
	runbookData, err := os.ReadFile(runbookPath)
	if err != nil {
		t.Fatal(err)
	}

	headingRE := regexp.MustCompile(`^#+ (.*)$`)
	slugStripRE := regexp.MustCompile(`[^a-z0-9 -]`)
	headingSlugs := make(map[string]int)
	for line := range strings.SplitSeq(string(runbookData), "\n") {
		if match := headingRE.FindStringSubmatch(line); match != nil {
			slug := slugStripRE.ReplaceAllString(strings.ToLower(match[1]), "")
			headingSlugs[strings.ReplaceAll(slug, " ", "-")]++
		}
	}
	metricRE := regexp.MustCompile(`witself_[a-z_]+`)
	groupingRE := regexp.MustCompile(`\b(?:by|without)\s*\(`)
	wantLabels := []string{"service", "severity", "witself_alert"}
	allowedAnnotations := map[string]bool{"summary": true, "description": true, "runbook": true}
	var inventory []string
	for _, group := range document.Groups {
		for _, rule := range group.Rules {
			if !strings.HasPrefix(rule.Alert, "WitselfSupport") {
				continue
			}
			inventory = append(inventory, rule.Alert)
			t.Run(rule.Alert, func(t *testing.T) {
				var labelKeys []string
				for key := range rule.Labels {
					labelKeys = append(labelKeys, key)
				}
				slices.Sort(labelKeys)
				if !slices.Equal(labelKeys, wantLabels) {
					t.Errorf("support alert label keys = %v, want %v", labelKeys, wantLabels)
				}
				for key, value := range rule.Annotations {
					if !allowedAnnotations[key] {
						t.Errorf("unsupported support alert annotation key %q", key)
					}
					if strings.Contains(value, "{{") || strings.Contains(value, "$") {
						t.Errorf("support alert annotation %q contains value interpolation", key)
					}
				}
				for _, metric := range metricRE.FindAllString(rule.Expr, -1) {
					if !strings.HasPrefix(metric, "witself_support_") {
						t.Errorf("support alert references a metric outside the support family: %s", metric)
					}
				}
				if groupingRE.MatchString(rule.Expr) || strings.Contains(rule.Expr, "{") {
					t.Error("support alert expression contains grouping or a label matcher")
				}
				fragment, ok := strings.CutPrefix(rule.Annotations["runbook"], "docs/runbooks.md#")
				if !ok || fragment == "" {
					t.Error("support alert runbook must be a docs/runbooks.md heading fragment")
				} else if count := headingSlugs[fragment]; count != 1 {
					t.Errorf("support alert runbook fragment %q resolves to %d headings, want exactly one", fragment, count)
				}
			})
		}
	}
	wantInventory := []string{
		"WitselfSupportFirstResponseBreach",
		"WitselfSupportTicketOpened",
		"WitselfSupportFirstResponseSlow",
		"WitselfSupportUrgentTicketWaiting",
	}
	if !slices.Equal(inventory, wantInventory) {
		t.Errorf("support alert inventory = %v, want %v in file order", inventory, wantInventory)
	}
}
