package lifecycle

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/witwave-ai/witself/internal/billing"
	"github.com/witwave-ai/witself/internal/blob"
)

const (
	// BillingRolloutInventorySchema is the strict count-only artifact consumed
	// by the incompatible billing-transition preflight.
	BillingRolloutInventorySchema = "witself.billing-rollout-inventory.v1"

	billingRolloutInventoryMaxAccountObjects = 1_000_000
	billingRolloutInventoryMaxReceiptObjects = 1_000_000
	billingRolloutInventoryMaxObjectBytes    = 16 << 20
)

var (
	// ErrBillingRolloutInventoryInput means the caller did not supply every
	// external cutover attestation in its canonical form.
	ErrBillingRolloutInventoryInput = errors.New(
		"invalid billing rollout inventory input")
	// ErrBillingRolloutInventoryIncomplete means a bounded R2 read could not
	// prove one stable, complete view. The collector deliberately drops the
	// partial counts and never wraps a storage error that might contain an
	// account id, operation id, object key, ETag, or endpoint.
	ErrBillingRolloutInventoryIncomplete = errors.New(
		"billing rollout inventory is incomplete or changed during capture")
)

// BillingRolloutInventoryReader is the read-only R2 boundary. ListComplete
// must return one strictly ordered, complete page-set with immutable object
// identity metadata; GetBounded must refuse an object larger than maxBytes.
type BillingRolloutInventoryReader interface {
	ListComplete(
		ctx context.Context,
		prefix string,
		maxKeys int,
	) ([]blob.ObjectInfo, error)
	GetBounded(
		ctx context.Context,
		key string,
		maxBytes int64,
	) (data []byte, etag string, err error)
}

// BillingRolloutInventoryOptions combines the R2 namespace with external
// stop-the-world evidence. Pointer counts are intentional: nil means the
// evidence was omitted, which is different from an attested zero.
type BillingRolloutInventoryOptions struct {
	R2Prefix string
	// CapturedAt is the exact operator fence and must already be canonical UTC
	// at whole-second precision. The collector never silently rounds it.
	CapturedAt time.Time

	BillingMutationCohortAccounts *int
	SourceAPIReplicas             *int
	SourceReconcilerReplicas      *int
}

// BillingRolloutInventory is deliberately value-free. It is safe to share as
// JSON: no identifier, object metadata, raw error, or inspected field can be
// represented by this type.
type BillingRolloutInventory struct {
	Schema                        string                             `json:"schema"`
	CapturedAt                    string                             `json:"captured_at"`
	BillingMutationCohortAccounts int                                `json:"billing_mutation_cohort_accounts"`
	SourceFleet                   BillingRolloutInventorySourceFleet `json:"source_fleet"`
	Records                       BillingRolloutInventoryRecords     `json:"records"`
}

// BillingRolloutInventorySourceFleet is the externally attested stopped
// source fleet. The collector requires both counts even when each is zero.
type BillingRolloutInventorySourceFleet struct {
	APIReplicas        int `json:"api_replicas"`
	ReconcilerReplicas int `json:"reconciler_replicas"`
}

// BillingRolloutInventoryRecords contains independent object-class counts.
// Pending-state and receipt counts can describe the same operation and must
// not be summed into a unique-operation total.
type BillingRolloutInventoryRecords struct {
	PreparedDowngrades        int `json:"prepared_downgrades"`
	TargetlessPendingChanges  int `json:"targetless_pending_changes"`
	MalformedPendingChanges   int `json:"malformed_pending_changes"`
	MalformedMutationReceipts int `json:"malformed_mutation_receipts"`
	PostRetryHorizonReceipts  int `json:"post_retry_horizon_receipts"`
}

type billingRolloutPendingClass uint8

const (
	billingRolloutPendingNone billingRolloutPendingClass = iota
	billingRolloutPendingMalformed
	billingRolloutPendingTargetless
	billingRolloutPendingPrepared
)

type billingRolloutScannedAccount struct {
	record Record
	valid  bool
}

