package trust

import (
	"context"
	"io"
	"strings"
)

// This is single-owner, internal preparation only. Full-access private trees
// remain writable; completion is neither sealing nor publication. File Sync
// and observed reopen do not establish power-loss durability of directory entries.
// The object and byte caps are conservative subsets of the authenticated trust
// inventory limits (including directories in the object cap). The smaller ASCII
// path/depth caps avoid native alias and Unicode case-fold ambiguity.
const (
	privateGenerationMaxObjects   = MaxInventoryEntries
	privateGenerationMaxBytes     = MaxArchiveBytes
	privateGenerationMaxDepth     = 16 // includes the generation root
	privateGenerationMaxPath      = 240
	privateGenerationMaxComponent = 64
)

type privateGenerationHandle interface {
	io.Reader
	io.Writer
	Sync() error
	Close() error
}

// A non-nil handle returned with an error is still owned by the caller. create
// exclusively creates, with privacy at creation, relative to the retained parent
// (nil denotes the supplied existing parent). check observes retained ancestors,
// the retained object, and its current name; cleanup may omit only DACL policy.
// remove is handle-bound and independently refuses unknown identities. close
// attempts all adapter-owned ancestor handles, without retrying ambiguous closes.
type privateGenerationOps interface {
	checkParent() error
	create(parent privateGenerationHandle, name string, directory bool) (privateGenerationHandle, error)
	check(h privateGenerationHandle, privacy bool) error
	reopen(h privateGenerationHandle) (privateGenerationHandle, error)
	remove(h privateGenerationHandle) error
	close() error
}

type privateGenerationNode struct {
	handle    privateGenerationHandle
	name      string
	directory bool
	size      int64
	synced    bool
	// attempted and closed are deliberately distinct: an error does not tell
	// us whether a raw native handle remains live or has already been reused.
	attempted bool
	closed    bool
}

type privateGeneration struct {
	ops       privateGenerationOps
	nodes     []*privateGenerationNode
	readers   []*privateGenerationNode
	names     map[string]*privateGenerationNode
	bytes     int64
	terminal  bool
	completed bool
	result    error
	opsClosed bool // close attempted, not a closure assertion on failure
}

func privateGenerationPath(name string) bool {
	if name == "" || len(name) > privateGenerationMaxPath {
		return false
	}
	parts := strings.Split(name, "/")
	if len(parts) >= privateGenerationMaxDepth {
		return false
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || len(part) > privateGenerationMaxComponent || strings.HasSuffix(part, ".") {
			return false
		}
		for _, c := range part {
			switch {
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
			default:
				return false
			}
		}
		base, _, _ := strings.Cut(strings.ToUpper(part), ".")
		switch base {
		case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$", "CLOCK$":
			return false
		}
		if len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9' {
			return false
		}
	}
	return true
}

// newPrivateGeneration takes ownership of ops even on refusal. root is one new
// component under the already-existing private parent, never an adopted tree.
func newPrivateGeneration(ctx context.Context, ops privateGenerationOps, root string) (*privateGeneration, error) {
	g := &privateGeneration{ops: ops, names: make(map[string]*privateGenerationNode)}
	if !privateGenerationPath(root) || strings.Contains(root, "/") {
		return nil, g.fail(ReasonStateInvalid)
	}
	if err := g.ready(ctx); err != nil {
		return nil, err
	}
	if ops.checkParent() != nil {
		return nil, g.fail(ReasonStateIO)
	}
	if err := g.ready(ctx); err != nil {
		return nil, err
	}
	h, err := ops.create(nil, root, true)
	if h != nil {
		g.nodes = append(g.nodes, &privateGenerationNode{handle: h, directory: true})
		g.names[""] = g.nodes[0]
	}
	if err != nil || h == nil || ops.check(h, true) != nil {
		return nil, g.fail(ReasonStateIO)
	}
	if err := g.ready(ctx); err != nil {
		return nil, err
	}
	return g, nil
}

func (g *privateGeneration) ready(ctx context.Context) error {
	if g.terminal {
		if g.completed {
			return ReasonStateIO
		}
		return g.result
	}
	if err := ctx.Err(); err != nil {
		return g.fail(err)
	}
	return nil
}

