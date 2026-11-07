package trust

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"runtime"
	"sync"
)

// ReleaseRecord retains the security high-water mark even during an authorized rollback.
type ReleaseRecord struct {
	Sequence       uint64   `json:"sequence"`
	Tag            string   `json:"tag"`
	Digest         string   `json:"digest"`
	AllowedTargets []uint64 `json:"allowed_targets"`
}

// InstalledRecord binds an offline start to exact bytes committed by AcceptSet.
type InstalledRecord struct {
	EnvelopeDigest     string `json:"envelope_digest"`
	KeyID              string `json:"key_id"`
	RootSequence       uint64 `json:"root_sequence"`
	DelegationSequence uint64 `json:"delegation_sequence"`
	ReleaseSequence    uint64 `json:"release_sequence"`
	ReleaseTag         string `json:"release_tag"`
	OS                 string `json:"os"`
	Arch               string `json:"arch"`
	IssuedAt           string `json:"issued_at"`
	ExpiresAt          string `json:"expires_at"`
}

// StateSnapshot is local private state, not a portable signed authority document.
type StateSnapshot struct {
	FormatVersion      int                        `json:"format_version"`
	InitialRootID      string                     `json:"initial_root_id"`
	RootSequence       uint64                     `json:"root_sequence"`
	Root               PublicKey                  `json:"root"`
	RootHistory        []string                   `json:"root_history"`
	DelegationSequence uint64                     `json:"delegation_sequence"`
	DelegationDigest   string                     `json:"delegation_digest"`
	Delegation         *Delegation                `json:"delegation,omitempty"`
	Releases           map[string]ReleaseRecord   `json:"releases"`
	Installed          map[string]InstalledRecord `json:"installed"`
}

// TrustState serializes a read/validate/write transaction. Implementations must
// leave the previous state intact if fn returns an error or a pre-rename write
// fails. A post-rename directory-sync failure returns an error but may have
// committed the complete next state; callers must reload, never lower state.
// A transaction callback must not call back into the same state adapter.
//
//revive:disable-next-line:exported TrustState is the frozen public API name.
type TrustState interface {
	Load() (StateSnapshot, error)
	Update(fn func(*StateSnapshot) error) error
}

func emptyState() StateSnapshot {
	return StateSnapshot{FormatVersion: 1, Releases: map[string]ReleaseRecord{}, Installed: map[string]InstalledRecord{}}
}

func cloneState(s StateSnapshot) StateSnapshot {
	// State contains only JSON-compatible values; a round trip also detaches every
	// caller-owned slice and map before exposing or committing a transaction.
	b, _ := json.Marshal(s)
	var result StateSnapshot
	_ = json.Unmarshal(b, &result)
	return result
}

// MemoryState provides an isolated, concurrency-safe ephemeral trust state.
type MemoryState struct {
	mu    sync.Mutex
	state StateSnapshot
}

// NewMemoryState constructs an empty private in-memory state adapter.
func NewMemoryState() *MemoryState { return &MemoryState{state: emptyState()} }

// Load returns a detached snapshot of the committed in-memory state.
func (s *MemoryState) Load() (StateSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.FormatVersion == 0 {
		s.state = emptyState()
	}
	return cloneState(s.state), nil
}

// Update commits one serialized in-memory transaction if its callback succeeds.
func (s *MemoryState) Update(fn func(*StateSnapshot) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.FormatVersion == 0 {
		s.state = emptyState()
	}
	next := cloneState(s.state)
	if err := fn(&next); err != nil {
		return err
	}
	s.state = cloneState(next)
	return nil
}

// FileState stores trust-state.json in a caller-owned 0700 directory. A
// cross-process exclusive lock refuses concurrent or interrupted writers; it
// never guesses that another writer's lock is stale.
type FileState struct {
	dir          string
	mu           sync.Mutex
	beforeRename func() error // private crash-injection seam, used only by tests
}

// NewFileState opens or creates a caller-selected 0700 Unix private directory.
// Windows persistence refuses until the full native persistence boundary is proven.
func NewFileState(dir string) (*FileState, error) {
	// The standard library cannot establish owner-private Windows ACLs through
	// chmod. Refuse that unsupported persistence boundary rather than imply it.
	if runtime.GOOS == "windows" {
		return nil, ReasonStateIO
	}
	if dir == "" {
		return nil, ReasonStateIO
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, ReasonStateIO
	}
	state := &FileState{dir: dir}
	if err := state.checkDirectory(); err != nil {
		return nil, err
	}
	return state, nil
}

func (s *FileState) checkDirectory() error {
	info, err := os.Lstat(s.dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0700 {
		return ReasonStateIO
	}
	return nil
}

// Load reads a bounded, private committed state file without acquiring a writer fence.
func (s *FileState) Load() (StateSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load()
}

const maxStateBytes = 8 << 20

func (s *FileState) load() (StateSnapshot, error) {
	ops, err := newPrivateRecordOps(s.dir, false)
	if err != nil {
		return StateSnapshot{}, err
	}
	defer func() { _ = ops.close() }()
	b, err := readPrivateRecord(ops, "trust-state.json")
	if errors.Is(err, os.ErrNotExist) {
		return emptyState(), nil
	}
	if err != nil {
		return StateSnapshot{}, err
	}
	if err := StrictProfile(b); err != nil {
		return StateSnapshot{}, ReasonStateInvalid
	}
	var state StateSnapshot
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return StateSnapshot{}, ReasonStateInvalid
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return StateSnapshot{}, ReasonStateInvalid
	}
	if state.FormatVersion != 1 || state.Releases == nil || state.Installed == nil {
		return StateSnapshot{}, ReasonStateInvalid
	}
	return state, nil
}

// Update fences competing file writers and atomically commits a successful callback.
func (s *FileState) Update(fn func(*StateSnapshot) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ops, err := newPrivateRecordOps(s.dir, false)
	if err != nil {
		return err
	}
	defer func() { _ = ops.close() }()
	return withPrivateRecordTransaction(ops, ".trust-state.lock", func() error {
		next, err := s.load()
		if err != nil {
			return err
		}
		if err := fn(&next); err != nil {
			return err
		}
		b, err := json.Marshal(next)
		if err != nil || len(b) > maxStateBytes {
			return ReasonStateInvalid
		}
		return writePrivateRecord(ops, "trust-state.json", ".trust-state-", b, s.beforeRename)
	})
}
