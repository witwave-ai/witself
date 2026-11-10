package trust

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"strings"
)

// FILE_GENERIC_READ | FILE_GENERIC_EXECUTE, explicitly mapped (not GENERIC_*).
// This root-only policy deliberately leaves descendants at B1 full access.
const privateGenerationSealReadExecute uint32 = 0x1200a9

func checkPrivateGenerationSealPolicy(p privateRecordPolicy) error {
	if !p.valid || !p.ownerUser || !p.protected || !p.daclPresent || p.reparse || p.ancestorReparse || !p.directory || !p.wantDirectory || p.identity != p.observedIdentity {
		return ReasonStateIO
	}
	want := 2
	if p.userIsSystem {
		want = 1
	}
	if len(p.aces) != want {
		return ReasonStateIO
	}
	seen := map[privateRecordPrincipal]bool{}
	for _, ace := range p.aces {
		if ace.kind != 0 || ace.flags != 0 || ace.rights != privateGenerationSealReadExecute || (ace.principal != recordUser && ace.principal != recordSystem) || seen[ace.principal] {
			return ReasonStateIO
		}
		seen[ace.principal] = true
	}
	if !seen[recordUser] || (!p.userIsSystem && !seen[recordSystem]) {
		return ReasonStateIO
	}
	return nil
}

// These labels are caller expectations, not signature/authentication evidence
// or an NTFS executable-bit distinction. The snapshot has process-local scope.
type privateGenerationSealExpected struct {
	name       string
	directory  bool
	size       int64
	digest     [32]byte
	mode       uint32
	executable bool
	metadata   bool
}

type privateGenerationSealNode struct {
	privateGenerationSealExpected
	identity [3]uint64
}

type privateGenerationSealSnapshot struct {
	nodes            []privateGenerationSealNode
	parentPath       string
	rootName         string
	parentIdentities [][3]uint64
}

func validPrivateGenerationSealNodes(nodes []privateGenerationSealNode) bool {
	if len(nodes) == 0 || len(nodes) > privateGenerationMaxObjects {
		return false
	}
	names := make(map[string]privateGenerationSealNode, len(nodes))
	identities := make(map[[3]uint64]bool, len(nodes))
	var total int64
	for i, n := range nodes {
		if len(n.name) > privateGenerationMaxPath || n.identity == [3]uint64{} || identities[n.identity] || n.size < 0 || n.size > privateGenerationMaxBytes-total {
			return false
		}
		identities[n.identity] = true
		key := strings.ToLower(n.name)
		if _, exists := names[key]; exists {
			return false
		}
		if i == 0 {
			if n.name != "" || !n.directory || n.mode != 0500 {
				return false
			}
		} else {
			if !privateGenerationPath(n.name) {
				return false
			}
			parent := privateGenerationSealParent(n.name)
			p, exists := names[strings.ToLower(parent)]
			if !exists || !p.directory || p.name != parent {
				return false
			}
			if n.directory && n.mode != 0700 {
				return false
			}
		}
		if n.directory {
			if n.size != 0 || n.digest != [32]byte{} || n.executable || n.metadata {
				return false
			}
		} else {
			if n.metadata {
				if n.mode != 0600 || n.executable {
					return false
				}
			} else if (n.mode != 0644 || n.executable) && (n.mode != 0755 || !n.executable) {
				return false
			}
			total += n.size
		}
		names[key] = n
	}
	return true
}

func privateGenerationSealParent(name string) string {
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		return name[:i]
	}
	return ""
}

// Only the native constructor supplies identities, copied from known completed
// D0A nodes. This function never dereferences a closed raw handle or resumes D0A.
func snapshotPrivateGeneration(g *privateGeneration, expected []privateGenerationSealExpected, identities [][3]uint64) (*privateGenerationSealSnapshot, error) {
	if g == nil || g.ops == nil || !g.terminal || !g.completed || g.result != nil || !g.opsClosed || len(g.readers) != 0 || len(g.nodes) != len(expected) || len(g.nodes) != len(identities) || len(g.names) != len(g.nodes) || len(g.nodes) == 0 || len(g.nodes) > privateGenerationMaxObjects {
		return nil, ReasonStateInvalid
	}
	wanted := make(map[string]privateGenerationSealExpected, len(expected))
	for _, e := range expected {
		if len(e.name) > privateGenerationMaxPath {
			return nil, ReasonStateInvalid
		}
		key := strings.ToLower(e.name)
		if _, exists := wanted[key]; exists {
			return nil, ReasonStateInvalid
		}
		wanted[key] = e
	}
	s := &privateGenerationSealSnapshot{nodes: make([]privateGenerationSealNode, len(g.nodes))}
	var total int64
	for i, n := range g.nodes {
		if n == nil || len(n.name) > privateGenerationMaxPath || n.handle == nil || !n.attempted || !n.closed || (!n.directory && !n.synced) || g.names[strings.ToLower(n.name)] != n {
			return nil, ReasonStateInvalid
		}
		e, exists := wanted[strings.ToLower(n.name)]
		if !exists || e.name != n.name || e.directory != n.directory || e.size != n.size {
			return nil, ReasonStateInvalid
		}
		s.nodes[i] = privateGenerationSealNode{privateGenerationSealExpected: e, identity: identities[i]}
		if n.size < 0 || n.size > privateGenerationMaxBytes-total {
			return nil, ReasonStateInvalid
		}
		total += n.size
	}
	if total != g.bytes || !validPrivateGenerationSealNodes(s.nodes) {
		return nil, ReasonStateInvalid
	}
	return s, nil
}

