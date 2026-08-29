package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/infra/pulumi/internal/fleet"
)

const destroyTestCell = "aws-sandbox-usw2-dev"

type fakePlacementStatusReader struct {
	status fleet.PlacementStatus
	err    error
	calls  int
}

func (f *fakePlacementStatusReader) GetPlacementStatus(context.Context, int) (fleet.PlacementStatus, error) {
	f.calls++
	return f.status, f.err
}

func TestDestroyInventoryGuardPhantomAndOverride(t *testing.T) {
	path := writeConfig(t, sampleConfig)
	phantom := "aws-sandbox-usw2-typo"

	var out strings.Builder
	err := checkDestroyInventory(phantom, path, false, &out)
	if err == nil {
		t.Fatal("phantom cell must be refused")
	}
	if got := err.Error(); !strings.Contains(got, "phantom-stack mismatch") || !strings.Contains(got, "--allow-unknown-cell") {
		t.Fatalf("phantom refusal = %q", got)
	}
	if strings.Contains(err.Error(), "witself_flt_") {
		t.Fatalf("phantom refusal must not expose credentials: %q", err)
	}

	out.Reset()
	if err := checkDestroyInventory(phantom, path, true, &out); err != nil {
		t.Fatalf("explicit phantom override: %v", err)
	}
	if !strings.Contains(out.String(), "--allow-unknown-cell") {
		t.Fatalf("override warning = %q", out.String())
	}
}

func TestDestroyAccountGuardLiveAndArchivedWithForce(t *testing.T) {
	var status fleet.PlacementStatus
	if err := json.Unmarshal(
		[]byte(`{"cells":[{"name":"aws-sandbox-usw2-dev","account_count":2,"archived_count":3}]}`),
		&status,
	); err != nil {
		t.Fatal(err)
	}
	reader := &fakePlacementStatusReader{status: status}

	var out strings.Builder
	err := checkDestroyAccounts(
		context.Background(), destroyTestCell, "https://control.invalid", "unused",
		false, false, reader, &out,
	)
	if err == nil {
		t.Fatal("cell with placed accounts must be refused")
	}
	for _, want := range []string{"2 live account(s)", "3 archived account(s)", "--force-with-accounts"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("account refusal %q does not contain %q", err, want)
		}
	}

	out.Reset()
	if err := checkDestroyAccounts(
		context.Background(), destroyTestCell, "https://control.invalid", "unused",
		true, false, reader, &out,
	); err != nil {
		t.Fatalf("explicit account override: %v", err)
	}
	if !strings.Contains(out.String(), "--force-with-accounts") || !strings.Contains(out.String(), "2 live account(s)") || !strings.Contains(out.String(), "3 archived account(s)") {
		t.Fatalf("force warning = %q", out.String())
	}
	if reader.calls != 2 {
		t.Fatalf("placement-status reads = %d, want 2", reader.calls)
	}
}

func TestDestroyAccountGuardUnreachableAndSkip(t *testing.T) {
	unreachable := &fakePlacementStatusReader{err: errors.New("control plane unavailable")}
	var out strings.Builder
	err := checkDestroyAccounts(
		context.Background(), destroyTestCell, "https://control.invalid", "unused",
		true, false, unreachable, &out,
	)
	if err == nil {
		t.Fatal("unreachable control plane must fail closed even with the account-count override")
	}
	if got := err.Error(); !strings.Contains(got, "placement status is unavailable") || !strings.Contains(got, "--skip-account-check") {
		t.Fatalf("unreachable refusal = %q", got)
	}
	if unreachable.calls != 1 {
		t.Fatalf("placement reader calls = %d, want 1", unreachable.calls)
	}

	out.Reset()
	if err := checkDestroyAccounts(
		context.Background(), destroyTestCell, "https://control.invalid", "unused",
		false, true, unreachable, &out,
	); err != nil {
		t.Fatalf("explicit account-check skip: %v", err)
	}
	if unreachable.calls != 1 {
		t.Fatalf("skip must avoid the placement read; calls = %d", unreachable.calls)
	}
	if !strings.Contains(out.String(), "--skip-account-check") {
		t.Fatalf("skip warning = %q", out.String())
	}
}

func TestDestroyAccountGuardRejectsAmbiguousStatus(t *testing.T) {
	tests := map[string]fleet.PlacementStatus{
		"missing target": {},
		"duplicate target": {Cells: []fleet.PlacementStatusCell{
			{Name: destroyTestCell},
			{Name: destroyTestCell},
		}},
		"negative count": {Cells: []fleet.PlacementStatusCell{{
			Name:         destroyTestCell,
			AccountCount: -1,
		}}},
	}
	for name, status := range tests {
		t.Run(name, func(t *testing.T) {
			reader := &fakePlacementStatusReader{status: status}
			err := checkDestroyAccounts(
				context.Background(), destroyTestCell, "https://control.invalid", "unused",
				true, false, reader, &strings.Builder{},
			)
			if err == nil || !strings.Contains(err.Error(), "placement status is unavailable") {
				t.Fatalf("ambiguous status refusal = %v", err)
			}
		})
	}
}