// CollectBillingRolloutInventory takes one complete, stable, read-only view of
// the canonical account and billing-mutation receipt namespaces. Any listing,
// bounded-read, metadata, cancellation, or second-list failure returns a nil
// artifact. Malformed stored JSON is inventory data and is counted instead.
func CollectBillingRolloutInventory(
	ctx context.Context,
	reader BillingRolloutInventoryReader,
	options BillingRolloutInventoryOptions,
) (*BillingRolloutInventory, error) {
	prefix, cohortAccounts, sourceAPI, sourceReconcilers, err :=
		validateBillingRolloutInventoryOptions(reader, options)
	if err != nil {
		return nil, err
	}
	accountPrefix := prefix + "accounts/"
	receiptPrefix := prefix + "billing-mutations/receipts/"

	accountObjects, err := reader.ListComplete(
		ctx, accountPrefix, billingRolloutInventoryMaxAccountObjects)
	if err != nil || !validBillingRolloutObjectList(
		accountObjects, accountPrefix,
		billingRolloutInventoryMaxAccountObjects) {
		return nil, billingRolloutInventoryIncomplete("account listing failed")
	}
	accountObjects = append([]blob.ObjectInfo(nil), accountObjects...)
	receiptObjects, err := reader.ListComplete(
		ctx, receiptPrefix, billingRolloutInventoryMaxReceiptObjects)
	if err != nil || !validBillingRolloutObjectList(
		receiptObjects, receiptPrefix,
		billingRolloutInventoryMaxReceiptObjects) {
		return nil, billingRolloutInventoryIncomplete("receipt listing failed")
	}
	receiptObjects = append([]blob.ObjectInfo(nil), receiptObjects...)

	recordCounts := BillingRolloutInventoryRecords{}
	accounts := make(map[string]billingRolloutScannedAccount, len(accountObjects))
	for _, object := range accountObjects {
		data, readErr := readBillingRolloutObject(ctx, reader, object)
		if readErr != nil {
			return nil, readErr
		}
		record, class, valid := classifyBillingRolloutAccount(
			prefix, object.Key, data)
		switch class {
		case billingRolloutPendingMalformed:
			recordCounts.MalformedPendingChanges++
		case billingRolloutPendingTargetless:
			recordCounts.TargetlessPendingChanges++
		case billingRolloutPendingPrepared:
			recordCounts.PreparedDowngrades++
		}
		if valid {
			accounts[record.AccountID] = billingRolloutScannedAccount{
				record: record,
				valid:  true,
			}
		}
	}

	for _, object := range receiptObjects {
		data, readErr := readBillingRolloutObject(ctx, reader, object)
		if readErr != nil {
			return nil, readErr
		}
		receipt, valid := decodeBillingRolloutReceipt(prefix, object.Key, data)
		if !valid {
			recordCounts.MalformedMutationReceipts++
			continue
		}
		if receipt.Status == BillingMutationPending &&
			!options.CapturedAt.Before(
				receipt.CreatedAt.Add(billingMutationAutomaticRetryHorizon)) &&
			!billingRolloutHasTerminalAccountEvidence(receipt, accounts) {
			recordCounts.PostRetryHorizonReceipts++
		}
	}

	// Bind every parsed byte sequence to one stable final namespace view. The
	// first-list ETag was checked by each GET; an exact second list proves that
	// no key, ETag, or size drifted while either namespace was inspected.
	finalAccounts, err := reader.ListComplete(
		ctx, accountPrefix, billingRolloutInventoryMaxAccountObjects)
	if err != nil || !sameBillingRolloutObjectList(accountObjects, finalAccounts) {
		return nil, billingRolloutInventoryIncomplete(
			"account listing changed during capture")
	}
	finalReceipts, err := reader.ListComplete(
		ctx, receiptPrefix, billingRolloutInventoryMaxReceiptObjects)
	if err != nil || !sameBillingRolloutObjectList(receiptObjects, finalReceipts) {
		return nil, billingRolloutInventoryIncomplete(
			"receipt listing changed during capture")
	}
	if ctx.Err() != nil {
		return nil, billingRolloutInventoryIncomplete(
			"capture was cancelled before completion")
	}

	return &BillingRolloutInventory{
		Schema:                        BillingRolloutInventorySchema,
		CapturedAt:                    options.CapturedAt.Format(time.RFC3339),
		BillingMutationCohortAccounts: cohortAccounts,
		SourceFleet: BillingRolloutInventorySourceFleet{
			APIReplicas: sourceAPI, ReconcilerReplicas: sourceReconcilers,
		},
		Records: recordCounts,
	}, nil
}

