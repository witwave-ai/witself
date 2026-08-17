package featurestatus

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/plans"
)

func TestCanonicalCatalog(t *testing.T) {
	catalog, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	root := repoRoot(t)
	if err := catalog.ValidateReferences(root); err != nil {
		t.Fatalf("ValidateReferences: %v", err)
	}
}

func TestCanonicalCatalogOwnsEveryPlanContractKeyExactlyOnce(t *testing.T) {
	catalog, err := Load()
	if err != nil {
		t.Fatalf("Load feature status: %v", err)
	}
	plansCatalog, err := plans.Load()
	if err != nil {
		t.Fatalf("Load plans: %v", err)
	}
	wantFeatures := make(map[string]bool)
	wantLimits := make(map[string]bool)
	wantPolicies := make(map[string]bool)
	for _, key := range plans.SupportedLimitKeys() {
		wantLimits[key] = true
	}
	for _, key := range plans.SupportedPolicyKeys() {
		wantPolicies[key] = true
	}
	for _, key := range plans.SupportedFeatureKeys() {
		wantFeatures[key] = true
	}
	for _, plan := range plansCatalog.Plans {
		for _, feature := range plan.Features {
			if !wantFeatures[feature] {
				t.Errorf("plan %q contains feature outside supported vocabulary: %q", plan.ID, feature)
			}
		}
	}
	featureOwners := make(map[string][]string)
	limitOwners := make(map[string][]string)
	policyOwners := make(map[string][]string)
	for _, feature := range catalog.Features {
		for _, key := range feature.PlanFeatureKeys {
			featureOwners[key] = append(featureOwners[key], feature.ID)
		}
		for _, key := range feature.PlanLimitKeys {
			limitOwners[key] = append(limitOwners[key], feature.ID)
		}
		for _, key := range feature.PlanPolicyKeys {
			policyOwners[key] = append(policyOwners[key], feature.ID)
		}
	}
	assertExactPlanKeyOwners(t, "feature", wantFeatures, featureOwners)
	assertExactPlanKeyOwners(t, "limit", wantLimits, limitOwners)
	assertExactPlanKeyOwners(t, "policy", wantPolicies, policyOwners)
}

func assertExactPlanKeyOwners(t *testing.T, kind string, want map[string]bool, owners map[string][]string) {
	t.Helper()
	for key := range want {
		if len(owners[key]) != 1 {
			t.Errorf("plan %s %q owners = %v; want exactly one", kind, key, owners[key])
		}
	}
	for key, keyOwners := range owners {
		if !want[key] {
			t.Errorf("feature status references unknown plan %s %q from %v", kind, key, keyOwners)
		}
	}
}

func TestGeneratedMarkdownIsCurrent(t *testing.T) {
	catalog, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	root := repoRoot(t)
	want := RenderMarkdown(*catalog)
	got, err := os.ReadFile(filepath.Join(root, "docs", "feature-status.md"))
	if err != nil {
		t.Fatalf("read generated feature status: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("docs/feature-status.md is stale; run `make feature-status`")
	}
}

func TestRenderedSummaryTargetsStableFeatureAnchors(t *testing.T) {
	catalog, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rendered := RenderMarkdown(*catalog)
	for _, feature := range catalog.Features {
		href := []byte(fmt.Sprintf("](#%s)", feature.ID))
		anchor := []byte(fmt.Sprintf("<a id=\"%s\"></a>", feature.ID))
		if bytes.Count(rendered, href) != 1 {
			t.Errorf("feature %q summary href count = %d; want 1", feature.ID, bytes.Count(rendered, href))
		}
		if bytes.Count(rendered, anchor) != 1 {
			t.Errorf("feature %q anchor count = %d; want 1", feature.ID, bytes.Count(rendered, anchor))
		}
	}
}

func TestParseRejectsUnknownFieldsAndUnsortedFeatures(t *testing.T) {
	if _, err := Parse([]byte(`{"schema":"witself.feature-status.v1","features":[],"extra":true}`)); err == nil {
		t.Fatal("Parse accepted unknown field")
	}
	catalog, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(catalog.Features) < 2 {
		t.Fatal("canonical fixture needs two features")
	}
	slices.Reverse(catalog.Features)
	if err := catalog.Validate(); err == nil {
		t.Fatal("Validate accepted unsorted features")
	}
}

func TestFeatureReadinessPrecedence(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Feature)
		want   string
	}{
		{name: "accepted", want: "accepted"},
		{name: "retired wins", mutate: func(f *Feature) { f.Implementation = ImplementationRetired }, want: "retired"},
		{name: "failed gate blocks", mutate: func(f *Feature) { f.Gates.Behavior.State = GateFail }, want: "blocked"},
		{name: "building is not ready", mutate: func(f *Feature) { f.Implementation = ImplementationBuilding }, want: "not ready"},
		{name: "dark is not ready", mutate: func(f *Feature) { f.ManagedRollout = RolloutDark }, want: "not ready"},
		{name: "conditional gate", mutate: func(f *Feature) { f.Gates.Behavior.State = GateConditional }, want: "conditional"},
		{name: "not applicable rollout can be accepted", mutate: func(f *Feature) { f.ManagedRollout = RolloutNotApplicable }, want: "accepted"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			feature := acceptedFixtureFeature()
			if tc.mutate != nil {
				tc.mutate(&feature)
			}
			if got := feature.Readiness(); got != tc.want {
				t.Fatalf("Readiness() = %q; want %q", got, tc.want)
			}
		})
	}
}

