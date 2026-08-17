package lifecycle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	billingMutationLegacyReceiptSchemaVersion = 1
	billingMutationReceiptSchemaVersion       = 2
	billingAccountMutationLeaseSchemaVersion  = 1
	billingMutationPendingIndexSchemaVersion  = 1

	// The recovery queue is split across a fixed number of small global shards.
	// A cron pass reads every shard, while CAS cursor rotation lets multiple
	// replicas share work without an account-directory scan. At the maximum
	// depth, a 64-item pass revisits every item within 64 five-minute ticks.
	billingMutationPendingShardCount     = 16
	maxPendingBillingMutationsPerShard   = 256
	maxBillingMutationsPerReconcile      = 64
	billingMutationAutomaticRetryHorizon = 23 * time.Hour
	billingMutationReconcileConcurrency  = 8
)

// BillingMutationOperation is one allowlisted customer billing mutation.
type BillingMutationOperation string

const (
	// BillingMutationSetup creates or replays a hosted payment setup flow.
	BillingMutationSetup BillingMutationOperation = "billing_setup"
	// BillingMutationPlanUpgrade purchases or requests a higher plan.
	BillingMutationPlanUpgrade BillingMutationOperation = "plan_upgrade"
	// BillingMutationPlanDowngrade schedules a lower plan.
	BillingMutationPlanDowngrade BillingMutationOperation = "plan_downgrade"
	// BillingMutationPlanCancel cancels one pending plan change.
	BillingMutationPlanCancel BillingMutationOperation = "plan_cancel"
	// BillingMutationUpgrade is the concise plan-upgrade alias.
	BillingMutationUpgrade = BillingMutationPlanUpgrade
	// BillingMutationDowngrade is the concise plan-downgrade alias.
	BillingMutationDowngrade = BillingMutationPlanDowngrade
	// BillingMutationCancel is the concise pending-cancellation alias.
	BillingMutationCancel = BillingMutationPlanCancel
)

// BillingMutationExecutionClass is the immutable effect approved before a
// receipt becomes visible. Recovery executes this class exactly; it never
// reinterprets a changed catalog into a different customer or provider effect.
type BillingMutationExecutionClass string

const (
	// BillingMutationExecutionSetup creates or replays hosted payment setup.
	BillingMutationExecutionSetup BillingMutationExecutionClass = "setup"
	// BillingMutationExecutionUpgradeContact records a contact-only upgrade.
	BillingMutationExecutionUpgradeContact BillingMutationExecutionClass = "upgrade_contact"
	// BillingMutationExecutionUpgradeSelfServe purchases a priced upgrade.
	BillingMutationExecutionUpgradeSelfServe BillingMutationExecutionClass = "upgrade_self_serve"
	// BillingMutationExecutionDowngrade schedules a priced downgrade.
	BillingMutationExecutionDowngrade BillingMutationExecutionClass = "downgrade"
	// BillingMutationExecutionCancel cancels the exact pending billing change.
	BillingMutationExecutionCancel BillingMutationExecutionClass = "cancel"
)

// BillingMutationStatus is the durable state of an outbound billing mutation.
type BillingMutationStatus string

const (
	// BillingMutationPending is an operation that has not reached a terminal receipt.
	BillingMutationPending BillingMutationStatus = "pending"
	// BillingMutationCompleted is an operation with an immutable result.
	BillingMutationCompleted BillingMutationStatus = "completed"
	// BillingMutationSuperseded is an operation retired by a newer account generation.
	BillingMutationSuperseded BillingMutationStatus = "superseded"
)

// BillingMutationResultKind classifies the value-minimal terminal result.
type BillingMutationResultKind string

const (
	// BillingMutationResultDone means no hosted follow-up is required.
	BillingMutationResultDone BillingMutationResultKind = "done"
	// BillingMutationResultAction returns a provider-hosted follow-up URL.
	BillingMutationResultAction BillingMutationResultKind = "action"
	// BillingMutationResultScheduled records a future effective plan change.
	BillingMutationResultScheduled BillingMutationResultKind = "scheduled"
	// BillingMutationResultCancelled confirms the targeted pending state was cancelled.
	BillingMutationResultCancelled BillingMutationResultKind = "cancelled"
	// BillingMutationResultResolved means the targeted state disappeared or changed first.
	BillingMutationResultResolved BillingMutationResultKind = "resolved"
	// BillingMutationResultContact records a non-self-serve plan request.
	BillingMutationResultContact BillingMutationResultKind = "contact"
)

