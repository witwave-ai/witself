package lifecycle

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/billing"
	"github.com/witwave-ai/witself/internal/blob"
	"github.com/witwave-ai/witself/internal/plans"
)

type billingRolloutFakeReader struct {
	mu      sync.Mutex
	objects map[string][]byte
	calls   map[string]int

	listTransform func(prefix string, call int, in []blob.ObjectInfo) ([]blob.ObjectInfo, error)
	getTransform  func(key string, data []byte, etag string) ([]byte, string, error)
}

func newBillingRolloutFakeReader() *billingRolloutFakeReader {
	return &billingRolloutFakeReader{
		objects: make(map[string][]byte),
		calls:   make(map[string]int),
	}
}

func (r *billingRolloutFakeReader) ListComplete(
	ctx context.Context,
	prefix string,
	maxKeys int,
) ([]blob.ObjectInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if maxKeys < 1 {
		return nil, errors.New("invalid test bound")
	}
	r.calls[prefix]++
	call := r.calls[prefix]
	keys := make([]string, 0)
	for key := range r.objects {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	if len(keys) > maxKeys {
		return nil, errors.New("test inventory exceeds bound")
	}
	objects := make([]blob.ObjectInfo, 0, len(keys))
	for _, key := range keys {
		data := r.objects[key]
		objects = append(objects, blob.ObjectInfo{
			Key: key, ETag: billingRolloutTestETag(data), Size: int64(len(data)),
		})
	}
	if r.listTransform != nil {
		return r.listTransform(prefix, call, objects)
	}
	return objects, nil
}

func (r *billingRolloutFakeReader) GetBounded(
	ctx context.Context,
	key string,
	maxBytes int64,
) ([]byte, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	data, ok := r.objects[key]
	if !ok {
		return nil, "", fmt.Errorf("missing secret object %s", key)
	}
	if int64(len(data)) > maxBytes {
		return nil, "", fmt.Errorf("secret object %s is too large", key)
	}
	copyData := append([]byte(nil), data...)
	etag := billingRolloutTestETag(copyData)
	if r.getTransform != nil {
		return r.getTransform(key, copyData, etag)
	}
	return copyData, etag, nil
}

func billingRolloutTestETag(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func billingRolloutTestOptions(at time.Time) BillingRolloutInventoryOptions {
	cohort, api, reconcilers := 0, 0, 0
	return BillingRolloutInventoryOptions{
		R2Prefix: "registry", CapturedAt: at,
		BillingMutationCohortAccounts: &cohort,
		SourceAPIReplicas:             &api,
		SourceReconcilerReplicas:      &reconcilers,
	}
}

func billingRolloutTestAccountKey(prefix, accountID string) string {
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return prefix + "accounts/" + accountID + ".json"
}

func billingRolloutTestReceiptKey(prefix, operationID string) string {
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return prefix + "billing-mutations/receipts/" +
		base64.RawURLEncoding.EncodeToString([]byte(operationID)) + ".json"
}

func billingRolloutAddRecord(
	t *testing.T,
	reader *billingRolloutFakeReader,
	prefix string,
	record Record,
) {
	t.Helper()
	if record.Version == 0 {
		record.Version = 1
	}
	data, err := marshalR2Record(record)
	if err != nil {
		t.Fatal(err)
	}
	reader.objects[billingRolloutTestAccountKey(prefix, record.AccountID)] = data
}

func billingRolloutAddReceipt(
	t *testing.T,
	reader *billingRolloutFakeReader,
	prefix string,
	receipt BillingMutationReceipt,
) {
	t.Helper()
	data, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	reader.objects[billingRolloutTestReceiptKey(prefix, receipt.OperationID)] = data
}

func billingRolloutPendingReceipt(
	t *testing.T,
	accountID string,
	operation BillingMutationOperation,
	targetPlan string,
	createdAt time.Time,
	suffix string,
) BillingMutationReceipt {
	t.Helper()
	approval := billingMutationApproval{}
	switch operation {
	case BillingMutationSetup:
		approval.ExecutionClass = BillingMutationExecutionSetup
	case BillingMutationPlanUpgrade:
		approval = billingMutationApproval{
			ExecutionClass:     BillingMutationExecutionUpgradeSelfServe,
			ApprovedPriceCents: 3000, ApprovedCurrency: "usd",
		}
	case BillingMutationPlanDowngrade:
		approval = billingMutationApproval{
			ExecutionClass:   BillingMutationExecutionDowngrade,
			ApprovedCurrency: "usd",
		}
	case BillingMutationPlanCancel:
		approval.ExecutionClass = BillingMutationExecutionCancel
	default:
		t.Fatalf("unsupported test operation %q", operation)
	}
	receipt, err := buildBillingMutationReceipt(
		accountID,
		"owner@example.invalid",
		BillingActor{ID: "usr_inventory_operator", Role: "owner"},
		BillingMutationCommand{
			Operation: operation, Plan: targetPlan, Reason: "rollout test",
			Confirmed: true, IdempotencyKey: "inventory-key-" + suffix,
		},
		1,
		approval,
		createdAt,
	)
	if err != nil {
		t.Fatal(err)
	}
	receipt.Version = 1
	return receipt
}

func billingRolloutBaseRecord(accountID string) Record {
	return Record{
		AccountID: accountID, Entitled: "standard", Applied: "standard",
		Version: 1,
	}
}

func billingRolloutPreparedPending(
	operationID string,
	requested, effective time.Time,
) *Pending {
	providerObjectID := "sub_inventory_prepared"
	return &Pending{
		Kind: PendingDowngrade, Plan: plans.Free, OperationID: operationID,
		ProviderObjectID: providerObjectID, PreparedEffective: effective,
		ProviderPhase: pendingProviderPrepared, CancelPrevious: true,
		CancelPreviousTarget: &billing.PendingCancellation{
			Kind: preparedDowngradeFenceKind, ProviderObjectID: providerObjectID,
			OriginalOperationID: operationID,
		},
		Requested: requested,
	}
}

func TestCollectBillingRolloutInventoryCountOnlyArtifact(t *testing.T) {
	capturedAt := time.Date(2026, 8, 17, 22, 0, 0, 0, time.UTC)
	reader := newBillingRolloutFakeReader()

	preparedReceipt := billingRolloutPendingReceipt(
		t, "acct_prepared_private", BillingMutationPlanDowngrade, plans.Free,
		capturedAt.Add(-24*time.Hour), "prepared-0001")
	prepared := billingRolloutBaseRecord(preparedReceipt.AccountID)
	prepared.Provider = "stripe"
	prepared.CustomerID = "cus_prepared_private"
	prepared.Pending = billingRolloutPreparedPending(
		preparedReceipt.OperationID, capturedAt.Add(-24*time.Hour),
		capturedAt.Add(30*24*time.Hour))
	billingRolloutAddRecord(t, reader, "registry", prepared)
	billingRolloutAddReceipt(t, reader, "registry", preparedReceipt)

	targetless := billingRolloutBaseRecord("acct_targetless_private")
	targetless.Provider = "stripe"
	targetless.CustomerID = "cus_targetless_private"
	targetless.Pending = &Pending{
		Kind: PendingUpgrade, Plan: "professional",
		OperationID: "bop_targetless_private", Requested: capturedAt.Add(-time.Hour),
		Expires: capturedAt.Add(time.Hour),
	}
	billingRolloutAddRecord(t, reader, "registry", targetless)

	malformedKey := billingRolloutTestAccountKey("registry", "acct_malformed_private")
	reader.objects[malformedKey] = []byte(`{"AccountID":`)
	malformedReceiptKey := billingRolloutTestReceiptKey(
		"registry", "bop_malformed_private")
	reader.objects[malformedReceiptKey] = []byte(`{"schema_version":2}`)

	oldSetup := billingRolloutPendingReceipt(
		t, "acct_old_setup_private", BillingMutationSetup, "",
		capturedAt.Add(-billingMutationAutomaticRetryHorizon), "old-setup-0001")
	billingRolloutAddReceipt(t, reader, "registry", oldSetup)
	youngSetup := billingRolloutPendingReceipt(
		t, "acct_young_setup_private", BillingMutationSetup, "",
		capturedAt.Add(-billingMutationAutomaticRetryHorizon+time.Second),
		"young-setup-01")
	billingRolloutAddReceipt(t, reader, "registry", youngSetup)

	options := billingRolloutTestOptions(capturedAt)
	cohort, api, reconcilers := 3, 4, 5
	options.BillingMutationCohortAccounts = &cohort
	options.SourceAPIReplicas = &api
	options.SourceReconcilerReplicas = &reconcilers
	inventory, err := CollectBillingRolloutInventory(
		context.Background(), reader, options)
	if err != nil {
		t.Fatal(err)
	}
	wantRecords := BillingRolloutInventoryRecords{
		PreparedDowngrades: 1, TargetlessPendingChanges: 1,
		MalformedPendingChanges: 1, MalformedMutationReceipts: 1,
		PostRetryHorizonReceipts: 2,
	}
	if inventory.Schema != BillingRolloutInventorySchema ||
		inventory.CapturedAt != "2026-08-17T22:00:00Z" ||
		inventory.BillingMutationCohortAccounts != cohort ||
		inventory.SourceFleet.APIReplicas != api ||
		inventory.SourceFleet.ReconcilerReplicas != reconcilers ||
		inventory.Records != wantRecords {
		t.Fatalf("inventory = %+v; want records %+v", inventory, wantRecords)
	}
	encoded, err := json.Marshal(inventory)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		"acct_", "bop_", "cus_", "sub_", "owner@", "registry/", "private",
	} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("count-only artifact leaked %q: %s", forbidden, encoded)
		}
	}
	var shape map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &shape); err != nil {
		t.Fatal(err)
	}
	if len(shape) != 5 {
		t.Fatalf("top-level artifact fields = %v", shape)
	}
}