func clonePrivateGenerationSealSnapshot(s *privateGenerationSealSnapshot) (*privateGenerationSealSnapshot, error) {
	if s == nil || len(s.parentIdentities) > 128 || len(s.parentPath) > 4*260 || !validPrivateGenerationSealNodes(s.nodes) {
		return nil, ReasonStateInvalid
	}
	return &privateGenerationSealSnapshot{
		nodes: append([]privateGenerationSealNode(nil), s.nodes...), parentPath: s.parentPath, rootName: s.rootName,
		parentIdentities: append([][3]uint64(nil), s.parentIdentities...),
	}, nil
}

type privateGenerationSealHandle interface {
	ReadAt([]byte, int64) (int, error)
	Close() error
}

type privateGenerationSealObservation struct {
	policy   privateRecordPolicy
	size     int64
	readOnly bool
}

type privateGenerationSealEntry struct {
	name      string // one immediate child component, never a relative subtree
	directory bool
}

// All returned handles, even alongside errors, transfer to the protocol. observe
// checks retained and current named identities (nil handle means named only).
// inventory uses bounded retained-directory native enumeration. setRoot changes
// only the root DACL by handle; nil means seal, non-nil means exact restoration.
// No operation can delete, rename, truncate or write payload data.
type privateGenerationSealOps interface {
	checkParent() error
	openRecovery() (privateGenerationSealHandle, []byte, error)
	openReader(index int) (privateGenerationSealHandle, error)
	observe(index int, h privateGenerationSealHandle, stage string) (privateGenerationSealObservation, error)
	inventory(ctx context.Context, index int, h privateGenerationSealHandle, limit int) ([]privateGenerationSealEntry, error)
	setRoot(h privateGenerationSealHandle, original []byte) error
	close() error
}

type privateGenerationSealOwned struct {
	handle    privateGenerationSealHandle
	attempted bool
	closed    bool
}

func (h *privateGenerationSealOwned) close() error {
	if h == nil {
		return nil
	}
	if h.attempted {
		if h.closed {
			return nil
		}
		return ReasonStateIO
	}
	h.attempted = true
	if h.handle.Close() != nil {
		return ReasonStateIO
	}
	h.closed = true
	return nil
}

// Single owner only. A committed session retains read-only tree handles and B1
// ancestor metadata guards. Closing never unseals; child DELETE and ordinary
// descendant writes after closure remain outside this root-only guarantee.
type sealedPrivateGeneration struct {
	snapshot     *privateGenerationSealSnapshot
	ops          privateGenerationSealOps
	readers      []*privateGenerationSealOwned
	sealed       bool
	terminal     bool
	result       error
	opsAttempted bool
}

func (s *sealedPrivateGeneration) close() error {
	if s.terminal {
		return s.result
	}
	s.terminal = true
	for i := len(s.readers) - 1; i >= 0; i-- {
		if s.readers[i].close() != nil {
			s.result = ReasonStateIO
		}
	}
	if !s.opsAttempted && s.ops != nil {
		s.opsAttempted = true
		if s.ops.close() != nil {
			s.result = ReasonStateIO
		}
	}
	return s.result
}

func (s *sealedPrivateGeneration) fail(cause error) error {
	if s.close() != nil {
		return ReasonStateIO
	}
	s.result = cause
	return cause
}

