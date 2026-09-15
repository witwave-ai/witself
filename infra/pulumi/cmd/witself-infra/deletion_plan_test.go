package main

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/auto"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/events"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optpreview"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optup"
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
)

// Pulumi resource mocks do not perform provider Diff calls. Synthetic engine
// events exercise the replacement boundary separately from resource rendering.
type protectionPlanStack struct {
	t           *testing.T
	steps       []apitype.StepEventMetadata
	previewErr  error
	streamErr   error
	closeStream bool
	planPath    string
	upCalls     int
}

func (s *protectionPlanStack) Preview(_ context.Context, opts ...optpreview.Option) (auto.PreviewResult, error) {
	s.t.Helper()
	options := &optpreview.Options{}
	for _, opt := range opts {
		opt.ApplyOption(options)
	}
	s.planPath = options.Plan
	if s.planPath != "" {
		if err := os.WriteFile(s.planPath, []byte("reviewed-plan"), 0o600); err != nil {
			s.t.Fatal(err)
		}
	}
	for _, stream := range options.EventStreams {
		if s.streamErr != nil {
			stream <- events.EngineEvent{Error: s.streamErr}
		}
		for _, step := range s.steps {
			stream <- events.EngineEvent{EngineEvent: apitype.EngineEvent{
				ResourcePreEvent: &apitype.ResourcePreEvent{Metadata: step, Planning: true},
			}}
		}
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
	if err != nil || string(raw) != "reviewed-plan" {
		s.t.Fatalf("up must consume exact reviewed plan: %q, %v", raw, err)
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