func TestBillingRolloutPendingClassificationOrder(t *testing.T) {
	now := time.Date(2026, 8, 17, 22, 0, 0, 0, time.UTC)
	effective := now.Add(30 * 24 * time.Hour)
	validTarget := &billing.PendingCancellation{
		Kind:             billing.PendingCancellationHostedAction,
		ProviderObjectID: "cs_previous", OriginalOperationID: "bop_previous",
	}
	tests := []struct {
		name   string
		record Record
		want   billingRolloutPendingClass
	}{
		{name: "none", record: billingRolloutBaseRecord("acct_none"), want: billingRolloutPendingNone},
		{
			name: "contact",
			record: func() Record {
				r := billingRolloutBaseRecord("acct_contact")
				r.Pending = &Pending{Kind: PendingContact, Plan: "enterprise", OperationID: "bop_contact", Requested: now}
				return r
			}(),
			want: billingRolloutPendingNone,
		},
		{
			name: "exact replacement cleanup",
			record: func() Record {
				r := billingRolloutBaseRecord("acct_cleanup")
				r.Provider, r.CustomerID = "stripe", "cus_cleanup"
				r.Pending = &Pending{Kind: PendingContact, Plan: "enterprise", OperationID: "bop_cleanup", Requested: now, CancelPrevious: true, CancelPreviousTarget: validTarget}
				return r
			}(),
			want: billingRolloutPendingNone,
		},
		{
			name: "replacement missing target",
			record: func() Record {
				r := billingRolloutBaseRecord("acct_missing_target")
				r.Provider, r.CustomerID = "stripe", "cus_missing_target"
				r.Pending = &Pending{Kind: PendingContact, Plan: "enterprise", OperationID: "bop_missing_target", Requested: now, CancelPrevious: true}
				return r
			}(),
			want: billingRolloutPendingTargetless,
		},
		{
			name: "hosted effect missing object",
			record: func() Record {
				r := billingRolloutBaseRecord("acct_hosted_targetless")
				r.Provider, r.CustomerID = "stripe", "cus_hosted_targetless"
				r.Pending = &Pending{Kind: PendingUpgrade, Plan: "professional", OperationID: "bop_hosted_targetless", Requested: now, Expires: now.Add(time.Hour)}
				return r
			}(),
			want: billingRolloutPendingTargetless,
		},
		{
			name: "legacy schedule missing object",
			record: func() Record {
				r := billingRolloutBaseRecord("acct_schedule_targetless")
				r.Provider, r.CustomerID = "stripe", "cus_schedule_targetless"
				r.Pending = &Pending{Kind: PendingDowngrade, Plan: plans.Free, OperationID: "bop_schedule_targetless", Requested: now, Effective: effective}
				return r
			}(),
			want: billingRolloutPendingTargetless,
		},
		{
			name: "prepared downgrade",
			record: func() Record {
				r := billingRolloutBaseRecord("acct_prepared")
				r.Provider, r.CustomerID = "stripe", "cus_prepared"
				r.Pending = billingRolloutPreparedPending("bop_prepared", now, effective)
				return r
			}(),
			want: billingRolloutPendingPrepared,
		},
		{
			name: "applied downgrade",
			record: func() Record {
				r := billingRolloutBaseRecord("acct_applied")
				r.Provider, r.CustomerID = "stripe", "cus_applied"
				r.Pending = &Pending{Kind: PendingDowngrade, Plan: plans.Free, OperationID: "bop_applied", Requested: now, ProviderObjectID: "sub_applied", ProviderPhase: pendingProviderApplied, Effective: effective}
				return r
			}(),
			want: billingRolloutPendingNone,
		},
		{
			name: "legacy applied downgrade",
			record: func() Record {
				r := billingRolloutBaseRecord("acct_legacy_applied")
				r.Provider, r.CustomerID = "stripe", "cus_legacy_applied"
				r.Pending = &Pending{Kind: PendingDowngrade, Plan: plans.Free, OperationID: "bop_legacy_applied", Requested: now, ProviderObjectID: "sub_legacy_applied", Effective: effective}
				return r
			}(),
			want: billingRolloutPendingNone,
		},
		{
			name: "unknown kind is malformed before targetless",
			record: func() Record {
				r := billingRolloutBaseRecord("acct_unknown")
				r.Provider, r.CustomerID = "stripe", "cus_unknown"
				r.Pending = &Pending{Kind: "other", Plan: plans.Free, OperationID: "bop_unknown", Requested: now, CancelPrevious: true}
				return r
			}(),
			want: billingRolloutPendingMalformed,
		},
		{
			name: "invalid replacement target is malformed before targetless",
			record: func() Record {
				r := billingRolloutBaseRecord("acct_bad_target")
				r.Provider, r.CustomerID = "stripe", "cus_bad_target"
				r.Pending = &Pending{Kind: PendingContact, Plan: "enterprise", OperationID: "bop_bad_target", Requested: now, CancelPrevious: true, CancelPreviousTarget: &billing.PendingCancellation{Kind: billing.PendingCancellationHostedAction, ProviderObjectID: "bad object", OriginalOperationID: "bop_previous"}}
				return r
			}(),
			want: billingRolloutPendingMalformed,
		},
		{
			name: "prepared boundary mismatch is malformed",
			record: func() Record {
				r := billingRolloutBaseRecord("acct_bad_prepared")
				r.Provider, r.CustomerID = "stripe", "cus_bad_prepared"
				r.Pending = billingRolloutPreparedPending("bop_bad_prepared", now, effective)
				r.Pending.PreparedEffective = time.Time{}
				return r
			}(),
			want: billingRolloutPendingMalformed,
		},
		{
			name: "applied phase without boundary is malformed",
			record: func() Record {
				r := billingRolloutBaseRecord("acct_bad_applied")
				r.Provider, r.CustomerID = "stripe", "cus_bad_applied"
				r.Pending = &Pending{Kind: PendingDowngrade, Plan: plans.Free, OperationID: "bop_bad_applied", Requested: now, ProviderObjectID: "sub_bad_applied", ProviderPhase: pendingProviderApplied}
				return r
			}(),
			want: billingRolloutPendingMalformed,
		},
		{
			name: "hosted object without URL is malformed",
			record: func() Record {
				r := billingRolloutBaseRecord("acct_bad_hosted")
				r.Provider, r.CustomerID = "stripe", "cus_bad_hosted"
				r.Pending = &Pending{Kind: PendingUpgrade, Plan: "professional", OperationID: "bop_bad_hosted", Requested: now, Expires: now.Add(time.Hour), ProviderObjectID: "cs_bad_hosted"}
				return r
			}(),
			want: billingRolloutPendingMalformed,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyBillingRolloutPending(test.record); got != test.want {
				t.Fatalf("class = %d; want %d", got, test.want)
			}
		})
	}
}