func validateBillingRolloutInventoryOptions(
	reader BillingRolloutInventoryReader,
	options BillingRolloutInventoryOptions,
) (prefix string, cohortAccounts, sourceAPI, sourceReconcilers int, err error) {
	if reader == nil || options.CapturedAt.IsZero() ||
		options.CapturedAt.Location() != time.UTC ||
		options.CapturedAt.Nanosecond() != 0 ||
		options.CapturedAt.Year() < 1 || options.CapturedAt.Year() > 9999 ||
		options.BillingMutationCohortAccounts == nil ||
		options.SourceAPIReplicas == nil ||
		options.SourceReconcilerReplicas == nil {
		return "", 0, 0, 0, ErrBillingRolloutInventoryInput
	}
	cohortAccounts = *options.BillingMutationCohortAccounts
	sourceAPI = *options.SourceAPIReplicas
	sourceReconcilers = *options.SourceReconcilerReplicas
	if cohortAccounts < 0 || sourceAPI < 0 || sourceReconcilers < 0 {
		return "", 0, 0, 0, ErrBillingRolloutInventoryInput
	}
	prefix = options.R2Prefix
	if strings.TrimSpace(prefix) != prefix || strings.ContainsRune(prefix, '\x00') {
		return "", 0, 0, 0, ErrBillingRolloutInventoryInput
	}
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return prefix, cohortAccounts, sourceAPI, sourceReconcilers, nil
}

func validBillingRolloutObjectList(
	objects []blob.ObjectInfo,
	prefix string,
	maxObjects int,
) bool {
	if maxObjects < 1 || len(objects) > maxObjects {
		return false
	}
	previous := ""
	for i, object := range objects {
		if object.Key == "" || !strings.HasPrefix(object.Key, prefix) ||
			object.ETag == "" || object.Size < 0 ||
			object.Size > billingRolloutInventoryMaxObjectBytes ||
			(i > 0 && object.Key <= previous) {
			return false
		}
		previous = object.Key
	}
	return true
}

func sameBillingRolloutObjectList(left, right []blob.ObjectInfo) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func readBillingRolloutObject(
	ctx context.Context,
	reader BillingRolloutInventoryReader,
	object blob.ObjectInfo,
) ([]byte, error) {
	data, etag, err := reader.GetBounded(
		ctx, object.Key, billingRolloutInventoryMaxObjectBytes)
	if err != nil || etag == "" || etag != object.ETag ||
		int64(len(data)) != object.Size {
		return nil, billingRolloutInventoryIncomplete("object read was not stable")
	}
	return data, nil
}

func billingRolloutInventoryIncomplete(stage string) error {
	// stage is always a source-code constant. Never pass reader or object data.
	return fmt.Errorf("%w: %s", ErrBillingRolloutInventoryIncomplete, stage)
}

func classifyBillingRolloutAccount(
	prefix, key string,
	data []byte,
) (Record, billingRolloutPendingClass, bool) {
	var strictRecord Record
	record, err := unmarshalR2Record(data)
	if err != nil || decodeCanonicalBillingRolloutJSON(data, &strictRecord) != nil ||
		!validBillingMutationText(record.AccountID, 255) ||
		key != prefix+"accounts/"+record.AccountID+".json" ||
		record.Version < 1 || !validBillingRolloutProviderIdentity(record) ||
		validateBillingRolloutTombstone(record) != nil {
		return Record{}, billingRolloutPendingMalformed, false
	}
	class := classifyBillingRolloutPending(record)
	if class == billingRolloutPendingMalformed {
		return Record{}, class, false
	}
	return record, class, true
}

