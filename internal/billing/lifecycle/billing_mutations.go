package lifecycle

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/witwave-ai/witself/internal/billing"
)

const (
	billingMutationClaimLease       = 5 * time.Minute
	billingMutationExecutionTimeout = 4 * time.Minute
	maxBillingReasonBytes           = 512
	minIdempotencyKeyBytes          = 16
	maxIdempotencyKeyBytes          = 128
)

var (
	// ErrBillingMutationInput identifies a malformed customer mutation
	// envelope. It is safe for an HTTP boundary to map this to 400.
	ErrBillingMutationInput = errors.New("invalid billing mutation request")
	// ErrBillingMutationInProgress means another control-plane replica owns
	// the live receipt lease. The caller may retry the exact same request.
	ErrBillingMutationInProgress = errors.New("billing mutation is already in progress")
	// ErrBillingMutationApprovalDrift means the approved provider effect can no
	// longer be reproduced from the deployed provider catalog. The receipt stays
	// pending for operator resolution; it is never reinterpreted.
	ErrBillingMutationApprovalDrift = errors.New("billing mutation approval drift")
	// ErrBillingMutationEffectUnpinned protects pending schema-v1 receipts
	// written before execution effects were immutable. They may be completed
	// from exact durable account evidence, but never sent back to a provider.
	ErrBillingMutationEffectUnpinned = errors.New(
		"billing mutation execution effect is not pinned")
)

type billingMutationApproval struct {
	ExecutionClass     BillingMutationExecutionClass
	ApprovedPriceCents int64
	ApprovedCurrency   string
}

// BillingActor is authenticated account authority captured on the immutable
// receipt. The HTTP layer derives it from AccountAccess, never request JSON.
type BillingActor struct {
	ID   string
	Role string
}

// BillingMutationCommand is the provider-neutral mutation envelope. Apply
// commands require Confirmed and IdempotencyKey; previews require neither.
type BillingMutationCommand struct {
	Operation      BillingMutationOperation
	Plan           string
	Reason         string
	Confirmed      bool
	IdempotencyKey string
}

// BillingMutationPreview is a write-free plan for one command.
type BillingMutationPreview struct {
	Operation            BillingMutationOperation
	Plan                 string
	Allowed              bool
	ConfirmationRequired bool
	Effects              []string
	Violations           []string
	approval             billingMutationApproval
}

// BillingMutationExecution is the customer-safe terminal projection. Reason,
// email hashes, provider ids, and the raw retry key stay in the control plane.
type BillingMutationExecution struct {
	OperationID string
	Operation   BillingMutationOperation
	Actor       BillingActor
	Confirmed   bool
	Replayed    bool
	Outcome     Outcome
}

