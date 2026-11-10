package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/auto"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/events"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optpreview"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optup"
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/common/tokens"
)

// Pulumi resource mocks do not perform provider Diff calls. Synthetic engine
// events exercise the replacement boundary separately from resource rendering.
type protectionPlanStack struct {
	t              *testing.T
	steps          []apitype.StepEventMetadata
	previewErr     error
	streamErr      error
	closeStream    bool
	planPath       string
	upCalls        int
	plan           *apitype.DeploymentPlanV1
	planRaw        []byte
	planMissing    bool
	outputsOnly    bool
	writtenPlan    []byte
	summaryChanges map[apitype.OpType]int
}

func (s *protectionPlanStack) Preview(_ context.Context, opts ...optpreview.Option) (auto.PreviewResult, error) {
	s.t.Helper()
	options := &optpreview.Options{}
	for _, opt := range opts {
		opt.ApplyOption(options)
	}
	s.planPath = options.Plan
	if s.planPath != "" && !s.planMissing {
		s.writtenPlan = s.planRaw
		if s.writtenPlan == nil {
			plan := s.plan
			if plan == nil {
				plan = planFromTestSteps(s.steps)
			}
			var err error
			s.writtenPlan, err = json.Marshal(plan)
			if err != nil {
				s.t.Fatal(err)
			}
		}
		if err := os.WriteFile(s.planPath, s.writtenPlan, 0o600); err != nil {
			s.t.Fatal(err)
		}
	}
	changes := s.summaryChanges
	if changes == nil {
		changes = map[apitype.OpType]int{}
		if s.plan != nil {
			for _, entry := range s.plan.ResourcePlans {
				for _, op := range entry.Steps {
					changes[op]++
				}
			}
		} else {
			for _, step := range s.steps {
				changes[step.Op]++
			}
		}
	}
	for _, stream := range options.EventStreams {
		if s.streamErr != nil {
			stream <- events.EngineEvent{Error: s.streamErr}
		}
		for _, step := range s.steps {
			event := apitype.EngineEvent{ResourcePreEvent: &apitype.ResourcePreEvent{Metadata: step, Planning: true}}
			if s.outputsOnly {
				event = apitype.EngineEvent{ResOutputsEvent: &apitype.ResOutputsEvent{Metadata: step, Planning: true}}
			}
			stream <- events.EngineEvent{EngineEvent: event}
		}
		// A successful summary is not evidence that the saved steps were reviewed.
		stream <- events.EngineEvent{EngineEvent: apitype.EngineEvent{SummaryEvent: &apitype.SummaryEvent{ResourceChanges: changes}}}
		if s.closeStream {
			// The normal Automation API path drains and closes its streams.
			close(stream)
		}
		// Leaving the channel open models failures before the Automation
		// API opened (and therefore took ownership of) the event watcher.
	}
	return auto.PreviewResult{}, s.previewErr
}

func (s *protectionPlanStack) Up(_ context.Context, opts ...optup.Option) (auto.UpResult, error) {
	s.t.Helper()
	s.upCalls++
	options := &optup.Options{}
	for _, opt := range opts {
		opt.ApplyOption(options)
	}
	if options.Plan == "" || options.Plan != s.planPath {
		s.t.Fatalf("up plan = %q, reviewed %q", options.Plan, s.planPath)
	}
	raw, err := os.ReadFile(options.Plan)
	if err != nil || !bytes.Equal(raw, s.writtenPlan) {
		s.t.Fatalf("up must consume exact reviewed plan: read error=%v, bytes match=%t", err, bytes.Equal(raw, s.writtenPlan))
	}
	return auto.UpResult{}, nil
}

func TestProtectionPlanRefusesReplacementBeforeUp(t *testing.T) {
	for _, typ := range []string{
		"aws:rds/instance:Instance", "aws:secretsmanager/secret:Secret",
		"gcp:sql/databaseInstance:DatabaseInstance", "gcp:sql/database:Database",
		"gcp:secretmanager/secret:Secret", "azure-native:dbforpostgresql:Server",
		"azure-native:dbforpostgresql:Database", "azure-native:keyvault:Vault",
		"azure-native:keyvault:Secret",
	} {
		t.Run(typ, func(t *testing.T) {
			for _, op := range []apitype.OpType{
				apitype.OpReplace, apitype.OpCreateReplacement, apitype.OpDeleteReplaced,
				apitype.OpDelete, apitype.OpReadReplacement, apitype.OpDiscardReplaced, apitype.OpImportReplacement,
			} {
				t.Run(string(op), func(t *testing.T) {
					s := &protectionPlanStack{t: t, steps: []apitype.StepEventMetadata{{
						Type: typ, URN: "urn:pulumi:test::witself::" + typ + "::database",
						Op: op, Old: &apitype.StepEventStateMetadata{Protect: false},
					}}}
					_, err := upWithDeletionProtection(context.Background(), s, "test-cell", true, io.Discard)
					for _, want := range []string{typ, "force replacement", "deletion_protection: false", "separate up", "in place"} {
						if err == nil || !strings.Contains(err.Error(), want) {
							t.Fatalf("error = %v, want %q", err, want)
						}
					}
					if s.upCalls != 0 {
						t.Fatal("unsafe preview reached Up")
					}
					if _, err := os.Stat(s.planPath); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("temporary plan remains: %v", err)
					}
				})
			}
		})
	}
}

