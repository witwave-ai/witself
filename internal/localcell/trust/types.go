// Package trust implements the offline, fixture-only component manifest v1 contract.
// It is deliberately absent from every shipped command's dependency graph.
package trust

import "time"

// Algorithm and limits are frozen wire-profile v1 constants.
const (
	Algorithm            = "rsa3072-pkcs1v15-sha256"
	MaxEnvelopeBytes     = 8 << 10
	MaxComponentSetBytes = 256 << 10
	MaxAuthorityBytes    = 16 << 10
	MaxArchiveBytes      = 2 << 30
	MaxInventoryEntries  = 4096
)

// Reason is a closed, value-free refusal or acceptance code.
type Reason string

func (r Reason) Error() string { return string(r) }

// Fixed outcomes never contain document bytes or caller-supplied error details.
const (
	ReasonAccepted         Reason = "accepted"
	ReasonNoTrustRoot      Reason = "no_trust_root"
	ReasonDocumentTooLarge Reason = "document_too_large"
	ReasonEnvelopeTooLarge Reason = "envelope_too_large"
	ReasonInvalidJSON      Reason = "invalid_json"
	ReasonDuplicateKey     Reason = "duplicate_key"
	ReasonProfileViolation Reason = "profile_violation"
	ReasonUnknownCritical  Reason = "unknown_critical"
	ReasonUnknownField     Reason = "unknown_field"
	ReasonInvalidDocument  Reason = "invalid_document"
	ReasonWrongKind        Reason = "wrong_kind"
	ReasonWrongAlgorithm   Reason = "wrong_algorithm"
	ReasonInvalidKey       Reason = "invalid_key"
	ReasonInvalidEncoding  Reason = "invalid_encoding"
	ReasonSignatureLength  Reason = "signature_length"
	ReasonSignatureCount   Reason = "signature_count"
	ReasonSignatureInvalid Reason = "signature_invalid"
	ReasonUnknownKey       Reason = "unknown_key"
	ReasonRevokedKey       Reason = "revoked_key"
	ReasonWrongSigner      Reason = "wrong_signer"
	ReasonStateInvalid     Reason = "state_invalid"
	ReasonStateIO          Reason = "state_io"
	ReasonSequence         Reason = "sequence_invalid"
	ReasonReplay           Reason = "replay"
	ReasonRollbackDenied   Reason = "rollback_denied"
	ReasonWrongTag         Reason = "wrong_tag"
	ReasonWrongPlatform    Reason = "wrong_platform"
	ReasonSchema           Reason = "schema_invalid"
	ReasonPostgreSQL       Reason = "postgresql_invalid"
	ReasonExtensions       Reason = "extensions_unsupported"
	ReasonRoles            Reason = "roles_invalid"
	ReasonPath             Reason = "path_invalid"
	ReasonInventory        Reason = "inventory_invalid"
	ReasonMode             Reason = "mode_invalid"
	ReasonNotInstalled     Reason = "not_installed"
	ReasonMixedSet         Reason = "mixed_set"
	ReasonExpired          Reason = "expired"
	ReasonNotYetValid      Reason = "not_yet_valid"
	ReasonUnknownClock     Reason = "unknown_clock"
)

// Freshness is the explicit clock-relative validity classification.
type Freshness string

// Freshness values report time eligibility independently of acceptance.
const (
	Fresh        Freshness = "fresh"
	Expired      Freshness = "expired"
	UnknownClock Freshness = "unknown_clock"
	NotYetValid  Freshness = "not_yet_valid"
)

// Clock is a caller-supplied time and trust decision.
type Clock struct {
	Now     time.Time
	Trusted bool
}

// Outcome reports value-free verification evidence.
type Outcome struct {
	Accepted           bool      `json:"accepted"`
	Reason             Reason    `json:"reason"`
	Freshness          Freshness `json:"freshness"`
	KeyID              string    `json:"key_id,omitempty"`
	RootSequence       uint64    `json:"root_sequence,omitempty"`
	DelegationSequence uint64    `json:"delegation_sequence,omitempty"`
	ReleaseSequence    uint64    `json:"release_sequence,omitempty"`
}

// PublicKey contains an SPKI public key and its digest identifier.
type PublicKey struct {
	KeyID string `json:"key_id"`
	SPKI  string `json:"spki"`
}

// Signature contains one detached RSA signature.
type Signature struct {
	KeyID     string `json:"key_id"`
	Signature string `json:"signature"`
}

// Envelope is the strictly bounded detached signature container.
type Envelope struct {
	FormatVersion int         `json:"format_version"`
	Kind          string      `json:"kind"`
	Algorithm     string      `json:"algorithm"`
	Signatures    []Signature `json:"signatures"`
}