func validBillingRolloutProviderIdentity(record Record) bool {
	if (record.Provider == "") != (record.CustomerID == "") {
		return false
	}
	if record.Provider != "" &&
		(!validBillingMutationToken(record.Provider, 64) ||
			billing.ValidateProviderObjectID(record.CustomerID) != nil) {
		return false
	}
	if record.ManagedSubscriptionID != "" &&
		billing.ValidateProviderObjectID(record.ManagedSubscriptionID) != nil {
		return false
	}
	if record.Entitled != "" &&
		!validBillingMutationToken(record.Entitled, 128) {
		return false
	}
	if record.Applied != "" && !validBillingMutationToken(record.Applied, 128) {
		return false
	}
	return true
}

func validateBillingRolloutTombstone(record Record) error {
	hasTombstone := record.LastBillingMutationOperationID != "" ||
		record.LastBillingMutationKind != "" ||
		record.LastBillingMutationResultKind != "" ||
		record.LastBillingMutationPlan != "" ||
		!record.LastBillingMutationEffective.IsZero() ||
		!record.LastBillingMutationAt.IsZero()
	if !hasTombstone {
		return nil
	}
	if !validBillingOperationID(record.LastBillingMutationOperationID) ||
		billing.ValidateOperationID(record.LastBillingMutationOperationID) != nil ||
		record.LastBillingMutationAt.IsZero() {
		return errors.New("invalid billing mutation tombstone")
	}
	switch record.LastBillingMutationKind {
	case BillingMutationPlanDowngrade:
		if record.LastBillingMutationResultKind != BillingMutationResultScheduled ||
			!validBillingMutationToken(record.LastBillingMutationPlan, 128) ||
			record.LastBillingMutationEffective.IsZero() {
			return errors.New("invalid downgrade tombstone")
		}
	case BillingMutationPlanCancel:
		if record.LastBillingMutationPlan != "" ||
			!record.LastBillingMutationEffective.IsZero() ||
			(record.LastBillingMutationResultKind != "" &&
				record.LastBillingMutationResultKind != BillingMutationResultCancelled &&
				record.LastBillingMutationResultKind != BillingMutationResultResolved) {
			return errors.New("invalid cancellation tombstone")
		}
	default:
		return errors.New("unsupported billing mutation tombstone")
	}
	return nil
}