func TestBillingRolloutReceiptTerminalEvidence(t *testing.T) {
	capturedAt := time.Date(2026, 8, 17, 22, 0, 0, 0, time.UTC)
	effective := capturedAt.Add(30 * 24 * time.Hour)
	tests := []struct {
		name      string
		operation BillingMutationOperation
		plan      string
		mutate    func(*Record, BillingMutationReceipt)
		want      bool
	}{
		{
			name: "upgrade entitlement", operation: BillingMutationPlanUpgrade, plan: "professional",
			mutate: func(record *Record, receipt BillingMutationReceipt) { record.Entitled = receipt.TargetPlan },
			want:   true,
		},
		{
			name: "upgrade hosted action", operation: BillingMutationPlanUpgrade, plan: "professional",
			mutate: func(record *Record, receipt BillingMutationReceipt) {
				record.Provider, record.CustomerID = "stripe", "cus_action"
				record.Pending = &Pending{Kind: PendingUpgrade, Plan: receipt.TargetPlan, OperationID: receipt.OperationID, Requested: capturedAt, Expires: capturedAt.Add(time.Hour), URL: "https://billing.example.invalid/action", ProviderObjectID: "cs_action"}
			},
			want: true,
		},
		{
			name: "upgrade contact", operation: BillingMutationPlanUpgrade, plan: "enterprise",
			mutate: func(record *Record, receipt BillingMutationReceipt) {
				record.Pending = &Pending{Kind: PendingContact, Plan: receipt.TargetPlan, OperationID: receipt.OperationID, Requested: capturedAt}
			},
			want: true,
		},
		{
			name: "downgrade applied", operation: BillingMutationPlanDowngrade, plan: plans.Free,
			mutate: func(record *Record, receipt BillingMutationReceipt) {
				record.Provider, record.CustomerID = "stripe", "cus_down"
				record.Pending = &Pending{Kind: PendingDowngrade, Plan: receipt.TargetPlan, OperationID: receipt.OperationID, Requested: capturedAt, ProviderObjectID: "sub_down", ProviderPhase: pendingProviderApplied, Effective: effective}
			},
			want: true,
		},
		{
			name: "downgrade tombstone", operation: BillingMutationPlanDowngrade, plan: plans.Free,
			mutate: func(record *Record, receipt BillingMutationReceipt) {
				setBillingMutationTombstone(record, receipt.OperationID, BillingMutationPlanDowngrade, BillingMutationResultScheduled, receipt.TargetPlan, effective, capturedAt)
			},
			want: true,
		},
		{
			name: "cancel tombstone", operation: BillingMutationPlanCancel,
			mutate: func(record *Record, receipt BillingMutationReceipt) {
				setBillingMutationTombstone(record, receipt.OperationID, BillingMutationPlanCancel, BillingMutationResultCancelled, "", time.Time{}, capturedAt)
			},
			want: true,
		},
		{
			name: "setup has no account evidence", operation: BillingMutationSetup,
			mutate: func(*Record, BillingMutationReceipt) {},
			want:   false,
		},
		{
			name: "different operation is not evidence", operation: BillingMutationPlanDowngrade, plan: plans.Free,
			mutate: func(record *Record, receipt BillingMutationReceipt) {
				setBillingMutationTombstone(record, "bop_different", BillingMutationPlanDowngrade, BillingMutationResultScheduled, receipt.TargetPlan, effective, capturedAt)
			},
			want: false,
		},
	}
	for i, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			receipt := billingRolloutPendingReceipt(
				t, fmt.Sprintf("acct_evidence_%d", i), test.operation, test.plan,
				capturedAt.Add(-24*time.Hour), fmt.Sprintf("evidence-%04d", i))
			record := billingRolloutBaseRecord(receipt.AccountID)
			test.mutate(&record, receipt)
			accounts := map[string]billingRolloutScannedAccount{
				record.AccountID: {record: record, valid: true},
			}
			if got := billingRolloutHasTerminalAccountEvidence(receipt, accounts); got != test.want {
				t.Fatalf("terminal evidence = %t; want %t", got, test.want)
			}
		})
	}
}

