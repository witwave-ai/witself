package main

import (
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/auto"
)

func TestRegistrationCredentialsRequireBackupOutput(t *testing.T) {
	tests := []struct {
		name    string
		outputs auto.OutputMap
		wantErr string
	}{
		{
			name: "valid distinct credentials",
			outputs: auto.OutputMap{
				"provisionToken": {
					Value:  "witself_prv_provision-only",
					Secret: true,
				},
				"backupToken": {
					Value:  "witself_bak_backup-only",
					Secret: true,
				},
			},
		},
		{
			name: "missing provision output",
			outputs: auto.OutputMap{
				"backupToken": {
					Value:  "witself_bak_backup-only",
					Secret: true,
				},
			},
			wantErr: "exports no provisionToken",
		},
		{
			name: "non-string provision output",
			outputs: auto.OutputMap{
				"provisionToken": {
					Value:  123,
					Secret: true,
				},
				"backupToken": {
					Value:  "witself_bak_backup-only",
					Secret: true,
				},
			},
			wantErr: "provisionToken output is not a nonempty string",
		},
		{
			name: "public provision output",
			outputs: auto.OutputMap{
				"provisionToken": {
					Value: "witself_prv_provision-only",
				},
				"backupToken": {
					Value:  "witself_bak_backup-only",
					Secret: true,
				},
			},
			wantErr: "provisionToken output is not marked secret",
		},
		{
			name: "missing backup output",
			outputs: auto.OutputMap{
				"provisionToken": {
					Value:  "witself_prv_provision-only",
					Secret: true,
				},
			},
			wantErr: "exports no backupToken",
		},
		{
			name: "non-string backup output",
			outputs: auto.OutputMap{
				"provisionToken": {
					Value:  "witself_prv_provision-only",
					Secret: true,
				},
				"backupToken": {
					Value: 123,
				},
			},
			wantErr: "not a nonempty string",
		},
		{
			name: "empty backup output",
			outputs: auto.OutputMap{
				"provisionToken": {
					Value:  "witself_prv_provision-only",
					Secret: true,
				},
				"backupToken": {
					Value:  "",
					Secret: true,
				},
			},
			wantErr: "not a nonempty string",
		},
		{
			name: "public backup output",
			outputs: auto.OutputMap{
				"provisionToken": {
					Value:  "witself_prv_provision-only",
					Secret: true,
				},
				"backupToken": {
					Value: "witself_bak_backup-only",
				},
			},
			wantErr: "backupToken output is not marked secret",
		},
		{
			name: "same authority",
			outputs: auto.OutputMap{
				"provisionToken": {
					Value:  "witself_bak_same",
					Secret: true,
				},
				"backupToken": {
					Value:  "witself_bak_same",
					Secret: true,
				},
			},
			wantErr: "must be distinct",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provisionToken, backupToken, err := registrationCredentials(
				test.outputs,
			)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if provisionToken != "witself_prv_provision-only" ||
					backupToken != "witself_bak_backup-only" {
					t.Fatalf(
						"credentials = %q / %q",
						provisionToken, backupToken,
					)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestFleetRegistrationPinsRestoreTargetIsolation(t *testing.T) {
	tests := []struct {
		name   string
		target bool
	}{
		{name: "ordinary cell", target: false},
		{name: "restore test cell", target: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := fleetRegistration(
				"civo-sandbox-use1-dev",
				"api.example.com",
				"civo",
				"nyc1",
				"use1",
				"experimental",
				"witself_prv_provision-only",
				"witself_bak_backup-only",
				test.target,
			)
			if got.BackupValidationTarget != test.target {
				t.Fatalf("backup validation target = %v, want %v", got.BackupValidationTarget, test.target)
			}
			if got.Accepting == nil || *got.Accepting == test.target {
				t.Fatalf("accepting = %v, want %v", got.Accepting, !test.target)
			}
			if got.Name != "civo-sandbox-use1-dev" ||
				got.Endpoint != "https://api.example.com" {
				t.Fatalf("cell identity changed: %#v", got)
			}
		})
	}
}
