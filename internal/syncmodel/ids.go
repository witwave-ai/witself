package syncmodel

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// MaxCounter matches the nonnegative PostgreSQL bigint fixture storage bound.
const MaxCounter uint64 = 9223372036854775807

// MaxLifecycleEpoch is the exact-integer lifecycle interoperability bound.
const MaxLifecycleEpoch uint64 = 9007199254740991

// ErrIdentity rejects malformed or noncanonical typed identifiers.
var ErrIdentity = errors.New("sync model: invalid typed identity")

// ErrCounter rejects counters outside their exact-decimal wire range.
var ErrCounter = errors.New("sync model: invalid decimal counter")

func encodeID(prefix string, b []byte) string {
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}
func decodeID(text, prefix string, dst []byte) error {
	if !strings.HasPrefix(text, prefix) {
		return ErrIdentity
	}
	raw := strings.TrimPrefix(text, prefix)
	b, err := base64.RawURLEncoding.Strict().DecodeString(raw)
	if err != nil || len(b) != len(dst) || base64.RawURLEncoding.EncodeToString(b) != raw {
		return ErrIdentity
	}
	copy(dst, b)
	return nil
}
func decodeIDJSON(data []byte, prefix string, dst []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return ErrIdentity
	}
	return decodeID(text, prefix, dst)
}

// OperationID is an opaque 128-bit identity; tests may supply its exact bytes.
type OperationID [16]byte

// NewOperationID draws a full 128-bit identity from crypto/rand.
func NewOperationID() (OperationID, error) {
	var id OperationID
	if _, err := rand.Read(id[:]); err != nil {
		return OperationID{}, errors.New("sync model: identity entropy unavailable")
	}
	return id, nil
}

// ParseOperationID rejects noncanonical wire text before constructing the typed value.
func ParseOperationID(text string) (OperationID, error) {
	var id OperationID
	err := decodeID(text, "op_", id[:])
	return id, err
}

// String returns the canonical typed identifier spelling.
func (id OperationID) String() string { return encodeID("op_", id[:]) }

// MarshalText emits the explicit canonical wire representation.
func (id OperationID) MarshalText() ([]byte, error) { return []byte(id.String()), nil }

// UnmarshalText validates canonical input before replacing the receiver.
func (id *OperationID) UnmarshalText(data []byte) error {
	var next OperationID
	if err := decodeID(string(data), "op_", next[:]); err != nil {
		return err
	}
	*id = next
	return nil
}

// MarshalJSON emits the explicit canonical wire representation.
func (id OperationID) MarshalJSON() ([]byte, error) { return json.Marshal(id.String()) }

// UnmarshalJSON validates canonical input before replacing the receiver.
func (id *OperationID) UnmarshalJSON(data []byte) error {
	var next OperationID
	if err := decodeIDJSON(data, "op_", next[:]); err != nil {
		return err
	}
	*id = next
	return nil
}

// TransferID is an opaque 128-bit identity; tests may supply its exact bytes.
type TransferID [16]byte

// NewTransferID draws a full 128-bit identity from crypto/rand.
func NewTransferID() (TransferID, error) {
	var id TransferID
	if _, err := rand.Read(id[:]); err != nil {
		return TransferID{}, errors.New("sync model: identity entropy unavailable")
	}
	return id, nil
}

// ParseTransferID rejects noncanonical wire text before constructing the typed value.
func ParseTransferID(text string) (TransferID, error) {
	var id TransferID
	err := decodeID(text, "transfer_", id[:])
	return id, err
}

// String returns the canonical typed identifier spelling.
func (id TransferID) String() string { return encodeID("transfer_", id[:]) }

// MarshalText emits the explicit canonical wire representation.
func (id TransferID) MarshalText() ([]byte, error) { return []byte(id.String()), nil }

// UnmarshalText validates canonical input before replacing the receiver.
func (id *TransferID) UnmarshalText(data []byte) error {
	var next TransferID
	if err := decodeID(string(data), "transfer_", next[:]); err != nil {
		return err
	}
	*id = next
	return nil
}