func TestCollectBillingRolloutInventoryValidatesEveryTerminalReceipt(t *testing.T) {
	capturedAt := time.Date(2026, 8, 17, 22, 0, 0, 0, time.UTC)
	reader := newBillingRolloutFakeReader()

	completed := billingRolloutPendingReceipt(
		t, "acct_completed", BillingMutationPlanDowngrade, plans.Free,
		capturedAt.Add(-48*time.Hour), "completed-0001")
	completedAt := capturedAt.Add(-47 * time.Hour)
	effective := capturedAt.Add(30 * 24 * time.Hour)
	completed.Status = BillingMutationCompleted
	completed.Result = &BillingMutationResult{
		Kind: BillingMutationResultScheduled, Plan: plans.Free, Effective: &effective,
	}
	completed.CompletedAt = &completedAt
	completed.UpdatedAt = completedAt
	billingRolloutAddReceipt(t, reader, "registry", completed)

	superseded := billingRolloutPendingReceipt(
		t, "acct_superseded", BillingMutationSetup, "",
		capturedAt.Add(-48*time.Hour), "superseded-001")
	superseded.Status = BillingMutationSuperseded
	superseded.CompletedAt = &completedAt
	superseded.UpdatedAt = completedAt
	superseded.SupersededByOperationID = "bop_newer_operation"
	billingRolloutAddReceipt(t, reader, "registry", superseded)

	invalidTerminal := completed
	invalidTerminal.OperationID = "bop_invalid_terminal"
	invalidTerminal.CompletedAt = nil
	billingRolloutAddReceipt(t, reader, "registry", invalidTerminal)

	inventory, err := CollectBillingRolloutInventory(
		context.Background(), reader, billingRolloutTestOptions(capturedAt))
	if err != nil {
		t.Fatal(err)
	}
	if inventory.Records.MalformedMutationReceipts != 1 ||
		inventory.Records.PostRetryHorizonReceipts != 0 {
		t.Fatalf("terminal receipt counts = %+v", inventory.Records)
	}
}

