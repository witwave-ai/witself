package plans

import (
	"maps"
	"slices"
	"strings"
	"testing"
)

// TestLoadCanonicalCatalog validates the REAL embedded catalog — the same
// document the Cloudflare Worker serves. An invalid catalog edit fails here
// before it can ship anywhere.
func TestLoadCanonicalCatalog(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.Plans) != 4 {
		t.Fatalf("catalog has %d plans; want 4", len(c.Plans))
	}
	if c.Updated != "2026-07-24" {
		t.Fatalf("catalog updated = %q; want 2026-07-24", c.Updated)
	}
	if c.Currency != "USD" {
		t.Fatalf("catalog currency = %q; want USD", c.Currency)
	}

	free, ok := c.Get(Free)
	if !ok || free.Paid() || !free.Available || free.Purchasable() {
		t.Fatalf("free = %+v; want available, unpaid, not purchasable", free)
	}
	if free.Name != "Personal" || free.Policies[TranscriptRetentionDaysPolicy] != 30 {
		t.Fatalf("free = %+v; want Personal with 30-day transcript retention", free)
	}
	if !free.HasFeature("memory") || !free.HasFeature("facts") || free.HasFeature("secrets") {
		t.Fatalf("free features = %v; want memory+facts, no secrets", free.Features)
	}

	std, ok := c.Get("standard")
	if !ok || !std.Purchasable() || std.PriceCents() != 3000 {
		t.Fatalf("standard = %+v; want purchasable at 3000 cents", std)
	}
	if !std.HasFeature("secrets") || !std.HasFeature("collaboration") || !std.HasFeature("support") {
		t.Fatalf("standard features = %v; want secrets+collaboration+support", std.Features)
	}
	if std.Name != "Professional" || std.Policies[TranscriptRetentionDaysPolicy] != 90 {
		t.Fatalf("standard = %+v; want Professional with 90-day transcript retention", std)
	}

	team, ok := c.Get("team")
	if !ok || team.Available || team.Purchasable() || !team.Paid() || !team.UsageBilled {
		t.Fatalf("team = %+v; want priced + usage-billed but not available", team)
	}
	if team.Policies[TranscriptRetentionDaysPolicy] != 365 {
		t.Fatalf("team policies = %v; want 365-day transcript retention", team.Policies)
	}
	enterprise, _ := c.Get("enterprise")
	if enterprise.Available || enterprise.Purchasable() || enterprise.Paid() || !enterprise.UsageBilled {
		t.Fatalf("enterprise = %+v; want custom/unpriced + usage-billed but not available", enterprise)
	}
	if _, capped := enterprise.Policies[TranscriptRetentionDaysPolicy]; capped {
		t.Fatalf("enterprise policies = %v; want indefinite transcript retention", enterprise.Policies)
	}
	monthly := func(value int64) *int64 { return &value }
	type expectedPlan struct {
		name         string
		priceMonthly *int64
		available    bool
		usageBilled  bool
		limits       map[string]int64
		policies     map[string]int64
		features     []string
		summary      string
	}
	wantPlans := map[string]expectedPlan{
		Free: {
			name:         "Personal",
			priceMonthly: monthly(0),
			available:    true,
			limits: map[string]int64{
				AgentLimit:         10,
				AgentPerRealmLimit: 10,
				RealmLimit:         1,
				StoredSecretLimit:  0,
			},
			policies: map[string]int64{TranscriptRetentionDaysPolicy: 30},
			features: []string{"memory", "facts"},
			summary:  "Limited and capped. Agent memory and facts for up to 10 agents in one realm. No support included.",
		},
		"standard": {
			name:         "Professional",
			priceMonthly: monthly(30),
			available:    true,
			limits: map[string]int64{
				AgentLimit:         100,
				AgentPerRealmLimit: 100,
				RealmLimit:         1,
				StoredSecretLimit:  100,
			},
			policies: map[string]int64{TranscriptRetentionDaysPolicy: 90},
			features: []string{"memory", "facts", "secrets", "collaboration", "support"},
			summary:  "Capped. Memory, facts, secrets, and collaboration for up to 100 agents in one realm, support included.",
		},
		"team": {
			name:         "Team",
			priceMonthly: monthly(250),
			usageBilled:  true,
			limits: map[string]int64{
				AgentLimit:         2500,
				AgentPerRealmLimit: 100,
				RealmLimit:         25,
				StoredSecretLimit:  250,
			},
			policies: map[string]int64{TranscriptRetentionDaysPolicy: 365},
			features: []string{"memory", "facts", "secrets", "collaboration", "support"},
			summary:  "Coming soon. Everything in Professional for up to 100 agents per realm across 25 realms, plus usage-based billing.",
		},
		"enterprise": {
			name:        "Enterprise",
			usageBilled: true,
			limits:      map[string]int64{StoredSecretLimit: 1000},
			policies:    map[string]int64{},
			features:    []string{"memory", "facts", "secrets", "collaboration", "support"},
			summary:     "Coming soon. Everything in Team with custom pricing and support; details to follow.",
		},
	}
	equalOptionalInt64 := func(left, right *int64) bool {
		return left == nil && right == nil ||
			left != nil && right != nil && *left == *right
	}
	for planID, want := range wantPlans {
		plan, ok := c.Get(planID)
		if !ok {
			t.Fatalf("catalog missing plan %q", planID)
		}
		if plan.Name != want.name ||
			!equalOptionalInt64(plan.PriceMonthly, want.priceMonthly) ||
			plan.PriceMonthlyMin != nil ||
			plan.Available != want.available ||
			plan.UsageBilled != want.usageBilled ||
			!maps.Equal(plan.Limits, want.limits) ||
			!maps.Equal(plan.Policies, want.policies) ||
			!slices.Equal(plan.Features, want.features) ||
			plan.Summary != want.summary {
			t.Fatalf("%s catalog entry = %+v; want exactly %+v", planID, plan, want)
		}
	}
	for _, feature := range team.Features {
		if !enterprise.HasFeature(feature) {
			t.Fatalf("enterprise features = %v; missing Team feature %q", enterprise.Features, feature)
		}
	}
	prices := c.Prices()
	if len(prices) != 1 || prices["standard"] != 3000 {
		t.Fatalf("Prices() = %v; want exactly {standard: 3000} while team/enterprise are unavailable", prices)
	}
}

