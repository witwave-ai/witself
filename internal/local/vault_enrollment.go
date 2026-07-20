package local

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/witwave-ai/witself/internal/sealed"
)

const (
	// MaxAgentVaultKeyEnrollmentStateBytes bounds each local enrollment file.
	// V1 records are below 4 KiB; 16 KiB leaves versioning room without allowing
	// unbounded reads from a corrupted owner-only path.
	MaxAgentVaultKeyEnrollmentStateBytes = 16 * 1024

	agentVaultKeyEnrollmentStateVersionV1 uint32 = 1
	agentVaultKeyEnrollmentPrivateFile           = "recipient.state"
	agentVaultKeyEnrollmentRequestFile           = "request.state"

	agentVaultKeyEnrollmentPrivateChecksumDomain = "witself/local/avk-enrollment-private/v1\x00"
	agentVaultKeyEnrollmentRequestChecksumDomain = "witself/local/avk-enrollment-request/v1\x00"

	// MaxAgentVaultKeyEnrollmentStates bounds discovery work and prevents a
	// corrupted or attacker-filled local directory from causing unbounded reads.
	MaxAgentVaultKeyEnrollmentStates = 64
)

var (
	// ErrAgentVaultKeyEnrollmentUnavailable means no local private preflight
	// state exists for the selected enrollment request.
	ErrAgentVaultKeyEnrollmentUnavailable = errors.New("agent vault key enrollment state is unavailable")

	// ErrAgentVaultKeyEnrollmentExists means exclusive preflight creation found
	// an existing private regular record. It is never overwritten.
	ErrAgentVaultKeyEnrollmentExists = errors.New("agent vault key enrollment state already exists")

	// ErrAgentVaultKeyEnrollmentConflict means a request cannot be bound to the
	// selected preflight state or a different request is already finalized.
	ErrAgentVaultKeyEnrollmentConflict = errors.New("agent vault key enrollment state conflicts")

	// ErrAgentVaultKeyEnrollmentUnsafe means a state path is not an owner-only
	// regular file rooted entirely below owner-only real directories.
	ErrAgentVaultKeyEnrollmentUnsafe = errors.New("agent vault key enrollment storage is unsafe")

	// ErrAgentVaultKeyEnrollmentInvalid deliberately hides malformed record and
	// private enrollment values.
	ErrAgentVaultKeyEnrollmentInvalid = errors.New("agent vault key enrollment state is invalid")

	// ErrAgentVaultKeyEnrollmentStorage deliberately hides OS paths and syscall
	// details from errors that may be rendered by an AI client.
	ErrAgentVaultKeyEnrollmentStorage = errors.New("agent vault key enrollment storage failed")

	// ErrAgentVaultKeyEnrollmentScope identifies invalid local selectors or an
	// invalid generated enrollment request ID without echoing them.
	ErrAgentVaultKeyEnrollmentScope = errors.New("agent vault key enrollment scope is invalid")

	// ErrAgentVaultKeyEnrollmentDisclosure prevents generic serialization of a
	// state object that owns recipient and pairing private material.
	ErrAgentVaultKeyEnrollmentDisclosure = errors.New("agent vault key enrollment state cannot be serialized generically")
)

// AgentVaultKeyEnrollmentState owns the local recipient key and pairing secret
// for one request. Request is nil after crash-safe preflight creation and is
// populated only after FinalizeAgentVaultKeyEnrollmentRequest atomically binds
// the complete server-returned public request. Callers must defer Clear.
type AgentVaultKeyEnrollmentState struct {
	RequestID     string
	RecipientKey  *sealed.AVKEnrollmentRecipientKey
	PairingSecret *sealed.AVKEnrollmentPairingSecret
	Request       *sealed.AVKEnrollmentRequest
}

// Finalized reports whether the immutable public request sidecar is present.
func (s *AgentVaultKeyEnrollmentState) Finalized() bool {
	return s != nil && s.Request != nil
}

// Clear overwrites the owned private values and releases all references.
func (s *AgentVaultKeyEnrollmentState) Clear() {
	if s == nil {
		return
	}
	if s.RecipientKey != nil {
		s.RecipientKey.Clear()
	}
	if s.PairingSecret != nil {
		s.PairingSecret.Clear()
	}
	s.RecipientKey = nil
	s.PairingSecret = nil
	s.Request = nil
}

func (s AgentVaultKeyEnrollmentState) String() string {
	return fmt.Sprintf("<agent-vault-key-enrollment-state request_id=%s finalized=%t private=redacted>",
		s.RequestID, s.Request != nil)
}

// GoString returns the same redacted diagnostic representation as String.
func (s AgentVaultKeyEnrollmentState) GoString() string { return s.String() }

// MarshalJSON rejects generic serialization of private enrollment state.
func (s AgentVaultKeyEnrollmentState) MarshalJSON() ([]byte, error) {
	return nil, ErrAgentVaultKeyEnrollmentDisclosure
}