func classifyBillingRolloutPending(record Record) billingRolloutPendingClass {
	pending := record.Pending
	if pending == nil {
		return billingRolloutPendingNone
	}
	if !validBillingMutationToken(pending.Plan, 128) ||
		!validBillingOperationID(pending.OperationID) ||
		billing.ValidateOperationID(pending.OperationID) != nil ||
		pending.Requested.IsZero() ||
		(pending.ProviderObjectID != "" &&
			billing.ValidateProviderObjectID(pending.ProviderObjectID) != nil) ||
		(!pending.CancelPrevious && pending.CancelPreviousTarget != nil) {
		return billingRolloutPendingMalformed
	}

	if pending.ProviderPhase == pendingProviderPrepared {
		if !validPreparedBillingRolloutDowngrade(record, pending) {
			return billingRolloutPendingMalformed
		}
		return billingRolloutPendingPrepared
	}
	if pending.CancelPreviousTarget != nil {
		if pending.CancelPreviousTarget.Kind == preparedDowngradeFenceKind ||
			validatePendingCancellationTarget(*pending.CancelPreviousTarget) != nil {
			return billingRolloutPendingMalformed
		}
	}
	if pending.CancelPrevious && record.CustomerID == "" {
		return billingRolloutPendingMalformed
	}
	if pending.CancelPrevious &&
		(pending.URL != "" || pending.ProviderObjectID != "" ||
			!pending.PreparedEffective.IsZero() ||
			pending.ProviderPhase != "" || !pending.Effective.IsZero()) {
		return billingRolloutPendingMalformed
	}

	switch pending.Kind {
	case PendingContact:
		if pending.URL != "" || pending.ProviderObjectID != "" ||
			!pending.PreparedEffective.IsZero() ||
			pending.ProviderPhase != "" || !pending.Expires.IsZero() ||
			!pending.Effective.IsZero() {
			return billingRolloutPendingMalformed
		}
	case PendingUpgrade:
		if pending.ProviderPhase != "" ||
			!pending.PreparedEffective.IsZero() ||
			!pending.Effective.IsZero() || pending.Expires.IsZero() ||
			pending.Expires.Before(pending.Requested) {
			return billingRolloutPendingMalformed
		}
		if pending.URL != "" && !validBillingMutationURL(pending.URL) {
			return billingRolloutPendingMalformed
		}
		if pending.ProviderObjectID != "" && pending.URL == "" {
			return billingRolloutPendingMalformed
		}
		if (pending.URL != "" || pending.ProviderObjectID != "") &&
			record.CustomerID == "" {
			return billingRolloutPendingMalformed
		}
	case PendingDowngrade:
		if pending.URL != "" || !pending.Expires.IsZero() {
			return billingRolloutPendingMalformed
		}
		switch pending.ProviderPhase {
		case pendingProviderApplied:
			if record.CustomerID == "" || !providerEffectApplied(pending) ||
				pending.Effective.Before(pending.Requested) {
				return billingRolloutPendingMalformed
			}
			return billingRolloutPendingNone
		case "":
			if !pending.PreparedEffective.IsZero() {
				return billingRolloutPendingMalformed
			}
			if !pending.Effective.IsZero() {
				if pending.CancelPrevious || pending.CancelPreviousTarget != nil {
					return billingRolloutPendingMalformed
				}
				if record.CustomerID == "" {
					return billingRolloutPendingMalformed
				}
				if pending.ProviderObjectID == "" {
					return billingRolloutPendingTargetless
				}
				if record.CustomerID == "" || !providerEffectApplied(pending) {
					return billingRolloutPendingMalformed
				}
				return billingRolloutPendingNone
			}
			if pending.ProviderObjectID != "" {
				return billingRolloutPendingMalformed
			}
		default:
			return billingRolloutPendingMalformed
		}
	default:
		return billingRolloutPendingMalformed
	}

	if pending.CancelPrevious && pending.CancelPreviousTarget == nil {
		return billingRolloutPendingTargetless
	}
	// Once a provider customer exists, a hosted or scheduled operation with no
	// exact object can represent an ambiguous provider response. The unsafe
	// predecessor would discover and broadly cancel partner state, so this is a
	// targetless hazard even when the local request is otherwise coherent.
	if record.CustomerID != "" && pending.Kind != PendingContact &&
		pending.ProviderObjectID == "" && !pending.CancelPrevious {
		return billingRolloutPendingTargetless
	}
	return billingRolloutPendingNone
}

func validPreparedBillingRolloutDowngrade(
	record Record,
	pending *Pending,
) bool {
	if record.CustomerID == "" || !isPreparedDowngradeFence(pending) ||
		pending.PreparedEffective.IsZero() || !pending.Effective.IsZero() ||
		pending.PreparedEffective.Before(pending.Requested) ||
		pending.URL != "" || !pending.Expires.IsZero() ||
		billing.ValidateProviderObjectID(pending.ProviderObjectID) != nil ||
		billing.ValidateOperationID(pending.OperationID) != nil {
		return false
	}
	target := pending.CancelPreviousTarget
	return target != nil &&
		billing.ValidateProviderObjectID(target.ProviderObjectID) == nil &&
		billing.ValidateOperationID(target.OriginalOperationID) == nil
}