// ComponentSet binds a complete release to its platform inventories.
type ComponentSet struct {
	FormatVersion      int        `json:"format_version"`
	Critical           []string   `json:"critical"`
	Kind               string     `json:"kind"`
	RootSequence       uint64     `json:"root_sequence"`
	DelegationSequence uint64     `json:"delegation_sequence"`
	ReleaseSequence    uint64     `json:"release_sequence"`
	ReleaseTag         string     `json:"release_tag"`
	Channel            string     `json:"channel"`
	IssuedAt           string     `json:"issued_at"`
	ExpiresAt          string     `json:"expires_at"`
	Source             Source     `json:"source"`
	Provenance         Provenance `json:"provenance"`
	License            string     `json:"license"`
	SBOM               string     `json:"sbom"`
	Rollback           Rollback   `json:"rollback"`
	Platforms          []Platform `json:"platforms"`
}

// Source identifies the source repository and exact commit.
type Source struct {
	Repository string `json:"repository"`
	Commit     string `json:"commit"`
}

// Provenance binds the checksum digest and independent Sigstore bundle path.
type Provenance struct {
	ChecksumsSHA256 string `json:"checksums_sha256"`
	SigstoreBundle  string `json:"sigstore_bundle"`
}

// Rollback names lower release sequences authorized by this release.
type Rollback struct {
	AllowedTargets []uint64 `json:"allowed_targets"`
}

// Platform describes a complete component selection for one target.
type Platform struct {
	OS         string      `json:"os"`
	Arch       string      `json:"arch"`
	MinOS      string      `json:"min_os"`
	GlibcFloor string      `json:"glibc_floor,omitempty"`
	Components []Component `json:"components"`
}

// Component is the immutable declaration for one payload archive.
type Component struct {
	manifestDigest     string
	Role               string           `json:"role"`
	Version            string           `json:"version"`
	Archive            Archive          `json:"archive"`
	Inventory          []InventoryEntry `json:"inventory"`
	DependsOn          []string         `json:"depends_on"`
	PostgreSQL         *PostgreSQL      `json:"postgresql,omitempty"`
	RequiredExtensions []Extension      `json:"required_extensions"`
	Schema             Schema           `json:"schema"`
	Ownership          Ownership        `json:"ownership"`
}

// Archive declares exact compressed bytes and their format.
type Archive struct {
	Name   string `json:"name"`
	Size   uint64 `json:"size"`
	SHA256 string `json:"sha256"`
	Format string `json:"format"`
}

// InventoryEntry declares one regular file and its required permissions.
type InventoryEntry struct {
	Path       string `json:"path"`
	Size       uint64 `json:"size"`
	SHA256     string `json:"sha256"`
	Mode       string `json:"mode"`
	Executable bool   `json:"executable"`
}

// PostgreSQL describes the required PG16 distribution and data layout.
type PostgreSQL struct {
	Major             uint64   `json:"major"`
	Version           string   `json:"version"`
	Distribution      string   `json:"distribution"`
	Build             string   `json:"build"`
	RuntimeLibraries  []string `json:"runtime_libraries"`
	DataLayoutVersion uint64   `json:"data_layout_version"`
}

// Extension reserves the extension compatibility schema; v1 lists are empty.
type Extension struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	ABI     string `json:"abi"`
}

// Schema declares the target within its accepted schema interval.
type Schema struct {
	Target      uint64 `json:"target"`
	AcceptedMin uint64 `json:"accepted_min"`
	AcceptedMax uint64 `json:"accepted_max"`
}

// Ownership partitions inventory files by package or private payload ownership.
type Ownership struct {
	PackageOwnedPaths   []string `json:"package_owned_paths"`
	PrivatePayloadPaths []string `json:"private_payload_paths"`
}

// Delegation grants release-key authority from the current root.
type Delegation struct {
	FormatVersion      int         `json:"format_version"`
	Critical           []string    `json:"critical"`
	Kind               string      `json:"kind"`
	RootSequence       uint64      `json:"root_sequence"`
	DelegationSequence uint64      `json:"delegation_sequence"`
	IssuedAt           string      `json:"issued_at"`
	ExpiresAt          string      `json:"expires_at"`
	Keys               []PublicKey `json:"keys"`
	RevokedKeyIDs      []string    `json:"revoked_key_ids"`
}

// RootRotation requests the next root sequence with dual possession proof.
type RootRotation struct {
	FormatVersion int       `json:"format_version"`
	Critical      []string  `json:"critical"`
	Kind          string    `json:"kind"`
	RootSequence  uint64    `json:"root_sequence"`
	NewSequence   uint64    `json:"new_sequence"`
	IssuedAt      string    `json:"issued_at"`
	ExpiresAt     string    `json:"expires_at"`
	NewRoot       PublicKey `json:"new_root"`
}