func TestProtectionPlanRequiresSeparateNativeUnprotectUp(t *testing.T) {
	for _, typ := range []string{
		"aws:rds/instance:Instance", "gcp:sql/databaseInstance:DatabaseInstance", "gcp:secretmanager/secret:Secret",
	} {
		t.Run(typ, func(t *testing.T) {
			// Legacy or imported state may lack Pulumi Protect, while its
			// provider-native protection is still enabled.
			s := &protectionPlanStack{t: t, closeStream: true, steps: []apitype.StepEventMetadata{{
				Type: typ, URN: "native-protected", Op: apitype.OpReplace,
				Old: &apitype.StepEventStateMetadata{Outputs: map[string]any{"deletionProtection": true}},
				New: &apitype.StepEventStateMetadata{Outputs: map[string]any{"deletionProtection": false}},
			}}}
			_, err := upWithDeletionProtection(context.Background(), s, "test-cell", false, io.Discard)
			if err == nil || !strings.Contains(err.Error(), "native-protected") || s.upCalls != 0 {
				t.Fatalf("native same-up unprotect+replace: calls=%d, err=%v", s.upCalls, err)
			}
			s.steps[0].Old.Outputs["deletionProtection"] = false
			if _, err := upWithDeletionProtection(context.Background(), s, "test-cell", false, io.Discard); err != nil || s.upCalls != 1 {
				t.Fatalf("native previously unprotected replacement: calls=%d, err=%v", s.upCalls, err)
			}
		})
	}
	// Cloud SQL has an independent API-side protection setting.
	s := &protectionPlanStack{t: t, steps: []apitype.StepEventMetadata{{
		Type: "gcp:sql/databaseInstance:DatabaseInstance", URN: "sql-api-protected", Op: apitype.OpDelete,
		Old: &apitype.StepEventStateMetadata{Inputs: map[string]any{"settings": map[string]any{"deletionProtectionEnabled": true}}},
	}}}
	if _, err := upWithDeletionProtection(context.Background(), s, "test-cell", false, io.Discard); err == nil || s.upCalls != 0 {
		t.Fatalf("Cloud SQL API protection: calls=%d, err=%v", s.upCalls, err)
	}
}

func TestProtectionPlanUnknownNativeProtectionFailsClosed(t *testing.T) {
	s := &protectionPlanStack{t: t, closeStream: true, steps: []apitype.StepEventMetadata{{
		Type: "aws:rds/instance:Instance", URN: "database", Op: apitype.OpReplace,
		Old: &apitype.StepEventStateMetadata{Outputs: map[string]any{"deletionProtection": "private-value-must-not-appear"}},
	}}}
	_, err := upWithDeletionProtection(context.Background(), s, "test-cell", false, io.Discard)
	if err == nil || s.upCalls != 0 || !strings.Contains(err.Error(), "cannot review deletion protection plan") {
		t.Fatalf("unreadable native protection: calls=%d, err=%v", s.upCalls, err)
	}
	if strings.Contains(err.Error(), "private-value-must-not-appear") {
		t.Fatalf("preview error exposed a property value: %v", err)
	}
}

func TestProtectionPlanRequiresSeparateUnprotectUp(t *testing.T) {
	s := &protectionPlanStack{t: t, steps: []apitype.StepEventMetadata{{
		Type: "azure-native:keyvault:Vault", URN: "vault", Op: "replace",
		Old: &apitype.StepEventStateMetadata{Protect: true},
		New: &apitype.StepEventStateMetadata{Protect: false},
	}}}
	if _, err := upWithDeletionProtection(context.Background(), s, "test-cell", false, io.Discard); err == nil || s.upCalls != 0 {
		t.Fatalf("same-up unprotect+replace: calls=%d, err=%v", s.upCalls, err)
	}
	s.steps[0].Old.Protect = false // a previous up durably removed protection
	if _, err := upWithDeletionProtection(context.Background(), s, "test-cell", false, io.Discard); err != nil || s.upCalls != 1 {
		t.Fatalf("already unprotected replacement: calls=%d, err=%v", s.upCalls, err)
	}
}

func TestProtectionPlanAllowsInPlaceChangesAndRotation(t *testing.T) {
	s := &protectionPlanStack{t: t, closeStream: true, steps: []apitype.StepEventMetadata{
		{Type: "aws:rds/instance:Instance", Op: "update", Old: &apitype.StepEventStateMetadata{Protect: false}},
		{Type: "gcp:sql/databaseInstance:DatabaseInstance", Op: "same"},
		{Type: "azure-native:keyvault:Vault", Op: "create"},
		{Type: "azure-native:authorization:ManagementLockAtResourceLevel", Op: "create"},
		{Type: "aws:secretsmanager/secretVersion:SecretVersion", Op: "replace"},
		{Type: "gcp:secretmanager/secretVersion:SecretVersion", Op: "replace"},
	}}
	if _, err := upWithDeletionProtection(context.Background(), s, "test-cell", true, io.Discard); err != nil || s.upCalls != 1 {
		t.Fatalf("in-place plan: calls=%d, err=%v", s.upCalls, err)
	}
	if _, err := os.Stat(s.planPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary plan remains: %v", err)
	}
	// Removing only the management lock is the intended unprotect update.
	s.steps = []apitype.StepEventMetadata{{Type: "azure-native:authorization:ManagementLockAtResourceLevel", Op: "delete"}}
	if _, err := upWithDeletionProtection(context.Background(), s, "test-cell", false, io.Discard); err != nil {
		t.Fatalf("remove lock during unprotect: %v", err)
	}
}

