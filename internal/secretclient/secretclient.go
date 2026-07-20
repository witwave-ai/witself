// Package secretclient orchestrates client-custodied sealed secrets. It is the
// only layer that combines authenticated identity, the local AVK file, remote
// ciphertext APIs, and client-side cryptography.
package secretclient

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/id"
	"github.com/witwave-ai/witself/internal/local"
	"github.com/witwave-ai/witself/internal/sealed"
)

const (
	initialValueVersion    = uint64(1)
	initialDEKGeneration   = uint64(1)
	initialWrapRevision    = uint64(1)
	maxPublicValueBytes    = 64 << 10
	maxIdempotencyBytes    = 512
	maxCreateMutationBytes = 1 << 20

	createJournalSchema    = "witself-secret-create-request-v1"
	createJournalDomain    = "witself/secret-create-journal/v1\x00"
	createJournalMACDomain = "witself/secret-create-journal-mac/v1\x00"
)

var (
	// ErrKeyUnavailable is the fail-closed backend-present/local-absent state.
	ErrKeyUnavailable = errors.New("key_unavailable")

	// ErrKeyMismatch is the fail-closed state where local and backend public AVK
	// identity differ. Neither side is overwritten.
	ErrKeyMismatch = errors.New("key_mismatch")

	// ErrInvalidConfiguration reports missing or malformed client configuration.
	ErrInvalidConfiguration = errors.New("secret client configuration is invalid")
	// ErrInvalidInput reports malformed caller-supplied secret input.
	ErrInvalidInput = errors.New("secret input is invalid")
	// ErrIdentityMismatch reports that the authenticated identity does not match
	// the local vault selectors.
	ErrIdentityMismatch = errors.New("authenticated secret identity does not match local selectors")
	// ErrIntegrity reports that encrypted material failed local verification.
	ErrIntegrity = errors.New("secret material failed integrity verification")
	// ErrOperation reports a remote or local secret operation failure whose
	// details are intentionally not exposed.
	ErrOperation = errors.New("secret operation failed")
)

