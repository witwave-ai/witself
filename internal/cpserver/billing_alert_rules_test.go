package cpserver

import (
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// Read names only where the entitlement emitter writes metrics. Other quoted
// strings (checkpoint fields, for example) do not establish an emitted suffix.
func billingAlertEntitlementEmission(source string) (map[string]bool, map[string]bool, error) {
	prefix := regexp.MustCompile(`(?m)^const PREFIX = "([^"]+)";$`).FindStringSubmatch(source)
	if prefix == nil || prefix[1] != "witself_entitlement_delivery_" {
		return nil, nil, errors.New("entitlement emitter PREFIX must be witself_entitlement_delivery_")
	}
	suffixes := make(map[string]bool)
	for _, call := range regexp.MustCompile(`(?m)^\s*gauge\("([^"]+)"\s*,`).FindAllStringSubmatch(source, -1) {
		suffixes[call[1]] = true
	}
	accountTuple := regexp.MustCompile(`(?m)^\s*for \(const \[name, values\] of \[\["([^"]+)", m\.last_page\], \["([^"]+)", m\.last_cycle\?\.counts\]\]\) \{`).FindStringSubmatch(source)
	if len(suffixes) == 0 || accountTuple == nil {
		return nil, nil, errors.New("entitlement gauge calls or account emission tuple not found")
	}
	for _, suffix := range accountTuple[1:] {
		suffixes[suffix] = true
	}
	outcomeArray := regexp.MustCompile(`(?m)^const OUTCOMES = (\[[^\n]+\]);$`).FindStringSubmatch(source)
	if outcomeArray == nil {
		return nil, nil, errors.New("entitlement emitter OUTCOMES array not found")
	}
	var values []string
	if err := json.Unmarshal([]byte(outcomeArray[1]), &values); err != nil {
		return nil, nil, err
	}
	outcomes := make(map[string]bool)
	for _, value := range values {
		outcomes[value] = true
	}
	return suffixes, outcomes, nil
}

func billingAlertReferencedMetrics(expression string) []string {
	var metrics []string
	// Capture the full Prometheus name grammar before checking our narrower
	// convention, so uppercase letters, digits and colons cannot truncate names.
	for _, name := range regexp.MustCompile(`[a-zA-Z_:][a-zA-Z0-9_:]*`).FindAllString(expression, -1) {
		family := strings.ToLower(name)
		if strings.Contains(family, "witself_cp_") || strings.Contains(family, "witself_entitlement_delivery_") {
			metrics = append(metrics, name)
		}
	}
	return metrics
}

func billingAlertUnknownEntitlementOutcomes(expression string, outcomes map[string]bool) []string {
	selectorRE := regexp.MustCompile(`\bwitself_entitlement_delivery_last_(?:cycle|page)_accounts\s*\{([^}]*)\}`)
	outcomeRE := regexp.MustCompile(`\boutcome\s*(=~|!~|!=|=)\s*"([^"]*)"`)
	var unknown []string
	for _, selector := range selectorRE.FindAllStringSubmatch(expression, -1) {
		for _, matcher := range outcomeRE.FindAllStringSubmatch(selector[1], -1) {
			values := []string{matcher[2]}
			if strings.HasSuffix(matcher[1], "~") {
				values = strings.Split(matcher[2], "|")
			}
			for _, value := range values {
				if !outcomes[value] {
					unknown = append(unknown, value)
				}
			}
		}
	}
	return unknown
}

func TestBillingAlertRulesMatchEmitterAndRunbooks(t *testing.T) {
	read := func(t *testing.T, path string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join("../..", path))
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		return string(data)
	}
	stripeEmitter := read(t, "infra/cloudflare/control-plane/src/stripe-observation.mjs")
	entitlementEmitter := read(t, "infra/cloudflare/control-plane/src/entitlement-delivery-metrics.mjs")
	entitlementSuffixes, entitlementOutcomes, err := billingAlertEntitlementEmission(entitlementEmitter)
	if err != nil {
		t.Fatal(err)
	}
	quotedLiteral := func(source, value string) bool {
		return strings.Contains(source, `"`+value+`"`) || strings.Contains(source, "'"+value+"'")
	}
	metricNameRE := regexp.MustCompile(`^[a-z0-9_]+$`)
	stripeSelectorRE := regexp.MustCompile(`\bwitself_cp_stripe_webhook_events_total\s*\{([^}]*)\}`)
	outcomeRE := regexp.MustCompile(`\boutcome\s*=\s*"([^"]+)"`)
	runbookRE := regexp.MustCompile(`^(docs/[^/#]+\.md)#([^#]+)$`)
	headingRE := regexp.MustCompile(`^#+ (.*)$`)
	slugStripRE := regexp.MustCompile(`[^a-z0-9 -]`)
	headingSlugs := make(map[string]map[string]int)

	wantNewLabels := map[string]map[string]string{
		"WitselfStripeWebhookProcessingFailures": {
			"severity": "warning", "service": "control-plane-stripe", "witself_alert": "true",
		},
		"WitselfBillingMutationRecoveryFailing": {
			"severity": "critical", "service": "control-plane-stripe", "witself_alert": "true",
		},
		"WitselfEntitlementDeliveryStuck": {
			"severity": "critical", "service": "entitlement-delivery", "witself_alert": "true",
		},
	}
	for _, file := range []struct {
		name      string
		group     string
		inventory []string
	}{
		{
			name:  "uptime-probes.rules.yaml",
			group: "witself-uptime-probes",
			inventory: []string{
				"WitselfProbeTargetDown",
				"WitselfProbeStale",
				"WitselfProbeScrapeDown",
				"WitselfProbeSchedulerStale",
				"WitselfProbeDirectoryUnavailable",
				"WitselfProbeResultsMissing",
				"WitselfProbeNoTargets",
				"WitselfProbeBudgetExhausted",
				"WitselfStripeWebhookRejections",
				"WitselfBillingReconciliationStalled",
				"WitselfStripeWebhookProcessingFailures",
				"WitselfBillingMutationRecoveryFailing",
				"WitselfSignupGlobalQuotaDenied",
				"WitselfSignupProvisioningFailureRatioHigh",
				"WitselfVerificationEmailFailureRatioHigh",
				"WitselfSignupMetricsUnavailable",
			},
		},
		{
			name:  "entitlement-delivery.rules.yaml",
			group: "witself-entitlement-delivery",
			inventory: []string{
				"WitselfEntitlementDeliveryMetricsUnavailable",
				"WitselfEntitlementDeliverySchedulerStale",
				"WitselfEntitlementDeliveryCycleIncomplete",
				"WitselfEntitlementDeliveryGap",
				"WitselfEntitlementDeliveryStuck",
			},
		},
	} {
		t.Run(file.group, func(t *testing.T) {
			var document struct {
				Groups []struct {
					Name  string `yaml:"name"`
					Rules []struct {
						Alert       string            `yaml:"alert"`
						Expr        string            `yaml:"expr"`
						Labels      map[string]string `yaml:"labels"`
						Annotations map[string]string `yaml:"annotations"`
					} `yaml:"rules"`
				} `yaml:"groups"`
			}
			if err := yaml.Unmarshal([]byte(read(t, ".gitops/charts/platform/files/"+file.name)), &document); err != nil {
				t.Fatalf("parse %s: %v", file.name, err)
			}
			if len(document.Groups) != 1 || document.Groups[0].Name != file.group {
				t.Fatalf("%s must contain exactly the %s group", file.name, file.group)
			}
			var inventory []string
			for _, rule := range document.Groups[0].Rules {
				inventory = append(inventory, rule.Alert)
				t.Run(rule.Alert, func(t *testing.T) {
					if want, ok := wantNewLabels[rule.Alert]; ok {
						if !maps.Equal(rule.Labels, want) {
							t.Errorf("%s labels = %v, want exactly %v", rule.Alert, rule.Labels, want)
						}
						keys := slices.Sorted(maps.Keys(rule.Annotations))
						if !slices.Equal(keys, []string{"runbook", "summary"}) {
							t.Errorf("%s annotation keys = %v, want exactly runbook and summary", rule.Alert, keys)
						}
						if strings.TrimSpace(rule.Annotations["summary"]) == "" {
							t.Errorf("%s summary annotation is empty", rule.Alert)
						}
					}

					runbook := rule.Annotations["runbook"]
					if match := runbookRE.FindStringSubmatch(runbook); match == nil {
						t.Errorf("%s runbook %q must name a docs/*.md file and heading fragment", rule.Alert, runbook)
					} else {
						path, fragment := match[1], match[2]
						slugs, ok := headingSlugs[path]
						if !ok {
							data, err := os.ReadFile(filepath.Join("../..", path))
							if err != nil {
								t.Fatalf("%s runbook %q: %v", rule.Alert, runbook, err)
							}
							slugs = make(map[string]int)
							for line := range strings.SplitSeq(string(data), "\n") {
								if heading := headingRE.FindStringSubmatch(line); heading != nil {
									slug := slugStripRE.ReplaceAllString(strings.ToLower(heading[1]), "")
									slugs[strings.ReplaceAll(slug, " ", "-")]++
								}
							}
							headingSlugs[path] = slugs
						}
						if count := slugs[fragment]; count != 1 {
							t.Errorf("%s runbook %q fragment %q resolves to %d headings, want exactly one", rule.Alert, path, fragment, count)
						}
					}

					for _, metric := range billingAlertReferencedMetrics(rule.Expr) {
						if !metricNameRE.MatchString(metric) {
							t.Errorf("%s metric %q must contain only [a-z0-9_]", rule.Alert, metric)
						}
						if suffix, ok := strings.CutPrefix(metric, "witself_entitlement_delivery_"); ok {
							if !entitlementSuffixes[suffix] {
								t.Errorf("%s metric %q suffix %q is not emitted by entitlement-delivery-metrics.mjs", rule.Alert, metric, suffix)
							}
						} else if !quotedLiteral(stripeEmitter, metric) {
							t.Errorf("%s metric %q has no full quoted literal in stripe-observation.mjs", rule.Alert, metric)
						}
					}
					for _, value := range billingAlertUnknownEntitlementOutcomes(rule.Expr, entitlementOutcomes) {
						t.Errorf("%s entitlement outcome %q is not in the emitter's OUTCOMES array", rule.Alert, value)
					}
					for _, selector := range stripeSelectorRE.FindAllStringSubmatch(rule.Expr, -1) {
						for _, outcome := range outcomeRE.FindAllStringSubmatch(selector[1], -1) {
							if !quotedLiteral(stripeEmitter, outcome[1]) {
								t.Errorf("%s Stripe webhook outcome %q has no quoted literal in stripe-observation.mjs", rule.Alert, outcome[1])
							}
						}
					}
				})
			}
			if !slices.Equal(inventory, file.inventory) {
				t.Errorf("%s alert inventory = %v, want %v in file order", file.group, inventory, file.inventory)
			}
		})
	}
}