func TestProtectionPreviewListsEveryResource(t *testing.T) {
	s := &protectionPlanStack{t: t, steps: []apitype.StepEventMetadata{
		{Type: "aws:secretsmanager/secret:Secret", URN: "secret-z", Op: "replace"},
		{Type: "aws:rds/instance:Instance", URN: "database-a", Op: "delete"},
		{Type: "aws:secretsmanager/secret:Secret", URN: "secret-z", Op: "create-replacement"},
	}}
	_, err := previewWithDeletionProtection(context.Background(), s, "test-cell", true)
	if err == nil || !strings.Contains(err.Error(), "database-a, secret-z;") || strings.Count(err.Error(), "secret-z") != 1 {
		t.Fatalf("replacement refusal = %v", err)
	}
}

func TestProtectionPlanFailsClosedOnPreviewErrors(t *testing.T) {
	for _, streamFailure := range []bool{false, true} {
		s := &protectionPlanStack{t: t}
		failure := errors.New("mock preview failure")
		if streamFailure {
			s.streamErr = failure
		} else {
			s.previewErr = failure
		}
		if _, err := upWithDeletionProtection(context.Background(), s, "test-cell", true, io.Discard); !errors.Is(err, failure) || s.upCalls != 0 {
			t.Fatalf("preview failure: calls=%d, err=%v", s.upCalls, err)
		}
	}
}

const civoProtectionTestCell = "civo-sandbox-use1-serving"

func civoPlanProtection(t *testing.T, field string) bool {
	t.Helper()
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	path := writeConfig(t, "version: 1\ncells:\n  "+civoProtectionTestCell+":\n    cloud: civo\n    account_alias: sandbox\n    region: nyc1\n    role: serving\n"+field)
	enabled, err := loadCellDeletionProtection(civoProtectionTestCell, path)
	if err != nil {
		t.Fatal(err)
	}
	return enabled
}

func reviewCivoPlan(t *testing.T, s *protectionPlanStack, command string, enabled bool) error {
	t.Helper()
	if command == "preview" {
		_, err := previewWithDeletionProtection(context.Background(), s, civoProtectionTestCell, enabled)
		if _, statErr := os.Stat(s.planPath); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("temporary preview plan remains: %v", statErr)
		}
		return err
	}
	_, err := upWithDeletionProtection(context.Background(), s, civoProtectionTestCell, enabled, io.Discard)
	if _, statErr := os.Stat(s.planPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("temporary plan remains: %v", statErr)
	}
	return err
}

func TestCivoProtectionPlanDestructiveOperations(t *testing.T) {
	for _, inventory := range []struct {
		name  string
		field string
		block bool
	}{
		{name: "absent defaults enabled", block: true},
		{name: "recorded false", field: "    deletion_protection: false\n"},
	} {
		t.Run(inventory.name, func(t *testing.T) {
			enabled := civoPlanProtection(t, inventory.field)
			if enabled != inventory.block {
				t.Fatalf("recorded protection = %t, want %t", enabled, inventory.block)
			}
			for _, resource := range []struct{ typ, name string }{
				{"civo:index/kubernetesCluster:KubernetesCluster", "civo-cluster"},
				{"kubernetes:core/v1:Namespace", "witself-namespace"},
			} {
				t.Run(resource.name, func(t *testing.T) {
					for _, op := range []apitype.OpType{
						apitype.OpDelete, apitype.OpReplace, apitype.OpCreateReplacement,
						apitype.OpDeleteReplaced, apitype.OpReadReplacement, apitype.OpDiscardReplaced,
						apitype.OpImportReplacement,
					} {
						t.Run(string(op), func(t *testing.T) {
							for _, command := range []string{"preview", "up"} {
								t.Run(command, func(t *testing.T) {
									urn := "urn:pulumi:test::witself::" + resource.typ + "::" + resource.name
									s := &protectionPlanStack{t: t, closeStream: command == "preview", steps: []apitype.StepEventMetadata{{
										Type: resource.typ, URN: urn, Op: op,
										Old: &apitype.StepEventStateMetadata{Protect: false},
									}}}
									err := reviewCivoPlan(t, s, command, enabled)
									if inventory.block {
										want := "Civo cluster/workload-namespace resources: " + urn
										if err == nil || !strings.Contains(err.Error(), want) || s.upCalls != 0 {
											t.Fatalf("destructive plan: calls=%d, err=%v; want %q and no Up", s.upCalls, err, want)
										}
									} else {
										wantCalls := 0
										if command == "up" {
											wantCalls = 1
										}
										if err != nil || s.upCalls != wantCalls {
											t.Fatalf("recorded false plan: calls=%d, err=%v; want %d calls", s.upCalls, err, wantCalls)
										}
									}
								})
							}
						})
					}
				})
			}
		})
	}
}