var (
	fieldNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,127}$`)
	templatePattern  = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,127}$`)
	tagPattern       = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$`)
)

// FieldInput is one public or sensitive field supplied to Create. Create
// consumes and clears Value for sensitive fields before returning.
type FieldInput struct {
	Name      string
	Kind      string
	Encoding  string
	Sensitive bool
	Value     []byte
}

// CreateInput is the local plaintext-facing create shape. No instance of this
// type is sent to the backend; sensitive values are replaced by envelopes.
type CreateInput struct {
	Name           string
	Description    string
	Template       string
	Tags           []string
	Fields         []FieldInput
	IdempotencyKey string
}

// VaultKeyState is the value-free relationship between this installation and
// the authenticated agent's backend binding.
type VaultKeyState string

const (
	// VaultKeyStateAbsent means neither the local AVK nor a backend binding exists.
	VaultKeyStateAbsent VaultKeyState = "absent"
	// VaultKeyStateLocalOnly means only the local AVK exists.
	VaultKeyStateLocalOnly VaultKeyState = "local_only"
	// VaultKeyStateBackendOnly means only the backend public binding exists.
	VaultKeyStateBackendOnly VaultKeyState = "backend_only"
	// VaultKeyStateMatch means the local AVK matches the backend public binding.
	VaultKeyStateMatch VaultKeyState = "match"
	// VaultKeyStateMismatch means the local AVK and backend public binding differ.
	VaultKeyStateMismatch VaultKeyState = "mismatch"
)

// VaultKeyStatus contains public metadata only. It never contains AVK bytes.
type VaultKeyStatus struct {
	State          VaultKeyState                 `json:"state"`
	LocalPresent   bool                          `json:"local_present"`
	BackendPresent bool                          `json:"backend_present"`
	Match          bool                          `json:"match"`
	LocalMetadata  *sealed.AgentVaultKeyMetadata `json:"local_metadata,omitempty"`
	BackendBinding *client.VaultKeyBinding       `json:"backend_binding,omitempty"`
}

// Service binds one token-authenticated remote client to canonical local path
// selectors. AccountName is a local alias; realm and agent names must match the
// authenticated self identity before any local AVK path is used.
type Service struct {
	remote      remote
	accountID   string
	accountName string
	realmName   string
	agentName   string
}

// Config pins the local vault selectors to the immutable account identity
// already resolved by the configured client. AccountID is required even
// though AccountName remains the local path alias: a token for a same-named
// agent in another account must fail before any local key path is read.
type Config struct {
	Endpoint    string
	Token       string
	AccountID   string
	AccountName string
	RealmName   string
	AgentName   string
}

// New creates a client-side sealed-secret service. It performs no network or
// key operation. Callers must first resolve AccountID from trusted local
// configuration or the authenticated self identity.
func New(config Config) (*Service, error) {
	config.Endpoint = strings.TrimSpace(config.Endpoint)
	config.AccountID = strings.TrimSpace(config.AccountID)
	if config.Endpoint == "" || strings.TrimSpace(config.Token) == "" ||
		!validGeneratedID(config.AccountID, "acc") {
		return nil, ErrInvalidConfiguration
	}
	if _, err := local.AgentVaultKeyPath(config.AccountName, config.RealmName, config.AgentName); err != nil {
		return nil, ErrInvalidConfiguration
	}
	return &Service{
		remote:      httpRemote{endpoint: config.Endpoint, token: config.Token},
		accountID:   config.AccountID,
		accountName: config.AccountName,
		realmName:   config.RealmName,
		agentName:   config.AgentName,
	}, nil
}

// VaultKeyStatus reads local and backend state without generating, registering,
// or replacing a key.
func (s *Service) VaultKeyStatus(ctx context.Context) (VaultKeyStatus, error) {
	identity, err := s.identity(ctx)
	if err != nil {
		return VaultKeyStatus{}, err
	}
	observation, err := s.observeVaultKey(ctx, identity)
	if err != nil {
		return VaultKeyStatus{}, err
	}
	defer observation.local.Clear()
	return observation.status, nil
}

// ReconcileVaultKey implements the fail-closed AVK bootstrap state machine. It
// generates only after authenticated backend state and the local file are both
// confirmed absent; there is deliberately no load-or-generate helper. The
// caller owns the returned in-memory key and should defer Clear immediately.
func (s *Service) ReconcileVaultKey(ctx context.Context) (*sealed.AgentVaultKey, error) {
	identity, err := s.identity(ctx)
	if err != nil {
		return nil, err
	}
	return s.reconcileVaultKey(ctx, identity)
}

// Create generates client-owned secret and field ids, encrypts every sensitive
// value locally, and sends only public values plus sealed envelopes.
func (s *Service) Create(ctx context.Context, in CreateInput) (*client.SecretMutationResult, error) {
	defer clearSensitiveInputs(in.Fields)
	normalized, err := normalizeCreateInput(in)
	if err != nil {
		return nil, err
	}
	idempotencyKey, err := operationKey(normalized.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	identity, err := s.identity(ctx)
	if err != nil {
		return nil, err
	}
	journalHash := createJournalHash(identity, idempotencyKey)
	request, found, err := s.readCreateJournal(identity, journalHash, idempotencyKey)
	if err != nil {
		return nil, err
	}

	if !found {
		var avk *sealed.AgentVaultKey
		if hasSensitiveFields(normalized.Fields) {
			avk, err = s.reconcileVaultKey(ctx, identity)
			if err != nil {
				return nil, err
			}
			defer avk.Clear()
		}
		request, err = buildCreateRequest(normalized, identity, avk)
		if err != nil {
			return nil, err
		}
		request, err = s.publishCreateJournal(identity, journalHash, idempotencyKey, request)
		if err != nil {
			return nil, err
		}
	}
	return s.submitJournaledCreate(ctx, identity, journalHash, idempotencyKey, request)
}

// submitJournaledCreate always authenticates and submits the exact durable
// request before considering an AVK rebase. The store performs idempotency
// receipt replay before its current-key check, so only the stable typed
// vault-key-mismatch response proves that this exact request did not commit.
func (s *Service) submitJournaledCreate(ctx context.Context, identity client.SelfIdentity, journalHash, idempotencyKey string, request client.CreateSecretInput) (*client.SecretMutationResult, error) {
	if !createRequestHasSensitiveFields(request) {
		result, err := s.sendCreateRequest(ctx, identity, idempotencyKey, request)
		if errors.Is(err, ErrIdentityMismatch) {
			return nil, err
		}
		if err != nil {
			return nil, ErrOperation
		}
		return result, nil
	}

	source, err := s.loadCreateRequestVaultKey(request)
	if err != nil {
		return nil, err
	}
	defer source.Clear()
	if !authenticateCreateRequest(request, identity, source) {
		return nil, ErrIntegrity
	}

	result, err := s.sendCreateRequest(ctx, identity, idempotencyKey, request)
	if err == nil {
		return result, nil
	}
	if errors.Is(err, ErrIdentityMismatch) {
		return nil, err
	}
	if !errors.Is(err, client.ErrSecretVaultKeyMismatch) {
		// Another local process may have won the authenticated CAS and committed
		// its exact replacement while this process's now-stale request was in
		// flight. An untyped error never authorizes mutation, but an already
		// advanced durable journal is safe to authenticate and retry once.
		if recovered, advanced, recoverErr := s.retryAdvancedCreateJournal(
			ctx, identity, journalHash, idempotencyKey, request,
		); advanced || recoverErr != nil {
			return recovered, recoverErr
		}
		return nil, ErrOperation
	}

	// A machine-coded key mismatch is emitted only after the backend has
	// serialized this idempotency key and found no matching receipt. Reconcile
	// the authenticated current binding now; no transport or generic conflict
	// is allowed to reach journal replacement.
	target, err := s.reconcileVaultKey(ctx, identity)
	if err != nil {
		return nil, err
	}
	defer target.Clear()
	if target.ID() == source.ID() || target.Version() <= source.Version() {
		return nil, ErrIntegrity
	}
	rebased, err := rewrapUncommittedCreateRequest(request, identity, source, target)
	if err != nil {
		return nil, err
	}
	if !sameCreateRequestLogicalValue(request, rebased) || !authenticateCreateRequest(rebased, identity, target) {
		return nil, ErrIntegrity
	}

	winner, err := s.replaceCreateJournalAfterVaultKeyAdvance(
		identity, journalHash, idempotencyKey, request, rebased,
	)
	if err != nil {
		return nil, err
	}
	if !sameCreateRequestLogicalValue(request, winner) {
		return nil, ErrIntegrity
	}
	winnerKey, err := s.loadCreateRequestVaultKey(winner)
	if err != nil {
		return nil, err
	}
	defer winnerKey.Clear()
	if winnerKey.Version() <= source.Version() || !authenticateCreateRequest(winner, identity, winnerKey) {
		return nil, ErrIntegrity
	}
	result, err = s.sendCreateRequest(ctx, identity, idempotencyKey, winner)
	if errors.Is(err, ErrIdentityMismatch) {
		return nil, err
	}
	if err != nil {
		return nil, ErrOperation
	}
	return result, nil
}

func (s *Service) retryAdvancedCreateJournal(ctx context.Context, identity client.SelfIdentity, journalHash, idempotencyKey string, stale client.CreateSecretInput) (*client.SecretMutationResult, bool, error) {
	current, found, err := s.readCreateJournal(identity, journalHash, idempotencyKey)
	if err != nil {
		return nil, true, err
	}
	if !found {
		return nil, true, ErrOperation
	}
	if sameCreateRequestExact(stale, current) {
		return nil, false, nil
	}
	if !sameCreateRequestLogicalValue(stale, current) {
		return nil, true, ErrIntegrity
	}
	key, err := s.loadCreateRequestVaultKey(current)
	if err != nil {
		return nil, true, err
	}
	defer key.Clear()
	if !authenticateCreateRequest(current, identity, key) {
		return nil, true, ErrIntegrity
	}
	result, err := s.sendCreateRequest(ctx, identity, idempotencyKey, current)
	if errors.Is(err, ErrIdentityMismatch) {
		return nil, true, err
	}
	if err != nil {
		return nil, true, ErrOperation
	}
	return result, true, nil
}

func (s *Service) sendCreateRequest(ctx context.Context, identity client.SelfIdentity, idempotencyKey string, request client.CreateSecretInput) (*client.SecretMutationResult, error) {
	wire := cloneCreateRequest(request)
	wire.IdempotencyKey = idempotencyKey
	result, err := s.remote.createSecret(ctx, wire)
	if err != nil {
		return nil, err
	}
	if result == nil || !secretMatchesIdentity(result.Secret, identity) || result.Secret.ID != request.ID {
		return nil, ErrIdentityMismatch
	}
	redactSecret(&result.Secret)
	return result, nil
}

func buildCreateRequest(normalized CreateInput, identity client.SelfIdentity, avk *sealed.AgentVaultKey) (client.CreateSecretInput, error) {
	secretID, err := id.New("sec")
	if err != nil {
		return client.CreateSecretInput{}, ErrOperation
	}
	fields := make([]client.CreateSecretFieldInput, 0, len(normalized.Fields))
	for _, field := range normalized.Fields {
		fieldID, err := id.New("fld")
		if err != nil {
			return client.CreateSecretInput{}, ErrOperation
		}
		out := client.CreateSecretFieldInput{
			ID:           fieldID,
			Name:         field.Name,
			Kind:         field.Kind,
			Sensitive:    field.Sensitive,
			Encoding:     field.Encoding,
			ValueVersion: int64(initialValueVersion),
		}
		if !field.Sensitive {
			value := string(field.Value)
			out.PublicValue = &value
			fields = append(fields, out)
			continue
		}
		domain, ok := fieldDomain(field.Kind)
		if !ok {
			return client.CreateSecretInput{}, ErrInvalidInput
		}
		envelope, err := sealed.SealSensitiveField(avk, field.Value, sealed.SensitiveFieldOptions{
			Scope: sealed.FieldScope{
				Domain:       domain,
				AccountID:    identity.AccountID,
				RealmID:      identity.RealmID,
				OwnerAgentID: identity.AgentID,
				SecretID:     secretID,
				FieldID:      fieldID,
			},
			ValueVersion:  initialValueVersion,
			DEKGeneration: initialDEKGeneration,
			ValueEncoding: field.Encoding,
			WrapRevision:  initialWrapRevision,
		})
		if err != nil {
			return client.CreateSecretInput{}, ErrInvalidInput
		}
		sealedField, err := toClientSealedField(envelope)
		if err != nil {
			return client.CreateSecretInput{}, err
		}
		out.Sealed = &sealedField
		fields = append(fields, out)
	}
	return client.CreateSecretInput{
		ID:          secretID,
		Name:        normalized.Name,
		Description: normalized.Description,
		Template:    normalized.Template,
		Tags:        append([]string(nil), normalized.Tags...),
		Fields:      fields,
	}, nil
}

type createJournalRecord struct {
	SchemaVersion string                   `json:"schema_version"`
	OperationHash string                   `json:"operation_hash"`
	RequestMAC    string                   `json:"request_mac"`
	AccountID     string                   `json:"account_id"`
	RealmID       string                   `json:"realm_id"`
	OwnerAgentID  string                   `json:"owner_agent_id"`
	Request       client.CreateSecretInput `json:"request"`
}

func (s *Service) readCreateJournal(identity client.SelfIdentity, operationHash, idempotencyKey string) (client.CreateSecretInput, bool, error) {
	raw, err := local.ReadSecretCreateJournal(s.accountName, s.realmName, s.agentName, operationHash)
	if errors.Is(err, local.ErrSecretCreateJournalUnavailable) {
		return client.CreateSecretInput{}, false, nil
	}
	if errors.Is(err, local.ErrSecretCreateJournalInvalid) {
		return client.CreateSecretInput{}, false, ErrIntegrity
	}
	if err != nil {
		return client.CreateSecretInput{}, false, ErrOperation
	}
	defer clear(raw)
	request, err := parseCreateJournal(raw, identity, operationHash, idempotencyKey)
	if err != nil {
		return client.CreateSecretInput{}, false, err
	}
	return request, true, nil
}

func (s *Service) publishCreateJournal(identity client.SelfIdentity, operationHash, idempotencyKey string, request client.CreateSecretInput) (client.CreateSecretInput, error) {
	raw, err := marshalCreateJournal(identity, operationHash, idempotencyKey, request)
	if err != nil {
		return client.CreateSecretInput{}, err
	}
	defer clear(raw)
	if err := local.CreateSecretCreateJournal(s.accountName, s.realmName, s.agentName, operationHash, raw); err != nil {
		if !errors.Is(err, local.ErrSecretCreateJournalExists) {
			return client.CreateSecretInput{}, ErrOperation
		}
		winner, found, readErr := s.readCreateJournal(identity, operationHash, idempotencyKey)
		if readErr != nil {
			return client.CreateSecretInput{}, readErr
		}
		if !found {
			return client.CreateSecretInput{}, ErrOperation
		}
		return winner, nil
	}
	return request, nil
}

func (s *Service) replaceCreateJournalAfterVaultKeyAdvance(identity client.SelfIdentity, operationHash, idempotencyKey string, expected, replacement client.CreateSecretInput) (client.CreateSecretInput, error) {
	expectedRaw, err := marshalCreateJournal(identity, operationHash, idempotencyKey, expected)
	if err != nil {
		return client.CreateSecretInput{}, err
	}
	defer clear(expectedRaw)
	replacementRaw, err := marshalCreateJournal(identity, operationHash, idempotencyKey, replacement)
	if err != nil {
		return client.CreateSecretInput{}, err
	}
	defer clear(replacementRaw)
	err = local.ReplaceSecretCreateJournalAfterVaultKeyAdvance(
		s.accountName, s.realmName, s.agentName, operationHash, expectedRaw, replacementRaw,
	)
	if err == nil {
		return replacement, nil
	}
	if !errors.Is(err, local.ErrSecretCreateJournalConflict) {
		if errors.Is(err, local.ErrSecretCreateJournalInvalid) {
			return client.CreateSecretInput{}, ErrIntegrity
		}
		return client.CreateSecretInput{}, ErrOperation
	}
	// A cooperating contender won the exact CAS. Parse and authenticate its
	// durable bytes rather than sending this process's losing random wrapper.
	winner, found, readErr := s.readCreateJournal(identity, operationHash, idempotencyKey)
	if readErr != nil {
		return client.CreateSecretInput{}, readErr
	}
	if !found {
		return client.CreateSecretInput{}, ErrOperation
	}
	return winner, nil
}

func marshalCreateJournal(identity client.SelfIdentity, operationHash, idempotencyKey string, request client.CreateSecretInput) ([]byte, error) {
	if !validCachedCreateRequest(request) {
		return nil, ErrOperation
	}
	requestMAC, err := createRequestMAC(identity, operationHash, idempotencyKey, request)
	if err != nil {
		return nil, ErrOperation
	}
	record := createJournalRecord{
		SchemaVersion: createJournalSchema,
		OperationHash: operationHash,
		RequestMAC:    requestMAC,
		AccountID:     identity.AccountID,
		RealmID:       identity.RealmID,
		OwnerAgentID:  identity.AgentID,
		Request:       request,
	}
	raw, err := json.Marshal(record)
	if err != nil || len(raw) == 0 || len(raw) > local.MaxSecretCreateJournalBytes {
		clear(raw)
		return nil, ErrOperation
	}
	return raw, nil
}

func parseCreateJournal(raw []byte, identity client.SelfIdentity, operationHash, idempotencyKey string) (client.CreateSecretInput, error) {
	var record createJournalRecord
	if len(raw) == 0 || json.Unmarshal(raw, &record) != nil {
		return client.CreateSecretInput{}, ErrIntegrity
	}
	canonical, err := json.Marshal(record)
	if err != nil {
		return client.CreateSecretInput{}, ErrIntegrity
	}
	canonicalMatch := bytes.Equal(raw, canonical)
	clear(canonical)
	if !canonicalMatch || record.SchemaVersion != createJournalSchema || record.OperationHash != operationHash ||
		record.AccountID != identity.AccountID || record.RealmID != identity.RealmID ||
		record.OwnerAgentID != identity.AgentID || !validCachedCreateRequest(record.Request) {
		return client.CreateSecretInput{}, ErrIntegrity
	}
	wantMAC, err := createRequestMAC(identity, operationHash, idempotencyKey, record.Request)
	if err != nil || !hmac.Equal([]byte(record.RequestMAC), []byte(wantMAC)) {
		return client.CreateSecretInput{}, ErrIntegrity
	}
	return record.Request, nil
}

func createJournalHash(identity client.SelfIdentity, idempotencyKey string) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte(createJournalDomain))
	for _, value := range []string{identity.AccountID, identity.RealmID, identity.AgentID, idempotencyKey} {
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(value)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write([]byte(value))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func createRequestMAC(identity client.SelfIdentity, operationHash, idempotencyKey string, request client.CreateSecretInput) (string, error) {
	raw, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	defer clear(raw)
	key := []byte(idempotencyKey)
	defer clear(key)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(createJournalMACDomain))
	for _, value := range []string{identity.AccountID, identity.RealmID, identity.AgentID, operationHash} {
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(value)))
		_, _ = mac.Write(length[:])
		_, _ = mac.Write([]byte(value))
	}
	_, _ = mac.Write(raw)
	return hex.EncodeToString(mac.Sum(nil)), nil
}

func validCachedCreateRequest(in client.CreateSecretInput) bool {
	if in.IdempotencyKey != "" || !validGeneratedID(in.ID, "sec") || in.Name == "" ||
		in.Name != strings.TrimSpace(in.Name) || len(in.Name) > 256 || strings.ContainsAny(in.Name, "\x00\r\n") ||
		!utf8.ValidString(in.Name) || in.Description != strings.TrimSpace(in.Description) ||
		len(in.Description) > 4096 || strings.ContainsRune(in.Description, '\x00') || !utf8.ValidString(in.Description) ||
		!templatePattern.MatchString(in.Template) || len(in.Tags) > 64 || len(in.Fields) < 1 || len(in.Fields) > 64 {
		return false
	}
	for index, tag := range in.Tags {
		if !tagPattern.MatchString(tag) || (index > 0 && in.Tags[index-1] >= tag) {
			return false
		}
	}
	fieldNames := make(map[string]bool, len(in.Fields))
	fieldIDs := make(map[string]bool, len(in.Fields))
	dekIDs := make(map[string]bool, len(in.Fields))
	totalBytes := len(in.Name) + len(in.Description) + len(in.Template)
	wrappingKeyID := ""
	var wrappingKeyVersion int64
	for _, field := range in.Fields {
		if !validGeneratedID(field.ID, "fld") || fieldIDs[field.ID] || !fieldNamePattern.MatchString(field.Name) ||
			fieldNames[field.Name] || !validFieldKind(field.Kind) || field.ValueVersion != int64(initialValueVersion) ||
			(fieldKindRequiresProtection(field.Kind) && !field.Sensitive) {
			return false
		}
		fieldIDs[field.ID] = true
		fieldNames[field.Name] = true
		if !field.Sensitive {
			if field.Encoding != sealed.ValueEncodingUTF8 || field.PublicValue == nil || field.Sealed != nil ||
				len(*field.PublicValue) > maxPublicValueBytes || strings.ContainsRune(*field.PublicValue, '\x00') ||
				!utf8.ValidString(*field.PublicValue) {
				return false
			}
			totalBytes += len(*field.PublicValue)
			continue
		}
		if field.PublicValue != nil || field.Sealed == nil || !validSensitiveEncoding(field.Encoding) {
			return false
		}
		value := field.Sealed
		if value.EnvelopeVersion != int64(sealed.EnvelopeVersionV1) ||
			len(value.Ciphertext) < sealed.MinSensitiveCiphertextBytes || len(value.Ciphertext) > sealed.MaxSensitiveCiphertextBytes ||
			value.Algorithm != sealed.AES256GCMAlgorithm || value.AADVersion != int64(sealed.AADVersionV1) ||
			!validGeneratedID(value.DEK.ID, "dek") || dekIDs[value.DEK.ID] || value.DEK.Generation != int64(initialDEKGeneration) ||
			len(value.DEK.WrappedDEK) != sealed.WrappedDEKBytes || value.DEK.WrapAlgorithm != sealed.AES256GCMAlgorithm ||
			value.DEK.AADVersion != int64(sealed.AADVersionV1) || value.DEK.WrapRevision < int64(initialWrapRevision) ||
			!validGeneratedID(value.DEK.WrappingKeyID, "avk") || value.DEK.WrappingKeyVersion < 1 {
			return false
		}
		dekIDs[value.DEK.ID] = true
		if wrappingKeyID == "" {
			wrappingKeyID = value.DEK.WrappingKeyID
			wrappingKeyVersion = value.DEK.WrappingKeyVersion
		} else if wrappingKeyID != value.DEK.WrappingKeyID || wrappingKeyVersion != value.DEK.WrappingKeyVersion {
			return false
		}
		totalBytes += len(value.Ciphertext) + len(value.DEK.WrappedDEK)
	}
	return totalBytes <= maxCreateMutationBytes
}

func createRequestHasSensitiveFields(in client.CreateSecretInput) bool {
	for _, field := range in.Fields {
		if field.Sensitive {
			return true
		}
	}
	return false
}

func authenticateCreateRequest(in client.CreateSecretInput, identity client.SelfIdentity, avk *sealed.AgentVaultKey) bool {
	if avk == nil || avk.Version() > uint64(1<<63-1) {
		return false
	}
	for _, field := range in.Fields {
		if !field.Sensitive || field.Sealed == nil {
			continue
		}
		domain, ok := fieldDomain(field.Kind)
		if !ok {
			return false
		}
		envelope, err := fromClientSecretMaterial(client.SecretMaterial{
			Encoding:        field.Encoding,
			ValueVersion:    field.ValueVersion,
			EnvelopeVersion: field.Sealed.EnvelopeVersion,
			Ciphertext:      field.Sealed.Ciphertext,
			Algorithm:       field.Sealed.Algorithm,
			AADVersion:      field.Sealed.AADVersion,
			DEK:             field.Sealed.DEK,
		})
		if err != nil {
			return false
		}
		plaintext, err := sealed.OpenSensitiveField(avk, sealed.FieldScope{
			Domain:       domain,
			AccountID:    identity.AccountID,
			RealmID:      identity.RealmID,
			OwnerAgentID: identity.AgentID,
			SecretID:     in.ID,
			FieldID:      field.ID,
		}, envelope)
		if err != nil {
			clear(plaintext)
			return false
		}
		clear(plaintext)
	}
	return true
}

func (s *Service) loadCreateRequestVaultKey(in client.CreateSecretInput) (*sealed.AgentVaultKey, error) {
	keyID, keyVersion, ok := createRequestWrappingEpoch(in)
	if !ok {
		return nil, ErrIntegrity
	}
	key, err := local.ReadAgentVaultKeyEpoch(
		s.accountName, s.realmName, s.agentName, keyID, keyVersion,
	)
	if errors.Is(err, local.ErrAgentVaultKeyUnavailable) {
		return nil, ErrKeyUnavailable
	}
	if err != nil {
		return nil, ErrOperation
	}
	if key.ID() != keyID || key.Version() != keyVersion {
		key.Clear()
		return nil, ErrIntegrity
	}
	return key, nil
}

func createRequestWrappingEpoch(in client.CreateSecretInput) (string, uint64, bool) {
	if !validCachedCreateRequest(in) || !createRequestHasSensitiveFields(in) {
		return "", 0, false
	}
	for _, field := range in.Fields {
		if !field.Sensitive || field.Sealed == nil {
			continue
		}
		return field.Sealed.DEK.WrappingKeyID, uint64(field.Sealed.DEK.WrappingKeyVersion), true
	}
	return "", 0, false
}

// rewrapUncommittedCreateRequest changes only each DEK's AVK wrapper. The
// value ciphertext, generated ids, logical metadata, and therefore any
// one-time generated password remain byte-for-byte unchanged.
func rewrapUncommittedCreateRequest(in client.CreateSecretInput, identity client.SelfIdentity, source, target *sealed.AgentVaultKey) (client.CreateSecretInput, error) {
	if source == nil || target == nil || target.ID() == source.ID() || target.Version() <= source.Version() {
		return client.CreateSecretInput{}, ErrIntegrity
	}
	out := cloneCreateRequest(in)
	for index := range out.Fields {
		field := &out.Fields[index]
		if !field.Sensitive || field.Sealed == nil {
			continue
		}
		domain, ok := fieldDomain(field.Kind)
		if !ok {
			return client.CreateSecretInput{}, ErrIntegrity
		}
		envelope, err := fromClientSecretMaterial(client.SecretMaterial{
			Encoding: field.Encoding, ValueVersion: field.ValueVersion,
			EnvelopeVersion: field.Sealed.EnvelopeVersion,
			Ciphertext:      field.Sealed.Ciphertext,
			Algorithm:       field.Sealed.Algorithm,
			AADVersion:      field.Sealed.AADVersion,
			DEK:             field.Sealed.DEK,
		})
		if err != nil {
			return client.CreateSecretInput{}, ErrIntegrity
		}
		if envelope.WrapRevision >= uint64(1<<63-1) {
			return client.CreateSecretInput{}, ErrOperation
		}
		rewrapped, err := sealed.RewrapSensitiveFieldDEK(source, target, sealed.FieldScope{
			Domain: domain, AccountID: identity.AccountID, RealmID: identity.RealmID,
			OwnerAgentID: identity.AgentID, SecretID: in.ID, FieldID: field.ID,
		}, envelope, envelope.WrapRevision+1)
		if err != nil {
			return client.CreateSecretInput{}, ErrIntegrity
		}
		sealedField, err := toClientSealedField(rewrapped)
		if err != nil {
			return client.CreateSecretInput{}, err
		}
		field.Sealed = &sealedField
	}
	if !validCachedCreateRequest(out) || !sameCreateRequestLogicalValue(in, out) {
		return client.CreateSecretInput{}, ErrIntegrity
	}
	return out, nil
}

func cloneCreateRequest(in client.CreateSecretInput) client.CreateSecretInput {
	out := in
	out.Tags = append([]string(nil), in.Tags...)
	out.Fields = make([]client.CreateSecretFieldInput, len(in.Fields))
	for index := range in.Fields {
		out.Fields[index] = in.Fields[index]
		if in.Fields[index].PublicValue != nil {
			value := *in.Fields[index].PublicValue
			out.Fields[index].PublicValue = &value
		}
		if in.Fields[index].Sealed != nil {
			value := *in.Fields[index].Sealed
			value.Ciphertext = append([]byte(nil), value.Ciphertext...)
			value.DEK.WrappedDEK = append([]byte(nil), value.DEK.WrappedDEK...)
			out.Fields[index].Sealed = &value
		}
	}
	return out
}

func sameCreateRequestLogicalValue(a, b client.CreateSecretInput) bool {
	if a.ID != b.ID || a.Name != b.Name || a.Description != b.Description || a.Template != b.Template ||
		a.IdempotencyKey != b.IdempotencyKey || len(a.Tags) != len(b.Tags) || len(a.Fields) != len(b.Fields) {
		return false
	}
	for index := range a.Tags {
		if a.Tags[index] != b.Tags[index] {
			return false
		}
	}
	for index := range a.Fields {
		left, right := a.Fields[index], b.Fields[index]
		if left.ID != right.ID || left.Name != right.Name || left.Kind != right.Kind ||
			left.Sensitive != right.Sensitive || left.Encoding != right.Encoding ||
			left.ValueVersion != right.ValueVersion || (left.PublicValue == nil) != (right.PublicValue == nil) ||
			(left.Sealed == nil) != (right.Sealed == nil) {
			return false
		}
		if left.PublicValue != nil && *left.PublicValue != *right.PublicValue {
			return false
		}
		if left.Sealed == nil {
			continue
		}
		ls, rs := left.Sealed, right.Sealed
		if ls.EnvelopeVersion != rs.EnvelopeVersion || !bytes.Equal(ls.Ciphertext, rs.Ciphertext) ||
			ls.Algorithm != rs.Algorithm || ls.AADVersion != rs.AADVersion ||
			ls.DEK.ID != rs.DEK.ID || ls.DEK.Generation != rs.DEK.Generation ||
			ls.DEK.WrapAlgorithm != rs.DEK.WrapAlgorithm || ls.DEK.AADVersion != rs.DEK.AADVersion {
			return false
		}
	}
	return true
}

func sameCreateRequestExact(a, b client.CreateSecretInput) bool {
	a.IdempotencyKey = ""
	b.IdempotencyKey = ""
	left, leftErr := json.Marshal(a)
	right, rightErr := json.Marshal(b)
	defer clear(left)
	defer clear(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(left, right)
}

// List returns public metadata and explicitly public values only. It never
// reads, creates, or reconciles an AVK.
func (s *Service) List(ctx context.Context, options client.SecretListOptions) (*client.SecretPage, error) {
	identity, err := s.identity(ctx)
	if err != nil {
		return nil, err
	}
	page, err := s.remote.listSecrets(ctx, options)
	if err != nil || page == nil {
		return nil, ErrOperation
	}
	if page.Items == nil {
		page.Items = []client.Secret{}
	}
	for index := range page.Items {
		if !secretMatchesIdentity(page.Items[index], identity) {
			return nil, ErrIdentityMismatch
		}
		redactSecret(&page.Items[index])
	}
	return page, nil
}

// Get returns redacted detail and public values only. It never reads, creates,
// or reconciles an AVK.
func (s *Service) Get(ctx context.Context, secretID string) (*client.Secret, error) {
	secretID = strings.TrimSpace(secretID)
	if !validGeneratedID(secretID, "sec") {
		return nil, ErrInvalidInput
	}
	identity, err := s.identity(ctx)
	if err != nil {
		return nil, err
	}
	value, err := s.remote.getSecret(ctx, secretID)
	if err != nil || value == nil {
		return nil, ErrOperation
	}
	if !secretMatchesIdentity(*value, identity) || value.ID != secretID {
		return nil, ErrIdentityMismatch
	}
	redactSecret(value)
	return value, nil
}

// RevealField retrieves exactly one encrypted package and decrypts it locally.
// The caller owns the returned plaintext buffer and must clear it after use.
func (s *Service) RevealField(ctx context.Context, secretID, fieldID, idempotencyKey string) ([]byte, error) {
	secretID = strings.TrimSpace(secretID)
	fieldID = strings.TrimSpace(fieldID)
	if !validGeneratedID(secretID, "sec") || !validGeneratedID(fieldID, "fld") {
		return nil, ErrInvalidInput
	}
	idempotencyKey, err := operationKey(idempotencyKey)
	if err != nil {
		return nil, err
	}
	identity, err := s.identity(ctx)
	if err != nil {
		return nil, err
	}
	avk, err := s.reconcileVaultKey(ctx, identity)
	if err != nil {
		return nil, err
	}
	defer avk.Clear()
	material, err := s.remote.accessSecretField(ctx, secretID, fieldID, idempotencyKey)
	if err != nil || material == nil {
		return nil, ErrOperation
	}
	if material.SecretID != secretID || material.FieldID != fieldID {
		return nil, ErrIntegrity
	}
	domain, ok := fieldDomain(material.FieldKind)
	if !ok {
		return nil, ErrIntegrity
	}
	envelope, err := fromClientSecretMaterial(*material)
	if err != nil {
		return nil, ErrIntegrity
	}
	plaintext, err := sealed.OpenSensitiveField(avk, sealed.FieldScope{
		Domain:       domain,
		AccountID:    identity.AccountID,
		RealmID:      identity.RealmID,
		OwnerAgentID: identity.AgentID,
		SecretID:     secretID,
		FieldID:      fieldID,
	}, envelope)
	if err != nil {
		clear(plaintext)
		return nil, ErrIntegrity
	}
	return plaintext, nil
}

type vaultObservation struct {
	local   *sealed.AgentVaultKey
	backend *client.VaultKeyBinding
	status  VaultKeyStatus
}

func (s *Service) observeVaultKey(ctx context.Context, identity client.SelfIdentity) (vaultObservation, error) {
	// Read the authenticated binding first. Local absence is never generation
	// authority unless the backend has already confirmed its side is absent.
	backend, err := s.remote.currentVaultKey(ctx)
	if err != nil {
		return vaultObservation{}, ErrOperation
	}
	if backend != nil && !vaultBindingMatchesIdentity(backend, identity) {
		return vaultObservation{}, ErrIdentityMismatch
	}
	var localKey *sealed.AgentVaultKey
	if backend != nil {
		if backend.KeyVersion < 1 {
			return vaultObservation{}, ErrOperation
		}
		localKey, err = local.ReadAgentVaultKeyEpoch(
			s.accountName, s.realmName, s.agentName, backend.ID, uint64(backend.KeyVersion),
		)
		if errors.Is(err, local.ErrAgentVaultKeyUnavailable) {
			// Preserve mismatch detection for an unrelated discoverable legacy
			// bootstrap key while still preferring an exact immutable epoch.
			localKey, err = local.ReadAgentVaultKey(s.accountName, s.realmName, s.agentName)
		}
	} else {
		// Until a backend epoch exists, the legacy location remains the only
		// compatible local-only state that may be registered.
		localKey, err = local.ReadAgentVaultKey(s.accountName, s.realmName, s.agentName)
	}
	if errors.Is(err, local.ErrAgentVaultKeyUnavailable) {
		localKey = nil
	} else if err != nil {
		return vaultObservation{}, ErrOperation
	}
	status := VaultKeyStatus{LocalPresent: localKey != nil, BackendPresent: backend != nil}
	if localKey != nil {
		metadata := localKey.Metadata()
		status.LocalMetadata = &metadata
	}
	if backend != nil {
		binding := *backend
		status.BackendBinding = &binding
	}
	switch {
	case localKey == nil && backend == nil:
		status.State = VaultKeyStateAbsent
	case localKey != nil && backend == nil:
		status.State = VaultKeyStateLocalOnly
	case localKey == nil && backend != nil:
		status.State = VaultKeyStateBackendOnly
	default:
		status.Match = vaultKeyMatches(localKey, backend, identity)
		if status.Match {
			status.State = VaultKeyStateMatch
		} else {
			status.State = VaultKeyStateMismatch
		}
	}
	return vaultObservation{local: localKey, backend: backend, status: status}, nil
}

func (s *Service) reconcileVaultKey(ctx context.Context, identity client.SelfIdentity) (*sealed.AgentVaultKey, error) {
	observation, err := s.observeVaultKey(ctx, identity)
	if err != nil {
		return nil, err
	}
	switch observation.status.State {
	case VaultKeyStateMatch:
		if err := s.publishVaultKeyEpoch(observation.local); err != nil {
			observation.local.Clear()
			return nil, err
		}
		return observation.local, nil
	case VaultKeyStateBackendOnly:
		observation.local.Clear()
		return nil, ErrKeyUnavailable
	case VaultKeyStateMismatch:
		observation.local.Clear()
		return nil, ErrKeyMismatch
	case VaultKeyStateLocalOnly:
		key, err := s.registerLocalVaultKey(ctx, identity, observation.local)
		if err != nil {
			observation.local.Clear()
			return nil, err
		}
		if err := s.publishVaultKeyEpoch(key); err != nil {
			key.Clear()
			return nil, err
		}
		return key, nil
	case VaultKeyStateAbsent:
		key, err := sealed.GenerateAgentVaultKey(sealed.InitialAgentVaultKeyVersion)
		if err != nil {
			return nil, ErrOperation
		}
		// The legacy/current path is the crash-discoverable staging pointer while
		// no backend binding exists. Never publish only an undiscoverable epoch
		// before registration has resolved.
		if err := local.CreateAgentVaultKey(s.accountName, s.realmName, s.agentName, key); err != nil {
			if !errors.Is(err, local.ErrAgentVaultKeyExists) {
				key.Clear()
				return nil, ErrOperation
			}
			key.Clear()
			key, err = local.ReadAgentVaultKey(s.accountName, s.realmName, s.agentName)
			if err != nil {
				return nil, ErrOperation
			}
		}
		registered, err := s.registerLocalVaultKey(ctx, identity, key)
		if err != nil {
			key.Clear()
			return nil, err
		}
		if err := s.publishVaultKeyEpoch(registered); err != nil {
			registered.Clear()
			return nil, err
		}
		return registered, nil
	default:
		observation.local.Clear()
		return nil, ErrOperation
	}
}

func (s *Service) publishVaultKeyEpoch(key *sealed.AgentVaultKey) error {
	if key == nil {
		return ErrOperation
	}
	err := local.CreateAgentVaultKeyEpoch(s.accountName, s.realmName, s.agentName, key)
	if err == nil || errors.Is(err, local.ErrAgentVaultKeyExists) {
		return nil
	}
	return ErrOperation
}

func (s *Service) registerLocalVaultKey(ctx context.Context, identity client.SelfIdentity, key *sealed.AgentVaultKey) (*sealed.AgentVaultKey, error) {
	metadata := key.Metadata()
	if metadata.Version > uint64(1<<63-1) {
		return nil, ErrOperation
	}
	idempotencyKey, err := operationKey("")
	if err != nil {
		return nil, err
	}
	result, registerErr := s.remote.registerVaultKey(ctx, client.RegisterVaultKeyInput{
		ID:             metadata.ID,
		KeyVersion:     int64(metadata.Version),
		Algorithm:      metadata.Algorithm,
		Fingerprint:    metadata.Fingerprint,
		IdempotencyKey: idempotencyKey,
	})
	if registerErr == nil && result != nil && vaultKeyMatches(key, &result.KeyEpoch, identity) {
		return key, nil
	}
	// A lost response or concurrent registration is resolved from canonical
	// backend state. A different winner is a mismatch, never an overwrite.
	backend, err := s.remote.currentVaultKey(ctx)
	if err != nil || backend == nil {
		return nil, ErrOperation
	}
	if !vaultKeyMatches(key, backend, identity) {
		return nil, ErrKeyMismatch
	}
	return key, nil
}

func (s *Service) identity(ctx context.Context) (client.SelfIdentity, error) {
	digest, err := s.remote.self(ctx)
	if err != nil {
		return client.SelfIdentity{}, ErrOperation
	}
	identity := digest.Identity
	if !validGeneratedID(identity.AccountID, "acc") || !validGeneratedID(identity.RealmID, "realm") ||
		!validGeneratedID(identity.AgentID, "agent") || identity.AccountID != s.accountID ||
		identity.RealmName != s.realmName || identity.AgentName != s.agentName {
		return client.SelfIdentity{}, ErrIdentityMismatch
	}
	return identity, nil
}

func vaultKeyMatches(key *sealed.AgentVaultKey, binding *client.VaultKeyBinding, identity client.SelfIdentity) bool {
	if key == nil || binding == nil || key.Version() > uint64(1<<63-1) {
		return false
	}
	return binding.ID == key.ID() && binding.AccountID == identity.AccountID &&
		binding.RealmID == identity.RealmID && binding.OwnerAgentID == identity.AgentID &&
		binding.KeyVersion == int64(key.Version()) && binding.Algorithm == key.Algorithm() &&
		binding.Fingerprint == key.Fingerprint() && binding.LifecycleState == "current"
}

func vaultBindingMatchesIdentity(binding *client.VaultKeyBinding, identity client.SelfIdentity) bool {
	return binding != nil && binding.AccountID == identity.AccountID && binding.RealmID == identity.RealmID &&
		binding.OwnerAgentID == identity.AgentID
}

func normalizeCreateInput(in CreateInput) (CreateInput, error) {
	in.Name = strings.TrimSpace(in.Name)
	in.Description = strings.TrimSpace(in.Description)
	in.Template = strings.TrimSpace(in.Template)
	if in.Template == "" {
		in.Template = "generic"
	}
	if in.Name == "" || len(in.Name) > 256 || strings.ContainsAny(in.Name, "\x00\r\n") || !utf8.ValidString(in.Name) ||
		len(in.Description) > 4096 || strings.ContainsRune(in.Description, '\x00') || !utf8.ValidString(in.Description) ||
		!templatePattern.MatchString(in.Template) || len(in.Fields) < 1 || len(in.Fields) > 64 {
		return CreateInput{}, ErrInvalidInput
	}
	tags := make([]string, 0, len(in.Tags))
	seenTags := map[string]bool{}
	if len(in.Tags) > 64 {
		return CreateInput{}, ErrInvalidInput
	}
	for _, tag := range in.Tags {
		tag = strings.ToLower(strings.TrimSpace(tag))
		if !tagPattern.MatchString(tag) || seenTags[tag] {
			return CreateInput{}, ErrInvalidInput
		}
		seenTags[tag] = true
		tags = append(tags, tag)
	}
	sort.Strings(tags)
	in.Tags = tags

	fields := make([]FieldInput, len(in.Fields))
	seenFields := map[string]bool{}
	for index, field := range in.Fields {
		field.Name = strings.TrimSpace(field.Name)
		field.Kind = strings.ToLower(strings.TrimSpace(field.Kind))
		field.Encoding = strings.ToLower(strings.TrimSpace(field.Encoding))
		if field.Encoding == "" {
			field.Encoding = sealed.ValueEncodingUTF8
		}
		if !fieldNamePattern.MatchString(field.Name) || seenFields[field.Name] || !validFieldKind(field.Kind) ||
			(fieldKindRequiresProtection(field.Kind) && !field.Sensitive) {
			return CreateInput{}, ErrInvalidInput
		}
		seenFields[field.Name] = true
		if field.Sensitive {
			if len(field.Value) < 1 || len(field.Value) > sealed.MaxSensitiveValueBytes ||
				!validSensitiveEncoding(field.Encoding) ||
				(field.Encoding == sealed.ValueEncodingUTF8 && !utf8.Valid(field.Value)) ||
				(field.Encoding == sealed.ValueEncodingJSON && !json.Valid(field.Value)) {
				return CreateInput{}, ErrInvalidInput
			}
		} else if field.Encoding != sealed.ValueEncodingUTF8 || len(field.Value) > maxPublicValueBytes ||
			bytesContainZero(field.Value) || !utf8.Valid(field.Value) {
			return CreateInput{}, ErrInvalidInput
		}
		fields[index] = field
	}
	in.Fields = fields
	if value := strings.TrimSpace(in.IdempotencyKey); value != "" &&
		(len(value) > maxIdempotencyBytes || !utf8.ValidString(value) || stringContainsControl(value)) {
		return CreateInput{}, ErrInvalidInput
	}
	return in, nil
}

func toClientSealedField(envelope sealed.SensitiveFieldEnvelope) (client.SealedField, error) {
	for _, value := range []uint64{
		uint64(envelope.EnvelopeVersion), uint64(envelope.AADVersion), envelope.ValueVersion,
		envelope.DEKGeneration, envelope.WrapRevision, envelope.WrappingKeyVersion,
	} {
		if value > uint64(1<<63-1) {
			return client.SealedField{}, ErrOperation
		}
	}
	return client.SealedField{
		EnvelopeVersion: int64(envelope.EnvelopeVersion),
		Ciphertext:      append([]byte(nil), envelope.Ciphertext...),
		Algorithm:       envelope.AEADAlgorithm,
		AADVersion:      int64(envelope.AADVersion),
		DEK: client.SealedDEK{
			ID:                 envelope.DEKID,
			Generation:         int64(envelope.DEKGeneration),
			WrappedDEK:         append([]byte(nil), envelope.WrappedDEK...),
			WrapAlgorithm:      envelope.WrapAlgorithm,
			AADVersion:         int64(envelope.AADVersion),
			WrapRevision:       int64(envelope.WrapRevision),
			WrappingKeyID:      envelope.WrappingKeyID,
			WrappingKeyVersion: int64(envelope.WrappingKeyVersion),
		},
	}, nil
}

func fromClientSecretMaterial(material client.SecretMaterial) (sealed.SensitiveFieldEnvelope, error) {
	if material.EnvelopeVersion < 1 || material.EnvelopeVersion > int64(^uint32(0)) ||
		material.AADVersion < 1 || material.AADVersion > int64(^uint32(0)) ||
		material.ValueVersion < 1 || material.DEK.Generation < 1 || material.DEK.WrapRevision < 1 ||
		material.DEK.WrappingKeyVersion < 1 || material.DEK.AADVersion != material.AADVersion ||
		material.Encoding == "" {
		return sealed.SensitiveFieldEnvelope{}, ErrIntegrity
	}
	return sealed.SensitiveFieldEnvelope{
		EnvelopeVersion:    uint32(material.EnvelopeVersion),
		AADVersion:         uint32(material.AADVersion),
		Ciphertext:         append([]byte(nil), material.Ciphertext...),
		AEADAlgorithm:      material.Algorithm,
		ValueEncoding:      material.Encoding,
		ValueVersion:       uint64(material.ValueVersion),
		DEKID:              material.DEK.ID,
		DEKGeneration:      uint64(material.DEK.Generation),
		WrappedDEK:         append([]byte(nil), material.DEK.WrappedDEK...),
		WrapAlgorithm:      material.DEK.WrapAlgorithm,
		WrapRevision:       uint64(material.DEK.WrapRevision),
		WrappingKeyID:      material.DEK.WrappingKeyID,
		WrappingKeyVersion: uint64(material.DEK.WrappingKeyVersion),
	}, nil
}

func fieldDomain(kind string) (sealed.ValueDomain, bool) {
	if kind == "totp" {
		return sealed.TOTPPayloadDomain, true
	}
	if !validFieldKind(kind) {
		return "", false
	}
	return sealed.FieldValueDomain, true
}

func validFieldKind(kind string) bool {
	switch kind {
	case "text", "username", "password", "url", "api_key", "token", "private_key", "totp", "recovery_code", "note":
		return true
	default:
		return false
	}
}

func fieldKindRequiresProtection(kind string) bool {
	switch kind {
	case "password", "api_key", "token", "private_key", "totp", "recovery_code":
		return true
	default:
		return false
	}
}

func validSensitiveEncoding(encoding string) bool {
	return encoding == sealed.ValueEncodingUTF8 || encoding == sealed.ValueEncodingJSON || encoding == sealed.ValueEncodingBinary
}

func hasSensitiveFields(fields []FieldInput) bool {
	for _, field := range fields {
		if field.Sensitive {
			return true
		}
	}
	return false
}

func clearSensitiveInputs(fields []FieldInput) {
	for index := range fields {
		if fields[index].Sensitive || fieldKindRequiresProtection(strings.ToLower(strings.TrimSpace(fields[index].Kind))) {
			clear(fields[index].Value)
		}
	}
}

func operationKey(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value != "" {
		if len(value) > maxIdempotencyBytes || !utf8.ValidString(value) || stringContainsControl(value) {
			return "", ErrInvalidInput
		}
		return value, nil
	}
	generated, err := id.New("op")
	if err != nil {
		return "", ErrOperation
	}
	return generated, nil
}

func stringContainsControl(value string) bool {
	for _, char := range value {
		if unicode.IsControl(char) {
			return true
		}
	}
	return false
}

func validGeneratedID(value, prefix string) bool {
	body := strings.TrimPrefix(value, prefix+"_")
	if body == value || len(body) != 16 {
		return false
	}
	for _, char := range body {
		if (char < 'a' || char > 'z') && (char < '2' || char > '7') {
			return false
		}
	}
	return true
}

func bytesContainZero(value []byte) bool {
	for _, b := range value {
		if b == 0 {
			return true
		}
	}
	return false
}

func secretMatchesIdentity(value client.Secret, identity client.SelfIdentity) bool {
	return value.AccountID == identity.AccountID && value.RealmID == identity.RealmID && value.OwnerAgentID == identity.AgentID
}

func redactSecret(value *client.Secret) {
	if value == nil {
		return
	}
	if value.Tags == nil {
		value.Tags = []string{}
	}
	for index := range value.Fields {
		if value.Fields[index].Sensitive ||
			fieldKindRequiresProtection(strings.ToLower(strings.TrimSpace(value.Fields[index].Kind))) {
			value.Fields[index].PublicValue = nil
			value.Fields[index].Redacted = true
		}
	}
}

type remote interface {
	self(context.Context) (client.SelfDigest, error)
	currentVaultKey(context.Context) (*client.VaultKeyBinding, error)
	registerVaultKey(context.Context, client.RegisterVaultKeyInput) (*client.VaultKeyMutationResult, error)
	createVaultKeyEnrollment(context.Context, client.CreateVaultKeyEnrollmentInput) (*client.VaultKeyEnrollment, error)
	listVaultKeyEnrollments(context.Context, client.VaultKeyEnrollmentListOptions) ([]client.VaultKeyEnrollment, error)
	getVaultKeyEnrollment(context.Context, string) (*client.VaultKeyEnrollment, error)
	approveVaultKeyEnrollment(context.Context, string, client.ApproveVaultKeyEnrollmentInput) (*client.VaultKeyEnrollment, error)
	receiveVaultKeyEnrollment(context.Context, string, client.ReceiveVaultKeyEnrollmentInput) (*client.VaultKeyEnrollmentTransfer, error)
	consumeVaultKeyEnrollment(context.Context, string, client.ConsumeVaultKeyEnrollmentInput) (*client.VaultKeyEnrollment, error)
	cancelVaultKeyEnrollment(context.Context, string, client.CancelVaultKeyEnrollmentInput) (*client.VaultKeyEnrollment, error)
	createSecret(context.Context, client.CreateSecretInput) (*client.SecretMutationResult, error)
	listSecrets(context.Context, client.SecretListOptions) (*client.SecretPage, error)
	getSecret(context.Context, string) (*client.Secret, error)
	accessSecretField(context.Context, string, string, string) (*client.SecretMaterial, error)
}

type httpRemote struct {
	endpoint string
	token    string
}

func (r httpRemote) self(ctx context.Context) (client.SelfDigest, error) {
	return client.GetSelf(ctx, r.endpoint, r.token, client.SelfOptions{})
}

func (r httpRemote) currentVaultKey(ctx context.Context) (*client.VaultKeyBinding, error) {
	return client.GetCurrentVaultKey(ctx, r.endpoint, r.token)
}

func (r httpRemote) registerVaultKey(ctx context.Context, input client.RegisterVaultKeyInput) (*client.VaultKeyMutationResult, error) {
	return client.RegisterVaultKey(ctx, r.endpoint, r.token, input)
}

func (r httpRemote) createVaultKeyEnrollment(ctx context.Context, input client.CreateVaultKeyEnrollmentInput) (*client.VaultKeyEnrollment, error) {
	return client.CreateVaultKeyEnrollment(ctx, r.endpoint, r.token, input)
}

func (r httpRemote) listVaultKeyEnrollments(ctx context.Context, options client.VaultKeyEnrollmentListOptions) ([]client.VaultKeyEnrollment, error) {
	return client.ListVaultKeyEnrollments(ctx, r.endpoint, r.token, options)
}

func (r httpRemote) getVaultKeyEnrollment(ctx context.Context, enrollmentID string) (*client.VaultKeyEnrollment, error) {
	return client.GetVaultKeyEnrollment(ctx, r.endpoint, r.token, enrollmentID)
}

func (r httpRemote) approveVaultKeyEnrollment(ctx context.Context, enrollmentID string, input client.ApproveVaultKeyEnrollmentInput) (*client.VaultKeyEnrollment, error) {
	return client.ApproveVaultKeyEnrollment(ctx, r.endpoint, r.token, enrollmentID, input)
}

func (r httpRemote) receiveVaultKeyEnrollment(ctx context.Context, enrollmentID string, input client.ReceiveVaultKeyEnrollmentInput) (*client.VaultKeyEnrollmentTransfer, error) {
	return client.ReceiveVaultKeyEnrollment(ctx, r.endpoint, r.token, enrollmentID, input)
}

func (r httpRemote) consumeVaultKeyEnrollment(ctx context.Context, enrollmentID string, input client.ConsumeVaultKeyEnrollmentInput) (*client.VaultKeyEnrollment, error) {
	return client.ConsumeVaultKeyEnrollment(ctx, r.endpoint, r.token, enrollmentID, input)
}

func (r httpRemote) cancelVaultKeyEnrollment(ctx context.Context, enrollmentID string, input client.CancelVaultKeyEnrollmentInput) (*client.VaultKeyEnrollment, error) {
	return client.CancelVaultKeyEnrollment(ctx, r.endpoint, r.token, enrollmentID, input)
}

func (r httpRemote) createSecret(ctx context.Context, input client.CreateSecretInput) (*client.SecretMutationResult, error) {
	return client.CreateSecret(ctx, r.endpoint, r.token, input)
}

func (r httpRemote) listSecrets(ctx context.Context, options client.SecretListOptions) (*client.SecretPage, error) {
	return client.ListSecrets(ctx, r.endpoint, r.token, options)
}

func (r httpRemote) getSecret(ctx context.Context, secretID string) (*client.Secret, error) {
	return client.GetSecret(ctx, r.endpoint, r.token, secretID)
}

func (r httpRemote) accessSecretField(ctx context.Context, secretID, fieldID, idempotencyKey string) (*client.SecretMaterial, error) {
	return client.AccessSecretField(ctx, r.endpoint, r.token, secretID, fieldID, idempotencyKey)
}