func (g *privateGeneration) add(ctx context.Context, name string, directory bool) (*privateGenerationNode, error) {
	if err := g.ready(ctx); err != nil {
		return nil, err
	}
	if !privateGenerationPath(name) || len(g.nodes) >= privateGenerationMaxObjects || g.names[strings.ToLower(name)] != nil {
		return nil, g.fail(ReasonStateInvalid)
	}
	parentName, leaf := "", name
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		parentName, leaf = name[:i], name[i+1:]
	}
	parent := g.names[strings.ToLower(parentName)]
	if parent == nil || !parent.directory || parent.name != parentName {
		return nil, g.fail(ReasonStateInvalid)
	}
	if g.ops.check(parent.handle, true) != nil {
		return nil, g.fail(ReasonStateIO)
	}
	if err := g.ready(ctx); err != nil {
		return nil, err
	}
	h, err := g.ops.create(parent.handle, leaf, directory)
	var node *privateGenerationNode
	if h != nil {
		node = &privateGenerationNode{handle: h, name: name, directory: directory}
		g.nodes = append(g.nodes, node)
		g.names[strings.ToLower(name)] = node
	}
	if err != nil || h == nil || g.ops.check(h, true) != nil {
		return nil, g.fail(ReasonStateIO)
	}
	if err := g.ready(ctx); err != nil {
		return nil, err
	}
	return node, nil
}

func (g *privateGeneration) mkdir(ctx context.Context, name string) error {
	_, err := g.add(ctx, name, true)
	return err
}

// write consumes exactly expected bytes, probing one extra byte without writing
// it. The reader must itself return promptly; context cancellation is observed
// before and after each reader operation and before every native effect.
func (g *privateGeneration) write(ctx context.Context, name string, expected int64, src io.Reader) error {
	if err := g.ready(ctx); err != nil {
		return err
	}
	if expected < 0 || expected > privateGenerationMaxBytes-g.bytes || src == nil {
		return g.fail(ReasonStateInvalid)
	}
	n, err := g.add(ctx, name, false)
	if err != nil {
		return err
	}
	buf := make([]byte, 32<<10)
	remaining := expected
	for remaining > 0 {
		if err := g.ready(ctx); err != nil {
			return err
		}
		want := int64(len(buf))
		if remaining < want {
			want = remaining
		}
		count, readErr := src.Read(buf[:want])
		if err := g.ready(ctx); err != nil {
			return err
		}
		if count <= 0 || count > int(want) || (readErr != nil && readErr != io.EOF) || (readErr == io.EOF && int64(count) != remaining) {
			return g.fail(ReasonStateIO)
		}
		if g.ops.check(n.handle, true) != nil { // pre-write identity refusal
			return g.fail(ReasonStateIO)
		}
		if err := g.ready(ctx); err != nil {
			return err
		}
		written, writeErr := n.handle.Write(buf[:count])
		if writeErr != nil || written != count {
			return g.fail(ReasonStateIO)
		}
		remaining -= int64(count)
	}
	if err := g.ready(ctx); err != nil {
		return err
	}
	count, readErr := src.Read(buf[:1])
	if err := g.ready(ctx); err != nil {
		return err
	}
	if count != 0 || readErr != io.EOF {
		return g.fail(ReasonStateIO)
	}
	if g.ops.check(n.handle, true) != nil {
		return g.fail(ReasonStateIO)
	}
	if err := g.ready(ctx); err != nil {
		return err
	}
	if n.handle.Sync() != nil { // successful sync is a completion prerequisite
		return g.fail(ReasonStateIO)
	}
	if err := g.ready(ctx); err != nil {
		return err
	}
	n.size, n.synced = expected, true
	g.bytes += expected
	return nil
}