func TestCivoProtectionPlanAllowsInPlaceOperations(t *testing.T) {
	enabled := civoPlanProtection(t, "")
	for _, command := range []string{"preview", "up"} {
		for _, op := range []apitype.OpType{apitype.OpUpdate, apitype.OpSame, apitype.OpRead, apitype.OpRefresh, apitype.OpCreate, apitype.OpImport} {
			t.Run(command+"/"+string(op), func(t *testing.T) {
				s := &protectionPlanStack{t: t, closeStream: true, steps: []apitype.StepEventMetadata{
					{Type: "civo:index/kubernetesCluster:KubernetesCluster", URN: "urn:pulumi:test::witself::civo:index/kubernetesCluster:KubernetesCluster::civo-cluster", Op: op, Diffs: []string{"pools.nodeCount"}},
					{Type: "kubernetes:core/v1:Namespace", URN: "urn:pulumi:test::witself::kubernetes:core/v1:Namespace::witself-namespace", Op: op},
					{Type: "civo:index/firewall:Firewall", Op: apitype.OpUpdate},
					{Type: "kubernetes:core/v1:Namespace", URN: "urn:pulumi:test::witself::kubernetes:core/v1:Namespace::unrelated", Op: apitype.OpReplace},
				}}
				wantCalls := 0
				if command == "up" {
					wantCalls = 1
				}
				if err := reviewCivoPlan(t, s, command, enabled); err != nil || s.upCalls != wantCalls {
					t.Fatalf("in-place plan: calls=%d, err=%v; want %d calls", s.upCalls, err, wantCalls)
				}
			})
		}
	}
}

func TestCivoProtectionRecordedFalsePermitsReplacementOnNextUp(t *testing.T) {
	enabled := civoPlanProtection(t, "    deletion_protection: false\n")
	s := &protectionPlanStack{t: t, steps: []apitype.StepEventMetadata{{
		Type: civoClusterTestType, URN: civoClusterTestURN,
		Op: apitype.OpReplace, Old: &apitype.StepEventStateMetadata{Protect: false}, New: &apitype.StepEventStateMetadata{Protect: false},
	}}}
	if err := reviewCivoPlan(t, s, "up", enabled); err != nil || s.upCalls != 1 {
		t.Fatalf("recorded false permits replacement without prior up: calls=%d, err=%v", s.upCalls, err)
	}
}

func TestCivoProtectionPreviewListsEveryResource(t *testing.T) {
	enabled := civoPlanProtection(t, "")
	s := &protectionPlanStack{t: t, plan: safeCivoTestPlan(), steps: []apitype.StepEventMetadata{
		{Type: "civo:index/kubernetesCluster:KubernetesCluster", URN: testPlanURN(civoClusterTestType, "cluster-z"), Op: apitype.OpReplace},
		{Type: "civo:index/kubernetesCluster:KubernetesCluster", URN: testPlanURN(civoClusterTestType, "cluster-a"), Op: apitype.OpDelete},
		{Type: "civo:index/kubernetesCluster:KubernetesCluster", URN: testPlanURN(civoClusterTestType, "cluster-z"), Op: apitype.OpCreateReplacement},
		{Type: "aws:rds/instance:Instance", URN: "store", Op: apitype.OpDelete},
	}}
	err := reviewCivoPlan(t, s, "preview", enabled)
	if err == nil || !strings.Contains(err.Error(), testPlanURN(civoClusterTestType, "cluster-a")+", "+testPlanURN(civoClusterTestType, "cluster-z")+";") || strings.Count(err.Error(), "cluster-z") != 1 {
		t.Fatalf("Civo replacement refusal = %v", err)
	}
	wantStore := protectionPlanError(civoProtectionTestCell, map[string]bool{"store": true})
	if !strings.Contains(err.Error(), wantStore.Error()) {
		t.Fatalf("mixed plan lost store refusal: %v", err)
	}
}

const (
	civoClusterTestType   = "civo:index/kubernetesCluster:KubernetesCluster"
	civoNamespaceTestType = "kubernetes:core/v1:Namespace"
	civoClusterTestURN    = "urn:pulumi:test::witself::civo:index/kubernetesCluster:KubernetesCluster::civo-cluster"
	civoNamespaceTestURN  = "urn:pulumi:test::witself::kubernetes:core/v1:Namespace::witself-namespace"
)

func testPlanURN(typ, name string) string {
	return "urn:pulumi:test::witself::" + typ + "::" + name
}

// Derive SDK-shaped plans independently of the production identity resolver.
// Legacy store fixtures use shorthand URNs; expand those only in the saved plan.
func planFromTestSteps(steps []apitype.StepEventMetadata) *apitype.DeploymentPlanV1 {
	plan := &apitype.DeploymentPlanV1{ResourcePlans: map[resource.URN]apitype.ResourcePlanV1{}}
	for i, step := range steps {
		typ, urn := step.Type, step.URN
		for _, state := range []*apitype.StepEventStateMetadata{step.Old, step.New} {
			if state != nil {
				if typ == "" {
					typ = state.Type
				}
				if urn == "" {
					urn = state.URN
				}
			}
		}
		if typ == "" {
			typ = "test:index:Unrelated"
		}
		if !resource.URN(urn).IsValid() {
			urn = testPlanURN(typ, fmt.Sprintf("fixture-%d", i))
		}
		entry := plan.ResourcePlans[resource.URN(urn)]
		entry.Steps = append(entry.Steps, step.Op)
		plan.ResourcePlans[resource.URN(urn)] = entry
	}
	return plan
}

