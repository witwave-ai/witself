package store

import (
	"bytes"
	"context"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/plans"
	"github.com/witwave-ai/witself/internal/testenv"
)

func TestMessagePoliciesSurviveAccountArchiveRoundTripPostgres(t *testing.T) {
	dsn := testenv.RequirePostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	source, _ := newMigrationTestStore(t, dsn)
	destination, _ := newMigrationTestStore(t, dsn)
	if err := source.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := destination.Migrate(); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		slug     string
		policies map[string]int64
	}{
		{
			name: "finite",
			slug: "finite",
			policies: map[string]int64{
				plans.MessageRetentionDaysPolicy:        90,
				plans.MessagingEntitlementVersionPolicy: plans.MessagingEntitlementVersion,
			},
		},
		{
			name: "explicit indefinite",
			slug: "indefinite",
			policies: map[string]int64{
				plans.MessagingEntitlementVersionPolicy: plans.MessagingEntitlementVersion,
			},
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			account, err := source.ProvisionAccount(
				ctx,
				"message-policy-archive-"+testCase.slug+"@witwave.ai",
				"message policy archive "+testCase.name,
				time.Hour,
			)
			if err != nil {
				t.Fatal(err)
			}
			if activated, err := source.ActivateAccount(ctx, account.AccountID); err != nil || !activated {
				t.Fatalf("activate = %v / %v", activated, err)
			}
			features := []string{"memory", plans.MessagingFeature}
			hash, err := plans.SnapshotHash(
				"enterprise",
				map[string]int64{},
				testCase.policies,
				features,
			)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := source.SetAccountPlan(
				ctx,
				account.AccountID,
				1,
				hash,
				"enterprise",
				map[string]int64{},
				testCase.policies,
				features,
			); err != nil {
				t.Fatal(err)
			}
			if err := source.SuspendAccountSystem(
				ctx,
				account.AccountID,
				"evacuation",
				"message policy archive test",
			); err != nil {
				t.Fatal(err)
			}

			var archive bytes.Buffer
			if err := source.ExportAccount(
				ctx,
				account.AccountID,
				"message-policy-source",
				"test",
				&archive,
			); err != nil {
				t.Fatal(err)
			}
			if _, err := destination.ImportAccount(
				ctx,
				account.AccountID,
				bytes.NewReader(archive.Bytes()),
			); err != nil {
				t.Fatal(err)
			}
			imported, err := destination.GetAccount(ctx, account.AccountID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(imported.PlanPolicies, testCase.policies) {
				t.Fatalf(
					"imported policies = %v, want %v",
					imported.PlanPolicies,
					testCase.policies,
				)
			}
			if !slices.Contains(imported.PlanFeatures, plans.MessagingFeature) {
				t.Fatalf("imported features = %v, want messaging", imported.PlanFeatures)
			}
		})
	}
}