var (
	// ErrBillingMutationConflict means one operation identity was reused with
	// different immutable request semantics or a different terminal result.
	ErrBillingMutationConflict = errors.New("billing mutation receipt conflict")
	// ErrBillingMutationClaimLost means a stale worker tried to complete or
	// release a receipt after another claim generation took ownership.
	ErrBillingMutationClaimLost = errors.New("billing mutation receipt claim was lost")
	// ErrBillingMutationSuperseded means a newer account-scoped operation
	// generation owns the billing lane or the receipt was terminally retired.
	ErrBillingMutationSuperseded = errors.New("billing mutation was superseded")
	// ErrBillingMutationClaimActive means a receipt cannot be superseded while
	// a worker still owns its unexpired processing lease.
	ErrBillingMutationClaimActive = errors.New("billing mutation receipt claim is still active")
	// errBillingMutationPendingIndexFull is internal backpressure. A receipt
	// and its account lane are already durable before R2 updates the recovery
	// accelerator, so saturation must never make synchronous provider work
	// unsafe or unrepeatable.
	errBillingMutationPendingIndexFull = errors.New(
		"billing mutation pending index shard is full")
	// ErrBillingMutationRetryHorizonExceeded leaves an old receipt pending for
	// operator reconciliation instead of risking a second provider effect after
	// a partner's idempotency retention window may have expired.
	ErrBillingMutationRetryHorizonExceeded = errors.New(
		"billing mutation automatic retry horizon exceeded")
)

// BillingMutationResult is the allowlisted terminal projection. It contains
// no provider error text, payment data, email, or raw idempotency material.
type BillingMutationResult struct {
	Kind             BillingMutationResultKind `json:"kind"`
	Plan             string                    `json:"plan,omitempty"`
	URL              string                    `json:"url,omitempty"`
	ProviderObjectID string                    `json:"provider_object_id,omitempty"`
	ActionExpiresAt  *time.Time                `json:"action_expires_at,omitempty"`
	Effective        *time.Time                `json:"effective,omitempty"`
	Cancelled        bool                      `json:"cancelled,omitempty"`
}

// BillingMutationReceipt durably fences one outbound provider mutation. Raw
// idempotency keys and email addresses are represented only by lowercase
// SHA-256 digests. Result is immutable once the receipt becomes completed.
type BillingMutationReceipt struct {
	SchemaVersion           int                           `json:"schema_version"`
	OperationID             string                        `json:"operation_id"`
	AccountID               string                        `json:"account_id"`
	ActorID                 string                        `json:"actor_id"`
	ActorRole               string                        `json:"actor_role"`
	Operation               BillingMutationOperation      `json:"operation"`
	ExecutionClass          BillingMutationExecutionClass `json:"execution_class"`
	AccountGeneration       int64                         `json:"account_generation"`
	IdempotencyKeySHA256    string                        `json:"idempotency_key_sha256"`
	RequestSHA256           string                        `json:"request_sha256"`
	Reason                  string                        `json:"reason"`
	ConfirmedAt             time.Time                     `json:"confirmed_at"`
	TargetPlan              string                        `json:"target_plan,omitempty"`
	ApprovedPriceCents      int64                         `json:"approved_price_cents"`
	ApprovedCurrency        string                        `json:"approved_currency,omitempty"`
	EmailSHA256             string                        `json:"email_sha256,omitempty"`
	Status                  BillingMutationStatus         `json:"status"`
	Result                  *BillingMutationResult        `json:"result,omitempty"`
	ClaimToken              string                        `json:"claim_token,omitempty"`
	ClaimGeneration         int64                         `json:"claim_generation,omitempty"`
	LeaseExpiresAt          *time.Time                    `json:"lease_expires_at,omitempty"`
	CreatedAt               time.Time                     `json:"created_at"`
	UpdatedAt               time.Time                     `json:"updated_at"`
	CompletedAt             *time.Time                    `json:"completed_at,omitempty"`
	SupersededByOperationID string                        `json:"superseded_by_operation_id,omitempty"`
	Version                 int64                         `json:"version"`
}