// UnmarshalJSON rejects generic deserialization of private enrollment state.
func (s *AgentVaultKeyEnrollmentState) UnmarshalJSON([]byte) error {
	return ErrAgentVaultKeyEnrollmentDisclosure
}

// AgentVaultKeyEnrollmentPath returns the request-scoped owner-only directory.
// Its two immutable children are recipient.state and, after finalization,
// request.state.
func AgentVaultKeyEnrollmentPath(account, realm, agent, requestID string) (string, error) {
	_, directory, err := agentVaultKeyEnrollmentLocation(account, realm, agent, requestID)
	return directory, err
}

// ListAgentVaultKeyEnrollmentStateIDs returns a bounded, sorted list of local
// request ids with a durably published private preflight record. It returns no
// private material and rejects unsafe directories, symlinks, unknown names,
// permission drift, and replacement races. Missing state is an empty list.
func ListAgentVaultKeyEnrollmentStateIDs(account, realm, agent string) ([]string, error) {
	for _, value := range []string{account, realm, agent} {
		if !namePattern.MatchString(value) {
			return nil, ErrAgentVaultKeyEnrollmentScope
		}
	}
	home, err := root()
	if err != nil {
		return nil, ErrAgentVaultKeyEnrollmentStorage
	}
	parent := filepath.Join(home, "keys", "accounts", account, "realms", realm,
		"agents", agent, "enrollments")
	if err := validateAgentVaultKeyEnrollmentDirectories(home, parent); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []string{}, nil
		}
		if errors.Is(err, ErrAgentVaultKeyEnrollmentUnsafe) {
			return nil, err
		}
		return nil, ErrAgentVaultKeyEnrollmentStorage
	}
	before, err := os.Lstat(parent)
	if err != nil || !privateAgentVaultKeyEnrollmentDirectory(before) {
		return nil, ErrAgentVaultKeyEnrollmentUnsafe
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		return nil, ErrAgentVaultKeyEnrollmentStorage
	}
	if len(entries) > MaxAgentVaultKeyEnrollmentStates {
		return nil, ErrAgentVaultKeyEnrollmentInvalid
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		requestID := entry.Name()
		if !validAgentVaultKeyEnrollmentRequestID(requestID) {
			return nil, ErrAgentVaultKeyEnrollmentUnsafe
		}
		directory := filepath.Join(parent, requestID)
		info, err := os.Lstat(directory)
		if err != nil || !privateAgentVaultKeyEnrollmentDirectory(info) {
			return nil, ErrAgentVaultKeyEnrollmentUnsafe
		}
		privateInfo, err := os.Lstat(filepath.Join(directory, agentVaultKeyEnrollmentPrivateFile))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !privateAgentVaultKeyEnrollmentFile(privateInfo) {
			return nil, ErrAgentVaultKeyEnrollmentUnsafe
		}
		ids = append(ids, requestID)
	}
	after, err := os.Lstat(parent)
	if err != nil || !os.SameFile(before, after) || !privateAgentVaultKeyEnrollmentDirectory(after) {
		return nil, ErrAgentVaultKeyEnrollmentUnsafe
	}
	return ids, nil
}

// CreateAgentVaultKeyEnrollmentPreflight durably publishes recipient and
// pairing private material before any server mutation. Publication is atomic
// and no-replace: concurrent or repeated creation never changes existing bytes.
func CreateAgentVaultKeyEnrollmentPreflight(account, realm, agent, requestID string, recipient *sealed.AVKEnrollmentRecipientKey, pairing *sealed.AVKEnrollmentPairingSecret) error {
	home, directory, err := agentVaultKeyEnrollmentLocation(account, realm, agent, requestID)
	if err != nil {
		return err
	}
	raw, err := marshalAgentVaultKeyEnrollmentPrivate(account, realm, agent, requestID, recipient, pairing)
	if err != nil {
		return err
	}
	defer clear(raw)

	if err := ensureAgentVaultKeyEnrollmentDirectories(home, directory); err != nil {
		return err
	}
	requestPath := filepath.Join(directory, agentVaultKeyEnrollmentRequestFile)
	if info, statErr := os.Lstat(requestPath); statErr == nil {
		if !privateAgentVaultKeyEnrollmentFile(info) {
			return ErrAgentVaultKeyEnrollmentUnsafe
		}
		return ErrAgentVaultKeyEnrollmentConflict
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return ErrAgentVaultKeyEnrollmentStorage
	}

	privatePath := filepath.Join(directory, agentVaultKeyEnrollmentPrivateFile)
	err = publishAgentVaultKeyEnrollmentFile(home, directory, privatePath, raw)
	if errors.Is(err, errAgentVaultKeyEnrollmentCollision) {
		return classifyAgentVaultKeyEnrollmentCollision(privatePath, ErrAgentVaultKeyEnrollmentExists)
	}
	return err
}

