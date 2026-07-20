package server

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const maxVaultEnrollmentRequestBytes int64 = 16 * 1024

// VaultKeyEnrollment is the value-free API representation of a short-lived
// request to enroll another installation with the agent's current vault key.
type VaultKeyEnrollment struct {
	ID                  string     `json:"id"`
	AccountID           string     `json:"account_id"`
	RealmID             string     `json:"realm_id"`
	OwnerAgentID        string     `json:"owner_agent_id"`
	VaultKeyID          string     `json:"vault_key_id"`
	VaultKeyVersion     int64      `json:"vault_key_version"`
	VaultKeyAlgorithm   string     `json:"vault_key_algorithm"`
	VaultKeyFingerprint string     `json:"vault_key_fingerprint"`
	TargetLocationID    string     `json:"target_location_id"`
	TargetLocationName  string     `json:"target_location_name,omitempty"`
	TargetPublicKey     string     `json:"target_public_key"`
	TargetKeyAlgorithm  string     `json:"target_key_algorithm"`
	PairingCommitment   string     `json:"pairing_commitment"`
	LifecycleState      string     `json:"lifecycle_state"`
	SourceLocationID    string     `json:"source_location_id,omitempty"`
	TransferAlgorithm   string     `json:"transfer_algorithm,omitempty"`
	RowVersion          int64      `json:"row_version"`
	CreatedAt           time.Time  `json:"created_at"`
	ExpiresAt           time.Time  `json:"expires_at"`
	ApprovedAt          *time.Time `json:"approved_at,omitempty"`
	ConsumedAt          *time.Time `json:"consumed_at,omitempty"`
	CancelledAt         *time.Time `json:"cancelled_at,omitempty"`
	ExpiredAt           *time.Time `json:"expired_at,omitempty"`
}

// VaultKeyEnrollmentTransfer is the recipient-bound encrypted vault-key
// capsule returned to the target installation during enrollment.
type VaultKeyEnrollmentTransfer struct {
	Enrollment               VaultKeyEnrollment `json:"enrollment"`
	SourceEphemeralPublicKey string             `json:"source_ephemeral_public_key"`
	Ciphertext               []byte             `json:"ciphertext"`
	ConsumeCommitment        string             `json:"consume_commitment"`
}

// CreateVaultKeyEnrollmentRequest contains the target installation's public
// enrollment material and expiry policy.
type CreateVaultKeyEnrollmentRequest struct {
	ID                 string    `json:"id"`
	TargetLocationID   string    `json:"target_location_id"`
	TargetLocationName string    `json:"target_location_name,omitempty"`
	TargetPublicKey    string    `json:"target_public_key"`
	TargetKeyAlgorithm string    `json:"target_key_algorithm"`
	PairingCommitment  string    `json:"pairing_commitment"`
	ExpiresAt          time.Time `json:"expires_at"`
	IdempotencyKey     string    `json:"-"`
}

// ApproveVaultKeyEnrollmentRequest contains the encrypted transfer capsule and
// concurrency fence supplied by an installation that already holds the AVK.
type ApproveVaultKeyEnrollmentRequest struct {
	ExpectedRowVersion       int64  `json:"expected_row_version"`
	SourceLocationID         string `json:"source_location_id"`
	SourceEphemeralPublicKey string `json:"source_ephemeral_public_key"`
	TransferCiphertext       []byte `json:"transfer_ciphertext"`
	TransferAlgorithm        string `json:"transfer_algorithm"`
	ConsumeCommitment        string `json:"consume_commitment"`
	IdempotencyKey           string `json:"-"`
}

// ReceiveVaultKeyEnrollmentRequest identifies the target installation that is
// retrieving its approved transfer capsule.
type ReceiveVaultKeyEnrollmentRequest struct {
	TargetLocationID string `json:"target_location_id"`
}

// ConsumeVaultKeyEnrollmentRequest proves that the target installation
// decrypted the transfer and fences the enrollment state being consumed.
type ConsumeVaultKeyEnrollmentRequest struct {
	ExpectedRowVersion int64  `json:"expected_row_version"`
	TargetLocationID   string `json:"target_location_id"`
	ConsumeProof       []byte `json:"consume_proof"`
	IdempotencyKey     string `json:"-"`
}

// CancelVaultKeyEnrollmentRequest fences cancellation of an active enrollment.
type CancelVaultKeyEnrollmentRequest struct {
	ExpectedRowVersion int64  `json:"expected_row_version"`
	IdempotencyKey     string `json:"-"`
}

// VaultKeyEnrollmentListOptions filters and bounds enrollment list responses.
type VaultKeyEnrollmentListOptions struct {
	State string
	Limit int
}

