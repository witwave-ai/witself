package server

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	maxVaultKeyRotationRequestBytes int64 = 16 * 1024
	maxVaultKeyRotationStageBytes   int64 = 256 * 1024
	maxVaultKeyRotationItems              = 100
	maxVaultKeyRotationCursorBytes        = 1024
	vaultKeyRotationWrappedDEKBytes       = 60
)

// VaultKeyRotation is value-free lifecycle metadata for a client-driven AVK
// rotation. The backend never receives either the source or target AVK.
type VaultKeyRotation struct {
	ID                      string     `json:"id"`
	AccountID               string     `json:"account_id"`
	RealmID                 string     `json:"realm_id"`
	OwnerAgentID            string     `json:"owner_agent_id"`
	SourceKeyID             string     `json:"source_key_id"`
	SourceKeyVersion        int64      `json:"source_key_version"`
	SourceKeyAlgorithm      string     `json:"source_key_algorithm"`
	SourceKeyFingerprint    string     `json:"source_key_fingerprint"`
	TargetKeyID             string     `json:"target_key_id"`
	TargetKeyVersion        int64      `json:"target_key_version"`
	TargetKeyAlgorithm      string     `json:"target_key_algorithm"`
	TargetKeyFingerprint    string     `json:"target_key_fingerprint"`
	LifecycleState          string     `json:"lifecycle_state"`
	RecoveryDispositionMode string     `json:"recovery_disposition_mode,omitempty"`
	RecoveryArtifactSHA256  string     `json:"recovery_artifact_sha256,omitempty"`
	ItemCount               int64      `json:"item_count"`
	StagedCount             int64      `json:"staged_count"`
	RowVersion              int64      `json:"row_version"`
	StagedPlanHash          string     `json:"staged_plan_hash,omitempty"`
	CreatedAt               time.Time  `json:"created_at"`
	UpdatedAt               time.Time  `json:"updated_at"`
	CommittedAt             *time.Time `json:"committed_at,omitempty"`
	CancelledAt             *time.Time `json:"cancelled_at,omitempty"`
}

// StartVaultKeyRotationRequest describes the source epoch observed by the
// client and the value-free metadata for the next AVK epoch.
type StartVaultKeyRotationRequest struct {
	ID                          string `json:"id"`
	ExpectedSourceKeyID         string `json:"expected_source_key_id"`
	ExpectedSourceKeyVersion    int64  `json:"expected_source_key_version"`
	ExpectedSourceKeyRowVersion int64  `json:"expected_source_key_row_version"`
	TargetKeyID                 string `json:"target_key_id"`
	TargetKeyVersion            int64  `json:"target_key_version"`
	TargetAlgorithm             string `json:"target_algorithm"`
	TargetFingerprint           string `json:"target_fingerprint"`
	IdempotencyKey              string `json:"-"`
}

// VaultKeyRotationItem contains only opaque wrapped DEKs and the exact source
// fences needed by a client to compute a replacement wrapper locally.
type VaultKeyRotationItem struct {
	RotationID               string     `json:"rotation_id"`
	SecretID                 string     `json:"secret_id"`
	FieldID                  string     `json:"field_id"`
	FieldKind                string     `json:"field_kind"`
	DEKID                    string     `json:"dek_id"`
	DEKGeneration            int64      `json:"dek_generation"`
	SourceDEKRowVersion      int64      `json:"source_dek_row_version"`
	SourceWrapRevision       int64      `json:"source_wrap_revision"`
	SourceWrappedDEK         []byte     `json:"source_wrapped_dek"`
	SourceWrapAlgorithm      string     `json:"source_wrap_algorithm"`
	SourceAADVersion         int64      `json:"source_aad_version"`
	SourceWrappingKeyID      string     `json:"source_wrapping_key_id"`
	SourceWrappingKeyVersion int64      `json:"source_wrapping_key_version"`
	TargetWrappingKeyID      string     `json:"target_wrapping_key_id"`
	TargetWrappingKeyVersion int64      `json:"target_wrapping_key_version"`
	TargetWrappedDEK         []byte     `json:"target_wrapped_dek,omitempty"`
	TargetWrapRevision       int64      `json:"target_wrap_revision,omitempty"`
	TargetWrapperSHA256      string     `json:"target_wrapper_sha256,omitempty"`
	StagedAt                 *time.Time `json:"staged_at,omitempty"`
}