func (s *sealedPrivateGeneration) check(ctx context.Context, index int, h privateGenerationSealHandle, stage string, policy bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.ops.checkParent() != nil {
		return ReasonStateIO
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p, err := s.ops.observe(index, h, stage)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	n := s.snapshot.nodes[index]
	if err != nil || p.policy.identity != n.identity || p.policy.observedIdentity != n.identity || p.policy.reparse || p.policy.ancestorReparse || p.policy.directory != n.directory || p.policy.wantDirectory != n.directory {
		return ReasonStateIO
	}
	if !policy {
		return nil
	}
	if !n.directory && (p.readOnly || p.size != n.size) {
		return ReasonStateIO
	}
	if index == 0 && s.sealed {
		return checkPrivateGenerationSealPolicy(p.policy)
	}
	return checkPrivateRecordPolicy(p.policy)
}

// Observe every already-retained ancestor before an open or inventory/read
// effect. The native adapter independently checks current names as well.
func (s *sealedPrivateGeneration) ancestors(ctx context.Context, index int) error {
	name := s.snapshot.nodes[index].name
	for name != "" {
		name = privateGenerationSealParent(name)
		for i, n := range s.snapshot.nodes {
			if n.name == name {
				if i >= len(s.readers) || s.readers[i] == nil || s.check(ctx, i, s.readers[i].handle, "ancestor", true) != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					return ReasonStateIO
				}
				break
			}
		}
	}
	return nil
}

