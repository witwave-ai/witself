package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/auto"
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
)

const protectionStateTestCell = "aws-sandbox-usw2-dev"

type fakeDeploymentExporter struct {
	deployment apitype.UntypedDeployment
	err        error
	calls      int
}

func (f *fakeDeploymentExporter) Export(context.Context) (apitype.UntypedDeployment, error) {
	f.calls++
	return f.deployment, f.err
}

func protectionDeployment(raw string) apitype.UntypedDeployment {
	return apitype.UntypedDeployment{Version: 3, Deployment: json.RawMessage(raw)}
}

func TestAppliedUnprotectRefusesNewlyEnabledInventoryPolicy(t *testing.T) {
	stack := &fakeDeploymentExporter{deployment: protectionDeployment(`{"resources":[{"type":"pulumi:pulumi:Stack","outputs":{"deletionProtection":false}}]}`)}
	calls := 0
	mutate := func() error { calls++; return nil }
	// An early preflight may have observed false before the CLI reloaded
	// inventory. Persisted false must never override a newer true policy.
	err := runDestroyAfterUnprotect(context.Background(), stack, protectionStateTestCell, true, mutate, mutate)
	if err == nil || stack.calls != 0 || calls != 0 {
		t.Fatalf("newly protected policy reached teardown: exports=%d mutations=%d err=%v", stack.calls, calls, err)
	}
	for _, want := range []string{protectionStateTestCell, "deletion_protection: false", "separate up", "BEFORE destroy"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not contain %q", err, want)
		}
	}
}

func TestPersistedStoreProtection(t *testing.T) {
	for _, typ := range []string{
		"aws:rds/instance:Instance", "aws:secretsmanager/secret:Secret",
		"gcp:sql/databaseInstance:DatabaseInstance", "gcp:sql/database:Database",
		"gcp:secretmanager/secret:Secret", "azure-native:dbforpostgresql:Server",
		"azure-native:dbforpostgresql:Database", "azure-native:keyvault:Vault",
		"azure-native:keyvault:Secret",
	} {
		t.Run(typ, func(t *testing.T) {
			deployment := protectionDeployment(`{"resources":[{"urn":"store","type":"` + typ + `","protect":true}]}`)
			got, err := protectedDeploymentResources(deployment)
			if err != nil || !reflect.DeepEqual(got, []string{"store"}) {
				t.Fatalf("persisted Protect should block: %v, %v", got, err)
			}
		})
	}
}

func TestPersistedNativeProtection(t *testing.T) {
	for name, resource := range map[string]string{
		"RDS inputs":           `{"type":"aws:rds/instance:Instance","inputs":{"deletionProtection":true}}`,
		"RDS outputs":          `{"type":"aws:rds/instance:Instance","inputs":{"deletionProtection":false},"outputs":{"deletionProtection":true}}`,
		"Cloud SQL provider":   `{"type":"gcp:sql/databaseInstance:DatabaseInstance","inputs":{"deletionProtection":true}}`,
		"Cloud SQL API inputs": `{"type":"gcp:sql/databaseInstance:DatabaseInstance","inputs":{"settings":{"deletionProtectionEnabled":true}}}`,
		"Cloud SQL API output": `{"type":"gcp:sql/databaseInstance:DatabaseInstance","outputs":{"settings":{"deletionProtectionEnabled":true}}}`,
		"GCP secret":           `{"type":"gcp:secretmanager/secret:Secret","outputs":{"deletionProtection":true}}`,
		"Azure DB named lock":  `{"type":"azure-native:authorization:ManagementLockAtResourceLevel","urn":"urn:pulumi:cell::witself-infra::azure-native:authorization:ManagementLockAtResourceLevel::witself-db-deletion-protection"}`,
		"Azure DB scoped lock": `{"type":"azure-native:authorization:ManagementLockAtResourceLevel","inputs":{"resourceProviderNamespace":"Microsoft.DBforPostgreSQL","resourceType":"flexibleServers","level":"CanNotDelete"}}`,
		"Civo stack output":    `{"type":"pulumi:pulumi:Stack","outputs":{"cloud":"civo","deletionProtection":true}}`,
	} {
		t.Run(name, func(t *testing.T) {
			got, err := protectedDeploymentResources(protectionDeployment(`{"resources":[` + resource + `]}`))
			if err != nil || len(got) != 1 {
				t.Fatalf("persisted native protection should block: %v, %v", got, err)
			}
		})
	}
	got, err := protectedDeploymentResources(protectionDeployment(`{"pending_operations":[{"type":"updating","resource":{"type":"aws:rds/instance:Instance","protect":true}}]}`))
	if err != nil || len(got) != 1 {
		t.Fatalf("pending protected resource must block: %v, %v", got, err)
	}
}