func singleResourceTestPlan(urn string, goal *apitype.GoalV1, ops ...apitype.OpType) *apitype.DeploymentPlanV1 {
	return &apitype.DeploymentPlanV1{ResourcePlans: map[resource.URN]apitype.ResourcePlanV1{
		resource.URN(urn): {Goal: goal, Steps: ops},
	}}
}

func safeCivoTestPlan() *apitype.DeploymentPlanV1 {
	return singleResourceTestPlan(civoClusterTestURN, nil, apitype.OpSame)
}

func TestCivoProtectionIncompleteOrConflictingEventIdentity(t *testing.T) {
	enabled := civoPlanProtection(t, "")
	cases := []struct {
		name string
		step apitype.StepEventMetadata
	}{
		{"namespace_old_urn", apitype.StepEventMetadata{Type: civoNamespaceTestType, Op: apitype.OpDelete,
			Old: &apitype.StepEventStateMetadata{URN: civoNamespaceTestURN}}},
		{"cluster_old_type", apitype.StepEventMetadata{URN: civoClusterTestURN, Op: apitype.OpDelete,
			Old: &apitype.StepEventStateMetadata{Type: civoClusterTestType}}},
		{"no_identity", apitype.StepEventMetadata{Op: apitype.OpDelete}},
		{"old_identity_only", apitype.StepEventMetadata{Op: apitype.OpDelete,
			Old: &apitype.StepEventStateMetadata{Type: civoClusterTestType, URN: civoClusterTestURN}}},
		{"new_identity_only", apitype.StepEventMetadata{Op: apitype.OpReplace,
			New: &apitype.StepEventStateMetadata{Type: civoNamespaceTestType, URN: civoNamespaceTestURN}}},
		{"urn_type_only", apitype.StepEventMetadata{URN: civoClusterTestURN, Op: apitype.OpDelete}},
		{"old_type_disagrees", apitype.StepEventMetadata{Type: "civo:index/firewall:Firewall", URN: testPlanURN("civo:index/firewall:Firewall", "firewall"), Op: apitype.OpSame,
			Old: &apitype.StepEventStateMetadata{Type: civoClusterTestType, URN: civoClusterTestURN}}},
		{"new_type_disagrees", apitype.StepEventMetadata{Type: civoClusterTestType, URN: civoClusterTestURN, Op: apitype.OpUpdate,
			New: &apitype.StepEventStateMetadata{Type: "civo:index/firewall:Firewall"}}},
		{"new_urn_disagrees", apitype.StepEventMetadata{Type: civoClusterTestType, URN: civoClusterTestURN, Op: apitype.OpUpdate,
			New: &apitype.StepEventStateMetadata{URN: testPlanURN(civoClusterTestType, "other-cluster")}}},
		{"urn_type_disagrees", apitype.StepEventMetadata{Type: civoNamespaceTestType, URN: civoClusterTestURN, Op: apitype.OpUpdate}},
		{"malformed_urn", apitype.StepEventMetadata{Type: civoClusterTestType, URN: "private-identity-must-not-appear", Op: apitype.OpDelete}},
		{"cluster_missing_urn", apitype.StepEventMetadata{Type: civoClusterTestType, Op: apitype.OpUpdate}},
		{"namespace_missing_urn", apitype.StepEventMetadata{Type: civoNamespaceTestType, Op: apitype.OpDelete}},
		{"urn_empty_name", apitype.StepEventMetadata{URN: testPlanURN(civoClusterTestType, ""), Op: apitype.OpDelete}},
	}
	for _, tc := range cases {
		for _, command := range []string{"preview", "up"} {
			t.Run(tc.name+"/"+command, func(t *testing.T) {
				// A safe file isolates the event boundary, including the Old.* mutation.
				s := &protectionPlanStack{t: t, steps: []apitype.StepEventMetadata{tc.step}, plan: safeCivoTestPlan()}
				err := reviewCivoPlan(t, s, command, enabled)
				if err == nil || s.upCalls != 0 {
					t.Fatalf("identity evidence escaped review: calls=%d, err=%v", s.upCalls, err)
				}
				if strings.Contains(err.Error(), "private-identity-must-not-appear") {
					t.Fatal("identity error exposed uninterpretable identity value")
				}
			})
		}
	}
}