// VaultKeyRotationItemListOptions controls pagination of rotation items.
type VaultKeyRotationItemListOptions struct {
	Limit  int
	Cursor string
}

// VaultKeyRotationItemPage is one page of opaque DEK-wrapper rotation work.
type VaultKeyRotationItemPage struct {
	Items      []VaultKeyRotationItem `json:"items"`
	NextCursor string                 `json:"next_cursor,omitempty"`
}

// StageVaultKeyRotationItemRequest supplies one locally rewrapped DEK and the
// source fences used to derive it.
type StageVaultKeyRotationItemRequest struct {
	DEKID                       string `json:"dek_id"`
	ExpectedSourceDEKRowVersion int64  `json:"expected_source_dek_row_version"`
	ExpectedSourceWrapRevision  int64  `json:"expected_source_wrap_revision"`
	TargetWrappedDEK            []byte `json:"target_wrapped_dek"`
	TargetWrapRevision          int64  `json:"target_wrap_revision"`
}

// StageVaultKeyRotationRequest atomically stages a bounded batch of locally
// rewrapped DEKs against an exact rotation revision.
type StageVaultKeyRotationRequest struct {
	ExpectedRotationRowVersion int64                              `json:"expected_rotation_row_version"`
	Items                      []StageVaultKeyRotationItemRequest `json:"items"`
	IdempotencyKey             string                             `json:"-"`
}

// CommitVaultKeyRotationRequest supplies the completed-plan fences and the
// client's value-free recovery disposition for an AVK rotation.
type CommitVaultKeyRotationRequest struct {
	ExpectedRotationRowVersion int64                               `json:"expected_rotation_row_version"`
	ExpectedItemCount          int64                               `json:"expected_item_count"`
	ExpectedPlanHash           string                              `json:"expected_plan_hash"`
	RecoveryDisposition        VaultKeyRotationRecoveryDisposition `json:"recovery_disposition"`
	IdempotencyKey             string                              `json:"-"`
}

const (
	// VaultKeyRotationRecoveryArtifact records that the client verified an
	// external recovery artifact before committing the rotation.
	VaultKeyRotationRecoveryArtifact = "recovery_artifact"
	// VaultKeyRotationRiskAccepted records explicit acceptance of unrecoverable
	// key-loss risk in place of an external recovery artifact.
	VaultKeyRotationRiskAccepted = "risk_accepted"
)

// VaultKeyRotationRecoveryDisposition is the value-free recovery proof or
// explicit risk choice submitted when a client commits a rotation.
type VaultKeyRotationRecoveryDisposition struct {
	Mode           string `json:"mode"`
	ArtifactSHA256 string `json:"artifact_sha256,omitempty"`
}

// CancelVaultKeyRotationRequest fences cancellation of an open rotation.
type CancelVaultKeyRotationRequest struct {
	ExpectedRotationRowVersion int64  `json:"expected_rotation_row_version"`
	IdempotencyKey             string `json:"-"`
}

// VaultKeyRotationReceipt is the durable idempotency receipt returned for a
// rotation mutation.
type VaultKeyRotationReceipt struct {
	Operation      string    `json:"operation"`
	RequestHash    string    `json:"request_hash"`
	RotationID     string    `json:"rotation_id"`
	ResultRevision int64     `json:"result_revision"`
	Replayed       bool      `json:"replayed"`
	CreatedAt      time.Time `json:"created_at"`
}

// VaultKeyRotationMutationResult combines updated lifecycle state with its
// durable mutation receipt.
type VaultKeyRotationMutationResult struct {
	Rotation VaultKeyRotation        `json:"rotation"`
	Receipt  VaultKeyRotationReceipt `json:"receipt"`
}