func TestAppliedUnprotectAllowsLegacyAndIrreversibleProtection(t *testing.T) {
	for name, raw := range map[string]string{
		"legacy fields absent": `{"resources":[{"type":"aws:rds/instance:Instance"},{"type":"pulumi:pulumi:Stack"}]}`,
		"unprotected applied":  `{"resources":[{"type":"pulumi:pulumi:Stack","outputs":{"deletionProtection":false}},{"type":"aws:rds/instance:Instance","inputs":{"deletionProtection":false},"outputs":{"deletionProtection":false}},{"type":"gcp:sql/databaseInstance:DatabaseInstance","inputs":{"deletionProtection":false,"settings":{"deletionProtectionEnabled":false}}},{"type":"gcp:secretmanager/secret:Secret","outputs":{"deletionProtection":false}}]}`,
		"irreversible purge":   `{"resources":[{"type":"azure-native:keyvault:Vault","inputs":{"properties":{"enablePurgeProtection":true,"enableSoftDelete":true}}}]}`,
		"legacy AWS recovery":  `{"resources":[{"type":"aws:secretsmanager/secret:Secret","inputs":{"recoveryWindowInDays":30},"outputs":{"recoveryWindowInDays":30}}]}`,
		"external store":       `{"resources":[{"type":"aws:rds/instance:Instance","external":true,"protect":true}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			stack := &fakeDeploymentExporter{deployment: protectionDeployment(raw)}
			var sequence []string
			err := runDestroyAfterUnprotect(context.Background(), stack, protectionStateTestCell, false,
				func() error { sequence = append(sequence, "remove"); return nil },
				func() error { sequence = append(sequence, "destroy"); return nil })
			if err != nil || stack.calls != 1 || !reflect.DeepEqual(sequence, []string{"remove", "destroy"}) {
				t.Fatalf("applied unprotect: sequence=%v exports=%d err=%v", sequence, stack.calls, err)
			}
		})
	}
}

func TestAppliedUnprotectExportAndDecodeFailuresFailClosed(t *testing.T) {
	for name, stack := range map[string]*fakeDeploymentExporter{
		"export failure":       {err: errors.New("unavailable checkpoint")},
		"invalid JSON":         {deployment: protectionDeployment(`{`)},
		"null deployment":      {deployment: protectionDeployment(`null`)},
		"unknown schema":       {deployment: apitype.UntypedDeployment{Version: 99, Deployment: json.RawMessage(`{}`)}},
		"unknown bool":         {deployment: protectionDeployment(`{"resources":[{"type":"aws:rds/instance:Instance","outputs":{"deletionProtection":"private-value-must-not-appear"}}]}`)},
		"unknown SQL settings": {deployment: protectionDeployment(`{"resources":[{"type":"gcp:sql/databaseInstance:DatabaseInstance","outputs":{"settings":"private-value-must-not-appear"}}]}`)},
	} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			mutate := func() error { calls++; return nil }
			err := runDestroyAfterUnprotect(context.Background(), stack, protectionStateTestCell, false, mutate, mutate)
			if err == nil || calls != 0 || !strings.Contains(err.Error(), "cannot verify applied unprotection") {
				t.Fatalf("fail-closed result: mutations=%d err=%v", calls, err)
			}
			if strings.Contains(err.Error(), "private-value-must-not-appear") {
				t.Fatalf("snapshot protection error exposed a property value: %v", err)
			}
		})
	}
}

type protectionExportCommand struct {
	// Local select/export operations only use Run; fail if another method is used.
	auto.PulumiCommand
	t           *testing.T
	stdout      string
	stderr      string
	exitCode    int
	err         error
	exportCalls int
}

func (c *protectionExportCommand) Run(_ context.Context, _ string, _ io.Reader, _, _ []io.Writer, _ []string, args ...string) (string, string, int, error) {
	c.t.Helper()
	switch {
	case reflect.DeepEqual(args, []string{"stack", "select", "--stack", protectionStateTestCell}):
		return "", "", 0, nil
	case reflect.DeepEqual(args, []string{"stack", "export", "--show-secrets", "--stack", protectionStateTestCell}):
		c.exportCalls++
		return c.stdout, c.stderr, c.exitCode, c.err
	default:
		c.t.Fatalf("unexpected Pulumi command: %v", args)
		return "", "", 1, errors.New("unexpected Pulumi command")
	}
}

func TestAppliedUnprotectRedactsPulumiExportFailures(t *testing.T) {
	for _, tc := range []struct {
		name     string
		exitCode int
		err      error
	}{
		{name: "malformed JSON with successful exit"},
		{name: "nonzero exit", exitCode: 1, err: errors.New("interrupted export: ERROR_TOKEN_SENTINEL")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Exercise the real SDK error construction: both JSON decoding and
			// command failures can embed decrypted export stdout and stderr.
			command := &protectionExportCommand{
				t: t, stdout: `{"version":3,"deployment":{"outputs":{"dbPassword":"LEAK_SENTINEL"}`,
				stderr: "export diagnostic: STDERR_TOKEN_SENTINEL", exitCode: tc.exitCode, err: tc.err,
			}
			ctx := context.Background()
			workspace, err := auto.NewLocalWorkspace(ctx, auto.WorkDir(t.TempDir()), auto.Pulumi(command))
			if err != nil {
				t.Fatal(err)
			}
			stack, err := auto.SelectStack(ctx, protectionStateTestCell, workspace)
			if err != nil {
				t.Fatal(err)
			}
			mutations := 0
			mutate := func() error { mutations++; return nil }
			err = runDestroyAfterUnprotect(ctx, &stack, protectionStateTestCell, false, mutate, mutate)
			if err == nil || command.exportCalls != 1 || mutations != 0 {
				t.Fatalf("export failure must stop teardown: exports=%d mutations=%d err=%v", command.exportCalls, mutations, err)
			}
			for _, sentinel := range []string{"LEAK_SENTINEL", "STDERR_TOKEN_SENTINEL", "ERROR_TOKEN_SENTINEL"} {
				if strings.Contains(fmt.Sprintf("%v %+v", err, err), sentinel) {
					t.Errorf("export failure exposed %s", sentinel)
				}
			}
			if errors.Unwrap(err) != nil {
				t.Error("export failure retained the SDK error containing decrypted output")
			}
			for _, want := range []string{protectionStateTestCell, "cannot verify applied unprotection", "refusing destroy before fleet removal", "stack export failed"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("export failure lacks %q", want)
				}
			}
		})
	}
}

func TestAppliedUnprotectRemovalFailureStopsDestroy(t *testing.T) {
	stack := &fakeDeploymentExporter{deployment: protectionDeployment(`{"resources":[]}`)}
	removeErr := errors.New("fleet removal failed")
	destroyCalls := 0
	err := runDestroyAfterUnprotect(context.Background(), stack, protectionStateTestCell, false,
		func() error { return removeErr },
		func() error { destroyCalls++; return nil })
	if !errors.Is(err, removeErr) || destroyCalls != 0 {
		t.Fatalf("removal failure: destroy calls=%d err=%v", destroyCalls, err)
	}
}