func TestParseValidation(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want string // substring of the expected error
	}{
		{"bad schema", `{"schema_version":"witself.plans.v1","plans":[{"id":"free","available":true}]}`, "schema_version"},
		{"no plans", `{"schema_version":"witself.plans.v0","plans":[]}`, "no plans"},
		{"empty id", `{"schema_version":"witself.plans.v0","plans":[{"id":""}]}`, "empty id"},
		{"duplicate id", `{"schema_version":"witself.plans.v0","plans":[{"id":"free","available":true},{"id":"free"}]}`, "duplicate"},
		{"both prices", `{"schema_version":"witself.plans.v0","plans":[{"id":"free","available":true},{"id":"x","price_monthly":1,"price_monthly_min":2}]}`, "both"},
		{"missing free", `{"schema_version":"witself.plans.v0","plans":[{"id":"standard","price_monthly":30,"available":true}]}`, `missing the "free" plan`},
		{"paid free", `{"schema_version":"witself.plans.v0","plans":[{"id":"free","price_monthly":5,"available":true}]}`, "must cost 0"},
		{"unavailable free", `{"schema_version":"witself.plans.v0","plans":[{"id":"free","available":false}]}`, "must be available"},
		{"negative limit", `{"schema_version":"witself.plans.v0","plans":[{"id":"free","available":true,"limits":{"stored_secret":-1}}]}`, "between 0"},
		{"unknown limit", `{"schema_version":"witself.plans.v0","plans":[{"id":"free","available":true,"limits":{"stored_secrets":1}}]}`, "unknown limit"},
		{"unsafe integer limit", `{"schema_version":"witself.plans.v0","plans":[{"id":"free","available":true,"limits":{"stored_secret":9007199254740992}}]}`, "between 0"},
		{"zero retention", `{"schema_version":"witself.plans.v0","plans":[{"id":"free","available":true,"policies":{"transcript_retention_days":0}}]}`, "between 1"},
		{"unknown policy", `{"schema_version":"witself.plans.v0","plans":[{"id":"free","available":true,"policies":{"transcript_retention_dayz":30}}]}`, "unknown policy"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.doc))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Parse error = %v; want substring %q", err, tc.want)
			}
		})
	}
}

func TestValidateLimitsZeroAndMissingUnlimited(t *testing.T) {
	for _, limits := range []map[string]int64{
		nil,
		{},
		{StoredSecretLimit: 0},
		{
			StoredSecretLimit:  100,
			AgentLimit:         25,
			AgentPerRealmLimit: 10,
			RealmLimit:         1,
		},
	} {
		if err := ValidateLimits(limits); err != nil {
			t.Fatalf("ValidateLimits(%v): %v", limits, err)
		}
	}
}