// MarshalJSON emits the explicit canonical wire representation.
func (id TransferID) MarshalJSON() ([]byte, error) { return json.Marshal(id.String()) }

// UnmarshalJSON validates canonical input before replacing the receiver.
func (id *TransferID) UnmarshalJSON(data []byte) error {
	var next TransferID
	if err := decodeIDJSON(data, "transfer_", next[:]); err != nil {
		return err
	}
	*id = next
	return nil
}

// RestoreMarker is an opaque 128-bit identity; tests may supply its exact bytes.
type RestoreMarker [16]byte

// NewRestoreMarker draws a full 128-bit identity from crypto/rand.
func NewRestoreMarker() (RestoreMarker, error) {
	var id RestoreMarker
	if _, err := rand.Read(id[:]); err != nil {
		return RestoreMarker{}, errors.New("sync model: identity entropy unavailable")
	}
	return id, nil
}

// ParseRestoreMarker rejects noncanonical wire text before constructing the typed value.
func ParseRestoreMarker(text string) (RestoreMarker, error) {
	var id RestoreMarker
	err := decodeID(text, "restore_", id[:])
	return id, err
}

// String returns the canonical typed identifier spelling.
func (id RestoreMarker) String() string { return encodeID("restore_", id[:]) }

// MarshalText emits the explicit canonical wire representation.
func (id RestoreMarker) MarshalText() ([]byte, error) { return []byte(id.String()), nil }

// UnmarshalText validates canonical input before replacing the receiver.
func (id *RestoreMarker) UnmarshalText(data []byte) error {
	var next RestoreMarker
	if err := decodeID(string(data), "restore_", next[:]); err != nil {
		return err
	}
	*id = next
	return nil
}

// MarshalJSON emits the explicit canonical wire representation.
func (id RestoreMarker) MarshalJSON() ([]byte, error) { return json.Marshal(id.String()) }

// UnmarshalJSON validates canonical input before replacing the receiver.
func (id *RestoreMarker) UnmarshalJSON(data []byte) error {
	var next RestoreMarker
	if err := decodeIDJSON(data, "restore_", next[:]); err != nil {
		return err
	}
	*id = next
	return nil
}

// RevisionID is an opaque 128-bit identity; tests may supply its exact bytes.
type RevisionID [16]byte

// NewRevisionID draws a full 128-bit identity from crypto/rand.
func NewRevisionID() (RevisionID, error) {
	var id RevisionID
	if _, err := rand.Read(id[:]); err != nil {
		return RevisionID{}, errors.New("sync model: identity entropy unavailable")
	}
	return id, nil
}

// ParseRevisionID rejects noncanonical wire text before constructing the typed value.
func ParseRevisionID(text string) (RevisionID, error) {
	var id RevisionID
	err := decodeID(text, "revision_", id[:])
	return id, err
}

// String returns the canonical typed identifier spelling.
func (id RevisionID) String() string { return encodeID("revision_", id[:]) }

// MarshalText emits the explicit canonical wire representation.
func (id RevisionID) MarshalText() ([]byte, error) { return []byte(id.String()), nil }

// UnmarshalText validates canonical input before replacing the receiver.
func (id *RevisionID) UnmarshalText(data []byte) error {
	var next RevisionID
	if err := decodeID(string(data), "revision_", next[:]); err != nil {
		return err
	}
	*id = next
	return nil
}

// MarshalJSON emits the explicit canonical wire representation.
func (id RevisionID) MarshalJSON() ([]byte, error) { return json.Marshal(id.String()) }

// UnmarshalJSON validates canonical input before replacing the receiver.
func (id *RevisionID) UnmarshalJSON(data []byte) error {
	var next RevisionID
	if err := decodeIDJSON(data, "revision_", next[:]); err != nil {
		return err
	}
	*id = next
	return nil
}

// GenerationID is an opaque 128-bit identity; tests may supply its exact bytes.
type GenerationID [16]byte