// BillingMutationPendingBatch is a value-free bounded view selected from the
// global recovery shards. Scanned includes malformed and terminal references;
// valid pending receipts are returned for claims. Poison references stay
// indexed and surface through the accompanying error.
type BillingMutationPendingBatch struct {
	Receipts                []BillingMutationReceipt
	Scanned                 int
	TerminalCleanup         int
	ScanCapped              bool
	OldestObservedPendingAt *time.Time
}

type billingMutationPendingReference struct {
	operationID string
	shardID     int
}

// BillingAccountMutationLease serializes provider mutations for one account.
// OperationGeneration advances only when a new operation enters an idle lane;
// ClaimGeneration advances on every worker takeover and fences stale release.
type BillingAccountMutationLease struct {
	SchemaVersion       int        `json:"schema_version"`
	AccountID           string     `json:"account_id"`
	OperationID         string     `json:"operation_id"`
	OperationGeneration int64      `json:"operation_generation"`
	ClaimToken          string     `json:"claim_token,omitempty"`
	ClaimGeneration     int64      `json:"claim_generation"`
	LeaseExpiresAt      *time.Time `json:"lease_expires_at,omitempty"`
	UpdatedAt           time.Time  `json:"updated_at"`
	Version             int64      `json:"version"`
}

// BillingMutationStore is the durable create/replay, exclusive-fold, and
// bounded recovery seam implemented by both lifecycle stores. Pending lookup
// rotates through a fixed global shard set; receipt and account claims remain
// authoritative while the index is only a value-minimal discovery structure.
type BillingMutationStore interface {
	ClaimBillingMutationAccount(
		ctx context.Context,
		accountID, operationID string,
		expectedOperationGeneration int64,
		claimToken string,
		now, leaseExpiresAt time.Time,
	) (lease BillingAccountMutationLease, acquired bool, err error)
	ReleaseBillingMutationAccount(
		ctx context.Context,
		lease BillingAccountMutationLease,
		releasedAt time.Time,
	) error
	GetBillingMutation(
		ctx context.Context,
		operationID string,
	) (BillingMutationReceipt, bool, error)
	ReceiveBillingMutation(
		ctx context.Context,
		receipt BillingMutationReceipt,
	) (stored BillingMutationReceipt, created bool, err error)
	ClaimBillingMutation(
		ctx context.Context,
		receipt BillingMutationReceipt,
		claimToken string,
		now, leaseExpiresAt time.Time,
	) (claimed BillingMutationReceipt, acquired bool, err error)
	CompleteBillingMutation(
		ctx context.Context,
		receipt BillingMutationReceipt,
		result BillingMutationResult,
		completedAt time.Time,
	) (BillingMutationReceipt, error)
	SupersedeBillingMutation(
		ctx context.Context,
		receipt BillingMutationReceipt,
		supersededAt time.Time,
	) (BillingMutationReceipt, error)
	ReleaseBillingMutation(
		ctx context.Context,
		receipt BillingMutationReceipt,
		releasedAt time.Time,
	) error
	PendingBillingMutations(
		ctx context.Context,
		limit int,
	) (BillingMutationPendingBatch, error)
}