func TestCollectBillingRolloutInventoryMalformedAccountCannotSupplyReceiptEvidence(t *testing.T) {
	capturedAt := time.Date(2026, 8, 17, 22, 0, 0, 0, time.UTC)
	reader := newBillingRolloutFakeReader()
	receipt := billingRolloutPendingReceipt(
		t, "acct_untrusted_evidence", BillingMutationPlanUpgrade, "professional",
		capturedAt.Add(-24*time.Hour), "untrusted-0001")
	record := billingRolloutBaseRecord(receipt.AccountID)
	record.Entitled = receipt.TargetPlan
	data, err := marshalR2Record(record)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data[:len(data)-1], []byte(`,"UnknownAuthority":"yes"}`)...)
	reader.objects[billingRolloutTestAccountKey("registry", record.AccountID)] = data
	billingRolloutAddReceipt(t, reader, "registry", receipt)

	inventory, err := CollectBillingRolloutInventory(
		context.Background(), reader, billingRolloutTestOptions(capturedAt))
	if err != nil {
		t.Fatal(err)
	}
	if inventory.Records.MalformedPendingChanges != 1 ||
		inventory.Records.PostRetryHorizonReceipts != 1 {
		t.Fatalf("untrusted evidence counts = %+v", inventory.Records)
	}
}