func TestCivoProtectionSavedPlanBeforeUp(t *testing.T) {
	enabled := civoPlanProtection(t, "")
	cases := []struct {
		name string
		urn  string
		op   apitype.OpType
	}{
		{"cluster_delete", civoClusterTestURN, apitype.OpDelete},
		{"namespace_replace", civoNamespaceTestURN, apitype.OpReplace},
		{"namespace_create_replacement", civoNamespaceTestURN, apitype.OpCreateReplacement},
		{"namespace_delete_replaced", civoNamespaceTestURN, apitype.OpDeleteReplaced},
		{"namespace_read_replacement", civoNamespaceTestURN, apitype.OpReadReplacement},
		{"namespace_discard_replaced", civoNamespaceTestURN, apitype.OpDiscardReplaced},
		{"namespace_import_replacement", civoNamespaceTestURN, apitype.OpImportReplacement},
		{"cluster_discard", civoClusterTestURN, apitype.OpReadDiscard},
		{"cluster_remove_pending_replace", civoClusterTestURN, apitype.OpRemovePendingReplace},
	}
	for _, tc := range cases {
		for _, command := range []string{"preview", "up"} {
			t.Run(tc.name+"/"+command, func(t *testing.T) {
				// No pre-event: only the actual file exposes the destructive step.
				ops := []apitype.OpType{apitype.OpSame, tc.op}
				if tc.name == "cluster_delete" {
					ops = []apitype.OpType{tc.op}
				}
				s := &protectionPlanStack{t: t, closeStream: true, plan: singleResourceTestPlan(tc.urn, nil, ops...)}
				err := reviewCivoPlan(t, s, command, enabled)
				if err == nil || s.upCalls != 0 {
					t.Fatalf("saved destructive plan escaped review: calls=%d, err=%v", s.upCalls, err)
				}
			})
		}
	}
}

func TestCivoProtectionOutputsOnly(t *testing.T) {
	enabled := civoPlanProtection(t, "")
	for _, command := range []string{"preview", "up"} {
		for _, resourceURN := range []string{civoClusterTestURN, civoNamespaceTestURN} {
			t.Run(command+"/"+resource.URN(resourceURN).Name(), func(t *testing.T) {
				s := &protectionPlanStack{t: t, outputsOnly: true, steps: []apitype.StepEventMetadata{{URN: resourceURN, Op: apitype.OpDelete}}}
				if err := reviewCivoPlan(t, s, command, enabled); err == nil || s.upCalls != 0 {
					t.Fatalf("outputs-only destructive plan escaped review: calls=%d, err=%v", s.upCalls, err)
				}
			})
		}
	}
}

func TestCivoProtectionSavedPlanUnreadableOrUndecodable(t *testing.T) {
	enabled := civoPlanProtection(t, "")
	cases := []struct {
		name    string
		raw     []byte
		missing bool
	}{
		{name: "unreadable", missing: true},
		{name: "invalid_json", raw: []byte("private-plan-must-not-appear")},
		{name: "wrong_resource_map_type", raw: []byte(`{"resourcePlans":"private-plan-must-not-appear"}`)},
		{name: "empty_file", raw: []byte{}},
		{name: "missing_resource_map", raw: []byte(`{}`)},
		{name: "empty_resource_map", raw: []byte(`{"resourcePlans":{}}`)},
		{name: "null_plan", raw: []byte(`null`)},
		{name: "null_resource_entry", raw: []byte(`{"resourcePlans":{"` + civoClusterTestURN + `":null}}`)},
		{name: "trailing_json", raw: []byte(`{"resourcePlans":{}} {"private-plan-must-not-appear":true}`)},
	}
	for _, tc := range cases {
		for _, command := range []string{"preview", "up"} {
			t.Run(tc.name+"/"+command, func(t *testing.T) {
				s := &protectionPlanStack{t: t, planRaw: tc.raw, planMissing: tc.missing, steps: []apitype.StepEventMetadata{{
					Type: civoClusterTestType, URN: civoClusterTestURN, Op: apitype.OpSame,
				}}}
				err := reviewCivoPlan(t, s, command, enabled)
				if err == nil || s.upCalls != 0 {
					t.Fatalf("unreviewable saved plan escaped review: calls=%d, err=%v", s.upCalls, err)
				}
				if strings.Contains(err.Error(), "private-plan-must-not-appear") {
					t.Fatal("plan error exposed undecodable file contents")
				}
			})
		}
	}
}

func TestCivoProtectionUnknownOperation(t *testing.T) {
	enabled := civoPlanProtection(t, "")
	for _, command := range []string{"preview", "up"} {
		for _, source := range []string{"event", "plan"} {
			t.Run(source+"/"+command, func(t *testing.T) {
				s := &protectionPlanStack{t: t, plan: safeCivoTestPlan()}
				if source == "event" {
					s.steps = []apitype.StepEventMetadata{{Type: civoClusterTestType, URN: civoClusterTestURN, Op: "future-operation"}}
				} else {
					s.plan = singleResourceTestPlan(civoClusterTestURN, nil, "future-operation")
				}
				if err := reviewCivoPlan(t, s, command, enabled); err == nil || s.upCalls != 0 {
					t.Fatalf("unknown operation escaped review: calls=%d, err=%v", s.upCalls, err)
				}
			})
		}
	}
}

func TestCivoProtectionSavedPlanGoalIdentity(t *testing.T) {
	enabled := civoPlanProtection(t, "")
	for _, command := range []string{"preview", "up"} {
		for _, goal := range []*apitype.GoalV1{
			{Type: tokens.Type(civoClusterTestType), Name: "civo-cluster"},
			{Type: tokens.Type(civoNamespaceTestType), Name: "witself-namespace"},
		} {
			t.Run(command+"/"+goal.Name, func(t *testing.T) {
				// An apparently unrelated map key must not hide a protected goal.
				s := &protectionPlanStack{t: t, plan: singleResourceTestPlan(testPlanURN("test:index:Unrelated", "unrelated"), goal, apitype.OpReplace)}
				if err := reviewCivoPlan(t, s, command, enabled); err == nil || s.upCalls != 0 {
					t.Fatalf("protected goal escaped review: calls=%d, err=%v", s.upCalls, err)
				}
			})
		}
	}
}