// ReadAgentVaultKeyEnrollmentState securely reads the private preflight record
// and, when present, its finalized public request sidecar. It rejects symlinks,
// permission drift, replacement races, non-canonical JSON, checksum failures,
// and any request that no longer matches the private recipient and pairing
// material.
func ReadAgentVaultKeyEnrollmentState(account, realm, agent, requestID string) (*AgentVaultKeyEnrollmentState, error) {
	home, directory, err := agentVaultKeyEnrollmentLocation(account, realm, agent, requestID)
	if err != nil {
		return nil, err
	}
	if err := validateAgentVaultKeyEnrollmentDirectories(home, directory); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrAgentVaultKeyEnrollmentUnavailable
		}
		if errors.Is(err, ErrAgentVaultKeyEnrollmentUnsafe) {
			return nil, err
		}
		return nil, ErrAgentVaultKeyEnrollmentStorage
	}

	privatePath := filepath.Join(directory, agentVaultKeyEnrollmentPrivateFile)
	privateRaw, err := readAgentVaultKeyEnrollmentFile(home, directory, privatePath)
	if err != nil {
		if errors.Is(err, ErrAgentVaultKeyEnrollmentUnavailable) {
			if _, requestErr := os.Lstat(filepath.Join(directory, agentVaultKeyEnrollmentRequestFile)); requestErr == nil {
				return nil, ErrAgentVaultKeyEnrollmentInvalid
			}
		}
		return nil, err
	}
	defer clear(privateRaw)
	recipient, pairing, err := parseAgentVaultKeyEnrollmentPrivate(account, realm, agent, requestID, privateRaw)
	if err != nil {
		return nil, err
	}
	state := &AgentVaultKeyEnrollmentState{
		RequestID: requestID, RecipientKey: recipient, PairingSecret: pairing,
	}
	failed := true
	defer func() {
		if failed {
			state.Clear()
		}
	}()

	requestPath := filepath.Join(directory, agentVaultKeyEnrollmentRequestFile)
	requestRaw, err := readAgentVaultKeyEnrollmentFile(home, directory, requestPath)
	if errors.Is(err, ErrAgentVaultKeyEnrollmentUnavailable) {
		failed = false
		return state, nil
	}
	if err != nil {
		return nil, err
	}
	defer clear(requestRaw)
	request, err := parseAgentVaultKeyEnrollmentRequest(account, realm, agent, requestID, requestRaw)
	if err != nil || validateAgentVaultKeyEnrollmentRequestForPrivate(state, request) != nil {
		return nil, ErrAgentVaultKeyEnrollmentInvalid
	}
	state.Request = &request
	failed = false
	return state, nil
}

// FinalizeAgentVaultKeyEnrollmentRequest atomically adds the immutable public
// request sidecar after a successful server request. The exact same request is
// idempotent; a different or private-key-mismatched request never replaces it.
func FinalizeAgentVaultKeyEnrollmentRequest(account, realm, agent, requestID string, request sealed.AVKEnrollmentRequest) error {
	home, directory, err := agentVaultKeyEnrollmentLocation(account, realm, agent, requestID)
	if err != nil {
		return err
	}
	state, err := ReadAgentVaultKeyEnrollmentState(account, realm, agent, requestID)
	if err != nil {
		return err
	}
	defer state.Clear()
	if state.Request != nil {
		if *state.Request == request {
			return nil
		}
		return ErrAgentVaultKeyEnrollmentConflict
	}
	if err := validateAgentVaultKeyEnrollmentRequestForPrivate(state, request); err != nil {
		return err
	}
	raw, err := marshalAgentVaultKeyEnrollmentRequest(account, realm, agent, requestID, request)
	if err != nil {
		return err
	}
	defer clear(raw)
	path := filepath.Join(directory, agentVaultKeyEnrollmentRequestFile)
	if err := publishAgentVaultKeyEnrollmentFile(home, directory, path, raw); err != nil {
		if !errors.Is(err, errAgentVaultKeyEnrollmentCollision) {
			return err
		}
		// A retry racing the winning finalizer is successful only when the fully
		// published request is byte-for-byte the same logical request.
		current, readErr := ReadAgentVaultKeyEnrollmentState(account, realm, agent, requestID)
		if readErr != nil {
			return readErr
		}
		defer current.Clear()
		if current.Request != nil && *current.Request == request {
			return nil
		}
		return classifyAgentVaultKeyEnrollmentCollision(path, ErrAgentVaultKeyEnrollmentConflict)
	}
	return nil
}

// DeleteAgentVaultKeyEnrollmentAfterConsume removes a finalized request's
// public sidecar first, then its private recipient state, syncing every
// directory transition. It refuses to delete preflight-only state. Missing
// state is treated as an idempotent successful retry.
func DeleteAgentVaultKeyEnrollmentAfterConsume(account, realm, agent, requestID string) error {
	return deleteAgentVaultKeyEnrollmentFinalizedState(account, realm, agent, requestID)
}

// DeleteAgentVaultKeyEnrollmentAfterTerminal removes finalized private state
// only after its caller has authenticated canonical cancelled or expired
// backend state. Like consume cleanup, it is idempotent and refuses to remove a
// preflight that has not been bound to an exact public request.
func DeleteAgentVaultKeyEnrollmentAfterTerminal(account, realm, agent, requestID string) error {
	return deleteAgentVaultKeyEnrollmentFinalizedState(account, realm, agent, requestID)
}