func vaultKeyEnrollmentActionHandler(
	auth PrincipalAuthFunc,
	approve func(context.Context, DomainPrincipal, string, ApproveVaultKeyEnrollmentRequest) (VaultKeyEnrollment, error),
	receive func(context.Context, DomainPrincipal, string, string) (VaultKeyEnrollmentTransfer, error),
	consume func(context.Context, DomainPrincipal, string, ConsumeVaultKeyEnrollmentRequest) (VaultKeyEnrollment, error),
	cancel func(context.Context, DomainPrincipal, string, CancelVaultKeyEnrollmentRequest) (VaultKeyEnrollment, error),
) http.HandlerFunc {
	return secretAgentSafetyHandler(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if !secretNoQuery(w, r) {
			return
		}
		enrollmentID, operation, ok := vaultKeyEnrollmentActionPath(w, r)
		if !ok {
			return
		}
		if p.AccountStatus != "active" && operation != "cancel" {
			writeJSONError(w, http.StatusForbidden,
				"vault enrollment action requires an active account unless it is cancellation")
			return
		}
		switch operation {
		case "approve":
			if approve == nil {
				writeJSONError(w, http.StatusNotFound, "secret resource not found")
				return
			}
			var in ApproveVaultKeyEnrollmentRequest
			if !decodeSecretRequest(w, r, &in, maxVaultEnrollmentRequestBytes) || !setSecretIdempotencyKey(w, r, &in.IdempotencyKey) {
				return
			}
			value, err := approve(r.Context(), p, enrollmentID, in)
			if writeSecretError(w, err, "approve vault key enrollment") {
				return
			}
			writeSecretJSON(w, http.StatusOK, map[string]any{"schema_version": "witself.v0", "enrollment": value})
		case "receive":
			if receive == nil {
				writeJSONError(w, http.StatusNotFound, "secret resource not found")
				return
			}
			var in ReceiveVaultKeyEnrollmentRequest
			if !decodeSecretRequest(w, r, &in, maxVaultEnrollmentRequestBytes) {
				return
			}
			value, err := receive(r.Context(), p, enrollmentID, in.TargetLocationID)
			if writeSecretError(w, err, "receive vault key enrollment") {
				return
			}
			writeSecretJSON(w, http.StatusOK, map[string]any{"schema_version": "witself.v0", "transfer": value})
		case "consume":
			if consume == nil {
				writeJSONError(w, http.StatusNotFound, "secret resource not found")
				return
			}
			var in ConsumeVaultKeyEnrollmentRequest
			if !decodeSecretRequest(w, r, &in, maxVaultEnrollmentRequestBytes) || !setSecretIdempotencyKey(w, r, &in.IdempotencyKey) {
				return
			}
			value, err := consume(r.Context(), p, enrollmentID, in)
			if writeSecretError(w, err, "consume vault key enrollment") {
				return
			}
			writeSecretJSON(w, http.StatusOK, map[string]any{"schema_version": "witself.v0", "enrollment": value})
		case "cancel":
			if cancel == nil {
				writeJSONError(w, http.StatusNotFound, "secret resource not found")
				return
			}
			var in CancelVaultKeyEnrollmentRequest
			if !decodeSecretRequest(w, r, &in, maxVaultEnrollmentRequestBytes) || !setSecretIdempotencyKey(w, r, &in.IdempotencyKey) {
				return
			}
			value, err := cancel(r.Context(), p, enrollmentID, in)
			if writeSecretError(w, err, "cancel vault key enrollment") {
				return
			}
			writeSecretJSON(w, http.StatusOK, map[string]any{"schema_version": "witself.v0", "enrollment": value})
		}
	})
}

func vaultKeyEnrollmentActionPath(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	action := strings.TrimSpace(r.PathValue("action"))
	for _, operation := range []string{"approve", "receive", "consume", "cancel"} {
		suffix := ":" + operation
		if !strings.HasSuffix(action, suffix) {
			continue
		}
		id := strings.TrimSuffix(action, suffix)
		if secretResourceIDPattern.MatchString(id) && strings.HasPrefix(id, "enr_") {
			return id, operation, true
		}
		break
	}
	writeJSONError(w, http.StatusNotFound, "secret resource not found")
	return "", "", false
}

func createVaultKeyEnrollmentHandler(auth PrincipalAuthFunc, create func(context.Context, DomainPrincipal, CreateVaultKeyEnrollmentRequest) (VaultKeyEnrollment, error)) http.HandlerFunc {
	return secretAgentHandler(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if !secretNoQuery(w, r) {
			return
		}
		var in CreateVaultKeyEnrollmentRequest
		if !decodeSecretRequest(w, r, &in, maxVaultEnrollmentRequestBytes) || !setSecretIdempotencyKey(w, r, &in.IdempotencyKey) {
			return
		}
		value, err := create(r.Context(), p, in)
		if writeSecretError(w, err, "create vault key enrollment") {
			return
		}
		writeSecretJSON(w, http.StatusCreated, map[string]any{"schema_version": "witself.v0", "enrollment": value})
	})
}

func listVaultKeyEnrollmentsHandler(auth PrincipalAuthFunc, list func(context.Context, DomainPrincipal, VaultKeyEnrollmentListOptions) ([]VaultKeyEnrollment, error)) http.HandlerFunc {
	return secretAgentSafetyHandler(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		query := r.URL.Query()
		for key, values := range query {
			if (key != "state" && key != "limit") || len(values) != 1 {
				writeJSONError(w, http.StatusBadRequest, "invalid vault enrollment list query")
				return
			}
		}
		opts := VaultKeyEnrollmentListOptions{State: strings.TrimSpace(query.Get("state"))}
		if raw := strings.TrimSpace(query.Get("limit")); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil || value < 1 || value > 100 {
				writeJSONError(w, http.StatusBadRequest, "vault enrollment list limit must be between 1 and 100")
				return
			}
			opts.Limit = value
		}
		items, err := list(r.Context(), p, opts)
		if writeSecretError(w, err, "list vault key enrollments") {
			return
		}
		if items == nil {
			items = []VaultKeyEnrollment{}
		}
		writeSecretJSON(w, http.StatusOK, map[string]any{"schema_version": "witself.v0", "items": items})
	})
}

func getVaultKeyEnrollmentHandler(auth PrincipalAuthFunc, get func(context.Context, DomainPrincipal, string) (VaultKeyEnrollment, error)) http.HandlerFunc {
	return secretAgentSafetyHandler(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if !secretNoQuery(w, r) {
			return
		}
		id, ok := secretPathID(w, r, "enrollment", "enr")
		if !ok {
			return
		}
		value, err := get(r.Context(), p, id)
		if writeSecretError(w, err, "read vault key enrollment") {
			return
		}
		writeSecretJSON(w, http.StatusOK, map[string]any{"schema_version": "witself.v0", "enrollment": value})
	})
}