func validateBillingMutationReceipt(receipt BillingMutationReceipt) error {
	switch {
	case receipt.SchemaVersion != billingMutationLegacyReceiptSchemaVersion &&
		receipt.SchemaVersion != billingMutationReceiptSchemaVersion:
		return fmt.Errorf("billing mutation receipt: unsupported schema version %d", receipt.SchemaVersion)
	case !validBillingMutationOperationID(receipt.OperationID):
		return errors.New("billing mutation receipt: invalid operation id")
	case !validBillingMutationText(receipt.AccountID, 255):
		return errors.New("billing mutation receipt: invalid account id")
	case !validBillingMutationText(receipt.ActorID, 255):
		return errors.New("billing mutation receipt: invalid actor id")
	case !validBillingMutationToken(receipt.ActorRole, 64):
		return errors.New("billing mutation receipt: invalid actor role")
	case !validBillingMutationOperation(receipt.Operation):
		return fmt.Errorf("billing mutation receipt: unsupported operation %q", receipt.Operation)
	case receipt.SchemaVersion == billingMutationReceiptSchemaVersion &&
		!validBillingMutationExecutionClass(receipt.ExecutionClass):
		return fmt.Errorf("billing mutation receipt: unsupported execution class %q", receipt.ExecutionClass)
	case receipt.SchemaVersion == billingMutationLegacyReceiptSchemaVersion &&
		(receipt.ExecutionClass != "" || receipt.ApprovedPriceCents != 0 ||
			receipt.ApprovedCurrency != ""):
		return errors.New("billing mutation receipt: legacy receipt cannot carry approval fields")
	case receipt.AccountGeneration < 1:
		return errors.New("billing mutation receipt: account_generation must be positive")
	case !validLowerSHA256(receipt.IdempotencyKeySHA256):
		return errors.New("billing mutation receipt: invalid idempotency key digest")
	case !validLowerSHA256(receipt.RequestSHA256):
		return errors.New("billing mutation receipt: invalid request digest")
	case !validBillingMutationReason(receipt.Reason):
		return errors.New("billing mutation receipt: invalid reason")
	case receipt.ConfirmedAt.IsZero():
		return errors.New("billing mutation receipt: confirmed_at is required")
	case receipt.TargetPlan != "" && !validBillingMutationToken(receipt.TargetPlan, 128):
		return errors.New("billing mutation receipt: invalid target plan")
	case receipt.ApprovedPriceCents < 0:
		return errors.New("billing mutation receipt: approved price cannot be negative")
	case receipt.ApprovedCurrency != "" && !validBillingMutationToken(receipt.ApprovedCurrency, 16):
		return errors.New("billing mutation receipt: invalid approved currency")
	case receipt.EmailSHA256 != "" && !validLowerSHA256(receipt.EmailSHA256):
		return errors.New("billing mutation receipt: invalid email digest")
	case receipt.CreatedAt.IsZero() || receipt.UpdatedAt.IsZero():
		return errors.New("billing mutation receipt: created_at and updated_at are required")
	case receipt.UpdatedAt.Before(receipt.CreatedAt):
		return errors.New("billing mutation receipt: updated_at predates created_at")
	case receipt.Version < 0 || receipt.ClaimGeneration < 0:
		return errors.New("billing mutation receipt: versions cannot be negative")
	case (receipt.ClaimToken == "") != (receipt.LeaseExpiresAt == nil):
		return errors.New("billing mutation receipt: claim requires both token and lease expiry")
	}
	if (receipt.Operation == BillingMutationPlanUpgrade ||
		receipt.Operation == BillingMutationPlanDowngrade) &&
		receipt.TargetPlan == "" {
		return errors.New("billing mutation receipt: plan operation requires target_plan")
	}
	if receipt.Operation == BillingMutationSetup && receipt.TargetPlan != "" {
		return errors.New("billing mutation receipt: setup cannot carry target_plan")
	}
	if receipt.SchemaVersion == billingMutationReceiptSchemaVersion {
		if err := validateBillingMutationApproval(receipt); err != nil {
			return err
		}
	}
	if receipt.ClaimToken != "" {
		if !validBillingMutationClaimToken(receipt.ClaimToken) ||
			receipt.ClaimGeneration < 1 || receipt.LeaseExpiresAt.IsZero() ||
			!receipt.LeaseExpiresAt.After(receipt.UpdatedAt) {
			return errors.New("billing mutation receipt: invalid processing claim")
		}
	}
	switch receipt.Status {
	case BillingMutationPending:
		if receipt.Result != nil || receipt.CompletedAt != nil ||
			receipt.SupersededByOperationID != "" {
			return errors.New("billing mutation receipt: pending receipt cannot have a result or completed_at")
		}
	case BillingMutationCompleted:
		if receipt.Result == nil || receipt.CompletedAt == nil || receipt.CompletedAt.IsZero() {
			return errors.New("billing mutation receipt: completed receipt requires result and completed_at")
		}
		if receipt.ClaimToken != "" || receipt.LeaseExpiresAt != nil {
			return errors.New("billing mutation receipt: completed receipt cannot retain a claim")
		}
		if receipt.SupersededByOperationID != "" {
			return errors.New("billing mutation receipt: completed receipt cannot be superseded")
		}
		if receipt.CompletedAt.Before(receipt.CreatedAt) || receipt.UpdatedAt.Before(*receipt.CompletedAt) {
			return errors.New("billing mutation receipt: invalid completion timestamps")
		}
		if err := validateBillingMutationResultForOperation(
			receipt.Operation, receipt.ExecutionClass,
			receipt.TargetPlan, *receipt.Result); err != nil {
			return err
		}
	case BillingMutationSuperseded:
		if receipt.Result != nil || receipt.CompletedAt == nil || receipt.CompletedAt.IsZero() {
			return errors.New("billing mutation receipt: superseded receipt requires completed_at and no result")
		}
		if !validBillingMutationOperationID(receipt.SupersededByOperationID) ||
			receipt.SupersededByOperationID == receipt.OperationID {
			return errors.New("billing mutation receipt: invalid superseding operation id")
		}
		if receipt.ClaimToken != "" || receipt.LeaseExpiresAt != nil {
			return errors.New("billing mutation receipt: superseded receipt cannot retain a claim")
		}
		if receipt.CompletedAt.Before(receipt.CreatedAt) || receipt.UpdatedAt.Before(*receipt.CompletedAt) {
			return errors.New("billing mutation receipt: invalid superseded timestamps")
		}
	default:
		return fmt.Errorf("billing mutation receipt: unsupported status %q", receipt.Status)
	}
	return nil
}