func TestBillingAlertEntitlementEmission(t *testing.T) {
	const emitter = `const PREFIX = "witself_entitlement_delivery_";
const OUTCOMES = ["scanned", "seeded", "apply_pending", "failed"];
const checkpointField = "last_ack_at";
// gauge("comment_only", "", 1);
gauge("enabled", "Enabled", 1);
for (const [name, values] of [["last_page_accounts", m.last_page], ["last_cycle_accounts", m.last_cycle?.counts]]) {
`
	suffixes, outcomes, err := billingAlertEntitlementEmission(emitter)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"enabled", "last_cycle_accounts", "last_page_accounts"}; !slices.Equal(slices.Sorted(maps.Keys(suffixes)), want) {
		t.Errorf("emitted suffixes = %v, want %v; arbitrary literals and comments must not count", slices.Sorted(maps.Keys(suffixes)), want)
	}
	if want := []string{"apply_pending", "failed", "scanned", "seeded"}; !slices.Equal(slices.Sorted(maps.Keys(outcomes)), want) {
		t.Errorf("emitted outcomes = %v, want %v", slices.Sorted(maps.Keys(outcomes)), want)
	}
	if _, _, err := billingAlertEntitlementEmission(strings.Replace(emitter, "witself_entitlement_delivery_", "wrong_prefix_", 1)); err == nil {
		t.Error("changed emitter PREFIX passed")
	}
}