func TestCivoProtectionPlanAndStreamRefusalsAreJoined(t *testing.T) {
	enabled := civoPlanProtection(t, "")
	for _, command := range []string{"preview", "up"} {
		t.Run(command, func(t *testing.T) {
			s := &protectionPlanStack{t: t, steps: []apitype.StepEventMetadata{{Type: civoClusterTestType, URN: civoClusterTestURN, Op: apitype.OpDelete}},
				plan: singleResourceTestPlan(civoNamespaceTestURN, nil, apitype.OpReplace)}
			err := reviewCivoPlan(t, s, command, enabled)
			if err == nil || s.upCalls != 0 || !strings.Contains(err.Error(), civoClusterTestURN) || !strings.Contains(err.Error(), civoNamespaceTestURN) {
				t.Fatalf("both review sources must refuse together: calls=%d, err=%v", s.upCalls, err)
			}
		})
	}
}

func TestCivoProtectionSingleClusterUpdate(t *testing.T) {
	enabled := civoPlanProtection(t, "")
	for _, command := range []string{"preview", "up"} {
		t.Run(command, func(t *testing.T) {
			s := &protectionPlanStack{t: t, steps: []apitype.StepEventMetadata{{Type: civoClusterTestType, URN: civoClusterTestURN,
				Op: apitype.OpUpdate, Diffs: []string{"pools.nodeCount"}}}}
			wantCalls := 0
			if command == "up" {
				wantCalls = 1
			}
			if err := reviewCivoPlan(t, s, command, enabled); err != nil || s.upCalls != wantCalls {
				t.Fatalf("single in-place cluster update refused: calls=%d, err=%v", s.upCalls, err)
			}
		})
	}
}

func TestCivoProtectionAllowsUnrelatedDestruction(t *testing.T) {
	enabled := civoPlanProtection(t, "")
	for _, command := range []string{"preview", "up"} {
		for _, op := range []apitype.OpType{apitype.OpDelete, apitype.OpReplace} {
			t.Run(command+"/"+string(op), func(t *testing.T) {
				s := &protectionPlanStack{t: t, steps: []apitype.StepEventMetadata{
					{Type: civoClusterTestType, URN: civoClusterTestURN, Op: apitype.OpSame},
					{Type: civoNamespaceTestType, URN: testPlanURN(civoNamespaceTestType, "unrelated"), Op: op},
					{Type: "civo:index/firewall:Firewall", URN: testPlanURN("civo:index/firewall:Firewall", "firewall"), Op: op},
				}}
				wantCalls := 0
				if command == "up" {
					wantCalls = 1
				}
				if err := reviewCivoPlan(t, s, command, enabled); err != nil || s.upCalls != wantCalls {
					t.Fatalf("unrelated destruction refused: calls=%d, err=%v", s.upCalls, err)
				}
			})
		}
	}
}

func TestCivoProtectionQualifiedURNUsesLeafType(t *testing.T) {
	enabled := civoPlanProtection(t, "")
	for _, command := range []string{"preview", "up"} {
		t.Run(command, func(t *testing.T) {
			urn := testPlanURN("test:index:Component$"+civoClusterTestType, "civo-cluster")
			s := &protectionPlanStack{t: t, plan: singleResourceTestPlan(urn, &apitype.GoalV1{Type: tokens.Type(civoClusterTestType), Name: "civo-cluster"}, apitype.OpUpdate),
				steps: []apitype.StepEventMetadata{{Type: civoClusterTestType, URN: urn, Op: apitype.OpUpdate}}}
			wantCalls := 0
			if command == "up" {
				wantCalls = 1
			}
			if err := reviewCivoPlan(t, s, command, enabled); err != nil || s.upCalls != wantCalls {
				t.Fatalf("qualified URN leaf type disagreed incorrectly: calls=%d, err=%v", s.upCalls, err)
			}
		})
	}
}

func TestCivoProtectionDisabledSkipsSavedPlanReview(t *testing.T) {
	enabled := civoPlanProtection(t, "    deletion_protection: false\n")
	for _, command := range []string{"preview", "up"} {
		t.Run(command, func(t *testing.T) {
			s := &protectionPlanStack{t: t, planRaw: []byte("opaque-plan"), steps: []apitype.StepEventMetadata{{Op: apitype.OpDelete}}}
			wantCalls := 0
			if command == "up" {
				wantCalls = 1
			}
			if err := reviewCivoPlan(t, s, command, enabled); err != nil || s.upCalls != wantCalls {
				t.Fatalf("disabled review changed semantics: calls=%d, err=%v", s.upCalls, err)
			}
		})
	}
}

func TestCivoProtectionDisabledStillHonorsOldProtect(t *testing.T) {
	enabled := civoPlanProtection(t, "    deletion_protection: false\n")
	for _, command := range []string{"preview", "up"} {
		t.Run(command, func(t *testing.T) {
			s := &protectionPlanStack{t: t, plan: safeCivoTestPlan(), steps: []apitype.StepEventMetadata{{
				Type: civoClusterTestType, URN: civoClusterTestURN, Op: apitype.OpReplace,
				Old: &apitype.StepEventStateMetadata{Protect: true},
			}}}
			if err := reviewCivoPlan(t, s, command, enabled); err == nil || s.upCalls != 0 {
				t.Fatalf("explicit old protection ignored: calls=%d, err=%v", s.upCalls, err)
			}
		})
	}
}