// NewGenerationID draws a full 128-bit identity from crypto/rand.
func NewGenerationID() (GenerationID, error) {
	var id GenerationID
	if _, err := rand.Read(id[:]); err != nil {
		return GenerationID{}, errors.New("sync model: identity entropy unavailable")
	}
	return id, nil
}

// ParseGenerationID rejects noncanonical wire text before constructing the typed value.
func ParseGenerationID(text string) (GenerationID, error) {
	var id GenerationID
	err := decodeID(text, "generation_", id[:])
	return id, err
}

// String returns the canonical typed identifier spelling.
func (id GenerationID) String() string { return encodeID("generation_", id[:]) }

// MarshalText emits the explicit canonical wire representation.
func (id GenerationID) MarshalText() ([]byte, error) { return []byte(id.String()), nil }

// UnmarshalText validates canonical input before replacing the receiver.
func (id *GenerationID) UnmarshalText(data []byte) error {
	var next GenerationID
	if err := decodeID(string(data), "generation_", next[:]); err != nil {
		return err
	}
	*id = next
	return nil
}

// MarshalJSON emits the explicit canonical wire representation.
func (id GenerationID) MarshalJSON() ([]byte, error) { return json.Marshal(id.String()) }

// UnmarshalJSON validates canonical input before replacing the receiver.
func (id *GenerationID) UnmarshalJSON(data []byte) error {
	var next GenerationID
	if err := decodeIDJSON(data, "generation_", next[:]); err != nil {
		return err
	}
	*id = next
	return nil
}

// Commitment is private comparison evidence. Formatting never exposes its bytes.
// Wire encoding is explicit unpadded base64url, never the default byte-array codec.
type Commitment [32]byte

// Format redacts comparison bytes for every formatting verb.
func (c Commitment) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("[commitment redacted]"))
}

// MarshalJSON emits the explicit canonical wire representation.
func (c Commitment) MarshalJSON() ([]byte, error) { return json.Marshal(encodeID("commitment_", c[:])) }

// UnmarshalJSON validates canonical input before replacing the receiver.
func (c *Commitment) UnmarshalJSON(data []byte) error {
	var next Commitment
	if err := decodeIDJSON(data, "commitment_", next[:]); err != nil {
		return err
	}
	*c = next
	return nil
}

func parseCounter(text string, positive bool, maximum uint64) (uint64, error) {
	if text == "" || (len(text) > 1 && text[0] == '0') {
		return 0, ErrCounter
	}
	for _, r := range text {
		if r < '0' || r > '9' {
			return 0, ErrCounter
		}
	}
	value, err := strconv.ParseUint(text, 10, 64)
	if err != nil || value > maximum || (positive && value == 0) {
		return 0, ErrCounter
	}
	return value, nil
}
func counterJSON(data []byte, positive bool, maximum uint64) (uint64, error) {
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return 0, ErrCounter
	}
	return parseCounter(text, positive, maximum)
}

// LifecycleEpoch has canonical exact-decimal string wire representation.
type LifecycleEpoch uint64

// ParseLifecycleEpoch rejects noncanonical wire text before constructing the typed value.
func ParseLifecycleEpoch(text string) (LifecycleEpoch, error) {
	n, err := parseCounter(text, false, MaxLifecycleEpoch)
	return LifecycleEpoch(n), err
}

// Validate checks the typed counter storage and positivity bounds.
func (n LifecycleEpoch) Validate() error {
	_, err := parseCounter(strconv.FormatUint(uint64(n), 10), false, MaxLifecycleEpoch)
	return err
}

// MarshalJSON emits the explicit canonical wire representation.
func (n LifecycleEpoch) MarshalJSON() ([]byte, error) {
	if err := n.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(strconv.FormatUint(uint64(n), 10))
}

// UnmarshalJSON validates canonical input before replacing the receiver.
func (n *LifecycleEpoch) UnmarshalJSON(data []byte) error {
	next, err := counterJSON(data, false, MaxLifecycleEpoch)
	if err != nil {
		return err
	}
	*n = LifecycleEpoch(next)
	return nil
}

// CaptureEpoch has canonical exact-decimal string wire representation.
type CaptureEpoch uint64