func TestBillingAlertMetricNamesAreNotTruncated(t *testing.T) {
	want := []string{
		"witself_cp_metric2",
		"witself_cp_metric:sum",
		"witself_entitlement_delivery_enabledUpper",
		"witself_entitlement_delivery_enabled:",
		":witself_cp_metric",
		"WITSELF_entitlement_delivery_enabled",
		"prefixed_witself_cp_metric",
	}
	if got := billingAlertReferencedMetrics(strings.Join(want, " + ")); !slices.Equal(got, want) {
		t.Errorf("metric names = %v, want full names %v", got, want)
	}
}

func TestBillingAlertEntitlementOutcomeSelectors(t *testing.T) {
	outcomes := map[string]bool{"failed": true, "apply_pending": true}
	for _, tc := range []struct {
		name       string
		expression string
		unknown    []string
	}{
		{"cycle_equal", `witself_entitlement_delivery_last_cycle_accounts{outcome="failed"}`, nil},
		{"page_alternation", `witself_entitlement_delivery_last_page_accounts{outcome=~"apply_pending|failed"}`, nil},
		{"invalid_equal", `witself_entitlement_delivery_last_page_accounts{outcome="last_ack_at"}`, []string{"last_ack_at"}},
		{"invalid_alternation", `witself_entitlement_delivery_last_cycle_accounts{outcome=~"apply_pending|misspelled"}`, []string{"misspelled"}},
		{"equality_is_not_alternation", `witself_entitlement_delivery_last_cycle_accounts{outcome="apply_pending|failed"}`, []string{"apply_pending|failed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := billingAlertUnknownEntitlementOutcomes(tc.expression, outcomes); !slices.Equal(got, tc.unknown) {
				t.Errorf("unknown outcomes = %v, want %v", got, tc.unknown)
			}
		})
	}
}