func validateBillingMutationResultForOperation(
	operation BillingMutationOperation,
	executionClass BillingMutationExecutionClass,
	targetPlan string,
	result BillingMutationResult,
) error {
	if err := validateBillingMutationResult(result); err != nil {
		return err
	}
	legacyUnpinned := executionClass == ""
	switch operation {
	case BillingMutationSetup:
		if !legacyUnpinned && executionClass != BillingMutationExecutionSetup ||
			(result.Kind != BillingMutationResultDone &&
				result.Kind != BillingMutationResultAction) || result.Plan != "" {
			return errors.New("billing mutation receipt: result does not match setup operation")
		}
	case BillingMutationPlanUpgrade:
		validKind := legacyUnpinned &&
			(result.Kind == BillingMutationResultDone ||
				result.Kind == BillingMutationResultAction ||
				result.Kind == BillingMutationResultContact) ||
			executionClass == BillingMutationExecutionUpgradeContact &&
				result.Kind == BillingMutationResultContact ||
			executionClass == BillingMutationExecutionUpgradeSelfServe &&
				(result.Kind == BillingMutationResultDone ||
					result.Kind == BillingMutationResultAction)
		if !validKind || result.Plan != targetPlan {
			return errors.New("billing mutation receipt: result does not match upgrade operation")
		}
	case BillingMutationPlanDowngrade:
		if !legacyUnpinned && executionClass != BillingMutationExecutionDowngrade ||
			result.Kind != BillingMutationResultScheduled || result.Plan != targetPlan {
			return errors.New("billing mutation receipt: result does not match downgrade operation")
		}
	case BillingMutationPlanCancel:
		if !legacyUnpinned && executionClass != BillingMutationExecutionCancel ||
			(result.Kind != BillingMutationResultCancelled &&
				result.Kind != BillingMutationResultResolved) ||
			result.Plan != targetPlan {
			return errors.New("billing mutation receipt: result does not match cancel operation")
		}
	default:
		return fmt.Errorf("billing mutation receipt: unsupported operation %q", operation)
	}
	return nil
}

