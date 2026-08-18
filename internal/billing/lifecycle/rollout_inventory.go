package lifecycle

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/witwave-ai/witself/internal/billing"
	"github.com/witwave-ai/witself/internal/blob"
)

const (
	billingRolloutInventoryMaxAccountObjects = 1_000_000
	billingRolloutInventoryMaxReceiptObjects = 1_000_000
	billingRolloutInventoryMaxObjectBytes    = 16 << 20
	billingRolloutInventoryMaxObjectKeyBytes = 1_024
	billingRolloutInventoryMaxETagBytes      = 256
	billingRolloutInventoryMaxPrefixBytes    = billingRolloutInventoryMaxObjectKeyBytes -
		len("accounts/") - 255 - len(".json")
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

// BillingRolloutRegistryOptions binds one private R2 scan to the independently
// captured source observation that preceded it and to the reviewed non-secret
// registry authority. The lifecycle package does not interpret either digest;
// the rollout finalizer verifies them against the strict source attestations
// and the endpoint/bucket/prefix authority before it emits any public artifact.
type BillingRolloutRegistryOptions struct {
	R2Prefix                     string
	BeforeSourceInspectionSHA256 string
	RegistryAuthoritySHA256      string
	Now                          func() time.Time
}

// BillingRolloutRegistryCapture is a private in-memory projection. It is not
// the public rollout inventory and intentionally has no JSON representation;
// rollout orchestration owns the provisional schema, temporal bracketing, and
// final public v1 artifact.
type BillingRolloutRegistryCapture struct {
	ScanStartedAt                 time.Time                     `json:"-"`
	ScanCompletedAt               time.Time                     `json:"-"`
	BeforeSourceInspectionSHA256  string                        `json:"-"`
	RegistryAuthoritySHA256       string                        `json:"-"`
	AccountObjectsScanned         int                           `json:"-"`
	MutationReceiptObjectsScanned int                           `json:"-"`
	Records                       BillingRolloutRegistryRecords `json:"-"`
}

// BillingRolloutRegistryRecords contains independent object-class counts.
// Pending-state and receipt counts can describe the same operation and must
// not be summed into a unique-operation total.
type BillingRolloutRegistryRecords struct {
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

type billingRolloutAccountEvidence struct {
	accountID string
	entitled  string

	pendingOperationID        string
	pendingPlan               string
	pendingKind               PendingKind
	pendingUpgradeExactAction bool
	pendingDowngradeApplied   bool

	tombstoneOperationID string
	tombstoneKind        BillingMutationOperation
	tombstoneResultKind  BillingMutationResultKind
	tombstonePlan        string
	tombstoneEffective   bool
}

// CollectBillingRolloutRegistry takes one complete, stable, read-only view of
// the canonical account and billing-mutation receipt namespaces. It returns a
// private capture only. A separate finalizer must bracket this exact capture
// between independent source observations before public rollout evidence can
// exist. Any listing, bounded-read, metadata, cancellation, clock, or second-
// list failure drops the partial capture. Malformed stored JSON is counted.
func CollectBillingRolloutRegistry(
	ctx context.Context,
	reader BillingRolloutInventoryReader,
	options BillingRolloutRegistryOptions,
) (*BillingRolloutRegistryCapture, error) {
	prefix, err := validateBillingRolloutRegistryOptions(reader, options)
	if err != nil {
		return nil, err
	}
	scanStartedAt, ok := billingRolloutRegistryNow(options.Now)
	if !ok {
		return nil, ErrBillingRolloutInventoryInput
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

	recordCounts := BillingRolloutRegistryRecords{}
	accounts := make(map[string]billingRolloutAccountEvidence, len(accountObjects))
	for _, object := range accountObjects {
		data, readErr := readBillingRolloutObject(ctx, reader, object)
		if readErr != nil {
			return nil, readErr
		}
		evidence, class, valid := classifyBillingRolloutAccount(
			prefix, object.Key, data, scanStartedAt)
		switch class {
		case billingRolloutPendingMalformed:
			recordCounts.MalformedPendingChanges++
		case billingRolloutPendingTargetless:
			recordCounts.TargetlessPendingChanges++
		case billingRolloutPendingPrepared:
			recordCounts.PreparedDowngrades++
		}
		if valid {
			accounts[evidence.accountID] = evidence
		}
	}

	pendingWithoutEvidence := make([]time.Time, 0)
	for _, object := range receiptObjects {
		data, readErr := readBillingRolloutObject(ctx, reader, object)
		if readErr != nil {
			return nil, readErr
		}
		receipt, valid := decodeBillingRolloutReceipt(
			prefix, object.Key, data, scanStartedAt)
		if !valid {
			recordCounts.MalformedMutationReceipts++
			continue
		}
		if receipt.Status != BillingMutationPending {
			continue
		}
		hasEvidence := billingRolloutHasTerminalAccountEvidence(receipt, accounts)
		if receipt.SchemaVersion == billingMutationLegacyReceiptSchemaVersion &&
			!hasEvidence {
			// Schema-v1 did not pin an execution class. Current recovery refuses
			// to send it to a provider at any age unless exact account evidence
			// can complete it without a second provider effect.
			recordCounts.MalformedMutationReceipts++
			continue
		}
		if !hasEvidence {
			pendingWithoutEvidence = append(
				pendingWithoutEvidence, receipt.CreatedAt)
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
	scanCompletedAt, ok := billingRolloutRegistryNow(options.Now)
	if !ok || scanCompletedAt.Before(scanStartedAt) {
		return nil, billingRolloutInventoryIncomplete(
			"capture clock was invalid or moved backwards")
	}
	for _, createdAt := range pendingWithoutEvidence {
		if !scanCompletedAt.Before(
			createdAt.Add(billingMutationAutomaticRetryHorizon)) {
			recordCounts.PostRetryHorizonReceipts++
		}
	}

	return &BillingRolloutRegistryCapture{
		ScanStartedAt:                 scanStartedAt,
		ScanCompletedAt:               scanCompletedAt,
		BeforeSourceInspectionSHA256:  options.BeforeSourceInspectionSHA256,
		RegistryAuthoritySHA256:       options.RegistryAuthoritySHA256,
		AccountObjectsScanned:         len(accountObjects),
		MutationReceiptObjectsScanned: len(receiptObjects),
		Records:                       recordCounts,
	}, nil
}

func validateBillingRolloutRegistryOptions(
	reader BillingRolloutInventoryReader,
	options BillingRolloutRegistryOptions,
) (prefix string, err error) {
	if reader == nil || options.Now == nil ||
		!validLowerSHA256(options.BeforeSourceInspectionSHA256) ||
		!validLowerSHA256(options.RegistryAuthoritySHA256) {
		return "", ErrBillingRolloutInventoryInput
	}
	prefix = options.R2Prefix
	if strings.TrimSpace(prefix) != prefix || !utf8.ValidString(prefix) ||
		strings.IndexFunc(prefix, unicode.IsControl) >= 0 {
		return "", ErrBillingRolloutInventoryInput
	}
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	if len(prefix) > billingRolloutInventoryMaxPrefixBytes {
		return "", ErrBillingRolloutInventoryInput
	}
	return prefix, nil
}

func billingRolloutRegistryNow(now func() time.Time) (time.Time, bool) {
	value := now()
	if value.IsZero() {
		return time.Time{}, false
	}
	value = value.UTC().Truncate(time.Second)
	if value.Year() < 1 || value.Year() > 9999 {
		return time.Time{}, false
	}
	return value, true
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
			len(object.Key) > billingRolloutInventoryMaxObjectKeyBytes ||
			!validBillingRolloutNormalizedETag(object.ETag) ||
			object.Size < 0 ||
			object.Size > billingRolloutInventoryMaxObjectBytes ||
			(i > 0 && object.Key <= previous) {
			return false
		}
		previous = object.Key
	}
	return true
}

func validBillingRolloutNormalizedETag(value string) bool {
	if value == "" || len(value) > billingRolloutInventoryMaxETagBytes {
		return false
	}
	for index := range len(value) {
		if value[index] <= 0x20 || value[index] == '"' || value[index] == 0x7f {
			return false
		}
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
	causalityFence time.Time,
) (billingRolloutAccountEvidence, billingRolloutPendingClass, bool) {
	var strictRecord Record
	if decodeCanonicalBillingRolloutJSON(data, &strictRecord) != nil {
		return billingRolloutAccountEvidence{}, billingRolloutPendingMalformed, false
	}
	record, err := unmarshalR2Record(data)
	if err != nil ||
		!validBillingMutationText(record.AccountID, 255) ||
		key != prefix+"accounts/"+record.AccountID+".json" ||
		record.Version < 1 || !validBillingRolloutProviderIdentity(record) ||
		validateBillingRolloutTombstone(record) != nil ||
		billingRolloutRecordCausalityAfter(record, causalityFence) {
		return billingRolloutAccountEvidence{}, billingRolloutPendingMalformed, false
	}
	class := classifyBillingRolloutPending(record)
	if class == billingRolloutPendingMalformed {
		return billingRolloutAccountEvidence{}, class, false
	}
	return projectBillingRolloutAccountEvidence(record), class, true
}

func billingRolloutRecordCausalityAfter(record Record, fence time.Time) bool {
	if !record.EntitledAt.IsZero() && record.EntitledAt.After(fence) ||
		!record.DunningAt.IsZero() && record.DunningAt.After(fence) ||
		!record.LastBillingMutationAt.IsZero() &&
			record.LastBillingMutationAt.After(fence) {
		return true
	}
	return record.Pending != nil && record.Pending.Requested.After(fence)
}

func projectBillingRolloutAccountEvidence(
	record Record,
) billingRolloutAccountEvidence {
	evidence := billingRolloutAccountEvidence{
		accountID:            record.AccountID,
		entitled:             record.Entitled,
		tombstoneOperationID: record.LastBillingMutationOperationID,
		tombstoneKind:        record.LastBillingMutationKind,
		tombstoneResultKind:  record.LastBillingMutationResultKind,
		tombstonePlan:        record.LastBillingMutationPlan,
		tombstoneEffective:   !record.LastBillingMutationEffective.IsZero(),
	}
	if pending := record.Pending; pending != nil {
		evidence.pendingOperationID = pending.OperationID
		evidence.pendingPlan = pending.Plan
		evidence.pendingKind = pending.Kind
		evidence.pendingUpgradeExactAction = pending.Kind == PendingUpgrade &&
			pending.URL != "" && pending.ProviderObjectID != ""
		evidence.pendingDowngradeApplied = pending.Kind == PendingDowngrade &&
			providerEffectApplied(pending)
	}
	return evidence
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
	causalityFence time.Time,
) (BillingMutationReceipt, bool) {
	var receipt BillingMutationReceipt
	if err := decodeCanonicalBillingRolloutJSON(data, &receipt); err != nil ||
		validateBillingMutationReceipt(receipt) != nil || receipt.Version < 1 ||
		billingRolloutReceiptCausalityAfter(receipt, causalityFence) ||
		key != prefix+"billing-mutations/receipts/"+
			base64.RawURLEncoding.EncodeToString([]byte(receipt.OperationID))+".json" {
		return BillingMutationReceipt{}, false
	}
	return receipt, true
}

func billingRolloutReceiptCausalityAfter(
	receipt BillingMutationReceipt,
	fence time.Time,
) bool {
	if receipt.CreatedAt.After(fence) || receipt.UpdatedAt.After(fence) ||
		receipt.ConfirmedAt.After(fence) ||
		receipt.ConfirmedAt.After(receipt.CreatedAt) {
		return true
	}
	return receipt.CompletedAt != nil && receipt.CompletedAt.After(fence)
}

// decodeCanonicalBillingRolloutJSON rejects extensions, trailing values, and
// duplicate or non-exact field names before a stored object can become
// inventory evidence. encoding/json normally matches struct fields without
// case sensitivity; the exact shape walk closes that ambiguity so AccountID
// and accountid cannot both target one semantic field. R2 writers use
// encoding/json over the current structs, so only their emitted spelling is
// accepted. The caller converts every error to a count and never exposes input.
func decodeCanonicalBillingRolloutJSON(data []byte, target any) error {
	if !utf8.Valid(data) {
		return errors.New("invalid UTF-8")
	}
	if err := validateExactBillingRolloutJSONShape(
		data, reflect.TypeOf(target)); err != nil {
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

var billingRolloutJSONUnmarshalerType = reflect.TypeOf(
	(*json.Unmarshaler)(nil)).Elem()

func validateExactBillingRolloutJSONShape(
	data []byte,
	targetType reflect.Type,
) error {
	if targetType == nil || targetType.Kind() != reflect.Pointer {
		return errors.New("JSON destination must be a pointer")
	}
	// The outer pointer is the decoder destination, not a nullable part of the
	// stored schema. Nested pointer fields still accept their canonical null.
	targetType = targetType.Elem()
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := walkExactBillingRolloutJSON(decoder, targetType); err != nil {
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

func walkExactBillingRolloutJSON(
	decoder *json.Decoder,
	targetType reflect.Type,
) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	return walkExactBillingRolloutJSONToken(decoder, token, targetType)
}

func walkExactBillingRolloutJSONToken(
	decoder *json.Decoder,
	token json.Token,
	targetType reflect.Type,
) error {
	for targetType != nil && targetType.Kind() == reflect.Pointer {
		if token == nil {
			return nil
		}
		targetType = targetType.Elem()
	}
	if targetType == nil {
		return walkAnyBillingRolloutJSONToken(decoder, token)
	}
	if token == nil {
		switch targetType.Kind() {
		case reflect.Map, reflect.Slice, reflect.Interface:
			// encoding/json emits null for nil maps and slices. They remain
			// canonical values for those fields and contain no names to walk.
			return nil
		}
	}
	if billingRolloutJSONAtomic(targetType) {
		if _, composite := token.(json.Delim); composite {
			return errors.New("atomic JSON value was composite")
		}
		return nil
	}

	delimiter, isDelimiter := token.(json.Delim)
	switch targetType.Kind() {
	case reflect.Struct:
		if !isDelimiter || delimiter != '{' {
			return errors.New("JSON struct was not an object")
		}
		fields, ok := exactBillingRolloutJSONFields(targetType)
		if !ok {
			return errors.New("ambiguous JSON struct field set")
		}
		seen := make(map[string]struct{}, len(fields))
		for decoder.More() {
			nameToken, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := nameToken.(string)
			if !ok {
				return errors.New("invalid JSON object name")
			}
			fieldType, known := fields[name]
			if !known {
				return errors.New("non-exact JSON field name")
			}
			if _, duplicate := seen[name]; duplicate {
				return errors.New("duplicate JSON object name")
			}
			seen[name] = struct{}{}
			if err := walkExactBillingRolloutJSON(decoder, fieldType); err != nil {
				return err
			}
		}
		return closeBillingRolloutJSON(decoder, '}')
	case reflect.Map:
		if !isDelimiter || delimiter != '{' {
			return errors.New("JSON map was not an object")
		}
		seen := make(map[string]struct{})
		for decoder.More() {
			nameToken, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := nameToken.(string)
			if !ok {
				return errors.New("invalid JSON map name")
			}
			if _, duplicate := seen[name]; duplicate {
				return errors.New("duplicate JSON map name")
			}
			seen[name] = struct{}{}
			if err := walkExactBillingRolloutJSON(
				decoder, targetType.Elem()); err != nil {
				return err
			}
		}
		return closeBillingRolloutJSON(decoder, '}')
	case reflect.Slice, reflect.Array:
		if targetType == reflect.TypeOf(json.RawMessage{}) {
			return walkAnyBillingRolloutJSONToken(decoder, token)
		}
		if !isDelimiter || delimiter != '[' {
			return errors.New("JSON slice was not an array")
		}
		for decoder.More() {
			if err := walkExactBillingRolloutJSON(
				decoder, targetType.Elem()); err != nil {
				return err
			}
		}
		return closeBillingRolloutJSON(decoder, ']')
	case reflect.Interface:
		return walkAnyBillingRolloutJSONToken(decoder, token)
	default:
		if isDelimiter {
			return errors.New("scalar JSON value was composite")
		}
		return nil
	}
}

func walkAnyBillingRolloutJSONToken(
	decoder *json.Decoder,
	token json.Token,
) error {
	delimiter, composite := token.(json.Delim)
	if !composite {
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
			if err := walkExactBillingRolloutJSON(decoder, nil); err != nil {
				return err
			}
		}
		return closeBillingRolloutJSON(decoder, '}')
	case '[':
		for decoder.More() {
			if err := walkExactBillingRolloutJSON(decoder, nil); err != nil {
				return err
			}
		}
		return closeBillingRolloutJSON(decoder, ']')
	default:
		return errors.New("invalid JSON delimiter")
	}
}

func closeBillingRolloutJSON(decoder *json.Decoder, want json.Delim) error {
	closing, err := decoder.Token()
	if err != nil || closing != want {
		return errors.New("invalid JSON composite close")
	}
	return nil
}

func billingRolloutJSONAtomic(targetType reflect.Type) bool {
	return targetType.Implements(billingRolloutJSONUnmarshalerType) ||
		reflect.PointerTo(targetType).Implements(
			billingRolloutJSONUnmarshalerType)
}

func exactBillingRolloutJSONFields(
	targetType reflect.Type,
) (map[string]reflect.Type, bool) {
	fields := make(map[string]reflect.Type)
	for index := 0; index < targetType.NumField(); index++ {
		field := targetType.Field(index)
		if field.PkgPath != "" {
			continue
		}
		tag := field.Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		if name == "-" {
			continue
		}
		if field.Anonymous && name == "" {
			embedded := field.Type
			for embedded.Kind() == reflect.Pointer {
				embedded = embedded.Elem()
			}
			if embedded.Kind() == reflect.Struct {
				nested, ok := exactBillingRolloutJSONFields(embedded)
				if !ok {
					return nil, false
				}
				for nestedName, nestedType := range nested {
					if _, exists := fields[nestedName]; exists {
						return nil, false
					}
					fields[nestedName] = nestedType
				}
				continue
			}
		}
		if name == "" {
			name = field.Name
		}
		if _, exists := fields[name]; exists {
			return nil, false
		}
		fields[name] = field.Type
	}
	return fields, true
}

func billingRolloutHasTerminalAccountEvidence(
	receipt BillingMutationReceipt,
	accounts map[string]billingRolloutAccountEvidence,
) bool {
	record, ok := accounts[receipt.AccountID]
	if !ok {
		return false
	}
	switch receipt.Operation {
	case BillingMutationPlanUpgrade:
		if record.entitled == receipt.TargetPlan {
			return true
		}
		return record.pendingOperationID == receipt.OperationID &&
			record.pendingPlan == receipt.TargetPlan &&
			(record.pendingKind == PendingContact ||
				record.pendingUpgradeExactAction)
	case BillingMutationPlanDowngrade:
		if record.pendingOperationID == receipt.OperationID &&
			record.pendingKind == PendingDowngrade &&
			record.pendingPlan == receipt.TargetPlan &&
			record.pendingDowngradeApplied {
			return true
		}
		return record.tombstoneOperationID == receipt.OperationID &&
			record.tombstoneKind == BillingMutationPlanDowngrade &&
			record.tombstoneResultKind == BillingMutationResultScheduled &&
			record.tombstonePlan == receipt.TargetPlan &&
			record.tombstoneEffective
	case BillingMutationPlanCancel:
		return record.tombstoneOperationID == receipt.OperationID &&
			record.tombstoneKind == BillingMutationPlanCancel &&
			(record.tombstoneResultKind == "" ||
				record.tombstoneResultKind == BillingMutationResultCancelled ||
				record.tombstoneResultKind == BillingMutationResultResolved)
	default:
		return false
	}
}