func TestCivoProtectionPinnedOperationInventory(t *testing.T) {
	classification := map[apitype.OpType]bool{
		apitype.OpSame: true, apitype.OpCreate: true, apitype.OpUpdate: true,
		apitype.OpRead: true, apitype.OpRefresh: true, apitype.OpImport: true,
		apitype.OpDelete: false, apitype.OpReplace: false, apitype.OpCreateReplacement: false,
		apitype.OpDeleteReplaced: false, apitype.OpReadReplacement: false,
		apitype.OpReadDiscard: false, apitype.OpDiscardReplaced: false,
		apitype.OpRemovePendingReplace: false, apitype.OpImportReplacement: false,
	}
	// Inspect the compiled SDK's source, rather than maintaining an unchecked
	// second list. The Go parser handles LF and CRLF without text substitutions.
	fn := runtime.FuncForPC(reflect.ValueOf(apitype.OperationResultFromError).Pointer())
	source, _ := fn.FileLine(fn.Entry())
	entries, err := os.ReadDir(filepath.Dir(source))
	if err != nil {
		t.Fatal(err)
	}
	for _, newline := range []string{"LF", "CRLF"} {
		t.Run(newline, func(t *testing.T) {
			seen := map[apitype.OpType]bool{}
			for _, entry := range entries {
				if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
					continue
				}
				raw, err := os.ReadFile(filepath.Join(filepath.Dir(source), entry.Name()))
				if err != nil {
					t.Fatal(err)
				}
				raw = bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
				if newline == "CRLF" {
					raw = bytes.ReplaceAll(raw, []byte("\n"), []byte("\r\n"))
				}
				file, err := parser.ParseFile(token.NewFileSet(), entry.Name(), raw, 0)
				if err != nil {
					t.Fatal(err)
				}
				for _, declaration := range file.Decls {
					group, ok := declaration.(*ast.GenDecl)
					if !ok || group.Tok != token.CONST {
						continue
					}
					inheritedOpType := false
					for _, spec := range group.Specs {
						value := spec.(*ast.ValueSpec)
						if value.Type != nil {
							typ, ok := value.Type.(*ast.Ident)
							inheritedOpType = ok && typ.Name == "OpType"
						} else if len(value.Values) > 0 {
							inheritedOpType = false
						}
						if !inheritedOpType {
							continue
						}
						if len(value.Values) == 0 {
							t.Fatal("SDK operation uses inherited expression; review inventory")
						}
						for _, expr := range value.Values {
							literal, ok := expr.(*ast.BasicLit)
							if !ok {
								t.Fatal("SDK operation is no longer a string literal; review inventory")
							}
							name, err := strconv.Unquote(literal.Value)
							if err != nil {
								t.Fatal(err)
							}
							op := apitype.OpType(name)
							want, known := classification[op]
							if !known {
								t.Fatalf("SDK added unclassified operation %q", name)
							}
							seen[op] = true
							if got := civoPlanOperationAllowed(op); got != want {
								t.Errorf("operation %q allowed=%t, want %t", op, got, want)
							}
						}
					}
				}
			}
			if len(seen) != len(classification) {
				t.Fatalf("SDK inventory count=%d, classified=%d", len(seen), len(classification))
			}
		})
	}
	if civoPlanOperationAllowed("future-operation") || civoPlanOperationAllowed("") {
		t.Fatal("unrecognized operations must fail closed")
	}
}

func TestCivoProtectionSummaryOnlyEmptySavedPlan(t *testing.T) {
	enabled := civoPlanProtection(t, "")
	for _, command := range []string{"preview", "up"} {
		t.Run(command, func(t *testing.T) {
			s := &protectionPlanStack{t: t, planRaw: []byte(`{}`), summaryChanges: map[apitype.OpType]int{apitype.OpDelete: 1}}
			if err := reviewCivoPlan(t, s, command, enabled); err == nil || s.upCalls != 0 {
				t.Fatalf("summary resource steps require resource plans: calls=%d, err=%v", s.upCalls, err)
			}
		})
	}
}

func TestCivoProtectionAllowsOldIdentityInPlace(t *testing.T) {
	enabled := civoPlanProtection(t, "")
	for _, command := range []string{"preview", "up"} {
		for _, op := range []apitype.OpType{apitype.OpUpdate, apitype.OpCreate} {
			t.Run(command+"/"+string(op), func(t *testing.T) {
				s := &protectionPlanStack{t: t, steps: []apitype.StepEventMetadata{{Op: op,
					Old: &apitype.StepEventStateMetadata{Type: civoClusterTestType, URN: civoClusterTestURN}}}}
				wantCalls := 0
				if command == "up" {
					wantCalls = 1
				}
				if err := reviewCivoPlan(t, s, command, enabled); err != nil || s.upCalls != wantCalls {
					t.Fatalf("complete Old identity should permit in-place operation: calls=%d, err=%v", s.upCalls, err)
				}
			})
		}
	}
}