func validateBillingMutationResult(result BillingMutationResult) error {
	if result.Plan != "" && !validBillingMutationToken(result.Plan, 128) {
		return errors.New("billing mutation receipt: invalid result plan")
	}
	if result.Effective != nil && result.Effective.IsZero() {
		return errors.New("billing mutation receipt: invalid result effective time")
	}
	if result.ActionExpiresAt != nil && result.ActionExpiresAt.IsZero() {
		return errors.New("billing mutation receipt: invalid action expiry")
	}
	if result.ProviderObjectID != "" &&
		!validBillingMutationToken(result.ProviderObjectID, 255) {
		return errors.New("billing mutation receipt: invalid provider object id")
	}
	switch result.Kind {
	case BillingMutationResultDone:
		if result.URL != "" || result.ProviderObjectID != "" ||
			result.ActionExpiresAt != nil || result.Effective != nil || result.Cancelled {
			return errors.New("billing mutation receipt: invalid done result")
		}
	case BillingMutationResultAction:
		if !validBillingMutationURL(result.URL) || result.Effective != nil || result.Cancelled {
			return errors.New("billing mutation receipt: invalid action result")
		}
	case BillingMutationResultScheduled:
		if result.Plan == "" || result.URL != "" || result.ProviderObjectID != "" ||
			result.ActionExpiresAt != nil || result.Effective == nil || result.Cancelled {
			return errors.New("billing mutation receipt: invalid scheduled result")
		}
	case BillingMutationResultCancelled:
		if result.URL != "" || result.ProviderObjectID != "" ||
			result.ActionExpiresAt != nil || result.Effective != nil || !result.Cancelled {
			return errors.New("billing mutation receipt: invalid cancelled result")
		}
	case BillingMutationResultResolved:
		if result.Plan != "" || result.URL != "" || result.ProviderObjectID != "" ||
			result.ActionExpiresAt != nil || result.Effective != nil || result.Cancelled {
			return errors.New("billing mutation receipt: invalid resolved result")
		}
	case BillingMutationResultContact:
		if result.Plan == "" || result.URL != "" || result.ProviderObjectID != "" ||
			result.ActionExpiresAt != nil || result.Effective != nil || result.Cancelled {
			return errors.New("billing mutation receipt: invalid contact result")
		}
	default:
		return fmt.Errorf("billing mutation receipt: unsupported result kind %q", result.Kind)
	}
	return nil
}

func validateBillingMutationClaimRequest(
	claimToken string,
	now, leaseExpiresAt time.Time,
) error {
	if !validBillingMutationClaimToken(claimToken) {
		return errors.New("billing mutation receipt: invalid claim token")
	}
	if now.IsZero() || leaseExpiresAt.IsZero() || !leaseExpiresAt.After(now) {
		return errors.New("billing mutation receipt: claim lease must end after a non-zero claim time")
	}
	return nil
}

func validateBillingAccountMutationClaimRequest(
	accountID, operationID string,
	expectedOperationGeneration int64,
	claimToken string,
	now, leaseExpiresAt time.Time,
) error {
	if !validBillingMutationText(accountID, 255) {
		return errors.New("billing account mutation lease: invalid account id")
	}
	if !validBillingMutationOperationID(operationID) {
		return errors.New("billing account mutation lease: invalid operation id")
	}
	if expectedOperationGeneration < 0 {
		return errors.New("billing account mutation lease: expected generation cannot be negative")
	}
	if err := validateBillingMutationClaimRequest(
		claimToken, now, leaseExpiresAt); err != nil {
		return err
	}
	return nil
}

func validateBillingAccountMutationLease(lease BillingAccountMutationLease) error {
	switch {
	case lease.SchemaVersion != billingAccountMutationLeaseSchemaVersion:
		return fmt.Errorf("billing account mutation lease: unsupported schema version %d", lease.SchemaVersion)
	case !validBillingMutationText(lease.AccountID, 255):
		return errors.New("billing account mutation lease: invalid account id")
	case !validBillingMutationOperationID(lease.OperationID):
		return errors.New("billing account mutation lease: invalid operation id")
	case lease.OperationGeneration < 1 || lease.ClaimGeneration < 1:
		return errors.New("billing account mutation lease: generations must be positive")
	case lease.UpdatedAt.IsZero():
		return errors.New("billing account mutation lease: updated_at is required")
	case lease.Version < 1:
		return errors.New("billing account mutation lease: version must be positive")
	case (lease.ClaimToken == "") != (lease.LeaseExpiresAt == nil):
		return errors.New("billing account mutation lease: claim requires both token and lease expiry")
	}
	if lease.ClaimToken != "" &&
		(!validBillingMutationClaimToken(lease.ClaimToken) || lease.LeaseExpiresAt.IsZero() ||
			!lease.LeaseExpiresAt.After(lease.UpdatedAt)) {
		return errors.New("billing account mutation lease: invalid claim")
	}
	return nil
}