func deleteAgentVaultKeyEnrollmentFinalizedState(account, realm, agent, requestID string) error {
	home, directory, err := agentVaultKeyEnrollmentLocation(account, realm, agent, requestID)
	if err != nil {
		return err
	}
	state, err := ReadAgentVaultKeyEnrollmentState(account, realm, agent, requestID)
	if errors.Is(err, ErrAgentVaultKeyEnrollmentUnavailable) {
		return nil
	}
	if err != nil {
		return err
	}
	finalized := state.Finalized()
	state.Clear()
	if !finalized {
		return ErrAgentVaultKeyEnrollmentConflict
	}

	cleanupNames, err := agentVaultKeyEnrollmentCleanupNames(directory)
	if err != nil {
		return err
	}
	for _, name := range cleanupNames {
		if err := removeAgentVaultKeyEnrollmentFile(filepath.Join(directory, name)); err != nil {
			return err
		}
		if err := syncAgentVaultKeyEnrollmentDirectory(directory); err != nil {
			return ErrAgentVaultKeyEnrollmentStorage
		}
	}
	// Removing the now-empty request directory is best effort only when another
	// known-safe process has left no temporary inode. Private state is already
	// durably absent either way.
	parent := filepath.Dir(directory)
	if err := os.Remove(directory); err == nil {
		if err := syncAgentVaultKeyEnrollmentDirectory(parent); err != nil {
			return ErrAgentVaultKeyEnrollmentStorage
		}
	} else if !errors.Is(err, os.ErrNotExist) && !isDirectoryNotEmpty(err) {
		return ErrAgentVaultKeyEnrollmentStorage
	}
	if err := validateAgentVaultKeyEnrollmentDirectories(home, parent); err != nil {
		if errors.Is(err, ErrAgentVaultKeyEnrollmentUnsafe) || errors.Is(err, os.ErrNotExist) {
			return ErrAgentVaultKeyEnrollmentUnsafe
		}
		return ErrAgentVaultKeyEnrollmentStorage
	}
	return nil
}

type agentVaultKeyEnrollmentPrivateBody struct {
	StateVersion  uint32 `json:"state_version"`
	RequestID     string `json:"request_id"`
	RecipientKey  string `json:"recipient_key"`
	PairingSecret string `json:"pairing_secret"`
}

type agentVaultKeyEnrollmentPrivateRecord struct {
	StateVersion  uint32 `json:"state_version"`
	RequestID     string `json:"request_id"`
	RecipientKey  string `json:"recipient_key"`
	PairingSecret string `json:"pairing_secret"`
	Checksum      string `json:"checksum"`
}

type agentVaultKeyEnrollmentRequestBody struct {
	StateVersion uint32                      `json:"state_version"`
	Request      sealed.AVKEnrollmentRequest `json:"request"`
}

type agentVaultKeyEnrollmentRequestRecord struct {
	StateVersion uint32                      `json:"state_version"`
	Request      sealed.AVKEnrollmentRequest `json:"request"`
	Checksum     string                      `json:"checksum"`
}

func marshalAgentVaultKeyEnrollmentPrivate(account, realm, agent, requestID string, recipient *sealed.AVKEnrollmentRecipientKey, pairing *sealed.AVKEnrollmentPairingSecret) ([]byte, error) {
	recipientRaw, err := sealed.EncodeAVKEnrollmentRecipientKey(recipient)
	if err != nil {
		return nil, ErrAgentVaultKeyEnrollmentInvalid
	}
	defer clear(recipientRaw)
	pairingEncoded, err := sealed.EncodeAVKEnrollmentPairingSecret(pairing)
	if err != nil {
		return nil, ErrAgentVaultKeyEnrollmentInvalid
	}
	body := agentVaultKeyEnrollmentPrivateBody{
		StateVersion: agentVaultKeyEnrollmentStateVersionV1,
		RequestID:    requestID, RecipientKey: string(recipientRaw), PairingSecret: pairingEncoded,
	}
	bodyRaw, err := json.Marshal(body)
	if err != nil {
		return nil, ErrAgentVaultKeyEnrollmentInvalid
	}
	defer clear(bodyRaw)
	record := agentVaultKeyEnrollmentPrivateRecord{
		StateVersion: body.StateVersion, RequestID: body.RequestID,
		RecipientKey: body.RecipientKey, PairingSecret: body.PairingSecret,
		Checksum: agentVaultKeyEnrollmentChecksum(agentVaultKeyEnrollmentPrivateChecksumDomain,
			account, realm, agent, requestID, bodyRaw),
	}
	raw, err := json.Marshal(record)
	if err != nil || len(raw) == 0 || len(raw) > MaxAgentVaultKeyEnrollmentStateBytes {
		clear(raw)
		return nil, ErrAgentVaultKeyEnrollmentInvalid
	}
	return raw, nil
}