// ParseCaptureEpoch rejects noncanonical wire text before constructing the typed value.
func ParseCaptureEpoch(text string) (CaptureEpoch, error) {
	n, err := parseCounter(text, true, MaxCounter)
	return CaptureEpoch(n), err
}

// Validate checks the typed counter storage and positivity bounds.
func (n CaptureEpoch) Validate() error {
	_, err := parseCounter(strconv.FormatUint(uint64(n), 10), true, MaxCounter)
	return err
}

// MarshalJSON emits the explicit canonical wire representation.
func (n CaptureEpoch) MarshalJSON() ([]byte, error) {
	if err := n.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(strconv.FormatUint(uint64(n), 10))
}

// UnmarshalJSON validates canonical input before replacing the receiver.
func (n *CaptureEpoch) UnmarshalJSON(data []byte) error {
	next, err := counterJSON(data, true, MaxCounter)
	if err != nil {
		return err
	}
	*n = CaptureEpoch(next)
	return nil
}

// GraphEpoch has canonical exact-decimal string wire representation.
type GraphEpoch uint64

// ParseGraphEpoch rejects noncanonical wire text before constructing the typed value.
func ParseGraphEpoch(text string) (GraphEpoch, error) {
	n, err := parseCounter(text, true, MaxCounter)
	return GraphEpoch(n), err
}

// Validate checks the typed counter storage and positivity bounds.
func (n GraphEpoch) Validate() error {
	_, err := parseCounter(strconv.FormatUint(uint64(n), 10), true, MaxCounter)
	return err
}

// MarshalJSON emits the explicit canonical wire representation.
func (n GraphEpoch) MarshalJSON() ([]byte, error) {
	if err := n.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(strconv.FormatUint(uint64(n), 10))
}

// UnmarshalJSON validates canonical input before replacing the receiver.
func (n *GraphEpoch) UnmarshalJSON(data []byte) error {
	next, err := counterJSON(data, true, MaxCounter)
	if err != nil {
		return err
	}
	*n = GraphEpoch(next)
	return nil
}

// GraphRevision has canonical exact-decimal string wire representation.
type GraphRevision uint64

// ParseGraphRevision rejects noncanonical wire text before constructing the typed value.
func ParseGraphRevision(text string) (GraphRevision, error) {
	n, err := parseCounter(text, false, MaxCounter)
	return GraphRevision(n), err
}

// Validate checks the typed counter storage and positivity bounds.
func (n GraphRevision) Validate() error {
	_, err := parseCounter(strconv.FormatUint(uint64(n), 10), false, MaxCounter)
	return err
}

// MarshalJSON emits the explicit canonical wire representation.
func (n GraphRevision) MarshalJSON() ([]byte, error) {
	if err := n.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(strconv.FormatUint(uint64(n), 10))
}

// UnmarshalJSON validates canonical input before replacing the receiver.
func (n *GraphRevision) UnmarshalJSON(data []byte) error {
	next, err := counterJSON(data, false, MaxCounter)
	if err != nil {
		return err
	}
	*n = GraphRevision(next)
	return nil
}

// Ordinal has canonical exact-decimal string wire representation.
type Ordinal uint64

// ParseOrdinal rejects noncanonical wire text before constructing the typed value.
func ParseOrdinal(text string) (Ordinal, error) {
	n, err := parseCounter(text, false, MaxCounter)
	return Ordinal(n), err
}

// Validate checks the typed counter storage and positivity bounds.
func (n Ordinal) Validate() error {
	_, err := parseCounter(strconv.FormatUint(uint64(n), 10), false, MaxCounter)
	return err
}

// MarshalJSON emits the explicit canonical wire representation.
func (n Ordinal) MarshalJSON() ([]byte, error) {
	if err := n.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(strconv.FormatUint(uint64(n), 10))
}

// UnmarshalJSON validates canonical input before replacing the receiver.
func (n *Ordinal) UnmarshalJSON(data []byte) error {
	next, err := counterJSON(data, false, MaxCounter)
	if err != nil {
		return err
	}
	*n = Ordinal(next)
	return nil
}