func billingMutationReceiptTerminal(receipt BillingMutationReceipt) bool {
	return receipt.Status == BillingMutationCompleted ||
		receipt.Status == BillingMutationSuperseded
}

func sameBillingMutationIdentity(a, b BillingMutationReceipt) bool {
	return a.SchemaVersion == b.SchemaVersion &&
		a.OperationID == b.OperationID && a.AccountID == b.AccountID &&
		a.ActorID == b.ActorID && a.ActorRole == b.ActorRole &&
		a.Operation == b.Operation && a.ExecutionClass == b.ExecutionClass &&
		a.AccountGeneration == b.AccountGeneration &&
		a.IdempotencyKeySHA256 == b.IdempotencyKeySHA256 &&
		a.RequestSHA256 == b.RequestSHA256 && a.Reason == b.Reason &&
		a.TargetPlan == b.TargetPlan &&
		a.ApprovedPriceCents == b.ApprovedPriceCents &&
		a.ApprovedCurrency == b.ApprovedCurrency
}

func validBillingMutationExecutionClass(value BillingMutationExecutionClass) bool {
	switch value {
	case BillingMutationExecutionSetup,
		BillingMutationExecutionUpgradeContact,
		BillingMutationExecutionUpgradeSelfServe,
		BillingMutationExecutionDowngrade,
		BillingMutationExecutionCancel:
		return true
	default:
		return false
	}
}

func validateBillingMutationApproval(receipt BillingMutationReceipt) error {
	planApproval := receipt.ApprovedCurrency != ""
	switch receipt.ExecutionClass {
	case BillingMutationExecutionSetup:
		if receipt.Operation != BillingMutationSetup || receipt.TargetPlan != "" ||
			receipt.ApprovedPriceCents != 0 || planApproval {
			return errors.New("billing mutation receipt: invalid setup approval")
		}
	case BillingMutationExecutionUpgradeContact:
		if receipt.Operation != BillingMutationPlanUpgrade || receipt.TargetPlan == "" ||
			receipt.ApprovedPriceCents != 0 || planApproval {
			return errors.New("billing mutation receipt: invalid contact-upgrade approval")
		}
	case BillingMutationExecutionUpgradeSelfServe:
		if receipt.Operation != BillingMutationPlanUpgrade || receipt.TargetPlan == "" ||
			receipt.ApprovedPriceCents <= 0 || !planApproval {
			return errors.New("billing mutation receipt: invalid self-serve upgrade approval")
		}
	case BillingMutationExecutionDowngrade:
		if receipt.Operation != BillingMutationPlanDowngrade || receipt.TargetPlan == "" ||
			!planApproval {
			return errors.New("billing mutation receipt: invalid downgrade approval")
		}
	case BillingMutationExecutionCancel:
		if receipt.Operation != BillingMutationPlanCancel || receipt.TargetPlan != "" ||
			receipt.ApprovedPriceCents != 0 || planApproval {
			return errors.New("billing mutation receipt: invalid cancel approval")
		}
	}
	return nil
}

func pendingBillingMutationShard(operationID string) int {
	digest := sha256.Sum256([]byte(operationID))
	return int(digest[0]) % billingMutationPendingShardCount
}

func billingAccountMutationClaimMatches(
	current, claimed BillingAccountMutationLease,
) bool {
	return current.OperationID == claimed.OperationID &&
		current.OperationGeneration == claimed.OperationGeneration &&
		current.ClaimToken != "" && current.ClaimToken == claimed.ClaimToken &&
		current.ClaimGeneration == claimed.ClaimGeneration
}

func billingAccountMutationLeaseLive(
	lease BillingAccountMutationLease,
	now time.Time,
) bool {
	return lease.ClaimToken != "" && lease.LeaseExpiresAt != nil &&
		now.Before(*lease.LeaseExpiresAt)
}