func parseAgentVaultKeyEnrollmentPrivate(account, realm, agent, requestID string, raw []byte) (*sealed.AVKEnrollmentRecipientKey, *sealed.AVKEnrollmentPairingSecret, error) {
	var record agentVaultKeyEnrollmentPrivateRecord
	if len(raw) == 0 || len(raw) > MaxAgentVaultKeyEnrollmentStateBytes || !json.Valid(raw) ||
		json.Unmarshal(raw, &record) != nil || record.StateVersion != agentVaultKeyEnrollmentStateVersionV1 ||
		record.RequestID != requestID || !validAgentVaultKeyEnrollmentChecksum(record.Checksum) {
		return nil, nil, ErrAgentVaultKeyEnrollmentInvalid
	}
	canonical, err := json.Marshal(record)
	if err != nil || !bytes.Equal(canonical, raw) {
		clear(canonical)
		return nil, nil, ErrAgentVaultKeyEnrollmentInvalid
	}
	clear(canonical)
	body := agentVaultKeyEnrollmentPrivateBody{
		StateVersion: record.StateVersion, RequestID: record.RequestID,
		RecipientKey: record.RecipientKey, PairingSecret: record.PairingSecret,
	}
	bodyRaw, err := json.Marshal(body)
	if err != nil {
		return nil, nil, ErrAgentVaultKeyEnrollmentInvalid
	}
	defer clear(bodyRaw)
	want := agentVaultKeyEnrollmentChecksum(agentVaultKeyEnrollmentPrivateChecksumDomain,
		account, realm, agent, requestID, bodyRaw)
	if !constantTimeLocalEnrollmentStringEqual(want, record.Checksum) {
		return nil, nil, ErrAgentVaultKeyEnrollmentInvalid
	}
	recipient, err := sealed.ParseAVKEnrollmentRecipientKey([]byte(record.RecipientKey))
	if err != nil {
		return nil, nil, ErrAgentVaultKeyEnrollmentInvalid
	}
	pairing, err := sealed.ParseAVKEnrollmentPairingSecret(record.PairingSecret)
	if err != nil {
		recipient.Clear()
		return nil, nil, ErrAgentVaultKeyEnrollmentInvalid
	}
	return recipient, pairing, nil
}

func marshalAgentVaultKeyEnrollmentRequest(account, realm, agent, requestID string, request sealed.AVKEnrollmentRequest) ([]byte, error) {
	body := agentVaultKeyEnrollmentRequestBody{StateVersion: agentVaultKeyEnrollmentStateVersionV1, Request: request}
	bodyRaw, err := json.Marshal(body)
	if err != nil {
		return nil, ErrAgentVaultKeyEnrollmentInvalid
	}
	defer clear(bodyRaw)
	record := agentVaultKeyEnrollmentRequestRecord{
		StateVersion: body.StateVersion, Request: request,
		Checksum: agentVaultKeyEnrollmentChecksum(agentVaultKeyEnrollmentRequestChecksumDomain,
			account, realm, agent, requestID, bodyRaw),
	}
	raw, err := json.Marshal(record)
	if err != nil || len(raw) == 0 || len(raw) > MaxAgentVaultKeyEnrollmentStateBytes {
		clear(raw)
		return nil, ErrAgentVaultKeyEnrollmentInvalid
	}
	return raw, nil
}

func parseAgentVaultKeyEnrollmentRequest(account, realm, agent, requestID string, raw []byte) (sealed.AVKEnrollmentRequest, error) {
	var record agentVaultKeyEnrollmentRequestRecord
	if len(raw) == 0 || len(raw) > MaxAgentVaultKeyEnrollmentStateBytes || !json.Valid(raw) ||
		json.Unmarshal(raw, &record) != nil || record.StateVersion != agentVaultKeyEnrollmentStateVersionV1 ||
		record.Request.EnrollmentRequestID != requestID || !validAgentVaultKeyEnrollmentChecksum(record.Checksum) {
		return sealed.AVKEnrollmentRequest{}, ErrAgentVaultKeyEnrollmentInvalid
	}
	canonical, err := json.Marshal(record)
	if err != nil || !bytes.Equal(canonical, raw) {
		clear(canonical)
		return sealed.AVKEnrollmentRequest{}, ErrAgentVaultKeyEnrollmentInvalid
	}
	clear(canonical)
	body := agentVaultKeyEnrollmentRequestBody{StateVersion: record.StateVersion, Request: record.Request}
	bodyRaw, err := json.Marshal(body)
	if err != nil {
		return sealed.AVKEnrollmentRequest{}, ErrAgentVaultKeyEnrollmentInvalid
	}
	defer clear(bodyRaw)
	want := agentVaultKeyEnrollmentChecksum(agentVaultKeyEnrollmentRequestChecksumDomain,
		account, realm, agent, requestID, bodyRaw)
	if !constantTimeLocalEnrollmentStringEqual(want, record.Checksum) {
		return sealed.AVKEnrollmentRequest{}, ErrAgentVaultKeyEnrollmentInvalid
	}
	return record.Request, nil
}