func TestCollectBillingRolloutInventoryRejectsMismatchedObjectIdentity(t *testing.T) {
	capturedAt := time.Date(2026, 8, 17, 22, 0, 0, 0, time.UTC)
	reader := newBillingRolloutFakeReader()
	record := billingRolloutBaseRecord("acct_body_identity")
	recordData, err := marshalR2Record(record)
	if err != nil {
		t.Fatal(err)
	}
	reader.objects[billingRolloutTestAccountKey("registry", "acct_key_identity")] = recordData

	receipt := billingRolloutPendingReceipt(
		t, "acct_receipt_identity", BillingMutationSetup, "",
		capturedAt.Add(-time.Hour), "identity-00001")
	receiptData, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	reader.objects[billingRolloutTestReceiptKey(
		"registry", "bop_different_key_identity")] = receiptData

	inventory, err := CollectBillingRolloutInventory(
		context.Background(), reader, billingRolloutTestOptions(capturedAt))
	if err != nil {
		t.Fatal(err)
	}
	if inventory.Records.MalformedPendingChanges != 1 ||
		inventory.Records.MalformedMutationReceipts != 1 {
		t.Fatalf("identity mismatch counts = %+v", inventory.Records)
	}
}

func TestDecodeCanonicalBillingRolloutJSON(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		ok   bool
	}{
		{name: "canonical", data: []byte(`{"value":1}`), ok: true},
		{name: "unknown", data: []byte(`{"value":1,"extra":2}`)},
		{name: "duplicate", data: []byte(`{"value":1,"value":2}`)},
		{name: "nested duplicate", data: []byte(`{"value":1,"child":{"name":1,"name":2}}`)},
		{name: "trailing", data: []byte(`{"value":1} {"value":2}`)},
		{name: "invalid UTF-8", data: []byte{'{', '"', 'v', 'a', 'l', 'u', 'e', '"', ':', '"', 0xff, '"', '}'}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var target struct {
				Value int `json:"value"`
			}
			err := decodeCanonicalBillingRolloutJSON(test.data, &target)
			if (err == nil) != test.ok {
				t.Fatalf("decode error = %v; ok=%t", err, test.ok)
			}
		})
	}
}