func startVaultKeyRotationHandler(auth PrincipalAuthFunc, start func(context.Context, DomainPrincipal, StartVaultKeyRotationRequest) (VaultKeyRotationMutationResult, error)) http.HandlerFunc {
	return secretAgentHandler(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if !secretNoQuery(w, r) {
			return
		}
		var in StartVaultKeyRotationRequest
		if !decodeSecretRequest(w, r, &in, maxVaultKeyRotationRequestBytes) ||
			!setSecretIdempotencyKey(w, r, &in.IdempotencyKey) {
			return
		}
		if !validStartVaultKeyRotationRequest(in) {
			writeJSONError(w, http.StatusBadRequest, "invalid vault key rotation request")
			return
		}
		result, err := start(r.Context(), p, in)
		if writeSecretError(w, err, "start vault key rotation") {
			return
		}
		writeSecretJSON(w, http.StatusCreated, map[string]any{
			"schema_version": "witself.v0", "rotation": result.Rotation, "receipt": result.Receipt,
		})
	})
}

func getVaultKeyRotationHandler(auth PrincipalAuthFunc, get func(context.Context, DomainPrincipal, string) (VaultKeyRotation, error)) http.HandlerFunc {
	return secretAgentSafetyHandler(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if !secretNoQuery(w, r) {
			return
		}
		rotationID, ok := secretPathID(w, r, "rotation", "vkr")
		if !ok {
			return
		}
		rotation, err := get(r.Context(), p, rotationID)
		if writeSecretError(w, err, "read vault key rotation") {
			return
		}
		writeSecretJSON(w, http.StatusOK, map[string]any{
			"schema_version": "witself.v0", "rotation": rotation,
		})
	})
}

func getOpenVaultKeyRotationHandler(auth PrincipalAuthFunc, get func(context.Context, DomainPrincipal) (*VaultKeyRotation, error)) http.HandlerFunc {
	return secretAgentSafetyHandler(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if !secretNoQuery(w, r) {
			return
		}
		rotation, err := get(r.Context(), p)
		if writeSecretError(w, err, "read open vault key rotation") {
			return
		}
		writeSecretJSON(w, http.StatusOK, map[string]any{
			"schema_version": "witself.v0", "rotation": rotation,
		})
	})
}

func listVaultKeyRotationItemsHandler(auth PrincipalAuthFunc, list func(context.Context, DomainPrincipal, string, VaultKeyRotationItemListOptions) (VaultKeyRotationItemPage, error)) http.HandlerFunc {
	return secretAgentHandler(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		rotationID, ok := secretPathID(w, r, "rotation", "vkr")
		if !ok {
			return
		}
		options, ok := parseVaultKeyRotationItemListOptions(w, r)
		if !ok {
			return
		}
		page, err := list(r.Context(), p, rotationID, options)
		if writeSecretError(w, err, "list vault key rotation items") {
			return
		}
		if page.Items == nil {
			page.Items = []VaultKeyRotationItem{}
		}
		writeSecretJSON(w, http.StatusOK, map[string]any{
			"schema_version": "witself.v0", "items": page.Items, "next_cursor": page.NextCursor,
		})
	})
}