// PreviewBillingMutation performs policy/provider-capability reads only. It
// does not create a receipt, customer, session, subscription, or account row.
func (m *Manager) PreviewBillingMutation(
	ctx context.Context,
	accountID, email string,
	command BillingMutationCommand,
) (BillingMutationPreview, error) {
	command, err := normalizeBillingMutationCommand(command, false)
	if err != nil {
		return BillingMutationPreview{}, err
	}
	preview := BillingMutationPreview{
		Operation:            command.Operation,
		Plan:                 command.Plan,
		ConfirmationRequired: true,
		Effects:              []string{},
		Violations:           []string{},
	}
	if !m.BillingAvailable() {
		preview.Violations = append(preview.Violations,
			"billing is not supported by this control plane")
		return preview, nil
	}
	if _, ok := m.cfg.Store.(BillingMutationStore); !ok {
		preview.Violations = append(preview.Violations,
			"durable billing mutation receipts are not configured")
		return preview, nil
	}
	r, err := m.load(ctx, accountID, strings.TrimSpace(email))
	if err != nil {
		return BillingMutationPreview{}, err
	}

	_, provider, providerErr := m.providerFor(r)
	providerCapability := func(ok bool, capability string) {
		if providerErr != nil {
			preview.Violations = append(preview.Violations,
				"the account billing provider is unavailable")
			return
		}
		if !ok {
			preview.Violations = append(preview.Violations, fmt.Sprintf(
				"the configured billing provider does not support idempotent %s operations",
				capability))
		}
	}
	requiresPendingCancellation := r.Pending != nil &&
		(r.Pending.CancelPrevious ||
			(r.Pending.Kind != PendingContact && r.CustomerID != ""))

	switch command.Operation {
	case BillingMutationSetup:
		preview.approval.ExecutionClass = BillingMutationExecutionSetup
		providerCapability(providerErr == nil && implementsIdempotentSetup(provider), "setup")
		preview.Effects = append(preview.Effects,
			"create or reuse the account billing customer",
			"create or replay a provider-hosted payment setup flow")

	case BillingMutationPlanUpgrade:
		target, ok := m.cfg.Catalog.Get(command.Plan)
		if !ok {
			preview.Violations = append(preview.Violations,
				fmt.Sprintf("unknown plan %q", command.Plan))
			break
		}
		current, _ := m.cfg.Catalog.Get(r.Entitled)
		switch {
		case target.ID == r.Entitled:
			preview.Violations = append(preview.Violations,
				fmt.Sprintf("already on the %s plan", target.ID))
		case !target.Available:
			preview.approval.ExecutionClass = BillingMutationExecutionUpgradeContact
			preview.Effects = append(preview.Effects,
				fmt.Sprintf("record a contact request for the %s plan", target.ID))
		case target.PriceCents() <= current.PriceCents():
			preview.Violations = append(preview.Violations,
				fmt.Sprintf("%s is not an upgrade from %s", target.ID, current.ID))
		case !target.Purchasable():
			preview.Violations = append(preview.Violations,
				fmt.Sprintf("plan %q is not purchasable", target.ID))
		default:
			preview.approval = billingMutationApproval{
				ExecutionClass:     BillingMutationExecutionUpgradeSelfServe,
				ApprovedPriceCents: target.PriceCents(),
				ApprovedCurrency:   strings.ToLower(m.cfg.Catalog.Currency),
			}
			providerCapability(providerErr == nil && implementsIdempotentSubscribe(provider), "subscription")
			preview.Effects = append(preview.Effects,
				fmt.Sprintf("purchase or switch the subscription to %s", target.ID),
				"apply the entitled plan after provider confirmation")
		}
		if requiresPendingCancellation {
			providerCapability(providerErr == nil && implementsIdempotentCancel(provider), "pending cancellation")
			preview.Effects = append(preview.Effects,
				"replace the existing pending billing change")
		}

	case BillingMutationPlanDowngrade:
		target, ok := m.cfg.Catalog.Get(command.Plan)
		if !ok {
			preview.Violations = append(preview.Violations,
				fmt.Sprintf("unknown plan %q", command.Plan))
			break
		}
		current, _ := m.cfg.Catalog.Get(r.Entitled)
		preview.approval = billingMutationApproval{
			ExecutionClass:     BillingMutationExecutionDowngrade,
			ApprovedPriceCents: target.PriceCents(),
			ApprovedCurrency:   strings.ToLower(m.cfg.Catalog.Currency),
		}
		switch {
		case target.ID == r.Entitled:
			preview.Violations = append(preview.Violations,
				fmt.Sprintf("already on the %s plan", target.ID))
		case target.PriceCents() >= current.PriceCents():
			preview.Violations = append(preview.Violations,
				fmt.Sprintf("%s is not a downgrade from %s", target.ID, current.ID))
		default:
			targetSnapshot, resolveErr := m.resolveSnapshotForEntitlement(r, target.ID)
			if resolveErr != nil {
				return BillingMutationPreview{}, resolveErr
			}
			violations, fitErr := m.cfg.Fit.Fit(ctx, accountID, targetSnapshot)
			if fitErr != nil {
				return BillingMutationPreview{}, fitErr
			}
			preview.Violations = append(preview.Violations, violations...)
		}
		if r.CustomerID == "" {
			preview.Violations = append(preview.Violations,
				"the account has no billing customer to downgrade")
		}
		providerCapability(
			providerErr == nil && implementsIdempotentDowngrade(provider) &&
				supportsDowngradeTarget(provider, target.ID),
			fmt.Sprintf("downgrade to %s", target.ID))
		if requiresPendingCancellation {
			providerCapability(providerErr == nil && implementsIdempotentCancel(provider), "pending cancellation")
		}
		preview.Effects = append(preview.Effects,
			fmt.Sprintf("schedule the subscription to change to %s at period end", target.ID))

	case BillingMutationPlanCancel:
		preview.approval.ExecutionClass = BillingMutationExecutionCancel
		if r.Pending == nil {
			preview.Violations = append(preview.Violations, "nothing is pending")
		} else if r.Pending.Kind != PendingContact || r.Pending.CancelPrevious {
			providerCapability(providerErr == nil && implementsIdempotentCancel(provider), "pending cancellation")
		}
		preview.Effects = append(preview.Effects,
			"cancel the current pending plan change")
	}
	preview.Allowed = len(preview.Violations) == 0
	if preview.Allowed && !validBillingMutationExecutionClass(
		preview.approval.ExecutionClass) {
		return BillingMutationPreview{}, errors.New(
			"lifecycle: allowed billing preview has no approved execution class")
	}
	return preview, nil
}

