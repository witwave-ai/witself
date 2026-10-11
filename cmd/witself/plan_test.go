package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	witself "github.com/witwave-ai/witself"
	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/plans"
)

func TestPlanListShowsCanonicalPolicyMatrix(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/plans" || r.Header.Get("Authorization") != "" {
			t.Fatalf("catalog request = %s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(witself.PlansJSON)
	}))
	t.Cleanup(srv.Close)

	stdout, stderr, code := capturePlanCLI(t, func() int {
		return planList([]string{"--endpoint", srv.URL})
	})
	if code != 0 || stderr != "" {
		t.Fatalf("plan list = %d stderr=%q", code, stderr)
	}
	for _, want := range []string{
		"Personal", "Professional*", "Team", "Enterprise", "Most popular",
		"Active memories / agent", "Transcript retention", "Receive-email entitlement",
		"Send-email entitlement",
		"90 days", "365 days", "No catalog cap", "Entitlements do not prove delivery readiness",
		"Coming soon in a capability or allowance row means the entitlement is listed but not yet delivered to customer accounts.",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("plan list omitted %q:\n%s", want, stdout)
		}
	}
}

func TestPlanListJSONCanFilterUnavailablePlans(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(witself.PlansJSON)
	}))
	t.Cleanup(srv.Close)

	stdout, stderr, code := capturePlanCLI(t, func() int {
		return planList([]string{"--endpoint", srv.URL, "--available-only", "--json"})
	})
	if code != 0 || stderr != "" {
		t.Fatalf("plan list JSON = %d stderr=%q", code, stderr)
	}
	var doc struct {
		SchemaVersion      string       `json:"schema_version"`
		Updated            string       `json:"updated"`
		ComingSoonFeatures []string     `json:"coming_soon_features"`
		Plans              []plans.Plan `json:"plans"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("decode output: %v\n%s", err, stdout)
	}
	if doc.SchemaVersion != plans.SchemaVersion || doc.Updated != "2026-10-10" || len(doc.Plans) != 3 {
		t.Fatalf("filtered catalog = %+v", doc)
	}
	catalog, err := plans.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(doc.ComingSoonFeatures, catalog.ComingSoonFeatures) {
		t.Errorf("coming_soon_features = %v; want %v", doc.ComingSoonFeatures, catalog.ComingSoonFeatures)
	}
	if doc.Plans[0].ID != plans.Free || doc.Plans[1].ID != "standard" ||
		!doc.Plans[1].Recommended || doc.Plans[1].Badge != "Most popular" ||
		doc.Plans[2].ID != "team" {
		t.Fatalf("available plans = %+v", doc.Plans)
	}
}

func TestPlanListMarksComingSoonFeatures(t *testing.T) {
	catalog, err := plans.Load()
	if err != nil {
		t.Fatal(err)
	}
	const footer = "Coming soon in a capability or allowance row means the entitlement is listed but not yet delivered to customer accounts."
	gatedRows := []struct {
		label        string
		feature      string
		personal     string
		professional string
	}{
		{"Receive-email entitlement", plans.AgentEmailReceiveFeature, "Disabled", ""},
		{"Send-email entitlement", plans.AgentEmailSendFeature, "Disabled", "Disabled"},
		{"Email retention", plans.AgentEmailReceiveFeature, "30 days cleanup", ""},
		{"Maximum raw email", plans.AgentEmailReceiveFeature, "0", ""},
		{"Attachment storage / account", plans.AgentEmailReceiveFeature, "0", ""},
		{"Realm email aliases / realm", plans.AgentEmailRealmAliasFeature, "0", "0"},
		{"Custom inbound domains", plans.AgentEmailCustomDomainFeature, "0", "0"},
	}
	rowCells := func(t *testing.T, catalog *plans.Catalog, entries []plans.Plan) map[string][]string {
		t.Helper()
		rows := make(map[string][]string)
		for _, row := range planCatalogRows(catalog, entries) {
			if _, exists := rows[row.label]; exists {
				t.Fatalf("duplicate row %q", row.label)
			}
			if len(row.cells) != len(entries) {
				t.Fatalf("row %q has %d cells; want %d", row.label, len(row.cells), len(entries))
			}
			rows[row.label] = row.cells
		}
		return rows
	}
	t.Run("canonical", func(t *testing.T) {
		// Require the gates independently of the display rule, so removing a
		// canonical marker cannot make the expected cells silently change too.
		for _, gate := range gatedRows {
			if !catalog.FeatureComingSoon(gate.feature) {
				t.Errorf("canonical catalog must mark %s coming soon", gate.feature)
			}
		}
		for _, reverse := range []bool{false, true} {
			entries := slices.Clone(catalog.Plans)
			if reverse {
				slices.Reverse(entries)
			}
			rows := rowCells(t, catalog, entries)
			for _, gate := range gatedRows {
				cells, exists := rows[gate.label]
				if !exists {
					t.Fatalf("missing row %q", gate.label)
				}
				for i, plan := range entries {
					want := gate.personal
					if plan.ID != plans.Free {
						want = gate.professional
					}
					comingSoon := plan.HasFeature(gate.feature) && catalog.FeatureComingSoon(gate.feature)
					if comingSoon {
						want = "Coming soon"
					}
					if cells[i] != want || (cells[i] == "Coming soon") != comingSoon || cells[i] == "Enabled" {
						t.Errorf("%s / %s = %q; want %q (coming soon=%t)", gate.label, plan.Name, cells[i], want, comingSoon)
					}
				}
			}
		}
		for _, tc := range []struct {
			name    string
			entries []plans.Plan
			count   int
		}{
			{"all plans", catalog.Plans, 1},
			{"personal only", catalog.Plans[:1], 0},
		} {
			stdout, stderr, code := capturePlanCLI(t, func() int {
				printPlanCatalog(catalog, tc.entries)
				return 0
			})
			if code != 0 || stderr != "" || strings.Count(stdout, footer) != tc.count {
				t.Errorf("%s footer count = %d; want %d (code=%d stderr=%q)", tc.name, strings.Count(stdout, footer), tc.count, code, stderr)
			}
		}
	})
	t.Run("marker removed", func(t *testing.T) {
		var doc map[string]any
		if err := json.Unmarshal(witself.PlansJSON, &doc); err != nil {
			t.Fatal(err)
		}
		delete(doc, "coming_soon_features")
		raw, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		unmarked, err := plans.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		rows := rowCells(t, unmarked, unmarked.Plans)
		receiveCells, exists := rows["Receive-email entitlement"]
		if !exists {
			t.Fatal("unmarked catalog has no receive row")
		}
		foundTeam := false
		for i, plan := range unmarked.Plans {
			if plan.ID == "team" {
				foundTeam = true
				if got := receiveCells[i]; got != "Enabled" {
					t.Errorf("unmarked Team receive = %q; want Enabled", got)
				}
			}
		}
		if !foundTeam {
			t.Fatal("unmarked catalog has no Team plan")
		}
		stdout, stderr, code := capturePlanCLI(t, func() int {
			printPlanCatalog(unmarked, unmarked.Plans)
			return 0
		})
		if code != 0 || stderr != "" || strings.Contains(stdout, footer) {
			t.Errorf("unmarked catalog footer present=%t (code=%d stderr=%q)", strings.Contains(stdout, footer), code, stderr)
		}
	})
}

func TestResolvePlanTargetAcceptsStableIDAndCustomerName(t *testing.T) {
	catalog, err := plans.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{"standard", "STANDARD", "Professional", "professional"} {
		id, name, err := resolvePlanTarget(catalog, input)
		if err != nil || id != "standard" || name != "Professional" {
			t.Errorf("resolve %q = %q %q %v", input, id, name, err)
		}
	}
	if _, _, err := resolvePlanTarget(catalog, "premium"); err == nil ||
		!strings.Contains(err.Error(), "Professional (standard)") {
		t.Fatalf("unknown target error = %v", err)
	}
}

func TestPlanChangeRejectsTrailingFlagsBeforeAccountOrNetworkAccess(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	stdout, stderr, code := capturePlanCLI(t, func() int {
		return planChangeCLI("upgrade", []string{"standard", "--account", "team"})
	})
	if code != 2 || stdout != "" || !strings.Contains(stderr, "usage: witself plan upgrade") {
		t.Fatalf("trailing flags = %d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestPlanCancelRejectsPositionalsBeforeAccountOrNetworkAccess(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	stdout, stderr, code := capturePlanCLI(t, func() int {
		return planCancelCLI([]string{"junk", "--account", "team"})
	})
	if code != 2 || stdout != "" || !strings.Contains(stderr, "usage: witself plan cancel") {
		t.Fatalf("trailing flags = %d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestPlanMutationGuardFlagsFailBeforeAccountOrNetworkAccess(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	tests := []struct {
		name string
		call func() int
		want string
	}{
		{
			name: "reason required",
			call: func() int {
				return planChangeCLI("upgrade", []string{"--dry-run", "standard"})
			},
			want: "--reason is required",
		},
		{
			name: "dry run rejects apply guards",
			call: func() int {
				return planChangeCLI("upgrade", []string{
					"--reason", "test", "--dry-run", "--yes", "standard",
				})
			},
			want: "--dry-run cannot be combined",
		},
		{
			name: "key required",
			call: func() int {
				return planCancelCLI([]string{"--reason", "test", "--yes"})
			},
			want: "requires --yes and --idempotency-key",
		},
		{
			name: "confirmation required",
			call: func() int {
				return planCancelCLI([]string{
					"--reason", "test", "--idempotency-key", "cancel-key",
				})
			},
			want: "requires --yes and --idempotency-key",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code := capturePlanCLI(t, tc.call)
			if code != 2 || stdout != "" || !strings.Contains(stderr, tc.want) {
				t.Fatalf("code=%d stdout=%q stderr=%q; want %q", code, stdout, stderr, tc.want)
			}
		})
	}
}

func TestPlanMutationJSONPreservesFalseReplayWithoutZeroEffective(t *testing.T) {
	stdout, stderr, code := capturePlanCLI(t, func() int {
		return printBillingMutationOutcome(client.PlanOutcome{
			OperationID: "bop_upgrade", Operation: client.BillingMutationUpgrade,
			ActorID: "opr_owner", ActorRole: "account_owner", Confirmed: true,
			Kind: "done", Plan: "standard",
		})
	})
	if code != 0 || stderr != "" {
		t.Fatalf("JSON outcome = %d stderr=%q", code, stderr)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("decode outcome: %v", err)
	}
	if doc["operation_id"] != "bop_upgrade" || doc["confirmed"] != true ||
		doc["replayed"] != false || doc["kind"] != "done" {
		t.Fatalf("JSON outcome = %v", doc)
	}
	if _, present := doc["effective"]; present {
		t.Fatalf("zero effective time leaked into JSON outcome: %v", doc)
	}
}

func TestPlanResolvedCancelOutput(t *testing.T) {
	t.Run("human", func(t *testing.T) {
		stdout, stderr, code := capturePlanCLI(t, func() int {
			return printPlanCancelOutcome(client.PlanOutcome{Kind: "resolved"})
		})
		if code != 0 || stderr != "" ||
			stdout != "pending change was already resolved; no cancellation was applied\n" {
			t.Fatalf("resolved human output = code %d stdout=%q stderr=%q", code, stdout, stderr)
		}
	})

	t.Run("json", func(t *testing.T) {
		stdout, stderr, code := capturePlanCLI(t, func() int {
			return printBillingMutationOutcome(client.PlanOutcome{
				OperationID: "bop_resolved", Operation: client.BillingMutationCancel,
				ActorID: "opr_owner", ActorRole: "account_owner", Confirmed: true,
				Kind: "resolved",
			})
		})
		if code != 0 || stderr != "" {
			t.Fatalf("resolved JSON output = code %d stderr=%q", code, stderr)
		}
		var doc map[string]any
		if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
			t.Fatal(err)
		}
		if doc["kind"] != "resolved" || doc["operation"] != "plan_cancel" {
			t.Fatalf("resolved JSON output = %v", doc)
		}
		for _, forbidden := range []string{"cancelled", "plan", "url", "effective"} {
			if _, present := doc[forbidden]; present {
				t.Fatalf("resolved JSON output exposed %q: %v", forbidden, doc)
			}
		}
	})
}

func TestPlanStatusShowsEffectivePolicyAndOverrides(t *testing.T) {
	days30, days60, days90 := int64(30), int64(60), int64(90)
	status := client.PlanStatus{
		SchemaVersion:   "witself.v0",
		Plan:            "standard",
		PlanName:        "Professional",
		BillingPlan:     "standard",
		BillingPlanName: "Professional",
		Applied:         "standard",
		Limits: map[string]int64{
			plans.StoredMemoryLimit:                 20_000,
			plans.AgentEmailSentPerAgentMinuteLimit: 12,
		},
		LimitDefaults:   map[string]int64{plans.StoredMemoryLimit: 10_000},
		Policies:        map[string]int64{plans.TranscriptRetentionDaysPolicy: 60},
		PolicyDefaults:  map[string]int64{plans.TranscriptRetentionDaysPolicy: 90},
		Features:        []string{plans.AgentEmailReceiveFeature, plans.AgentEmailSendFeature, "memory"},
		FeatureDefaults: []string{"memory"},
		Transcript: &client.PlanRetentionStatus{
			DefaultDays: &days90, EffectiveDays: &days60, Overridden: true,
		},
		Messaging: &client.PlanFeatureStatus{
			DefaultEnabled: true, Enabled: false, Overridden: true,
		},
		MessageRetention: &client.PlanRetentionStatus{
			DefaultDays: &days90, EffectiveDays: &days30,
		},
		EmailReceive: &client.PlanFeatureStatus{
			DefaultEnabled: false, Enabled: true, Overridden: true,
		},
		EmailSend: &client.PlanFeatureStatus{
			DefaultEnabled: false, Enabled: true, Overridden: true,
		},
		EmailRetention: &client.PlanRetentionStatus{
			DefaultDays: &days90, EffectiveDays: nil, Overridden: true,
		},
	}

	stdout, stderr, code := capturePlanCLI(t, func() int {
		printPlanStatus(status, true)
		return 0
	})
	if code != 0 || stderr != "" {
		t.Fatalf("print status = %d stderr=%q", code, stderr)
	}
	for _, want := range []string{
		"Professional (standard)",
		"transcripts: 60 days (account override; plan default 90 days)",
		"messaging:   disabled (account override; plan default enabled)",
		"email entitlement: enabled (account override; plan default disabled)",
		"email delivery:    not reported by plan status (separate rollout gates)",
		"email sending:     enabled (account override; plan default disabled)",
		"email data:  indefinite (account override; plan default 90 days) retention",
		"agent_email_receive (account override)",
		"agent_email_send (account override)",
		"stored_memory: 20,000 (account override; plan default 10,000)",
		"agent_email_sent_per_agent_minute: 12 (account override; plan default no plan cap)",
		"agent_email_sent_per_realm_minute: no plan cap",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("plan status omitted %q:\n%s", want, stdout)
		}
	}
}

func TestPlanHumanOutputNeutralizesTerminalControls(t *testing.T) {
	status := client.PlanStatus{
		Plan:         "standard\x1b[2J\nforged",
		PlanName:     "Professional\u009b31m\u202e\tspoofed\u2069",
		Applied:      "standard\x1b[2J\nforged",
		ApplyBlocked: "blocked\x1b]0;owned\a\u2028next-line",
		Features:     []string{"email\x1b[31m\u2029forged\u2066"},
	}

	stdout, stderr, code := capturePlanCLI(t, func() int {
		printPlanStatus(status, true)
		return 0
	})
	if code != 0 || stderr != "" {
		t.Fatalf("print status = %d stderr=%q", code, stderr)
	}
	if strings.ContainsAny(stdout, "\x1b\u009b\a\t\u202e\u2066\u2069\u2028\u2029") {
		t.Fatalf("plan status retained terminal controls: %q", stdout)
	}
	for _, want := range []string{
		"Professional31m spoofed (standard[2J forged)",
		"blocked:  blocked]0;owned next-line",
		"email[31m forged (account override)",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("sanitized status omitted %q:\n%s", want, stdout)
		}
	}
}

func TestRootHelpAdvertisesPlanAndEmailSurfaces(t *testing.T) {
	var out bytes.Buffer
	usage(&out)
	for _, want := range []string{
		"witself plan list|status|upgrade|downgrade|cancel",
		"witself email status|address|list|listen|read",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("help omitted %q", want)
		}
	}
}

func capturePlanCLI(t *testing.T, command func() int) (stdout, stderr string, code int) {
	t.Helper()
	outReader, outWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errReader, errWriter, err := os.Pipe()
	if err != nil {
		_ = outReader.Close()
		_ = outWriter.Close()
		t.Fatal(err)
	}
	oldStdout, oldStderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outWriter, errWriter
	code = command()
	os.Stdout, os.Stderr = oldStdout, oldStderr
	_ = outWriter.Close()
	_ = errWriter.Close()
	outBytes, outErr := io.ReadAll(outReader)
	errBytes, errErr := io.ReadAll(errReader)
	_ = outReader.Close()
	_ = errReader.Close()
	if outErr != nil || errErr != nil {
		t.Fatalf("read captured output: stdout=%v stderr=%v", outErr, errErr)
	}
	return string(outBytes), string(errBytes), code
}