func validateAgentVaultKeyEnrollmentRequestForPrivate(state *AgentVaultKeyEnrollmentState, request sealed.AVKEnrollmentRequest) error {
	if state == nil || state.RecipientKey == nil || state.PairingSecret == nil ||
		request.EnrollmentRequestID != state.RequestID {
		return ErrAgentVaultKeyEnrollmentConflict
	}
	publicKey, err := state.RecipientKey.PublicKey()
	if err != nil || publicKey != request.TargetPublicKey ||
		sealed.VerifyAVKEnrollmentPairingSecret(state.PairingSecret, request) != nil {
		return ErrAgentVaultKeyEnrollmentConflict
	}
	return nil
}

func agentVaultKeyEnrollmentChecksum(domain, account, realm, agent, requestID string, body []byte) string {
	h := sha256.New()
	_, _ = h.Write([]byte(domain))
	for _, value := range []string{account, realm, agent, requestID} {
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(value)))
		_, _ = h.Write(length[:])
		_, _ = h.Write([]byte(value))
	}
	_, _ = h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

func validAgentVaultKeyEnrollmentChecksum(value string) bool {
	if len(value) != 2*sha256.Size {
		return false
	}
	raw, err := hex.DecodeString(value)
	if err != nil || len(raw) != sha256.Size || hex.EncodeToString(raw) != value {
		clear(raw)
		return false
	}
	clear(raw)
	return true
}

func constantTimeLocalEnrollmentStringEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func agentVaultKeyEnrollmentLocation(account, realm, agent, requestID string) (home, directory string, err error) {
	for _, value := range []string{account, realm, agent} {
		if !namePattern.MatchString(value) {
			return "", "", ErrAgentVaultKeyEnrollmentScope
		}
	}
	if !validAgentVaultKeyEnrollmentRequestID(requestID) {
		return "", "", ErrAgentVaultKeyEnrollmentScope
	}
	home, err = root()
	if err != nil {
		return "", "", ErrAgentVaultKeyEnrollmentStorage
	}
	directory = filepath.Join(home, "keys", "accounts", account, "realms", realm,
		"agents", agent, "enrollments", requestID)
	return home, directory, nil
}

func validAgentVaultKeyEnrollmentRequestID(value string) bool {
	if len(value) != len("enr_")+16 || !strings.HasPrefix(value, "enr_") {
		return false
	}
	for _, character := range value[len("enr_"):] {
		if (character < 'a' || character > 'z') && (character < '2' || character > '7') {
			return false
		}
	}
	return true
}

var errAgentVaultKeyEnrollmentCollision = errors.New("agent vault key enrollment publication collision")

func publishAgentVaultKeyEnrollmentFile(home, directory, path string, raw []byte) error {
	if len(raw) == 0 || len(raw) > MaxAgentVaultKeyEnrollmentStateBytes {
		return ErrAgentVaultKeyEnrollmentInvalid
	}
	if err := ensureAgentVaultKeyEnrollmentDirectories(home, directory); err != nil {
		return err
	}
	directoryBefore, err := os.Lstat(directory)
	if err != nil || !privateAgentVaultKeyEnrollmentDirectory(directoryBefore) {
		return ErrAgentVaultKeyEnrollmentUnsafe
	}
	if info, err := os.Lstat(path); err == nil {
		if !privateAgentVaultKeyEnrollmentFile(info) {
			return ErrAgentVaultKeyEnrollmentUnsafe
		}
		return errAgentVaultKeyEnrollmentCollision
	} else if !errors.Is(err, os.ErrNotExist) {
		return ErrAgentVaultKeyEnrollmentStorage
	}

	file, err := os.CreateTemp(directory, ".enrollment-write-*.tmp")
	if err != nil {
		return ErrAgentVaultKeyEnrollmentStorage
	}
	temporaryPath := file.Name()
	temporaryExists := true
	defer func() {
		_ = file.Close()
		if temporaryExists {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return ErrAgentVaultKeyEnrollmentStorage
	}
	temporaryInfo, err := file.Stat()
	if err != nil || !privateAgentVaultKeyEnrollmentFile(temporaryInfo) {
		return ErrAgentVaultKeyEnrollmentStorage
	}
	written, err := io.Copy(file, bytes.NewReader(raw))
	if err != nil || written != int64(len(raw)) {
		return ErrAgentVaultKeyEnrollmentStorage
	}
	if err := file.Sync(); err != nil {
		return ErrAgentVaultKeyEnrollmentStorage
	}
	if err := file.Close(); err != nil {
		return ErrAgentVaultKeyEnrollmentStorage
	}
	if err := os.Link(temporaryPath, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return errAgentVaultKeyEnrollmentCollision
		}
		return ErrAgentVaultKeyEnrollmentStorage
	}
	published := true
	defer func() {
		if published {
			removeAgentVaultKeyEnrollmentIfSame(path, temporaryInfo)
		}
	}()
	finalInfo, finalErr := os.Lstat(path)
	directoryAfter, directoryErr := os.Lstat(directory)
	if finalErr != nil || directoryErr != nil || !os.SameFile(temporaryInfo, finalInfo) ||
		!privateAgentVaultKeyEnrollmentFile(finalInfo) || !os.SameFile(directoryBefore, directoryAfter) ||
		!privateAgentVaultKeyEnrollmentDirectory(directoryAfter) {
		return ErrAgentVaultKeyEnrollmentUnsafe
	}
	if err := validateAgentVaultKeyEnrollmentDirectories(home, directory); err != nil {
		if errors.Is(err, ErrAgentVaultKeyEnrollmentUnsafe) || errors.Is(err, os.ErrNotExist) {
			return ErrAgentVaultKeyEnrollmentUnsafe
		}
		return ErrAgentVaultKeyEnrollmentStorage
	}
	// First durably publish the completed canonical link. Removing and syncing
	// the temporary link afterward cannot make the canonical record partial.
	if err := syncAgentVaultKeyEnrollmentDirectory(directory); err != nil {
		return ErrAgentVaultKeyEnrollmentStorage
	}
	if err := os.Remove(temporaryPath); err != nil {
		return ErrAgentVaultKeyEnrollmentStorage
	}
	temporaryExists = false
	if err := syncAgentVaultKeyEnrollmentDirectory(directory); err != nil {
		return ErrAgentVaultKeyEnrollmentStorage
	}
	published = false
	return nil
}