func vaultKeyRotationActionHandler(
	auth PrincipalAuthFunc,
	stage func(context.Context, DomainPrincipal, string, StageVaultKeyRotationRequest) (VaultKeyRotationMutationResult, error),
	commit func(context.Context, DomainPrincipal, string, CommitVaultKeyRotationRequest) (VaultKeyRotationMutationResult, error),
	cancel func(context.Context, DomainPrincipal, string, CancelVaultKeyRotationRequest) (VaultKeyRotationMutationResult, error),
) http.HandlerFunc {
	return secretAgentSafetyHandler(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if !secretNoQuery(w, r) {
			return
		}
		rotationID, operation, ok := vaultKeyRotationActionPath(w, r)
		if !ok {
			return
		}
		if p.AccountStatus != "active" && operation != "cancel" {
			writeJSONError(w, http.StatusForbidden,
				fmt.Sprintf("account is %s — this action requires an active account", p.AccountStatus))
			return
		}
		switch operation {
		case "stage":
			if stage == nil {
				writeJSONError(w, http.StatusNotFound, "secret resource not found")
				return
			}
			var in StageVaultKeyRotationRequest
			if !decodeSecretRequest(w, r, &in, maxVaultKeyRotationStageBytes) ||
				!setSecretIdempotencyKey(w, r, &in.IdempotencyKey) {
				return
			}
			if !validStageVaultKeyRotationRequest(in) {
				writeJSONError(w, http.StatusBadRequest, "invalid vault key rotation request")
				return
			}
			result, err := stage(r.Context(), p, rotationID, in)
			if writeSecretError(w, err, "stage vault key rotation") {
				return
			}
			writeVaultKeyRotationMutation(w, result)
		case "commit":
			if commit == nil {
				writeJSONError(w, http.StatusNotFound, "secret resource not found")
				return
			}
			var in CommitVaultKeyRotationRequest
			if !decodeSecretRequest(w, r, &in, maxVaultKeyRotationRequestBytes) ||
				!setSecretIdempotencyKey(w, r, &in.IdempotencyKey) {
				return
			}
			if in.ExpectedRotationRowVersion < 1 || in.ExpectedItemCount < 0 ||
				!validLowerHexSHA256(in.ExpectedPlanHash) ||
				!validVaultKeyRotationRecoveryDisposition(in.RecoveryDisposition) {
				writeJSONError(w, http.StatusBadRequest, "invalid vault key rotation request")
				return
			}
			result, err := commit(r.Context(), p, rotationID, in)
			if writeSecretError(w, err, "commit vault key rotation") {
				return
			}
			writeVaultKeyRotationMutation(w, result)
		case "cancel":
			if cancel == nil {
				writeJSONError(w, http.StatusNotFound, "secret resource not found")
				return
			}
			var in CancelVaultKeyRotationRequest
			if !decodeSecretRequest(w, r, &in, maxVaultKeyRotationRequestBytes) ||
				!setSecretIdempotencyKey(w, r, &in.IdempotencyKey) {
				return
			}
			if in.ExpectedRotationRowVersion < 1 {
				writeJSONError(w, http.StatusBadRequest, "invalid vault key rotation request")
				return
			}
			result, err := cancel(r.Context(), p, rotationID, in)
			if writeSecretError(w, err, "cancel vault key rotation") {
				return
			}
			writeVaultKeyRotationMutation(w, result)
		}
	})
}

func validStartVaultKeyRotationRequest(in StartVaultKeyRotationRequest) bool {
	return strings.HasPrefix(in.ID, "vkr_") && secretResourceIDPattern.MatchString(in.ID) &&
		strings.HasPrefix(in.ExpectedSourceKeyID, "avk_") && secretResourceIDPattern.MatchString(in.ExpectedSourceKeyID) &&
		strings.HasPrefix(in.TargetKeyID, "avk_") && secretResourceIDPattern.MatchString(in.TargetKeyID) &&
		in.TargetKeyID != in.ExpectedSourceKeyID && in.ExpectedSourceKeyVersion >= 1 &&
		in.ExpectedSourceKeyVersion != int64(^uint64(0)>>1) && in.ExpectedSourceKeyRowVersion >= 1 &&
		in.TargetKeyVersion == in.ExpectedSourceKeyVersion+1 &&
		strings.TrimSpace(in.TargetAlgorithm) != "" && validLowerHexSHA256(in.TargetFingerprint)
}

func validStageVaultKeyRotationRequest(in StageVaultKeyRotationRequest) bool {
	if in.ExpectedRotationRowVersion < 1 || len(in.Items) < 1 || len(in.Items) > maxVaultKeyRotationItems {
		return false
	}
	seen := make(map[string]bool, len(in.Items))
	for _, item := range in.Items {
		if !strings.HasPrefix(item.DEKID, "dek_") || !secretResourceIDPattern.MatchString(item.DEKID) ||
			seen[item.DEKID] || item.ExpectedSourceDEKRowVersion < 1 ||
			item.ExpectedSourceWrapRevision < 1 || item.ExpectedSourceWrapRevision == int64(^uint64(0)>>1) ||
			item.TargetWrapRevision != item.ExpectedSourceWrapRevision+1 ||
			len(item.TargetWrappedDEK) != vaultKeyRotationWrappedDEKBytes {
			return false
		}
		seen[item.DEKID] = true
	}
	return true
}

func validLowerHexSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func validVaultKeyRotationRecoveryDisposition(in VaultKeyRotationRecoveryDisposition) bool {
	switch in.Mode {
	case VaultKeyRotationRecoveryArtifact:
		return validLowerHexSHA256(in.ArtifactSHA256)
	case VaultKeyRotationRiskAccepted:
		return in.ArtifactSHA256 == ""
	default:
		return false
	}
}

func vaultKeyRotationActionPath(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	action := strings.TrimSpace(r.PathValue("action"))
	for _, operation := range []string{"stage", "commit", "cancel"} {
		suffix := ":" + operation
		if !strings.HasSuffix(action, suffix) {
			continue
		}
		rotationID := strings.TrimSuffix(action, suffix)
		if strings.HasPrefix(rotationID, "vkr_") && secretResourceIDPattern.MatchString(rotationID) {
			return rotationID, operation, true
		}
		break
	}
	writeJSONError(w, http.StatusNotFound, "secret resource not found")
	return "", "", false
}

func parseVaultKeyRotationItemListOptions(w http.ResponseWriter, r *http.Request) (VaultKeyRotationItemListOptions, bool) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid vault key rotation item query")
		return VaultKeyRotationItemListOptions{}, false
	}
	for key, values := range query {
		if (key != "limit" && key != "cursor") || len(values) != 1 {
			writeJSONError(w, http.StatusBadRequest, "invalid vault key rotation item query")
			return VaultKeyRotationItemListOptions{}, false
		}
	}
	options := VaultKeyRotationItemListOptions{}
	if values, present := query["limit"]; present {
		raw := strings.TrimSpace(values[0])
		limit, err := strconv.Atoi(raw)
		if raw == "" || err != nil || limit < 1 || limit > maxVaultKeyRotationItems {
			writeJSONError(w, http.StatusBadRequest, "vault key rotation item limit must be between 1 and 100")
			return VaultKeyRotationItemListOptions{}, false
		}
		options.Limit = limit
	}
	if values, present := query["cursor"]; present {
		options.Cursor = strings.TrimSpace(values[0])
		if options.Cursor == "" || len(options.Cursor) > maxVaultKeyRotationCursorBytes {
			writeJSONError(w, http.StatusBadRequest, "invalid vault key rotation item cursor")
			return VaultKeyRotationItemListOptions{}, false
		}
	}
	return options, true
}

func writeVaultKeyRotationMutation(w http.ResponseWriter, result VaultKeyRotationMutationResult) {
	writeSecretJSON(w, http.StatusOK, map[string]any{
		"schema_version": "witself.v0", "rotation": result.Rotation, "receipt": result.Receipt,
	})
}

// secretAgentSafetyHandler preserves the normal full-agent boundary while
// allowing a route-specific harm-reducing write to reach the store for a
// suspended account. Its caller must still reject every non-safety operation.
func secretAgentSafetyHandler(auth PrincipalAuthFunc, next func(http.ResponseWriter, *http.Request, DomainPrincipal)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "private, no-store")
		token, ok := bearerToken(r)
		if !ok {
			writeJSONError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		p, ok, err := auth(r.Context(), token)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if !ok {
			writeJSONError(w, http.StatusUnauthorized, "invalid token")
			return
		}
		if effectiveAccessProfile(p) != AccessProfileFull {
			writeJSONError(w, http.StatusForbidden, "credential profile is not authorized for this route")
			return
		}
		if p.AccountStatus != "active" && p.AccountStatus != "suspended" {
			writeJSONError(w, http.StatusForbidden,
				fmt.Sprintf("account is %s — this action requires an active or suspended account", p.AccountStatus))
			return
		}
		if p.Kind != PrincipalKindAgent || strings.TrimSpace(p.ID) == "" ||
			strings.TrimSpace(p.AccountID) == "" || strings.TrimSpace(p.RealmID) == "" {
			writeJSONError(w, http.StatusForbidden, "only a full agent token may use the secret vault")
			return
		}
		next(w, r, p)
	}
}
