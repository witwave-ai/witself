package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func protectionTestConfig(defaultField, cellField string) string {
	return "version: 1\ndefaults:\n  profile: prod\n" + defaultField + "cells:\n  " + destroyTestCell + ":\n    cloud: aws\n    account_alias: sandbox\n    region: us-west-2\n    role: dev\n" + cellField
}

func TestInventoryDeletionProtectionInheritance(t *testing.T) {
	for _, tt := range []struct {
		name         string
		defaultField string
		cellField    string
		want         bool
	}{
		{name: "absent defaults true", want: true},
		{name: "explicit true", cellField: "    deletion_protection: true\n", want: true},
		{name: "explicit false", cellField: "    deletion_protection: false\n"},
		{name: "inherit true", defaultField: "  deletion_protection: true\n", want: true},
		{name: "inherit false", defaultField: "  deletion_protection: false\n"},
		{name: "cell false overrides true", defaultField: "  deletion_protection: true\n", cellField: "    deletion_protection: false\n"},
		{name: "cell true overrides false", defaultField: "  deletion_protection: false\n", cellField: "    deletion_protection: true\n", want: true},
		{name: "null remains absent", cellField: "    deletion_protection: null\n", want: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := writeConfig(t, protectionTestConfig(tt.defaultField, tt.cellField))
			got, err := loadCellDeletionProtection(destroyTestCell, path)
			if err != nil || got != tt.want {
				t.Fatalf("effective deletion protection = %t, %v; want %t", got, err, tt.want)
			}
			fs := newTestFlagSet()
			if err := applyCellConfig(fs, destroyTestCell, path); err != nil {
				t.Fatalf("inventory-only field must not require a CLI flag: %v", err)
			}
		})
	}
}

func TestDeletionProtectionMissingInventoryOrCellDefaultsTrue(t *testing.T) {
	paths := []string{
		filepath.Join(t.TempDir(), "missing.yaml"),
		writeConfig(t, "version: 1\ndefaults:\n  deletion_protection: false\ncells: {}\n"),
	}
	for _, path := range paths {
		got, err := loadCellDeletionProtection(destroyTestCell, path)
		if err != nil || !got {
			t.Fatalf("unrecorded cell must default protected: %t, %v", got, err)
		}
	}
}

func TestDeletionProtectionRejectsInvalidInventory(t *testing.T) {
	path := writeConfig(t, protectionTestConfig("", "    deletion_protection: disabled\n"))
	if _, err := loadCellDeletionProtection(destroyTestCell, path); err == nil {
		t.Fatal("invalid safety policy must not be ignored")
	}
}

func TestConfigAddCellPreservesDeletionProtectionRecord(t *testing.T) {
	path := writeConfig(t, protectionTestConfig("  deletion_protection: true\n", "    deletion_protection: false\n"))
	fs := newTestFlagSet()
	if err := fs.Parse([]string{"-cloud", "aws", "-account-alias", "sandbox", "-region", "us-west-2", "-role", "new"}); err != nil {
		t.Fatal(err)
	}
	if err := configAddCell(fs, path); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := loadInfraConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Defaults.DeletionProtection == nil || !*cfg.Defaults.DeletionProtection {
		t.Fatal("adding another cell lost the protected inventory default")
	}
	entry := cfg.Cells[destroyTestCell]
	if entry.DeletionProtection == nil || *entry.DeletionProtection {
		t.Fatal("adding another cell lost the existing explicit break-glass record")
	}
	if !cfg.deletionProtection("aws-sandbox-usw2-new") {
		t.Fatal("the new cell must inherit protection")
	}
}

func TestDestroyDeletionProtectionRecordGuard(t *testing.T) {
	for _, tt := range []struct {
		name         string
		defaultField string
		cellField    string
		refused      bool
	}{
		{name: "absent", refused: true},
		{name: "true", cellField: "    deletion_protection: true\n", refused: true},
		{name: "false", cellField: "    deletion_protection: false\n"},
		{name: "inherited true", defaultField: "  deletion_protection: true\n", refused: true},
		{name: "inherited false", defaultField: "  deletion_protection: false\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := writeConfig(t, protectionTestConfig(tt.defaultField, tt.cellField))
			reader := &fakePlacementStatusReader{}
			var out strings.Builder
			err := runDestroySafety(context.Background(), destroyTestCell, destroySafetyOptions{
				ConfigPath: path, AllowUnknownCell: true, ForceWithAccounts: true,
				SkipAccountCheck: true, YesCell: destroyTestCell,
				PlacementStatusAPI: reader, Output: &out,
			})
			if !tt.refused {
				if err != nil {
					t.Fatalf("recorded unprotect should allow existing destroy checks: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("protected destroy must be refused despite every override flag")
			}
			for _, want := range []string{"deletion protection", destroyTestCell, path, "deletion_protection: false", "separate `witself-infra up", "BEFORE `destroy`", "no CLI flag"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q does not contain %q", err, want)
				}
			}
			if reader.calls != 0 || out.Len() != 0 {
				t.Fatalf("protected destroy reached later guards: reads=%d, output=%q", reader.calls, out.String())
			}
		})
	}
}