// ExecuteBillingMutation creates/replays, claims, executes, and completes one
// durable receipt. Exact completed retries never call the provider again.
func (m *Manager) ExecuteBillingMutation(
	ctx context.Context,
	accountID, email string,
	actor BillingActor,
	command BillingMutationCommand,
) (BillingMutationExecution, error) {
	command, err := normalizeBillingMutationCommand(command, true)
	if err != nil {
		return BillingMutationExecution{}, err
	}
	actor.ID = strings.TrimSpace(actor.ID)
	actor.Role = strings.TrimSpace(actor.Role)
	if !validBillingMutationText(actor.ID, 255) ||
		!validBillingMutationToken(actor.Role, 64) {
		return BillingMutationExecution{}, billingMutationInput("invalid authenticated actor")
	}
	accountID = strings.TrimSpace(accountID)
	if !validBillingMutationText(accountID, 255) {
		return BillingMutationExecution{}, billingMutationInput("invalid account id")
	}
	email = strings.TrimSpace(email)
	store, ok := m.cfg.Store.(BillingMutationStore)
	if !ok {
		return BillingMutationExecution{}, errors.New(
			"lifecycle: durable billing mutation store is required")
	}
	operationID := billingMutationOperationID(accountID, command.IdempotencyKey)
	stored, exists, err := store.GetBillingMutation(ctx, operationID)
	if err != nil {
		return BillingMutationExecution{}, err
	}
	var receipt BillingMutationReceipt
	created := false
	var approval billingMutationApproval
	if exists {
		approval = billingMutationApproval{
			ExecutionClass:     stored.ExecutionClass,
			ApprovedPriceCents: stored.ApprovedPriceCents,
			ApprovedCurrency:   stored.ApprovedCurrency,
		}
		// The initiating actor is immutable audit attribution, not part of the
		// semantic operation payload. Every retry is independently authorized at
		// the HTTP boundary, so an operator rotation or role change must not strand
		// an otherwise exact pending operation.
		initiatingActor := BillingActor{ID: stored.ActorID, Role: stored.ActorRole}
		if stored.SchemaVersion == billingMutationLegacyReceiptSchemaVersion {
			receipt, err = buildBillingMutationReceiptForSchema(
				billingMutationLegacyReceiptSchemaVersion,
				accountID, email, initiatingActor, command,
				stored.AccountGeneration, billingMutationApproval{},
				m.cfg.Now().UTC())
		} else {
			receipt, err = buildBillingMutationReceipt(
				accountID, email, initiatingActor, command,
				stored.AccountGeneration, approval, m.cfg.Now().UTC())
		}
		if err != nil {
			return BillingMutationExecution{}, err
		}
		if !sameBillingMutationIdentity(stored, receipt) {
			return BillingMutationExecution{}, ErrBillingMutationConflict
		}
		if stored.Status == BillingMutationCompleted {
			return billingExecutionFromReceipt(stored, true, m.cfg.Now().UTC())
		}
		if stored.Status == BillingMutationSuperseded {
			return BillingMutationExecution{}, ErrBillingMutationSuperseded
		}
		// Repair (or prove) the global recovery index before an exact retry can
		// reacquire claims and cross the provider boundary. Index saturation is
		// deliberate backpressure, never a reason to run untracked work.
		stored, _, err = store.ReceiveBillingMutation(ctx, receipt)
		if err != nil {
			return BillingMutationExecution{}, err
		}
	} else {
		previewCommand := command
		previewCommand.Confirmed = false
		previewCommand.IdempotencyKey = ""
		preview, previewErr := m.PreviewBillingMutation(
			ctx, accountID, email, previewCommand)
		if previewErr != nil {
			return BillingMutationExecution{}, previewErr
		}
		if !preview.Allowed {
			return BillingMutationExecution{}, refuse("%s",
				strings.Join(preview.Violations, "; "))
		}
		approval = preview.approval
	}

	claimToken, err := newBillingMutationClaimToken()
	if err != nil {
		return BillingMutationExecution{}, err
	}
	claimNow := m.cfg.Now().UTC()
	expectedAccountGeneration := int64(0)
	if exists {
		expectedAccountGeneration = stored.AccountGeneration
	}
	accountLease, acquired, err := store.ClaimBillingMutationAccount(
		ctx, accountID, operationID, expectedAccountGeneration,
		claimToken, claimNow, claimNow.Add(billingMutationClaimLease))
	if errors.Is(err, ErrBillingMutationSuperseded) {
		if exists && stored.Status == BillingMutationPending {
			if _, supersedeErr := store.SupersedeBillingMutation(
				ctx, stored, claimNow); supersedeErr != nil &&
				!errors.Is(supersedeErr, ErrBillingMutationClaimActive) {
				return BillingMutationExecution{}, supersedeErr
			}
		}
		return BillingMutationExecution{}, ErrBillingMutationSuperseded
	}
	if err != nil {
		return BillingMutationExecution{}, err
	}
	if !acquired {
		return BillingMutationExecution{}, ErrBillingMutationInProgress
	}
	releaseAccount := func() {
		_ = store.ReleaseBillingMutationAccount(
			ctx, accountLease, m.cfg.Now().UTC())
	}

	if !exists {
		receipt, err = buildBillingMutationReceipt(
			accountID, email, actor, command,
			accountLease.OperationGeneration, approval, claimNow)
		if err != nil {
			releaseAccount()
			return BillingMutationExecution{}, err
		}
		stored, created, err = store.ReceiveBillingMutation(ctx, receipt)
		if err != nil {
			releaseAccount()
			return BillingMutationExecution{}, err
		}
		if !created || !sameBillingMutationIdentity(stored, receipt) {
			releaseAccount()
			return BillingMutationExecution{}, ErrBillingMutationConflict
		}
		// Receipt creation and the account reservation are separate durable
		// writes. An expired receipt-less reservation may be overtaken by a new
		// operation while this worker is delayed in Receive. Revalidate the exact
		// operation generation and claim token after the receipt is visible, before
		// this worker can claim the receipt or reach the provider.
		revalidateNow := m.cfg.Now().UTC()
		revalidated, revalidatedAcquired, revalidateErr :=
			store.ClaimBillingMutationAccount(
				ctx, accountID, operationID, accountLease.OperationGeneration,
				claimToken, revalidateNow,
				revalidateNow.Add(billingMutationClaimLease))
		if errors.Is(revalidateErr, ErrBillingMutationSuperseded) {
			if _, supersedeErr := store.SupersedeBillingMutation(
				ctx, stored, revalidateNow); supersedeErr != nil &&
				!errors.Is(supersedeErr, ErrBillingMutationClaimActive) {
				return BillingMutationExecution{}, supersedeErr
			}
			return BillingMutationExecution{}, ErrBillingMutationSuperseded
		}
		if revalidateErr != nil {
			releaseAccount()
			return BillingMutationExecution{}, revalidateErr
		}
		if !revalidatedAcquired {
			releaseAccount()
			return BillingMutationExecution{}, ErrBillingMutationInProgress
		}
		accountLease = revalidated
		claimNow = revalidateNow
	}

	claimed, acquired, err := store.ClaimBillingMutation(
		ctx, stored, claimToken, claimNow,
		claimNow.Add(billingMutationClaimLease))
	if err != nil {
		releaseAccount()
		return BillingMutationExecution{}, err
	}
	if !acquired {
		releaseAccount()
		return BillingMutationExecution{}, ErrBillingMutationInProgress
	}
	releaseClaims := func() {
		_ = store.ReleaseBillingMutation(ctx, claimed, m.cfg.Now().UTC())
		releaseAccount()
	}
	complete := func(result BillingMutationResult) (BillingMutationExecution, error) {
		completed, completeErr := store.CompleteBillingMutation(
			ctx, claimed, result, m.cfg.Now().UTC())
		if completeErr != nil {
			// Completion can succeed remotely while its response is lost. Confirm
			// terminal state before deciding whether to hold both leases for safe
			// expiry/takeover recovery.
			current, currentOK, getErr := store.GetBillingMutation(
				ctx, claimed.OperationID)
			if getErr == nil && currentOK &&
				current.Status == BillingMutationCompleted {
				releaseAccount()
				return billingExecutionFromReceipt(
					current, !created, m.cfg.Now().UTC())
			}
			if getErr != nil {
				return BillingMutationExecution{}, getErr
			}
			return BillingMutationExecution{}, completeErr
		}
		releaseAccount()
		return billingExecutionFromReceipt(
			completed, !created, m.cfg.Now().UTC())
	}

	// A crash can occur after the account fold but before receipt completion.
	// Recover the value-minimal result from the exact account operation fence.
	if result, recovered, recoverErr := m.recoverBillingMutation(
		ctx, claimed, created); recoverErr != nil {
		releaseClaims()
		return BillingMutationExecution{}, recoverErr
	} else if recovered {
		return complete(result)
	}
	if claimed.ExecutionClass == "" {
		releaseClaims()
		return BillingMutationExecution{}, ErrBillingMutationEffectUnpinned
	}
	if !m.cfg.Now().UTC().Before(
		claimed.CreatedAt.Add(billingMutationAutomaticRetryHorizon)) {
		releaseClaims()
		return BillingMutationExecution{},
			ErrBillingMutationRetryHorizonExceeded
	}

	executionCtx, cancelExecution := context.WithTimeout(
		ctx, billingMutationExecutionTimeout)
	defer cancelExecution()
	result, executeErr := m.executeClaimedBillingMutation(
		executionCtx, accountID, email, claimed)
	if executeErr != nil {
		releaseClaims()
		return BillingMutationExecution{}, executeErr
	}
	return complete(result)
}

