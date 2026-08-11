package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
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
		"90 days", "365 days", "No catalog cap", "Entitlements do not prove delivery readiness",
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
		SchemaVersion string       `json:"schema_version"`
		Updated       string       `json:"updated"`
		Plans         []plans.Plan `json:"plans"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("decode output: %v\n%s", err, stdout)
	}
	if doc.SchemaVersion != plans.SchemaVersion || doc.Updated != "2026-08-10" || len(doc.Plans) != 2 {
		t.Fatalf("filtered catalog = %+v", doc)
	}
	if doc.Plans[0].ID != plans.Free || doc.Plans[1].ID != "standard" ||
		!doc.Plans[1].Recommended || doc.Plans[1].Badge != "Most popular" {
		t.Fatalf("available plans = %+v", doc.Plans)
	}
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

func TestPlanStatusShowsEffectivePolicyAndOverrides(t *testing.T) {
	days30, days60, days90 := int64(30), int64(60), int64(90)
	status := client.PlanStatus{
		SchemaVersion:   "witself.v0",
		Plan:            "standard",
		PlanName:        "Professional",
		BillingPlan:     "standard",
		BillingPlanName: "Professional",
		Applied:         "standard",
		Limits:          map[string]int64{plans.StoredMemoryLimit: 20_000},
		LimitDefaults:   map[string]int64{plans.StoredMemoryLimit: 10_000},
		Policies:        map[string]int64{plans.TranscriptRetentionDaysPolicy: 60},
		PolicyDefaults:  map[string]int64{plans.TranscriptRetentionDaysPolicy: 90},
		Features:        []string{plans.AgentEmailReceiveFeature, "memory"},
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
		"email data:  indefinite (account override; plan default 90 days) retention",
		"agent_email_receive (account override)",
		"stored_memory: 20,000 (account override; plan default 10,000)",
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