func TestDestroyDeletionProtectionUnknownCellHasNoFlagBypass(t *testing.T) {
	for _, path := range []string{
		filepath.Join(t.TempDir(), "missing.yaml"),
		writeConfig(t, "version: 1\ndefaults:\n  deletion_protection: false\ncells: {}\n"),
	} {
		err := runDestroySafety(context.Background(), destroyTestCell, destroySafetyOptions{
			ConfigPath: path, AllowUnknownCell: true, ForceWithAccounts: true,
			SkipAccountCheck: true, YesCell: destroyTestCell,
		})
		if err == nil || !strings.Contains(err.Error(), "deletion protection") {
			t.Fatalf("--allow-unknown-cell bypassed the required record: %v", err)
		}
	}
}

func TestDestroyCLIAndDashboardRefuseBeforeCloudAccess(t *testing.T) {
	path := writeConfig(t, protectionTestConfig("", ""))
	err := run([]string{"destroy", "-cell", destroyTestCell, "-config", path,
		"-allow-unknown-cell", "-force-with-accounts", "-skip-account-check", "-yes-cell", destroyTestCell})
	if err == nil || !strings.Contains(err.Error(), "deletion protection") {
		t.Fatalf("CLI protected destroy = %v", err)
	}
	err = run([]string{"destroy", "-config", path, "-cloud", "aws", "-account-alias", "sandbox",
		"-region", "us-west-2", "-role", "dev", "-skip-account-check", "-yes-cell", destroyTestCell})
	if err == nil || !strings.Contains(err.Error(), "deletion protection") {
		t.Fatalf("all-flags invocation bypassed the protected record: %v", err)
	}
	err = (liveDataSource{}).destroyPreflight(context.Background(), path, destroyTestCell)
	if err == nil || !strings.Contains(err.Error(), "deletion protection") {
		t.Fatalf("dashboard protected destroy = %v", err)
	}
}

func TestDeletionProtectionHasNoCLIFlag(t *testing.T) {
	err := run([]string{"destroy", "-deletion-protection=false"})
	if err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
		t.Fatalf("deletion protection must only come from inventory: %v", err)
	}
}

func TestAppliedUnprotectRefusalPreventsFleetRemovalAndDestroy(t *testing.T) {
	path := writeConfig(t, protectionTestConfig("", "    deletion_protection: false\n"))
	if err := runDestroySafety(context.Background(), destroyTestCell, destroySafetyOptions{
		ConfigPath: path, SkipAccountCheck: true, YesCell: destroyTestCell,
	}); err != nil {
		t.Fatalf("false record should pass inventory checks: %v", err)
	}
	// The inventory was edited, but the separate up was skipped. The saved
	// checkpoint still protects the store even though current config is false.
	stack := &fakeDeploymentExporter{deployment: protectionDeployment(`{"resources":[{"urn":"database","type":"aws:rds/instance:Instance","protect":true}]}`)}
	removeCalls, destroyCalls := 0, 0
	err := runDestroyAfterUnprotect(context.Background(), stack, destroyTestCell, false,
		func() error { removeCalls++; return nil },
		func() error { destroyCalls++; return nil })
	if err == nil {
		t.Fatal("unapplied unprotect must refuse before destructive work")
	}
	for _, want := range []string{"database", "deletion_protection: false", "separate up", "BEFORE destroy", "before fleet removal"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not contain %q", err, want)
		}
	}
	if stack.calls != 1 || removeCalls != 0 || destroyCalls != 0 {
		t.Fatalf("calls: export=%d remove=%d destroy=%d", stack.calls, removeCalls, destroyCalls)
	}
}