func (m *Manager) executeClaimedBillingMutation(
	ctx context.Context,
	accountID, email string,
	receipt BillingMutationReceipt,
) (BillingMutationResult, error) {
	switch receipt.ExecutionClass {
	case BillingMutationExecutionSetup:
		action, err := m.createBillingSetupMutation(
			ctx, accountID, email, receipt.OperationID)
		if err != nil {
			return BillingMutationResult{}, err
		}
		if action.Done {
			return BillingMutationResult{Kind: BillingMutationResultDone}, nil
		}
		return BillingMutationResult{
			Kind: BillingMutationResultAction, URL: action.URL,
			ProviderObjectID: action.ProviderObjectID,
			ActionExpiresAt:  timePointer(action.ExpiresAt),
		}, nil
	case BillingMutationExecutionUpgradeContact,
		BillingMutationExecutionUpgradeSelfServe:
		out, err := m.requestUpgrade(
			ctx, accountID, email, receipt.TargetPlan, receipt.OperationID, true,
			billingMutationApproval{
				ExecutionClass:     receipt.ExecutionClass,
				ApprovedPriceCents: receipt.ApprovedPriceCents,
				ApprovedCurrency:   receipt.ApprovedCurrency,
			})
		return billingResultFromOutcome(out), err
	case BillingMutationExecutionDowngrade:
		out, err := m.requestDowngrade(
			ctx, accountID, email, receipt.TargetPlan, receipt.OperationID, true,
			billingMutationApproval{
				ExecutionClass:     receipt.ExecutionClass,
				ApprovedPriceCents: receipt.ApprovedPriceCents,
				ApprovedCurrency:   receipt.ApprovedCurrency,
			})
		return billingResultFromOutcome(out), err
	case BillingMutationExecutionCancel:
		resolved, err := m.cancelPendingMutation(ctx, accountID, receipt.OperationID)
		if err != nil {
			return BillingMutationResult{}, err
		}
		if resolved {
			return BillingMutationResult{Kind: BillingMutationResultResolved}, nil
		}
		return BillingMutationResult{
			Kind: BillingMutationResultCancelled, Cancelled: true,
		}, nil
	default:
		return BillingMutationResult{}, billingMutationInput(
			"unsupported approved execution class")
	}
}