func readAgentVaultKeyEnrollmentFile(home, directory, path string) ([]byte, error) {
	directoryBefore, err := os.Lstat(directory)
	if err != nil || !privateAgentVaultKeyEnrollmentDirectory(directoryBefore) {
		return nil, ErrAgentVaultKeyEnrollmentUnsafe
	}
	before, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrAgentVaultKeyEnrollmentUnavailable
	}
	if err != nil {
		return nil, ErrAgentVaultKeyEnrollmentStorage
	}
	if !privateAgentVaultKeyEnrollmentFile(before) {
		return nil, ErrAgentVaultKeyEnrollmentUnsafe
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrAgentVaultKeyEnrollmentUnavailable
	}
	if err != nil {
		return nil, ErrAgentVaultKeyEnrollmentStorage
	}
	defer func() { _ = file.Close() }()
	after, err := file.Stat()
	if err != nil {
		return nil, ErrAgentVaultKeyEnrollmentStorage
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, after) || !os.SameFile(after, current) ||
		!privateAgentVaultKeyEnrollmentFile(after) || !privateAgentVaultKeyEnrollmentFile(current) {
		return nil, ErrAgentVaultKeyEnrollmentUnsafe
	}
	if after.Size() <= 0 || after.Size() > MaxAgentVaultKeyEnrollmentStateBytes {
		return nil, ErrAgentVaultKeyEnrollmentInvalid
	}
	raw, err := io.ReadAll(io.LimitReader(file, MaxAgentVaultKeyEnrollmentStateBytes+1))
	if err != nil {
		return nil, ErrAgentVaultKeyEnrollmentStorage
	}
	if len(raw) == 0 || len(raw) > MaxAgentVaultKeyEnrollmentStateBytes {
		clear(raw)
		return nil, ErrAgentVaultKeyEnrollmentInvalid
	}
	finalFile, fileErr := file.Stat()
	finalPath, pathErr := os.Lstat(path)
	finalDirectory, directoryErr := os.Lstat(directory)
	if fileErr != nil || pathErr != nil || directoryErr != nil ||
		!os.SameFile(after, finalFile) || !os.SameFile(finalFile, finalPath) ||
		!privateAgentVaultKeyEnrollmentFile(finalFile) || !privateAgentVaultKeyEnrollmentFile(finalPath) ||
		!os.SameFile(directoryBefore, finalDirectory) || !privateAgentVaultKeyEnrollmentDirectory(finalDirectory) {
		clear(raw)
		return nil, ErrAgentVaultKeyEnrollmentUnsafe
	}
	if err := validateAgentVaultKeyEnrollmentDirectories(home, directory); err != nil {
		clear(raw)
		if errors.Is(err, ErrAgentVaultKeyEnrollmentUnsafe) || errors.Is(err, os.ErrNotExist) {
			return nil, ErrAgentVaultKeyEnrollmentUnsafe
		}
		return nil, ErrAgentVaultKeyEnrollmentStorage
	}
	return raw, nil
}

