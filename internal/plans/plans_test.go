package plans

import (
	"fmt"
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
	if c.Updated != "2026-08-03" {
		t.Fatalf("catalog updated = %q; want 2026-08-03", c.Updated)
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
	if free.Policies[MessageRetentionDaysPolicy] != 30 {
		t.Fatalf("free policies = %v; want 30-day disabled-mailbox cleanup retention", free.Policies)
	}
	if free.Policies[MessagingEntitlementVersionPolicy] != MessagingEntitlementVersion {
		t.Fatalf("free policies = %v; want messaging entitlement marker v%d",
			free.Policies, MessagingEntitlementVersion)
	}
	if free.Policies[AgentEmailRetentionDaysPolicy] != 30 ||
		free.Policies[AgentEmailEntitlementVersionPolicy] != AgentEmailEntitlementVersion ||
		free.HasFeature(AgentEmailReceiveFeature) {
		t.Fatalf("free = %+v; want disabled inbound email with 30-day cleanup", free)
	}
	if !free.HasFeature("memory") || !free.HasFeature("facts") ||
		free.HasFeature("secrets") || free.HasFeature(MessagingFeature) {
		t.Fatalf("free features = %v; want memory+facts, no secrets or messaging", free.Features)
	}

	std, ok := c.Get("standard")
	if !ok || !std.Purchasable() || std.PriceCents() != 3000 {
		t.Fatalf("standard = %+v; want purchasable at 3000 cents", std)
	}
	if !std.HasFeature("secrets") || !std.HasFeature(MessagingFeature) ||
		!std.HasFeature("collaboration") || !std.HasFeature("support") {
		t.Fatalf("standard features = %v; want secrets+messaging+collaboration+support", std.Features)
	}
	if std.Name != "Professional" || std.Policies[TranscriptRetentionDaysPolicy] != 90 {
		t.Fatalf("standard = %+v; want Professional with 90-day transcript retention", std)
	}
	if std.Policies[MessageRetentionDaysPolicy] != 90 {
		t.Fatalf("standard policies = %v; want 90-day message retention", std.Policies)
	}
	if std.Policies[AgentEmailRetentionDaysPolicy] != 90 ||
		!std.HasFeature(AgentEmailReceiveFeature) {
		t.Fatalf("standard = %+v; want inbound email with 90-day retention", std)
	}

	team, ok := c.Get("team")
	if !ok || team.Available || team.Purchasable() || !team.Paid() || !team.UsageBilled {
		t.Fatalf("team = %+v; want priced + usage-billed but not available", team)
	}
	if team.Policies[TranscriptRetentionDaysPolicy] != 365 {
		t.Fatalf("team policies = %v; want 365-day transcript retention", team.Policies)
	}
	if team.Policies[MessageRetentionDaysPolicy] != 365 ||
		!team.HasFeature(MessagingFeature) {
		t.Fatalf("team = %+v; want messaging with 365-day retention", team)
	}
	if team.Policies[AgentEmailRetentionDaysPolicy] != 365 ||
		!team.HasFeature(AgentEmailReceiveFeature) {
		t.Fatalf("team = %+v; want inbound email with 365-day retention", team)
	}
	enterprise, _ := c.Get("enterprise")
	if enterprise.Available || enterprise.Purchasable() || enterprise.Paid() || !enterprise.UsageBilled {
		t.Fatalf("enterprise = %+v; want custom/unpriced + usage-billed but not available", enterprise)
	}
	if _, capped := enterprise.Policies[TranscriptRetentionDaysPolicy]; capped {
		t.Fatalf("enterprise policies = %v; want indefinite transcript retention", enterprise.Policies)
	}
	if enterprise.Policies[MessageRetentionDaysPolicy] != 365 ||
		!enterprise.HasFeature(MessagingFeature) {
		t.Fatalf("enterprise = %+v; want messaging with 365-day default retention", enterprise)
	}
	if enterprise.Policies[AgentEmailRetentionDaysPolicy] != 365 ||
		!enterprise.HasFeature(AgentEmailReceiveFeature) {
		t.Fatalf("enterprise = %+v; want inbound email with 365-day default retention", enterprise)
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
				AgentEmailAttachmentStorageBytesLimit:  0,
				AgentEmailCustomDomainsPerAccountLimit: 0,
				AgentEmailMaxRawBytesLimit:             0,
				AgentEmailRealmAliasesPerRealmLimit:    0,
				AgentLimit:                             10,
				AgentPerRealmLimit:                     10,
				RealmLimit:                             1,
				StoredFactLimit:                        1000,
				StoredMemoryLimit:                      1000,
				StoredSecretLimit:                      0,
			},
			policies: map[string]int64{
				AgentEmailEntitlementVersionPolicy: AgentEmailEntitlementVersion,
				AgentEmailRetentionDaysPolicy:      30,
				MessageRetentionDaysPolicy:         30,
				MessagingEntitlementVersionPolicy:  MessagingEntitlementVersion,
				TranscriptRetentionDaysPolicy:      30,
			},
			features: []string{"memory", "facts"},
			summary:  "Limited and capped. Agent memory and facts for up to 10 agents in one realm. No support included.",
		},
		"standard": {
			name:         "Professional",
			priceMonthly: monthly(30),
			available:    true,
			limits: map[string]int64{
				AgentEmailAttachmentStorageBytesLimit:   5 * 1024 * 1024 * 1024,
				AgentEmailCustomDomainsPerAccountLimit:  0,
				AgentEmailMaxRawBytesLimit:              10 * 1024 * 1024,
				AgentEmailRealmAliasesPerRealmLimit:     0,
				AgentLimit:                              100,
				AgentPerRealmLimit:                      100,
				MessageDeliveredPerRealmMinuteLimit:     500,
				MessageDeliveredPerRecipientMinuteLimit: 60,
				MessageSentPerAgentMinuteLimit:          30,
				RealmLimit:                              1,
				StoredFactLimit:                         10000,
				StoredMemoryLimit:                       10000,
				StoredSecretLimit:                       100,
			},
			policies: map[string]int64{
				AgentEmailEntitlementVersionPolicy: AgentEmailEntitlementVersion,
				AgentEmailRetentionDaysPolicy:      90,
				MessageRetentionDaysPolicy:         90,
				MessagingEntitlementVersionPolicy:  MessagingEntitlementVersion,
				TranscriptRetentionDaysPolicy:      90,
			},
			features: []string{"memory", "facts", "secrets", MessagingFeature, AgentEmailReceiveFeature, "collaboration", "support"},
			summary:  "Capped. Memory, facts, secrets, and collaboration for up to 100 agents in one realm, support included.",
		},
		"team": {
			name:         "Team",
			priceMonthly: monthly(250),
			usageBilled:  true,
			limits: map[string]int64{
				AgentEmailAttachmentStorageBytesLimit:   100 * 1024 * 1024 * 1024,
				AgentEmailCustomDomainsPerAccountLimit:  1,
				AgentEmailMaxRawBytesLimit:              25 * 1024 * 1024,
				AgentEmailRealmAliasesPerRealmLimit:     1,
				AgentLimit:                              2500,
				AgentPerRealmLimit:                      100,
				MessageDeliveredPerRealmMinuteLimit:     5000,
				MessageDeliveredPerRecipientMinuteLimit: 300,
				MessageSentPerAgentMinuteLimit:          120,
				RealmLimit:                              25,
				StoredFactLimit:                         50000,
				StoredMemoryLimit:                       50000,
				StoredSecretLimit:                       250,
			},
			policies: map[string]int64{
				AgentEmailEntitlementVersionPolicy: AgentEmailEntitlementVersion,
				AgentEmailRetentionDaysPolicy:      365,
				MessageRetentionDaysPolicy:         365,
				MessagingEntitlementVersionPolicy:  MessagingEntitlementVersion,
				TranscriptRetentionDaysPolicy:      365,
			},
			features: []string{"memory", "facts", "secrets", MessagingFeature, AgentEmailReceiveFeature, AgentEmailRealmAliasFeature, AgentEmailCustomDomainFeature, "collaboration", "support"},
			summary:  "Coming soon. Everything in Professional for up to 100 agents per realm across 25 realms, plus usage-based billing.",
		},
		"enterprise": {
			name:        "Enterprise",
			usageBilled: true,
			limits: map[string]int64{
				AgentEmailAttachmentStorageBytesLimit:   100 * 1024 * 1024 * 1024,
				AgentEmailCustomDomainsPerAccountLimit:  0,
				AgentEmailMaxRawBytesLimit:              25 * 1024 * 1024,
				AgentEmailRealmAliasesPerRealmLimit:     3,
				MessageDeliveredPerRealmMinuteLimit:     25000,
				MessageDeliveredPerRecipientMinuteLimit: 1000,
				MessageSentPerAgentMinuteLimit:          600,
				StoredFactLimit:                         250000,
				StoredMemoryLimit:                       250000,
				StoredSecretLimit:                       1000,
			},
			policies: map[string]int64{
				AgentEmailEntitlementVersionPolicy: AgentEmailEntitlementVersion,
				AgentEmailRetentionDaysPolicy:      365,
				MessageRetentionDaysPolicy:         365,
				MessagingEntitlementVersionPolicy:  MessagingEntitlementVersion,
			},
			features: []string{"memory", "facts", "secrets", MessagingFeature, AgentEmailReceiveFeature, AgentEmailRealmAliasFeature, AgentEmailCustomDomainFeature, "collaboration", "support"},
			summary:  "Coming soon. Everything in Team with custom pricing and support; details to follow.",
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

// Phase B publishes the commercial defaults only after Phase A binaries and
// the Founder explicit-unlimited override have converged. Enterprise keeps the
// feature but defaults to zero so every non-Founder allowance is contracted.
func TestCanonicalAgentEmailCustomDomainDefaultsPhaseB(t *testing.T) {
	catalog, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	wants := map[string]struct {
		limit   int64
		feature bool
	}{
		Free:         {limit: 0},
		"standard":   {limit: 0},
		"team":       {limit: 1, feature: true},
		"enterprise": {limit: 0, feature: true},
	}
	for planID, want := range wants {
		plan, ok := catalog.Get(planID)
		if !ok {
			t.Fatalf("catalog missing plan %q", planID)
		}
		if got, present := plan.Limits[AgentEmailCustomDomainsPerAccountLimit]; !present || got != want.limit {
			t.Errorf("%s %s = %d, present=%t; want %d",
				planID, AgentEmailCustomDomainsPerAccountLimit,
				got, present, want.limit)
		}
		if got := plan.HasFeature(AgentEmailCustomDomainFeature); got != want.feature {
			t.Errorf("%s %s feature = %t; want %t",
				planID, AgentEmailCustomDomainFeature, got, want.feature)
		}
	}
}

func TestCanonicalStoredFactDefaultsPhaseB(t *testing.T) {
	catalog, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for planID, want := range map[string]int64{
		Free:         1000,
		"standard":   10000,
		"team":       50000,
		"enterprise": 250000,
	} {
		plan, ok := catalog.Get(planID)
		if !ok {
			t.Fatalf("catalog missing plan %q", planID)
		}
		if got, present := plan.Limits[StoredFactLimit]; !present || got != want {
			t.Errorf("%s stored_fact = %d, present=%t; want %d",
				planID, got, present, want)
		}
	}
}

func TestCanonicalAgentEmailLimitDefaultsPhaseB(t *testing.T) {
	catalog, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for planID, want := range map[string]struct {
		rawBytes        int64
		attachmentBytes int64
		realmAliases    int64
		aliasFeature    bool
	}{
		Free: {
			rawBytes: 0, attachmentBytes: 0, realmAliases: 0,
		},
		"standard": {
			rawBytes: 10 * 1024 * 1024, attachmentBytes: 5 * 1024 * 1024 * 1024,
			realmAliases: 0,
		},
		"team": {
			rawBytes: 25 * 1024 * 1024, attachmentBytes: 100 * 1024 * 1024 * 1024,
			realmAliases: 1, aliasFeature: true,
		},
		"enterprise": {
			rawBytes: 25 * 1024 * 1024, attachmentBytes: 100 * 1024 * 1024 * 1024,
			realmAliases: 3, aliasFeature: true,
		},
	} {
		plan, ok := catalog.Get(planID)
		if !ok {
			t.Fatalf("catalog missing plan %q", planID)
		}
		if got, present := plan.Limits[AgentEmailMaxRawBytesLimit]; !present || got != want.rawBytes {
			t.Errorf("%s %s = %d, present=%t; want %d",
				planID, AgentEmailMaxRawBytesLimit, got, present, want.rawBytes)
		}
		if got, present := plan.Limits[AgentEmailAttachmentStorageBytesLimit]; !present || got != want.attachmentBytes {
			t.Errorf("%s %s = %d, present=%t; want %d",
				planID, AgentEmailAttachmentStorageBytesLimit, got, present,
				want.attachmentBytes)
		}
		if got, present := plan.Limits[AgentEmailRealmAliasesPerRealmLimit]; !present || got != want.realmAliases {
			t.Errorf("%s %s = %d, present=%t; want %d",
				planID, AgentEmailRealmAliasesPerRealmLimit, got, present,
				want.realmAliases)
		}
		if got := plan.HasFeature(AgentEmailRealmAliasFeature); got != want.aliasFeature {
			t.Errorf("%s %s feature = %t; want %t",
				planID, AgentEmailRealmAliasFeature, got, want.aliasFeature)
		}
	}
}

func TestCanonicalMessageRateLimitDefaultsPhaseB(t *testing.T) {
	catalog, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	wantByPlan := map[string]map[string]int64{
		Free: {},
		"standard": {
			MessageSentPerAgentMinuteLimit:          30,
			MessageDeliveredPerRealmMinuteLimit:     500,
			MessageDeliveredPerRecipientMinuteLimit: 60,
		},
		"team": {
			MessageSentPerAgentMinuteLimit:          120,
			MessageDeliveredPerRealmMinuteLimit:     5000,
			MessageDeliveredPerRecipientMinuteLimit: 300,
		},
		"enterprise": {
			MessageSentPerAgentMinuteLimit:          600,
			MessageDeliveredPerRealmMinuteLimit:     25000,
			MessageDeliveredPerRecipientMinuteLimit: 1000,
		},
	}
	for planID, want := range wantByPlan {
		plan, ok := catalog.Get(planID)
		if !ok {
			t.Fatalf("catalog missing plan %q", planID)
		}
		for _, dimension := range []string{
			MessageSentPerAgentMinuteLimit,
			MessageDeliveredPerRealmMinuteLimit,
			MessageDeliveredPerRecipientMinuteLimit,
		} {
			got, present := plan.Limits[dimension]
			wantValue, wantPresent := want[dimension]
			if present != wantPresent || (present && got != wantValue) {
				t.Errorf("%s %s = %d, present=%t; want %d, present=%t",
					planID, dimension, got, present, wantValue, wantPresent)
			}
		}
	}
}

func TestCanonicalAgentEmailRateLimitsRemainPlatformOnly(t *testing.T) {
	catalog, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	dimensions := []string{
		AgentEmailReceivedPerSenderMinuteLimit,
		AgentEmailReceivedPerRecipientMinuteLimit,
		AgentEmailReceivedPerRealmMinuteLimit,
		AgentEmailReceivedBytesPerSenderMinuteLimit,
		AgentEmailReceivedBytesPerRecipientMinuteLimit,
		AgentEmailReceivedBytesPerRealmMinuteLimit,
	}
	for _, plan := range catalog.Plans {
		for _, dimension := range dimensions {
			if got, present := plan.Limits[dimension]; present {
				t.Errorf("%s %s = %d, present=true; want key omitted so the platform breaker applies",
					plan.ID, dimension, got)
			}
		}
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
		{"raw email above service ceiling", `{"schema_version":"witself.plans.v0","plans":[{"id":"free","available":true,"limits":{"agent_email_max_raw_bytes":26214401}}]}`, "between 0 and 26214400"},
		{"agent send rate above platform maximum", `{"schema_version":"witself.plans.v0","plans":[{"id":"free","available":true,"limits":{"message_sent_per_agent_minute":2001}}]}`, "between 0 and 2000"},
		{"realm delivery rate above platform maximum", `{"schema_version":"witself.plans.v0","plans":[{"id":"free","available":true,"limits":{"message_delivered_per_realm_minute":100001}}]}`, "between 0 and 100000"},
		{"recipient delivery rate above platform maximum", `{"schema_version":"witself.plans.v0","plans":[{"id":"free","available":true,"limits":{"message_delivered_per_recipient_minute":5001}}]}`, "between 0 and 5000"},
		{"unknown agent email rate limit", `{"schema_version":"witself.plans.v0","plans":[{"id":"free","available":true,"limits":{"agent_email_received_per_source_minute":1}}]}`, "unknown limit"},
		{"zero retention", `{"schema_version":"witself.plans.v0","plans":[{"id":"free","available":true,"policies":{"transcript_retention_days":0}}]}`, "between 1"},
		{"zero message retention", `{"schema_version":"witself.plans.v0","plans":[{"id":"free","available":true,"policies":{"message_retention_days":0}}]}`, "between 1"},
		{"bad messaging entitlement marker", `{"schema_version":"witself.plans.v0","plans":[{"id":"free","available":true,"policies":{"messaging_entitlement_version":2}}]}`, "must be 1"},
		{"zero agent email retention", `{"schema_version":"witself.plans.v0","plans":[{"id":"free","available":true,"policies":{"agent_email_retention_days":0}}]}`, "between 1"},
		{"bad agent email entitlement marker", `{"schema_version":"witself.plans.v0","plans":[{"id":"free","available":true,"policies":{"agent_email_entitlement_version":2}}]}`, "must be 1"},
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

func TestValidateAgentEmailLimitBounds(t *testing.T) {
	if err := ValidateLimits(map[string]int64{
		AgentEmailMaxRawBytesLimit:             MaxAgentEmailRawBytes,
		AgentEmailAttachmentStorageBytesLimit:  107_374_182_400,
		AgentEmailRealmAliasesPerRealmLimit:    1,
		AgentEmailCustomDomainsPerAccountLimit: 1,
	}); err != nil {
		t.Fatalf("valid agent-email limits: %v", err)
	}
	if err := ValidateLimits(map[string]int64{
		AgentEmailMaxRawBytesLimit: MaxAgentEmailRawBytes + 1,
	}); err == nil || !strings.Contains(err.Error(), "between 0 and 26214400") {
		t.Fatalf("above-ceiling raw limit error = %v", err)
	}
}

func TestValidateMessageRateLimitBounds(t *testing.T) {
	limits := map[string]int64{
		MessageSentPerAgentMinuteLimit:          MaxMessageSentPerAgentMinute,
		MessageDeliveredPerRealmMinuteLimit:     MaxMessageDeliveredPerRealmMinute,
		MessageDeliveredPerRecipientMinuteLimit: MaxMessageDeliveredPerRecipientMinute,
	}
	if err := ValidateLimits(limits); err != nil {
		t.Fatalf("valid message-rate limits: %v", err)
	}
	for _, tc := range []struct {
		dimension string
		maximum   int64
	}{
		{MessageSentPerAgentMinuteLimit, MaxMessageSentPerAgentMinute},
		{MessageDeliveredPerRealmMinuteLimit, MaxMessageDeliveredPerRealmMinute},
		{MessageDeliveredPerRecipientMinuteLimit, MaxMessageDeliveredPerRecipientMinute},
	} {
		t.Run(tc.dimension, func(t *testing.T) {
			err := ValidateLimits(map[string]int64{tc.dimension: tc.maximum + 1})
			want := fmt.Sprintf("between 0 and %d", tc.maximum)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("above-maximum rate error = %v; want substring %q", err, want)
			}
		})
	}
}

func TestValidateAgentEmailRateLimitBounds(t *testing.T) {
	limits := map[string]int64{
		AgentEmailReceivedPerSenderMinuteLimit:         MaxAgentEmailReceivedPerSenderMinute,
		AgentEmailReceivedPerRecipientMinuteLimit:      MaxAgentEmailReceivedPerRecipientMinute,
		AgentEmailReceivedPerRealmMinuteLimit:          MaxAgentEmailReceivedPerRealmMinute,
		AgentEmailReceivedBytesPerSenderMinuteLimit:    MaxAgentEmailReceivedBytesPerSenderMinute,
		AgentEmailReceivedBytesPerRecipientMinuteLimit: MaxAgentEmailReceivedBytesPerRecipientMinute,
		AgentEmailReceivedBytesPerRealmMinuteLimit:     MaxAgentEmailReceivedBytesPerRealmMinute,
	}
	if err := ValidateLimits(limits); err != nil {
		t.Fatalf("valid agent-email rate limits: %v", err)
	}
	for dimension, maximum := range limits {
		t.Run(dimension, func(t *testing.T) {
			err := ValidateLimits(map[string]int64{dimension: maximum + 1})
			want := fmt.Sprintf("between 0 and %d", maximum)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("above-maximum rate error = %v; want substring %q", err, want)
			}
		})
	}
}

func TestValidateLimitsZeroAndMissingUnlimited(t *testing.T) {
	for _, limits := range []map[string]int64{
		nil,
		{},
		{StoredSecretLimit: 0},
		{AgentEmailCustomDomainsPerAccountLimit: 0},
		{
			MessageSentPerAgentMinuteLimit:          0,
			MessageDeliveredPerRealmMinuteLimit:     0,
			MessageDeliveredPerRecipientMinuteLimit: 0,
		},
		{
			AgentEmailReceivedPerSenderMinuteLimit:         0,
			AgentEmailReceivedPerRecipientMinuteLimit:      0,
			AgentEmailReceivedPerRealmMinuteLimit:          0,
			AgentEmailReceivedBytesPerSenderMinuteLimit:    0,
			AgentEmailReceivedBytesPerRecipientMinuteLimit: 0,
			AgentEmailReceivedBytesPerRealmMinuteLimit:     0,
		},
		{
			StoredSecretLimit:                       100,
			StoredMemoryLimit:                       1000,
			StoredFactLimit:                         1000,
			AgentLimit:                              25,
			AgentPerRealmLimit:                      10,
			RealmLimit:                              1,
			AgentEmailMaxRawBytesLimit:              10_485_760,
			AgentEmailAttachmentStorageBytesLimit:   5_368_709_120,
			MessageSentPerAgentMinuteLimit:          30,
			MessageDeliveredPerRealmMinuteLimit:     500,
			MessageDeliveredPerRecipientMinuteLimit: 60,
		},
	} {
		if err := ValidateLimits(limits); err != nil {
			t.Fatalf("ValidateLimits(%v): %v", limits, err)
		}
	}
}