func (s *sealedPrivateGeneration) acquire(ctx context.Context) error {
	for i := range s.snapshot.nodes {
		if err := s.ancestors(ctx, i); err != nil {
			return err
		}
		if err := s.check(ctx, i, nil, "pre-open", true); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		h, err := s.ops.openReader(i)
		if h != nil {
			s.readers = append(s.readers, &privateGenerationSealOwned{handle: h})
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil || h == nil {
			return ReasonStateIO
		}
		if err := s.check(ctx, i, h, "reader", true); err != nil {
			return err
		}
	}
	return nil
}

func (s *sealedPrivateGeneration) inventory(ctx context.Context, index int) error {
	if err := s.ancestors(ctx, index); err != nil {
		return err
	}
	h := s.readers[index].handle
	if err := s.check(ctx, index, h, "pre-inventory", true); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	entries, err := s.ops.inventory(ctx, index, h, privateGenerationMaxObjects)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil || len(entries) > privateGenerationMaxObjects {
		return ReasonStateIO
	}
	expected := map[string]bool{}
	dir := s.snapshot.nodes[index].name
	for i, n := range s.snapshot.nodes {
		if i > 0 && privateGenerationSealParent(n.name) == dir {
			leaf := n.name
			if dir != "" {
				leaf = strings.TrimPrefix(n.name, dir+"/")
			}
			expected[leaf] = n.directory
		}
	}
	seen := map[string]bool{}
	for _, e := range entries {
		kind, exists := expected[e.name]
		if !exists { // exact-set boundary: never follow an undeclared subtree
			return ReasonStateIO
		}
		if !privateGenerationPath(e.name) || strings.Contains(e.name, "/") || seen[strings.ToLower(e.name)] || kind != e.directory {
			return ReasonStateIO
		}
		seen[strings.ToLower(e.name)] = true
	}
	for name := range expected {
		if !seen[strings.ToLower(name)] {
			return ReasonStateIO
		}
	}
	return s.check(ctx, index, h, "post-inventory", true)
}

func (s *sealedPrivateGeneration) bytes(ctx context.Context, index int, dst io.Writer) error {
	n := s.snapshot.nodes[index]
	h := s.readers[index].handle
	digest := sha256.New()
	buf := make([]byte, 32<<10)
	var offset int64
	for {
		if err := s.ancestors(ctx, index); err != nil {
			return err
		}
		if err := s.check(ctx, index, h, "pre-read", true); err != nil {
			return err
		}
		want := min(int64(len(buf)), n.size-offset+1)
		if err := ctx.Err(); err != nil {
			return err
		}
		count, err := h.ReadAt(buf[:want], offset)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if count < 0 || count > int(want) || int64(count) > n.size-offset || (err != nil && err != io.EOF) || (count == 0 && err == nil) {
			return ReasonStateIO
		}
		if count > 0 {
			_, _ = digest.Write(buf[:count])
			if dst != nil {
				if written, writeErr := dst.Write(buf[:count]); writeErr != nil || written != count {
					return ReasonStateIO
				}
			}
			offset += int64(count)
		}
		if err := s.check(ctx, index, h, "post-read", true); err != nil {
			return err
		}
		if err == io.EOF {
			if offset != n.size || !bytes.Equal(digest.Sum(nil), n.digest[:]) {
				return ReasonStateIO
			}
			return nil
		}
	}
}

func (s *sealedPrivateGeneration) verifyTree(ctx context.Context) error {
	for i, n := range s.snapshot.nodes {
		if err := s.check(ctx, i, s.readers[i].handle, "verify", true); err != nil {
			return err
		}
		if n.directory {
			if err := s.inventory(ctx, i); err != nil {
				return err
			}
		} else if err := s.bytes(ctx, i, nil); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (s *sealedPrivateGeneration) verify(ctx context.Context) error {
	if s.terminal {
		return ReasonStateIO
	}
	if err := s.verifyTree(ctx); err != nil {
		return s.fail(err)
	}
	return nil
}

func (s *sealedPrivateGeneration) read(ctx context.Context, name string) ([]byte, error) {
	if s.terminal {
		return nil, ReasonStateIO
	}
	index := -1
	for i, n := range s.snapshot.nodes {
		if n.name == name && !n.directory {
			index = i
			break
		}
	}
	if index < 0 {
		return nil, ReasonStateInvalid
	}
	if err := s.verify(ctx); err != nil {
		return nil, err
	}
	var data bytes.Buffer
	if err := s.bytes(ctx, index, &data); err != nil {
		return nil, s.fail(err)
	}
	return data.Bytes(), nil
}

func newPrivateGenerationSealSession(snapshot *privateGenerationSealSnapshot, ops privateGenerationSealOps, sealed bool) (*sealedPrivateGeneration, error) {
	s := &sealedPrivateGeneration{ops: ops, sealed: sealed}
	owned, err := clonePrivateGenerationSealSnapshot(snapshot)
	if err != nil || ops == nil {
		return nil, s.fail(ReasonStateInvalid)
	}
	s.snapshot = owned
	return s, nil
}

func openSealedPrivateGeneration(ctx context.Context, snapshot *privateGenerationSealSnapshot, ops privateGenerationSealOps) (*sealedPrivateGeneration, error) {
	s, err := newPrivateGenerationSealSession(snapshot, ops, true)
	if err != nil {
		return nil, err
	}
	if err := s.acquire(ctx); err != nil {
		return nil, s.fail(err)
	}
	if err := s.verify(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

func sealPrivateGeneration(ctx context.Context, snapshot *privateGenerationSealSnapshot, ops privateGenerationSealOps) (*sealedPrivateGeneration, error) {
	s, err := newPrivateGenerationSealSession(snapshot, ops, false)
	if err != nil {
		return nil, err
	}
	var recovery *privateGenerationSealOwned
	var original []byte
	changed := false
	fail := func(cause error) (*sealedPrivateGeneration, error) {
		uncertain := false
		if changed {
			// Cleanup intentionally ignores cancelled context. Never reopen by
			// name or retry a possibly closed/reused recovery handle.
			if recovery == nil || recovery.attempted {
				uncertain = true
			} else if s.check(context.Background(), 0, recovery.handle, "rollback", false) != nil { // rollback identity boundary
				uncertain = true
			} else if ops.setRoot(recovery.handle, original) != nil {
				uncertain = true
			} else {
				s.sealed = false
				if s.check(context.Background(), 0, recovery.handle, "restored", true) != nil {
					uncertain = true
				}
			}
		}
		if s.close() != nil {
			uncertain = true
		}
		if recovery.close() != nil {
			uncertain = true
		}
		if uncertain {
			return nil, ReasonStateIO
		}
		return nil, cause
	}
	if err := s.check(ctx, 0, nil, "pre-open", true); err != nil {
		return fail(err)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	h, descriptor, err := ops.openRecovery()
	if h != nil {
		recovery = &privateGenerationSealOwned{handle: h}
	}
	if ctx.Err() != nil {
		return fail(ctx.Err())
	}
	if err != nil || h == nil || len(descriptor) < 20 || len(descriptor) > 65536 {
		return fail(ReasonStateIO)
	}
	original = append([]byte(nil), descriptor...)
	if err := s.check(ctx, 0, h, "recovery", true); err != nil {
		return fail(err)
	}
	if err := s.acquire(ctx); err != nil {
		return fail(err)
	}
	if err := s.verifyTree(ctx); err != nil {
		return fail(err)
	}
	if err := s.check(ctx, 0, h, "pre-set", true); err != nil { // immediate setter identity boundary
		return fail(err)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	changed = true // a failed native setter can still have changed the DACL
	if ops.setRoot(h, nil) != nil {
		if ctx.Err() != nil {
			return fail(ctx.Err())
		}
		return fail(ReasonStateIO)
	}
	s.sealed = true
	if err := s.check(ctx, 0, h, "post-set", true); err != nil {
		return fail(err)
	}
	if err := s.verifyTree(ctx); err != nil {
		return fail(err)
	}
	if err := ctx.Err(); err != nil { // last cancellation check before commit
		return fail(err)
	}
	if recovery.close() != nil { // confirmed privileged closure is the commit
		return fail(ReasonStateIO)
	}
	// No cancellation check here: rollback capability no longer exists.
	return s, nil
}