func ensureAgentVaultKeyEnrollmentDirectories(home, directory string) error {
	paths := agentVaultKeyEnrollmentDirectories(home, directory)
	if len(paths) == 0 {
		return ErrAgentVaultKeyEnrollmentScope
	}
	// WITSELF_HOME itself is trusted configuration but must still be a real,
	// owner-only directory; never descend through a symlink selected there.
	if err := os.MkdirAll(home, 0o700); err != nil {
		return ErrAgentVaultKeyEnrollmentStorage
	}
	homeInfo, err := os.Lstat(home)
	if err != nil || !homeInfo.IsDir() || homeInfo.Mode()&os.ModeSymlink != 0 {
		return ErrAgentVaultKeyEnrollmentUnsafe
	}
	for _, path := range paths {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				return ErrAgentVaultKeyEnrollmentStorage
			}
			if err := syncAgentVaultKeyEnrollmentDirectory(filepath.Dir(path)); err != nil {
				return ErrAgentVaultKeyEnrollmentStorage
			}
			info, err = os.Lstat(path)
		}
		if err != nil {
			return ErrAgentVaultKeyEnrollmentStorage
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return ErrAgentVaultKeyEnrollmentUnsafe
		}
		if err := os.Chmod(path, 0o700); err != nil {
			return ErrAgentVaultKeyEnrollmentStorage
		}
		info, err = os.Lstat(path)
		if err != nil || !privateAgentVaultKeyEnrollmentDirectory(info) {
			return ErrAgentVaultKeyEnrollmentUnsafe
		}
	}
	return nil
}

func validateAgentVaultKeyEnrollmentDirectories(home, directory string) error {
	homeInfo, err := os.Lstat(home)
	if err != nil {
		return err
	}
	if !homeInfo.IsDir() || homeInfo.Mode()&os.ModeSymlink != 0 {
		return ErrAgentVaultKeyEnrollmentUnsafe
	}
	for _, path := range agentVaultKeyEnrollmentDirectories(home, directory) {
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !privateAgentVaultKeyEnrollmentDirectory(info) {
			return ErrAgentVaultKeyEnrollmentUnsafe
		}
	}
	return nil
}

func agentVaultKeyEnrollmentDirectories(home, directory string) []string {
	root := filepath.Join(home, "keys")
	relative, err := filepath.Rel(root, directory)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil
	}
	paths := []string{root}
	current := root
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		paths = append(paths, current)
	}
	return paths
}

func privateAgentVaultKeyEnrollmentDirectory(info os.FileInfo) bool {
	return info.IsDir() && info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm() == 0o700
}

func privateAgentVaultKeyEnrollmentFile(info os.FileInfo) bool {
	return info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm() == 0o600
}

func classifyAgentVaultKeyEnrollmentCollision(path string, collision error) error {
	info, err := os.Lstat(path)
	if err != nil || !privateAgentVaultKeyEnrollmentFile(info) {
		return ErrAgentVaultKeyEnrollmentUnsafe
	}
	return collision
}

func removeAgentVaultKeyEnrollmentIfSame(path string, published os.FileInfo) {
	current, err := os.Lstat(path)
	if err == nil && os.SameFile(published, current) {
		_ = os.Remove(path)
	}
}

func removeAgentVaultKeyEnrollmentFile(path string) error {
	before, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return ErrAgentVaultKeyEnrollmentStorage
	}
	if !privateAgentVaultKeyEnrollmentFile(before) {
		return ErrAgentVaultKeyEnrollmentUnsafe
	}
	file, err := os.Open(path)
	if err != nil {
		return ErrAgentVaultKeyEnrollmentStorage
	}
	after, statErr := file.Stat()
	closeErr := file.Close()
	current, currentErr := os.Lstat(path)
	if statErr != nil || closeErr != nil || currentErr != nil || !os.SameFile(before, after) ||
		!os.SameFile(after, current) || !privateAgentVaultKeyEnrollmentFile(after) ||
		!privateAgentVaultKeyEnrollmentFile(current) {
		return ErrAgentVaultKeyEnrollmentUnsafe
	}
	if err := os.Remove(path); err != nil {
		return ErrAgentVaultKeyEnrollmentStorage
	}
	return nil
}

func agentVaultKeyEnrollmentCleanupNames(directory string) ([]string, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, ErrAgentVaultKeyEnrollmentStorage
	}
	allowedCanonical := map[string]bool{
		agentVaultKeyEnrollmentRequestFile: true,
		agentVaultKeyEnrollmentPrivateFile: true,
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		isTemporary := strings.HasPrefix(name, ".enrollment-write-") && strings.HasSuffix(name, ".tmp")
		if !allowedCanonical[name] && !isTemporary {
			return nil, ErrAgentVaultKeyEnrollmentUnsafe
		}
		info, err := os.Lstat(filepath.Join(directory, name))
		if err != nil || !privateAgentVaultKeyEnrollmentFile(info) {
			return nil, ErrAgentVaultKeyEnrollmentUnsafe
		}
		// Public request goes first, crash-left temporary links second, and the
		// canonical private recipient record last.
		if name != agentVaultKeyEnrollmentPrivateFile {
			names = append(names, name)
		}
	}
	if allowedCanonical[agentVaultKeyEnrollmentPrivateFile] {
		names = append(names, agentVaultKeyEnrollmentPrivateFile)
	}
	return names, nil
}

func syncAgentVaultKeyEnrollmentDirectory(directory string) error {
	dir, err := os.Open(directory)
	if err != nil {
		return err
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return err
	}
	return dir.Close()
}

func isDirectoryNotEmpty(err error) bool {
	return errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST)
}