func (m *Manager) recoverBillingMutation(
	ctx context.Context,
	receipt BillingMutationReceipt,
	created bool,
) (BillingMutationResult, bool, error) {
	if receipt.Operation == BillingMutationSetup {
		return BillingMutationResult{}, false, nil
	}
	r, err := m.load(ctx, receipt.AccountID, "")
	if err != nil {
		return BillingMutationResult{}, false, err
	}
	switch receipt.Operation {
	case BillingMutationPlanUpgrade:
		if r.Entitled == receipt.TargetPlan {
			return BillingMutationResult{
				Kind: BillingMutationResultDone, Plan: receipt.TargetPlan,
			}, true, nil
		}
		if r.Pending != nil && r.Pending.OperationID == receipt.OperationID &&
			r.Pending.Plan == receipt.TargetPlan {
			switch {
			case r.Pending.Kind == PendingContact:
				return BillingMutationResult{
					Kind: BillingMutationResultContact, Plan: receipt.TargetPlan,
				}, true, nil
			case r.Pending.Kind == PendingUpgrade && r.Pending.URL != "":
				expires := r.Pending.Expires
				return BillingMutationResult{
					Kind: BillingMutationResultAction,
					Plan: receipt.TargetPlan, URL: r.Pending.URL,
					ProviderObjectID: r.Pending.ProviderObjectID,
					ActionExpiresAt:  &expires,
				}, true, nil
			}
		}
	case BillingMutationPlanDowngrade:
		if r.Pending != nil && r.Pending.OperationID == receipt.OperationID &&
			r.Pending.Kind == PendingDowngrade &&
			r.Pending.Plan == receipt.TargetPlan &&
			!r.Pending.Effective.IsZero() {
			effective := r.Pending.Effective
			return BillingMutationResult{
				Kind: BillingMutationResultScheduled,
				Plan: receipt.TargetPlan, Effective: &effective,
			}, true, nil
		}
		if !created &&
			r.LastBillingMutationOperationID == receipt.OperationID &&
			r.LastBillingMutationKind == BillingMutationPlanDowngrade &&
			r.LastBillingMutationResultKind == BillingMutationResultScheduled &&
			r.LastBillingMutationPlan == receipt.TargetPlan &&
			!r.LastBillingMutationEffective.IsZero() {
			effective := r.LastBillingMutationEffective
			return BillingMutationResult{
				Kind: BillingMutationResultScheduled,
				Plan: receipt.TargetPlan, Effective: &effective,
			}, true, nil
		}
	case BillingMutationPlanCancel:
		// Pending disappearing is not proof of cancellation: a webhook can
		// resolve it concurrently. Only the exact account-fold tombstone can
		// recover a crash between the local fold and receipt completion.
		if !created &&
			r.LastBillingMutationOperationID == receipt.OperationID &&
			r.LastBillingMutationKind == BillingMutationPlanCancel {
			switch r.LastBillingMutationResultKind {
			case BillingMutationResultResolved:
				return BillingMutationResult{
					Kind: BillingMutationResultResolved,
				}, true, nil
			case BillingMutationResultCancelled, "":
				// Empty preserves recovery for cancel tombstones created before
				// the result-kind field was introduced.
				return BillingMutationResult{
					Kind: BillingMutationResultCancelled, Cancelled: true,
				}, true, nil
			}
		}
	}
	return BillingMutationResult{}, false, nil
}

// BillingMutationReconcileSummary is the value-free projection of one bounded
// global recovery pass. Busy means another live replica retained the exact
// fence; TerminalCleaned counts stale index markers, not deleted receipts.
type BillingMutationReconcileSummary struct {
	Scanned                 int
	Attempted               int
	Completed               int
	Superseded              int
	Busy                    int
	Failed                  int
	TerminalCleaned         int
	ScanCapped              bool
	OldestObservedPendingAt *time.Time
}

type billingMutationReconcileOutcome int