// read reopens only registered, synced files. Returned bytes are observations,
// not proof of immutable contents; the private policy deliberately permits writes.
func (g *privateGeneration) read(ctx context.Context, name string) ([]byte, error) {
	if err := g.ready(ctx); err != nil {
		return nil, err
	}
	n := g.names[strings.ToLower(name)]
	if !privateGenerationPath(name) || n == nil || n.name != name || n.directory || !n.synced {
		return nil, g.fail(ReasonStateInvalid)
	}
	if g.ops.check(n.handle, true) != nil { // pre-reopen identity refusal
		return nil, g.fail(ReasonStateIO)
	}
	if err := g.ready(ctx); err != nil {
		return nil, err
	}
	h, err := g.ops.reopen(n.handle)
	var reader *privateGenerationNode
	if h != nil {
		reader = &privateGenerationNode{handle: h}
		g.readers = append(g.readers, reader)
	}
	if err != nil || h == nil || g.ops.check(h, true) != nil {
		return nil, g.fail(ReasonStateIO)
	}
	if err := g.ready(ctx); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(contextReader{ctx: ctx, r: h}, n.size+1))
	if ctx.Err() != nil {
		return nil, g.fail(ctx.Err())
	}
	if err != nil || int64(len(data)) != n.size || g.ops.check(h, true) != nil {
		return nil, g.fail(ReasonStateIO)
	}
	if g.closeHandle(reader) != nil {
		return nil, g.fail(ReasonStateIO)
	}
	g.readers = g.readers[:len(g.readers)-1]
	if err := g.ready(ctx); err != nil {
		return nil, err
	}
	return data, nil
}

func (g *privateGeneration) closeHandle(n *privateGenerationNode) error {
	if n.attempted {
		if !n.closed {
			return ReasonStateIO
		}
		return nil
	}
	n.attempted = true
	if n.handle.Close() != nil {
		return ReasonStateIO
	}
	n.closed = true
	return nil
}

func (g *privateGeneration) closeOps() error {
	if g.opsClosed {
		return ReasonStateIO
	}
	g.opsClosed = true
	if g.ops.close() != nil {
		return ReasonStateIO
	}
	return nil
}

// complete is idempotent only after success. No writable/delete handle escapes a
// confirmed success. A close error is terminal uncertainty, never a retry of the
// raw handle. Already-closed objects may remain after a failed completion.
func (g *privateGeneration) complete(ctx context.Context) error {
	if g.terminal && g.completed {
		return nil
	}
	if err := g.ready(ctx); err != nil {
		return err
	}
	for _, n := range g.nodes {
		if err := g.ready(ctx); err != nil {
			return err
		}
		if !n.directory && !n.synced || g.ops.check(n.handle, true) != nil {
			return g.fail(ReasonStateIO)
		}
	}
	for i := len(g.nodes) - 1; i >= 0; i-- {
		if err := g.ready(ctx); err != nil {
			return err
		}
		if g.closeHandle(g.nodes[i]) != nil {
			return g.fail(ReasonStateIO)
		}
	}
	if g.closeOps() != nil {
		return g.fail(ReasonStateIO)
	}
	if err := g.ready(ctx); err != nil {
		return err
	}
	g.terminal, g.completed = true, true
	return nil
}

func (g *privateGeneration) fail(cause error) error {
	if g.terminal {
		return g.result
	}
	g.terminal, g.result = true, cause
	uncertain := false
	// Readback handles close first. Each reverse-order deletion precedes that
	// object's close; Windows delete-pending children must close before parent
	// deletion. Never reacquire by name or retry a possibly reused raw handle.
	for i := len(g.readers) - 1; i >= 0; i-- {
		if g.closeHandle(g.readers[i]) != nil {
			uncertain = true
		}
	}
	for i := len(g.nodes) - 1; i >= 0; i-- {
		n := g.nodes[i]
		if n.attempted || g.ops.check(n.handle, false) != nil || g.ops.remove(n.handle) != nil {
			uncertain = true
		}
		if g.closeHandle(n) != nil {
			uncertain = true
		}
	}
	if !g.opsClosed && g.closeOps() != nil {
		uncertain = true
	}
	if uncertain {
		g.result = ReasonStateIO
	}
	return g.result
}

func (g *privateGeneration) abort() error {
	if g.terminal {
		return g.result // successful trees are never removed by a later abort
	}
	return g.fail(ReasonStateIO)
}