func decodeBillingRolloutReceipt(
	prefix, key string,
	data []byte,
) (BillingMutationReceipt, bool) {
	var receipt BillingMutationReceipt
	if err := decodeCanonicalBillingRolloutJSON(data, &receipt); err != nil ||
		validateBillingMutationReceipt(receipt) != nil || receipt.Version < 1 ||
		key != prefix+"billing-mutations/receipts/"+
			base64.RawURLEncoding.EncodeToString([]byte(receipt.OperationID))+".json" {
		return BillingMutationReceipt{}, false
	}
	return receipt, true
}

// decodeCanonicalBillingRolloutJSON rejects extensions, trailing values, and
// duplicate names before a stored object can become inventory evidence. R2
// writers use encoding/json over the current structs, so none of those forms
// is canonical output. The caller deliberately converts every returned error
// to a count rather than exposing parser input or detail.
func decodeCanonicalBillingRolloutJSON(data []byte, target any) error {
	if !utf8.Valid(data) {
		return errors.New("invalid UTF-8")
	}
	if err := rejectDuplicateBillingRolloutJSONNames(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func rejectDuplicateBillingRolloutJSONNames(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var walk func() error
	walk = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, isDelimiter := token.(json.Delim)
		if !isDelimiter {
			return nil
		}
		switch delimiter {
		case '{':
			seen := make(map[string]struct{})
			for decoder.More() {
				nameToken, err := decoder.Token()
				if err != nil {
					return err
				}
				name, ok := nameToken.(string)
				if !ok {
					return errors.New("invalid JSON object name")
				}
				if _, duplicate := seen[name]; duplicate {
					return errors.New("duplicate JSON object name")
				}
				seen[name] = struct{}{}
				if err := walk(); err != nil {
					return err
				}
			}
			closing, err := decoder.Token()
			if err != nil || closing != json.Delim('}') {
				return errors.New("invalid JSON object")
			}
		case '[':
			for decoder.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			closing, err := decoder.Token()
			if err != nil || closing != json.Delim(']') {
				return errors.New("invalid JSON array")
			}
		default:
			return errors.New("invalid JSON delimiter")
		}
		return nil
	}
	if err := walk(); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func billingRolloutHasTerminalAccountEvidence(
	receipt BillingMutationReceipt,
	accounts map[string]billingRolloutScannedAccount,
) bool {
	account, ok := accounts[receipt.AccountID]
	if !ok || !account.valid {
		return false
	}
	record := account.record
	switch receipt.Operation {
	case BillingMutationPlanUpgrade:
		if record.Entitled == receipt.TargetPlan {
			return true
		}
		pending := record.Pending
		return pending != nil && pending.OperationID == receipt.OperationID &&
			pending.Plan == receipt.TargetPlan &&
			(pending.Kind == PendingContact ||
				(pending.Kind == PendingUpgrade && pending.URL != "" &&
					pending.ProviderObjectID != ""))
	case BillingMutationPlanDowngrade:
		pending := record.Pending
		if pending != nil && pending.OperationID == receipt.OperationID &&
			pending.Kind == PendingDowngrade &&
			pending.Plan == receipt.TargetPlan && providerEffectApplied(pending) {
			return true
		}
		return record.LastBillingMutationOperationID == receipt.OperationID &&
			record.LastBillingMutationKind == BillingMutationPlanDowngrade &&
			record.LastBillingMutationResultKind == BillingMutationResultScheduled &&
			record.LastBillingMutationPlan == receipt.TargetPlan &&
			!record.LastBillingMutationEffective.IsZero()
	case BillingMutationPlanCancel:
		return record.LastBillingMutationOperationID == receipt.OperationID &&
			record.LastBillingMutationKind == BillingMutationPlanCancel &&
			(record.LastBillingMutationResultKind == "" ||
				record.LastBillingMutationResultKind == BillingMutationResultCancelled ||
				record.LastBillingMutationResultKind == BillingMutationResultResolved)
	default:
		return false
	}
}