func sameBillingMutationResult(a, b BillingMutationResult) bool {
	if a.Kind != b.Kind || a.Plan != b.Plan || a.URL != b.URL ||
		a.ProviderObjectID != b.ProviderObjectID || a.Cancelled != b.Cancelled ||
		(a.ActionExpiresAt == nil) != (b.ActionExpiresAt == nil) ||
		(a.Effective == nil) != (b.Effective == nil) {
		return false
	}
	if a.ActionExpiresAt != nil && !a.ActionExpiresAt.Equal(*b.ActionExpiresAt) {
		return false
	}
	return a.Effective == nil || a.Effective.Equal(*b.Effective)
}

func billingMutationClaimMatches(current, claimed BillingMutationReceipt) bool {
	return current.ClaimToken != "" && current.ClaimToken == claimed.ClaimToken &&
		current.ClaimGeneration == claimed.ClaimGeneration
}

func cloneBillingMutationReceipt(receipt BillingMutationReceipt) BillingMutationReceipt {
	if receipt.LeaseExpiresAt != nil {
		value := *receipt.LeaseExpiresAt
		receipt.LeaseExpiresAt = &value
	}
	if receipt.Result != nil {
		result := *receipt.Result
		if result.ActionExpiresAt != nil {
			value := *result.ActionExpiresAt
			result.ActionExpiresAt = &value
		}
		if result.Effective != nil {
			value := *result.Effective
			result.Effective = &value
		}
		receipt.Result = &result
	}
	if receipt.CompletedAt != nil {
		value := *receipt.CompletedAt
		receipt.CompletedAt = &value
	}
	return receipt
}

func cloneBillingMutationResult(result BillingMutationResult) BillingMutationResult {
	if result.ActionExpiresAt != nil {
		value := *result.ActionExpiresAt
		result.ActionExpiresAt = &value
	}
	if result.Effective != nil {
		value := *result.Effective
		result.Effective = &value
	}
	return result
}

func cloneBillingAccountMutationLease(
	lease BillingAccountMutationLease,
) BillingAccountMutationLease {
	if lease.LeaseExpiresAt != nil {
		value := *lease.LeaseExpiresAt
		lease.LeaseExpiresAt = &value
	}
	return lease
}

func validBillingMutationOperation(operation BillingMutationOperation) bool {
	switch operation {
	case BillingMutationSetup, BillingMutationPlanUpgrade,
		BillingMutationPlanDowngrade, BillingMutationPlanCancel:
		return true
	default:
		return false
	}
}

func validBillingMutationOperationID(value string) bool {
	if len(value) < 5 || len(value) > 128 || !strings.HasPrefix(value, "bop_") {
		return false
	}
	for _, r := range value[4:] {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') &&
			(r < '0' || r > '9') && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

func validLowerSHA256(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validBillingMutationText(value string, maxBytes int) bool {
	if value == "" || len(value) > maxBytes || !utf8.ValidString(value) ||
		strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func validBillingMutationToken(value string, maxBytes int) bool {
	if !validBillingMutationText(value, maxBytes) {
		return false
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') &&
			r != '_' && r != '-' && r != '.' && r != ':' {
			return false
		}
	}
	return true
}

func validBillingMutationClaimToken(value string) bool {
	if len(value) < 8 || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') &&
			(r < '0' || r > '9') && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

func validBillingMutationReason(value string) bool {
	return validBillingReasonInput(value)
}

func validBillingMutationURL(value string) bool {
	if value == "" || len(value) > 2048 || value != strings.TrimSpace(value) ||
		!utf8.ValidString(value) || strings.ContainsRune(value, '\\') ||
		billingMutationURLHasUnsafeRune(value) {
		return false
	}
	decoded, err := url.PathUnescape(value)
	if err != nil || strings.ContainsRune(decoded, '\\') ||
		billingMutationURLHasUnsafeRune(decoded) {
		return false
	}
	parsed, err := url.Parse(value)
	return err == nil && strings.EqualFold(parsed.Scheme, "https") &&
		parsed.Opaque == "" && parsed.User == nil && parsed.Host != "" &&
		parsed.Hostname() != ""
}

func billingMutationURLHasUnsafeRune(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' ||
			r == '\u061c' || r == '\u200e' || r == '\u200f' ||
			(r >= '\u202a' && r <= '\u202e') ||
			(r >= '\u2066' && r <= '\u2069') {
			return true
		}
	}
	return false
}