func TestCollectBillingRolloutInventoryRejectsOmittedOrNoncanonicalEvidence(t *testing.T) {
	now := time.Date(2026, 8, 17, 22, 0, 0, 0, time.UTC)
	reader := newBillingRolloutFakeReader()
	tests := []struct {
		name    string
		reader  BillingRolloutInventoryReader
		options BillingRolloutInventoryOptions
	}{
		{name: "nil reader", options: billingRolloutTestOptions(now)},
		{
			name:   "missing cohort",
			reader: reader,
			options: func() BillingRolloutInventoryOptions {
				o := billingRolloutTestOptions(now)
				o.BillingMutationCohortAccounts = nil
				return o
			}(),
		},
		{
			name:   "missing api",
			reader: reader,
			options: func() BillingRolloutInventoryOptions {
				o := billingRolloutTestOptions(now)
				o.SourceAPIReplicas = nil
				return o
			}(),
		},
		{
			name:   "missing reconciler",
			reader: reader,
			options: func() BillingRolloutInventoryOptions {
				o := billingRolloutTestOptions(now)
				o.SourceReconcilerReplicas = nil
				return o
			}(),
		},
		{
			name:   "negative external count",
			reader: reader,
			options: func() BillingRolloutInventoryOptions {
				o := billingRolloutTestOptions(now)
				negative := -1
				o.SourceAPIReplicas = &negative
				return o
			}(),
		},
		{
			name:    "fractional capture",
			reader:  reader,
			options: billingRolloutTestOptions(now.Add(time.Nanosecond)),
		},
		{
			name:    "non UTC capture",
			reader:  reader,
			options: billingRolloutTestOptions(now.In(time.FixedZone("zero", 0))),
		},
		{
			name:   "unclean prefix",
			reader: reader,
			options: func() BillingRolloutInventoryOptions {
				o := billingRolloutTestOptions(now)
				o.R2Prefix = " registry"
				return o
			}(),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			inventory, err := CollectBillingRolloutInventory(
				context.Background(), test.reader, test.options)
			if inventory != nil || !errors.Is(err, ErrBillingRolloutInventoryInput) {
				t.Fatalf("inventory/error = %#v / %v", inventory, err)
			}
		})
	}
}