const (
	billingMutationReconcileNoop billingMutationReconcileOutcome = iota
	billingMutationReconcileCompleted
	billingMutationReconcileSuperseded
	billingMutationReconcileBusy
)

type billingMutationReconcileResult struct {
	outcome billingMutationReconcileOutcome
	err     error
}

// ReconcileBillingMutations resumes one bounded window from the global shard
// set. Valid receipts still run when another shard or reference is malformed.
// A small worker pool keeps one slow provider request from consuming the whole
// cron deadline; receipt and account claims remain the collision fence.
func (m *Manager) ReconcileBillingMutations(
	ctx context.Context,
) (BillingMutationReconcileSummary, error) {
	store, ok := m.cfg.Store.(BillingMutationStore)
	if !ok {
		return BillingMutationReconcileSummary{}, nil
	}
	batch, listErr := store.PendingBillingMutations(
		ctx, maxBillingMutationsPerReconcile)
	summary := BillingMutationReconcileSummary{
		Scanned:                 batch.Scanned,
		TerminalCleaned:         batch.TerminalCleanup,
		ScanCapped:              batch.ScanCapped,
		OldestObservedPendingAt: batch.OldestObservedPendingAt,
	}
	if len(batch.Receipts) == 0 {
		return summary, listErr
	}

	jobs := make(chan BillingMutationReceipt)
	results := make(chan billingMutationReconcileResult, len(batch.Receipts))
	workerCount := min(billingMutationReconcileConcurrency, len(batch.Receipts))
	var workers sync.WaitGroup
	for range workerCount {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for receipt := range jobs {
				outcome, err := m.reconcileBillingMutation(
					ctx, store, receipt)
				results <- billingMutationReconcileResult{
					outcome: outcome, err: err,
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, receipt := range batch.Receipts {
			select {
			case jobs <- receipt:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		workers.Wait()
		close(results)
	}()

	firstErr := listErr
	for result := range results {
		if result.outcome == billingMutationReconcileBusy {
			summary.Busy++
			continue
		}
		summary.Attempted++
		if result.err != nil {
			summary.Failed++
			if firstErr == nil {
				firstErr = result.err
			}
			continue
		}
		switch result.outcome {
		case billingMutationReconcileCompleted:
			summary.Completed++
		case billingMutationReconcileSuperseded:
			summary.Superseded++
		}
	}
	if ctx.Err() != nil && firstErr == nil {
		firstErr = errors.New("billing mutation reconciliation deadline exceeded")
	}
	return summary, firstErr
}

// reconcileBillingMutation resumes one exact stored receipt without requiring
// the caller's raw idempotency key or email. The operation id is already the
// provider retry fence, and an empty email deliberately skips only the mutable
// customer-contact update; it cannot change receipt semantics or create a
// second provider customer.
func (m *Manager) reconcileBillingMutation(
	ctx context.Context,
	store BillingMutationStore,
	stored BillingMutationReceipt,
) (billingMutationReconcileOutcome, error) {
	if err := validateBillingMutationReceipt(stored); err != nil {
		return billingMutationReconcileNoop, err
	}
	if stored.Status != BillingMutationPending {
		return billingMutationReconcileNoop, nil
	}
	claimToken, err := newBillingMutationClaimToken()
	if err != nil {
		return billingMutationReconcileNoop, err
	}
	claimNow := m.cfg.Now().UTC()
	accountLease, acquired, err := store.ClaimBillingMutationAccount(
		ctx, stored.AccountID, stored.OperationID, stored.AccountGeneration,
		claimToken, claimNow, claimNow.Add(billingMutationClaimLease))
	if errors.Is(err, ErrBillingMutationSuperseded) {
		_, supersedeErr := store.SupersedeBillingMutation(ctx, stored, claimNow)
		if errors.Is(supersedeErr, ErrBillingMutationClaimActive) {
			return billingMutationReconcileBusy, nil
		}
		if supersedeErr != nil {
			return billingMutationReconcileNoop, supersedeErr
		}
		return billingMutationReconcileSuperseded, nil
	}
	if err != nil {
		return billingMutationReconcileNoop, err
	}
	if !acquired {
		return billingMutationReconcileBusy, nil
	}
	releaseAccount := func() {
		_ = store.ReleaseBillingMutationAccount(
			ctx, accountLease, m.cfg.Now().UTC())
	}

	claimed, acquired, err := store.ClaimBillingMutation(
		ctx, stored, claimToken, claimNow,
		claimNow.Add(billingMutationClaimLease))
	if err != nil {
		releaseAccount()
		return billingMutationReconcileNoop, err
	}
	if !acquired {
		releaseAccount()
		return billingMutationReconcileBusy, nil
	}
	releaseClaims := func() {
		_ = store.ReleaseBillingMutation(ctx, claimed, m.cfg.Now().UTC())
		releaseAccount()
	}
	complete := func(
		result BillingMutationResult,
	) (billingMutationReconcileOutcome, error) {
		_, completeErr := store.CompleteBillingMutation(
			ctx, claimed, result, m.cfg.Now().UTC())
		if completeErr != nil {
			current, currentOK, getErr := store.GetBillingMutation(
				ctx, claimed.OperationID)
			if getErr == nil && currentOK &&
				current.Status == BillingMutationCompleted {
				releaseAccount()
				return billingMutationReconcileCompleted, nil
			}
			if getErr != nil {
				return billingMutationReconcileNoop, getErr
			}
			// Keep both claims until lease expiry after an ambiguous completion
			// failure. A successor must not race an unconfirmed terminal write.
			return billingMutationReconcileNoop, completeErr
		}
		releaseAccount()
		return billingMutationReconcileCompleted, nil
	}

	if result, recovered, recoverErr := m.recoverBillingMutation(
		ctx, claimed, false); recoverErr != nil {
		releaseClaims()
		return billingMutationReconcileNoop, recoverErr
	} else if recovered {
		return complete(result)
	}
	if claimed.ExecutionClass == "" {
		releaseClaims()
		return billingMutationReconcileNoop,
			ErrBillingMutationEffectUnpinned
	}
	if !m.cfg.Now().UTC().Before(
		claimed.CreatedAt.Add(billingMutationAutomaticRetryHorizon)) {
		releaseClaims()
		return billingMutationReconcileNoop,
			ErrBillingMutationRetryHorizonExceeded
	}

	executionCtx, cancelExecution := context.WithTimeout(
		ctx, billingMutationExecutionTimeout)
	defer cancelExecution()
	result, executeErr := m.executeClaimedBillingMutation(
		executionCtx, claimed.AccountID, "", claimed)
	if executeErr != nil {
		releaseClaims()
		return billingMutationReconcileNoop, executeErr
	}
	return complete(result)
}

func buildBillingMutationReceipt(
	accountID, email string,
	actor BillingActor,
	command BillingMutationCommand,
	accountGeneration int64,
	approval billingMutationApproval,
	now time.Time,
) (BillingMutationReceipt, error) {
	return buildBillingMutationReceiptForSchema(
		billingMutationReceiptSchemaVersion, accountID, email, actor, command,
		accountGeneration, approval, now)
}

func buildBillingMutationReceiptForSchema(
	schemaVersion int,
	accountID, email string,
	actor BillingActor,
	command BillingMutationCommand,
	accountGeneration int64,
	approval billingMutationApproval,
	now time.Time,
) (BillingMutationReceipt, error) {
	keyDigest := sha256.Sum256([]byte(command.IdempotencyKey))
	emailDigest := ""
	if email != "" {
		normalizedEmail := strings.ToLower(email)
		digest := sha256.Sum256([]byte(normalizedEmail))
		emailDigest = hex.EncodeToString(digest[:])
	}
	operationID := billingMutationOperationID(accountID, command.IdempotencyKey)
	// Actor and email are immutable initiating audit attribution, not semantic
	// mutation inputs. Excluding them lets an independently authorized exact
	// retry recover after account-operator or account-email rotation.
	requestDocument := struct {
		AccountID string                   `json:"account_id"`
		Operation BillingMutationOperation `json:"operation"`
		Plan      string                   `json:"plan,omitempty"`
		Reason    string                   `json:"reason"`
		Confirmed bool                     `json:"confirmed"`
	}{
		AccountID: accountID, Operation: command.Operation, Plan: command.Plan,
		Reason: command.Reason, Confirmed: command.Confirmed,
	}
	encoded, err := json.Marshal(requestDocument)
	if err != nil {
		return BillingMutationReceipt{}, fmt.Errorf("lifecycle: encode billing request identity: %w", err)
	}
	requestDigest := sha256.Sum256(encoded)
	receipt := BillingMutationReceipt{
		SchemaVersion: schemaVersion,
		OperationID:   operationID, AccountID: accountID,
		ActorID: actor.ID, ActorRole: actor.Role, Operation: command.Operation,
		ExecutionClass:       approval.ExecutionClass,
		AccountGeneration:    accountGeneration,
		IdempotencyKeySHA256: hex.EncodeToString(keyDigest[:]),
		RequestSHA256:        hex.EncodeToString(requestDigest[:]),
		Reason:               command.Reason, ConfirmedAt: now,
		TargetPlan: command.Plan, EmailSHA256: emailDigest,
		ApprovedPriceCents: approval.ApprovedPriceCents,
		ApprovedCurrency:   approval.ApprovedCurrency,
		Status:             BillingMutationPending, CreatedAt: now, UpdatedAt: now,
	}
	if err := validateBillingMutationReceipt(receipt); err != nil {
		return BillingMutationReceipt{}, err
	}
	return receipt, nil
}

func billingMutationOperationID(accountID, idempotencyKey string) string {
	operationDigest := sha256.Sum256([]byte(
		"witself.billing-operation.v1\x00" + accountID + "\x00" + idempotencyKey))
	return "bop_" + base64.RawURLEncoding.EncodeToString(operationDigest[:24])
}

func billingResultFromOutcome(out Outcome) BillingMutationResult {
	result := BillingMutationResult{
		Kind: BillingMutationResultKind(out.Kind), Plan: out.Plan, URL: out.URL,
		ProviderObjectID: out.ProviderObjectID,
	}
	if !out.ActionExpiresAt.IsZero() {
		expires := out.ActionExpiresAt
		result.ActionExpiresAt = &expires
	}
	if !out.Effective.IsZero() {
		effective := out.Effective
		result.Effective = &effective
	}
	return result
}

func timePointer(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	cloned := value
	return &cloned
}

func billingExecutionFromReceipt(
	receipt BillingMutationReceipt,
	replayed bool,
	now time.Time,
) (BillingMutationExecution, error) {
	if receipt.Status != BillingMutationCompleted || receipt.Result == nil {
		return BillingMutationExecution{}, errors.New("lifecycle: billing mutation is not completed")
	}
	result := receipt.Result
	if result.Kind == BillingMutationResultAction &&
		result.ActionExpiresAt != nil &&
		!now.Before(*result.ActionExpiresAt) {
		return BillingMutationExecution{}, refuse(
			"hosted billing action expired; start a new request with a new idempotency key")
	}
	out := Outcome{
		Kind: string(result.Kind), Plan: result.Plan, URL: result.URL,
	}
	if result.Effective != nil {
		out.Effective = *result.Effective
	}
	return BillingMutationExecution{
		OperationID: receipt.OperationID, Operation: receipt.Operation,
		Actor:     BillingActor{ID: receipt.ActorID, Role: receipt.ActorRole},
		Confirmed: true, Replayed: replayed, Outcome: out,
	}, nil
}

func normalizeBillingMutationCommand(
	command BillingMutationCommand,
	apply bool,
) (BillingMutationCommand, error) {
	command.Plan = strings.TrimSpace(command.Plan)
	command.Reason = strings.TrimSpace(command.Reason)
	switch command.Operation {
	case BillingMutationSetup, BillingMutationPlanCancel:
		if command.Plan != "" {
			return BillingMutationCommand{}, billingMutationInput("plan is not allowed for this operation")
		}
	case BillingMutationPlanUpgrade, BillingMutationPlanDowngrade:
		if !validBillingMutationToken(command.Plan, 128) {
			return BillingMutationCommand{}, billingMutationInput("a valid plan is required")
		}
	default:
		return BillingMutationCommand{}, billingMutationInput("unsupported operation")
	}
	if !validBillingReasonInput(command.Reason) {
		return BillingMutationCommand{}, billingMutationInput(
			fmt.Sprintf("reason must be a single safe line of 1-%d bytes", maxBillingReasonBytes))
	}
	if !apply {
		if command.Confirmed || command.IdempotencyKey != "" {
			return BillingMutationCommand{}, billingMutationInput(
				"preview cannot be confirmed or carry an idempotency key")
		}
		return command, nil
	}
	if !command.Confirmed {
		return BillingMutationCommand{}, billingMutationInput("confirmed=true is required")
	}
	if !validBillingIdempotencyKey(command.IdempotencyKey) {
		return BillingMutationCommand{}, billingMutationInput(fmt.Sprintf(
			"idempotency key must be %d-%d printable ASCII bytes",
			minIdempotencyKeyBytes, maxIdempotencyKeyBytes))
	}
	return command, nil
}

func validBillingIdempotencyKey(value string) bool {
	if len(value) < minIdempotencyKeyBytes || len(value) > maxIdempotencyKeyBytes ||
		strings.TrimSpace(value) != value {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 0x21 || value[i] > 0x7e {
			return false
		}
	}
	return true
}

func validBillingReasonInput(value string) bool {
	if value == "" || len(value) > maxBillingReasonBytes ||
		!utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) || r == '\u061c' || r == '\u200e' || r == '\u200f' ||
			r == '\u2028' || r == '\u2029' || (r >= '\u202a' && r <= '\u202e') ||
			(r >= '\u2066' && r <= '\u2069') {
			return false
		}
	}
	return true
}

func billingMutationInput(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrBillingMutationInput}, args...)...)
}

func newBillingMutationClaimToken() (string, error) {
	var entropy [18]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", fmt.Errorf("billing mutation: generate claim token: %w", err)
	}
	return "bcl_" + base64.RawURLEncoding.EncodeToString(entropy[:]), nil
}

func implementsIdempotentSetup(provider billing.Provider) bool {
	_, ok := provider.(billing.IdempotentSetupper)
	return ok
}

func implementsIdempotentSubscribe(provider billing.Provider) bool {
	_, ok := provider.(billing.IdempotentSubscriber)
	return ok
}

func implementsIdempotentDowngrade(provider billing.Provider) bool {
	_, ok := provider.(billing.IdempotentDowngrader)
	return ok
}

func supportsDowngradeTarget(provider billing.Provider, plan string) bool {
	checker, ok := provider.(billing.DowngradeTargetChecker)
	return !ok || checker.SupportsDowngradeTarget(plan)
}

func implementsIdempotentCancel(provider billing.Provider) bool {
	_, ok := provider.(billing.IdempotentPendingCanceller)
	return ok
}