func TestFeatureValidationAcceptanceAndRetirementRules(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Feature)
		wantErr string
	}{
		{name: "accepted general"},
		{name: "accepted not applicable", mutate: func(f *Feature) { f.ManagedRollout = RolloutNotApplicable }},
		{name: "planned cannot be accepted", mutate: func(f *Feature) { f.Implementation = ImplementationPlanned }, wantErr: "implementation is not implemented"},
		{name: "dark cannot be accepted", mutate: func(f *Feature) { f.ManagedRollout = RolloutDark }, wantErr: "managed rollout is not active"},
		{name: "active acceptance needs evidence", mutate: func(f *Feature) { f.EvidenceRelease, f.EvidenceScope = "", "" }, wantErr: "requires evidence_release"},
		{name: "not applicable acceptance needs evidence", mutate: func(f *Feature) { f.ManagedRollout, f.EvidenceRelease, f.EvidenceScope = RolloutNotApplicable, "", "" }, wantErr: "requires evidence_release"},
		{name: "release without scope", mutate: func(f *Feature) { f.EvidenceScope = "" }, wantErr: "must be set together"},
		{name: "scope without release", mutate: func(f *Feature) { f.EvidenceRelease = "" }, wantErr: "must be set together"},
		{name: "invalid release", mutate: func(f *Feature) { f.EvidenceRelease = "253" }, wantErr: "invalid evidence release"},
		{name: "valid retired", mutate: makeRetiredFixture},
		{name: "retired requires n-a gates", mutate: func(f *Feature) { makeRetiredFixture(f); f.Gates.Behavior = fixtureGate(GatePass) }, wantErr: "want not_applicable"},
		{name: "retired rollout requires retired implementation", mutate: func(f *Feature) { f.ManagedRollout = RolloutRetired }, wantErr: "requires retired implementation"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			feature := acceptedFixtureFeature()
			if tc.mutate != nil {
				tc.mutate(&feature)
			}
			err := feature.validate()
			if tc.wantErr == "" && err != nil {
				t.Fatalf("validate: %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("validate error = %v; want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestFeatureValidationOpenGateCoverage(t *testing.T) {
	conditional := func() Feature {
		feature := acceptedFixtureFeature()
		feature.Gates.Behavior = fixtureGate(GateConditional)
		feature.OpenGates = []OpenGate{{
			ID: "behavior-work", GateIDs: []string{"behavior"},
			Summary: "Finish the conditional behavior gate.", Ref: "docs/evidence.md",
		}}
		return feature
	}

	valid := conditional()
	if err := valid.validate(); err != nil {
		t.Fatalf("valid conditional feature: %v", err)
	}

	tests := []struct {
		name    string
		mutate  func(*Feature)
		wantErr string
	}{
		{name: "missing open gate", mutate: func(f *Feature) { f.OpenGates = nil }, wantErr: "require at least one"},
		{name: "unknown gate id", mutate: func(f *Feature) { f.OpenGates[0].GateIDs = []string{"unknown"} }, wantErr: "unknown gate"},
		{name: "link to pass", mutate: func(f *Feature) { f.OpenGates[0].GateIDs = []string{"bounds_abuse"} }, wantErr: "references pass gate"},
		{name: "conditional gate uncovered", mutate: func(f *Feature) { f.Gates.BoundsAbuse = fixtureGate(GateConditional) }, wantErr: "has no linked"},
		{name: "unsorted gate ids", mutate: func(f *Feature) {
			f.Gates.BoundsAbuse = fixtureGate(GateConditional)
			f.OpenGates[0].GateIDs = []string{"bounds_abuse", "behavior"}
		}, wantErr: "not sorted"},
		{name: "open gate on accepted feature", mutate: func(f *Feature) { f.Gates.Behavior = fixtureGate(GatePass) }, wantErr: "cannot retain open_gates"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			feature := conditional()
			tc.mutate(&feature)
			err := feature.validate()
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validate error = %v; want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestFeatureValidationRejectsNonCanonicalTextAndReferences(t *testing.T) {
	feature := acceptedFixtureFeature()
	feature.Name = " leading"
	if err := feature.validate(); err == nil || !strings.Contains(err.Error(), "leading or trailing whitespace") {
		t.Fatalf("leading whitespace error = %v", err)
	}
	feature = acceptedFixtureFeature()
	feature.Summary += "\nsecond line"
	if err := feature.validate(); err == nil || !strings.Contains(err.Error(), "newline") {
		t.Fatalf("newline error = %v", err)
	}
	for _, ref := range []string{
		"../outside.md", "docs/../outside.md", "docs//evidence.md",
		"docs/./evidence.md", "/absolute/evidence.md",
	} {
		if err := validateReference(ref); err == nil {
			t.Errorf("validateReference(%q) accepted noncanonical path", ref)
		}
	}
}

func TestValidateReferencesRejectsSymlinkEscapes(t *testing.T) {
	t.Run("final component", func(t *testing.T) {
		root := t.TempDir()
		outside := filepath.Join(t.TempDir(), "outside.md")
		if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(root, "docs"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(root, "docs", "evidence.md")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		catalog := referenceFixtureCatalog("docs/evidence.md")
		if err := catalog.ValidateReferences(root); err == nil || !strings.Contains(err.Error(), "symbolic link") {
			t.Fatalf("ValidateReferences error = %v; want final symlink rejection", err)
		}
	})

	t.Run("intermediate component", func(t *testing.T) {
		root := t.TempDir()
		outsideDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(outsideDir, "evidence.md"), []byte("outside"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(root, "docs"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outsideDir, filepath.Join(root, "docs", "link")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		catalog := referenceFixtureCatalog("docs/link/evidence.md")
		if err := catalog.ValidateReferences(root); err == nil || !strings.Contains(err.Error(), "outside repository root") {
			t.Fatalf("ValidateReferences error = %v; want intermediate symlink escape rejection", err)
		}
	})
}

func acceptedFixtureFeature() Feature {
	return Feature{
		ID: "fixture", Name: "Fixture", Area: "Tests",
		Summary:        "A complete feature-status validation fixture.",
		Implementation: ImplementationImplemented, ManagedRollout: RolloutGeneral,
		EvidenceRelease: "v1.2.3", EvidenceScope: "Fixture release and cohort evidence.",
		Docs: []string{"docs/evidence.md"},
		Gates: Gates{
			Behavior: fixtureGate(GatePass), EntitlementPolicy: fixtureGate(GatePass),
			BoundsAbuse: fixtureGate(GatePass), Observability: fixtureGate(GatePass),
			Recovery: fixtureGate(GatePass), RolloutCanaries: fixtureGate(GatePass),
			DocsSupport: fixtureGate(GatePass),
		},
	}
}

func fixtureGate(state string) Gate {
	gate := Gate{State: state, Summary: "Fixture gate conclusion."}
	if state != GateNotApplicable {
		gate.Evidence = []string{"docs/evidence.md"}
	}
	return gate
}

func makeRetiredFixture(feature *Feature) {
	feature.Implementation = ImplementationRetired
	feature.ManagedRollout = RolloutRetired
	feature.EvidenceRelease = ""
	feature.EvidenceScope = ""
	feature.Gates = Gates{
		Behavior: fixtureGate(GateNotApplicable), EntitlementPolicy: fixtureGate(GateNotApplicable),
		BoundsAbuse: fixtureGate(GateNotApplicable), Observability: fixtureGate(GateNotApplicable),
		Recovery: fixtureGate(GateNotApplicable), RolloutCanaries: fixtureGate(GateNotApplicable),
		DocsSupport: fixtureGate(GateNotApplicable),
	}
}

func referenceFixtureCatalog(ref string) Catalog {
	feature := acceptedFixtureFeature()
	makeRetiredFixture(&feature)
	feature.Docs = []string{ref}
	return Catalog{Schema: SchemaVersion, Features: []Feature{feature}}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(filename), ".."))
}