func TestDestroyAccountGuardRejectsOmittedCounts(t *testing.T) {
	for name, body := range map[string]string{
		"missing live count":     `{"cells":[{"name":"aws-sandbox-usw2-dev","archived_count":0}]}`,
		"missing archived count": `{"cells":[{"name":"aws-sandbox-usw2-dev","account_count":0}]}`,
		"null live count":        `{"cells":[{"name":"aws-sandbox-usw2-dev","account_count":null,"archived_count":0}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			var status fleet.PlacementStatus
			if err := json.Unmarshal([]byte(body), &status); err != nil {
				t.Fatal(err)
			}
			reader := &fakePlacementStatusReader{status: status}
			err := checkDestroyAccounts(
				context.Background(), destroyTestCell, "https://control.invalid", "unused",
				false, false, reader, &strings.Builder{},
			)
			if err == nil || !strings.Contains(err.Error(), "placement status is unavailable") {
				t.Fatalf("omitted-count result = %v", err)
			}
		})
	}
}

func TestDestroyInteractiveConfirmationRequiresExactCellName(t *testing.T) {
	tests := []struct {
		name  string
		input string
		ok    bool
	}{
		{name: "exact", input: destroyTestCell + "\n", ok: true},
		{name: "mismatch", input: "aws-sandbox-usw2-prod\n"},
		{name: "trailing space", input: destroyTestCell + " \n"},
		{name: "empty", input: "\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out strings.Builder
			err := confirmDestroy(destroyTestCell, "", true, strings.NewReader(tt.input), &out)
			if tt.ok && err != nil {
				t.Fatalf("exact confirmation: %v", err)
			}
			if !tt.ok && (err == nil || !strings.Contains(err.Error(), "confirmation mismatch")) {
				t.Fatalf("mismatch result = %v", err)
			}
			if !strings.Contains(out.String(), destroyTestCell) {
				t.Fatalf("prompt = %q", out.String())
			}
		})
	}
}

func TestDestroyNonInteractiveYesCellMustMatchExactly(t *testing.T) {
	if err := confirmDestroy(destroyTestCell, destroyTestCell, false, nil, &strings.Builder{}); err != nil {
		t.Fatalf("exact --yes-cell: %v", err)
	}
	for name, yesCell := range map[string]string{
		"missing":  "",
		"mismatch": "aws-sandbox-usw2-prod",
		"space":    destroyTestCell + " ",
	} {
		t.Run(name, func(t *testing.T) {
			err := confirmDestroy(destroyTestCell, yesCell, false, nil, &strings.Builder{})
			if err == nil || !strings.Contains(err.Error(), "--yes-cell") {
				t.Fatalf("non-interactive confirmation result = %v", err)
			}
		})
	}
}

func TestDestroySafetyGuardOrder(t *testing.T) {
	path := writeConfig(t, sampleConfig)
	reader := &fakePlacementStatusReader{status: fleet.PlacementStatus{
		Cells: []fleet.PlacementStatusCell{{Name: destroyTestCell, AccountCount: 1}},
	}}

	var out strings.Builder
	err := runDestroySafety(context.Background(), "aws-sandbox-usw2-typo", destroySafetyOptions{
		ConfigPath:         path,
		ControlPlane:       "https://control.invalid",
		YesCell:            "aws-sandbox-usw2-typo",
		Input:              strings.NewReader("aws-sandbox-usw2-typo\n"),
		Output:             &out,
		Interactive:        true,
		PlacementStatusAPI: reader,
	})
	if err == nil || !strings.Contains(err.Error(), "phantom-stack mismatch") {
		t.Fatalf("first guard result = %v", err)
	}
	if reader.calls != 0 {
		t.Fatalf("account check ran before phantom refusal; calls = %d", reader.calls)
	}
	if strings.Contains(out.String(), "Destroy target") {
		t.Fatalf("confirmation ran before phantom refusal: %q", out.String())
	}

	out.Reset()
	err = runDestroySafety(context.Background(), destroyTestCell, destroySafetyOptions{
		ConfigPath:         path,
		ControlPlane:       "https://control.invalid",
		YesCell:            destroyTestCell,
		Input:              strings.NewReader(destroyTestCell + "\n"),
		Output:             &out,
		Interactive:        true,
		PlacementStatusAPI: reader,
	})
	if err == nil || !strings.Contains(err.Error(), "live-accounts protection") {
		t.Fatalf("second guard result = %v", err)
	}
	if reader.calls != 1 {
		t.Fatalf("account check calls = %d, want 1", reader.calls)
	}
	if strings.Contains(out.String(), "Destroy target") {
		t.Fatalf("confirmation ran before account refusal: %q", out.String())
	}
}