func TestCollectBillingRolloutInventoryDropsPartialArtifactOnReadOrDrift(t *testing.T) {
	capturedAt := time.Date(2026, 8, 17, 22, 0, 0, 0, time.UTC)
	const secretID = "acct_do_not_leak_private_identifier"
	tests := []struct {
		name      string
		configure func(*billingRolloutFakeReader)
	}{
		{
			name: "account list failure",
			configure: func(reader *billingRolloutFakeReader) {
				reader.listTransform = func(prefix string, call int, in []blob.ObjectInfo) ([]blob.ObjectInfo, error) {
					if strings.HasSuffix(prefix, "accounts/") && call == 1 {
						return nil, errors.New(secretID)
					}
					return in, nil
				}
			},
		},
		{
			name: "receipt list failure",
			configure: func(reader *billingRolloutFakeReader) {
				reader.listTransform = func(prefix string, call int, in []blob.ObjectInfo) ([]blob.ObjectInfo, error) {
					if strings.HasSuffix(prefix, "receipts/") && call == 1 {
						return nil, errors.New(secretID)
					}
					return in, nil
				}
			},
		},
		{
			name: "object read failure",
			configure: func(reader *billingRolloutFakeReader) {
				reader.getTransform = func(key string, data []byte, etag string) ([]byte, string, error) {
					return nil, "", fmt.Errorf("%s at %s", secretID, key)
				}
			},
		},
		{
			name: "get etag drift",
			configure: func(reader *billingRolloutFakeReader) {
				reader.getTransform = func(key string, data []byte, etag string) ([]byte, string, error) {
					return data, "changed-" + etag, nil
				}
			},
		},
		{
			name: "get size drift",
			configure: func(reader *billingRolloutFakeReader) {
				reader.getTransform = func(key string, data []byte, etag string) ([]byte, string, error) {
					return append(data, 'x'), etag, nil
				}
			},
		},
		{
			name: "account second-list drift",
			configure: func(reader *billingRolloutFakeReader) {
				reader.listTransform = func(prefix string, call int, in []blob.ObjectInfo) ([]blob.ObjectInfo, error) {
					if strings.HasSuffix(prefix, "accounts/") && call == 2 {
						out := append([]blob.ObjectInfo(nil), in...)
						out[0].ETag = "changed"
						return out, nil
					}
					return in, nil
				}
			},
		},
		{
			name: "receipt second-list drift",
			configure: func(reader *billingRolloutFakeReader) {
				reader.listTransform = func(prefix string, call int, in []blob.ObjectInfo) ([]blob.ObjectInfo, error) {
					if strings.HasSuffix(prefix, "receipts/") && call == 2 {
						return append(in, blob.ObjectInfo{Key: prefix + secretID, ETag: "new", Size: 1}), nil
					}
					return in, nil
				}
			},
		},
		{
			name: "invalid ordering",
			configure: func(reader *billingRolloutFakeReader) {
				reader.listTransform = func(prefix string, call int, in []blob.ObjectInfo) ([]blob.ObjectInfo, error) {
					if strings.HasSuffix(prefix, "accounts/") && call == 1 {
						return append(in, in[0]), nil
					}
					return in, nil
				}
			},
		},
		{
			name: "oversized metadata",
			configure: func(reader *billingRolloutFakeReader) {
				reader.listTransform = func(prefix string, call int, in []blob.ObjectInfo) ([]blob.ObjectInfo, error) {
					if strings.HasSuffix(prefix, "accounts/") && call == 1 {
						out := append([]blob.ObjectInfo(nil), in...)
						out[0].Size = billingRolloutInventoryMaxObjectBytes + 1
						return out, nil
					}
					return in, nil
				}
			},
		},
		{
			name: "missing identity metadata",
			configure: func(reader *billingRolloutFakeReader) {
				reader.listTransform = func(prefix string, call int, in []blob.ObjectInfo) ([]blob.ObjectInfo, error) {
					if strings.HasSuffix(prefix, "receipts/") && call == 1 {
						out := append([]blob.ObjectInfo(nil), in...)
						out[0].ETag = ""
						return out, nil
					}
					return in, nil
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reader := newBillingRolloutFakeReader()
			record := billingRolloutBaseRecord(secretID)
			billingRolloutAddRecord(t, reader, "registry", record)
			receipt := billingRolloutPendingReceipt(
				t, secretID, BillingMutationSetup, "",
				capturedAt.Add(-time.Hour), "drift-test-0001")
			billingRolloutAddReceipt(t, reader, "registry", receipt)
			test.configure(reader)
			inventory, err := CollectBillingRolloutInventory(
				context.Background(), reader, billingRolloutTestOptions(capturedAt))
			if inventory != nil || !errors.Is(err, ErrBillingRolloutInventoryIncomplete) {
				t.Fatalf("inventory/error = %#v / %v", inventory, err)
			}
			if strings.Contains(err.Error(), secretID) || strings.Contains(err.Error(), "registry/") {
				t.Fatalf("sanitized error leaked identifier: %v", err)
			}
		})
	}
}

func TestCollectBillingRolloutInventoryCancellationReturnsNoArtifact(t *testing.T) {
	capturedAt := time.Date(2026, 8, 17, 22, 0, 0, 0, time.UTC)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	inventory, err := CollectBillingRolloutInventory(
		ctx, newBillingRolloutFakeReader(), billingRolloutTestOptions(capturedAt))
	if inventory != nil || !errors.Is(err, ErrBillingRolloutInventoryIncomplete) {
		t.Fatalf("inventory/error = %#v / %v", inventory, err)
	}
}

func TestCollectBillingRolloutInventoryConcurrentReadOnly(t *testing.T) {
	capturedAt := time.Date(2026, 8, 17, 22, 0, 0, 0, time.UTC)
	reader := newBillingRolloutFakeReader()
	record := billingRolloutBaseRecord("acct_concurrent_inventory")
	billingRolloutAddRecord(t, reader, "registry", record)
	options := billingRolloutTestOptions(capturedAt)

	const workers = 32
	errorsFound := make(chan error, workers)
	var wait sync.WaitGroup
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			inventory, err := CollectBillingRolloutInventory(
				context.Background(), reader, options)
			if err != nil {
				errorsFound <- err
				return
			}
			if inventory.Records != (BillingRolloutInventoryRecords{}) {
				errorsFound <- fmt.Errorf("unexpected counts: %+v", inventory.Records)
			}
		}()
	}
	wait.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Error(err)
	}
}
