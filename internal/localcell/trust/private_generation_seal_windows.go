//go:build windows

package trust

import (
	"context"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unicode/utf16"
	"unsafe"
)

// These hooks are per instance and nil in production. An injected observation
// follows a real native observation; it is not evidence that an OS race occurred.
type windowsGenerationSealHooks struct {
	before   func(string, *windowsGenerationSealFile) error
	afterSet func(*windowsGenerationSealFile, bool) error
	observe  func(string, *windowsGenerationSealFile, *privateGenerationSealObservation)
}

type windowsGenerationSealOps struct {
	ctx            context.Context
	snapshot       *privateGenerationSealSnapshot
	parent         *windowsRecordOps
	hooks          *windowsGenerationSealHooks
	files          map[*windowsGenerationSealFile]bool
	readers        map[int]*windowsGenerationSealFile
	recovery       *windowsGenerationSealFile
	sealed         bool
	closeAttempted bool
}

type windowsGenerationSealFile struct {
	owner          *windowsGenerationSealOps
	file           *os.File
	index          int
	recovery       bool
	closeAttempted bool
	closed         bool
	access         uint32
	share          uint32
}

const (
	windowsGenerationSealWriteDAC = 0x00040000
	windowsGenerationSealRead     = 0x00120089 // FILE_GENERIC_READ
	windowsGenerationSealRX       = 0x001200a9 // FILE_GENERIC_READ | FILE_GENERIC_EXECUTE
	windowsGenerationSealProtect  = 0x80000000 // PROTECTED_DACL_SECURITY_INFORMATION
)

var (
	windowsGenerationSealSetSecurity = windowsRecordAdvapi32.NewProc("SetKernelObjectSecurity")
	windowsGenerationSealQueryDir    = windowsRecordNTDLL.NewProc("NtQueryDirectoryFile")
)

// The completed builder contributes only its copied native identities and
// names. No closed raw handle is examined or revived. Parent guards have been
// released by D0A; the new parent identities are fresh B1 observations, not
// claims of parent identity continuity across D0A completion.
func sealWindowsPrivateGeneration(ctx context.Context, g *privateGeneration, expected []privateGenerationSealExpected, hooks *windowsGenerationSealHooks) (*sealedPrivateGeneration, error) {
	if g == nil {
		return nil, ReasonStateInvalid
	}
	old, ok := g.ops.(*windowsGenerationOps)
	if !ok || old == nil || old.parent == nil || !old.closeAttempted || len(old.parent.guards) != 0 || len(old.files) != len(g.nodes) || len(g.nodes) == 0 || len(g.nodes) > privateGenerationMaxObjects {
		return nil, ReasonStateInvalid
	}
	identities := make([][3]uint64, len(g.nodes))
	var rootName string
	for i, n := range g.nodes {
		if n == nil {
			return nil, ReasonStateInvalid
		}
		f, ok := n.handle.(*windowsGenerationFile)
		if !ok || f == nil || f.file == nil || f.owner != old || !old.files[f] || !f.created || !f.identityKnown || !f.closeAttempted || f.deleted || f.directory != n.directory {
			return nil, ReasonStateInvalid
		}
		if i == 0 {
			rootName = filepath.Base(f.path)
			if f.parent != nil || !windowsRecordLeafName(rootName) || strings.Contains(rootName, "/") || filepath.Join(old.parent.dir, rootName) != f.path {
				return nil, ReasonStateInvalid
			}
		} else {
			parentName := ""
			if at := strings.LastIndexByte(n.name, '/'); at >= 0 {
				parentName = n.name[:at]
			}
			parent := g.names[strings.ToLower(parentName)]
			if parent == nil || parent.handle != f.parent || f.path != filepath.Join(old.parent.dir, rootName, filepath.FromSlash(n.name)) {
				return nil, ReasonStateInvalid
			}
		}
		identities[i] = f.identity
	}
	snapshot, err := snapshotPrivateGeneration(g, expected, identities)
	if err != nil {
		return nil, err
	}
	snapshot.parentPath, snapshot.rootName = old.parent.dir, rootName
	ops, err := newWindowsGenerationSealOps(ctx, snapshot, hooks, false)
	if err != nil {
		return nil, err
	}
	return sealPrivateGeneration(ctx, snapshot, ops)
}

// Fresh verification consumes a process-local expectation, not an authenticated
// serialized token. It never repairs ACLs and never acquires WRITE_DAC.
func openWindowsSealedPrivateGeneration(ctx context.Context, snapshot *privateGenerationSealSnapshot, hooks *windowsGenerationSealHooks) (*sealedPrivateGeneration, error) {
	owned, err := clonePrivateGenerationSealSnapshot(snapshot)
	if err != nil {
		return nil, err
	}
	ops, err := newWindowsGenerationSealOps(ctx, owned, hooks, true)
	if err != nil {
		return nil, err
	}
	return openSealedPrivateGeneration(ctx, owned, ops)
}

func newWindowsGenerationSealOps(ctx context.Context, snapshot *privateGenerationSealSnapshot, hooks *windowsGenerationSealHooks, sealed bool) (*windowsGenerationSealOps, error) {
	if snapshot == nil || !privateGenerationPath(snapshot.rootName) || strings.Contains(snapshot.rootName, "/") || len(snapshot.parentIdentities) > 128 {
		return nil, ReasonStateInvalid
	}
	if _, err := windowsRecordCleanPath(snapshot.parentPath); err != nil {
		return nil, ReasonStateInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	parent, err := newWindowsPrivateRecordOps(snapshot.parentPath, false, nil)
	if err != nil {
		return nil, ReasonStateIO
	}
	ops := &windowsGenerationSealOps{ctx: ctx, snapshot: snapshot, parent: parent, hooks: hooks, files: make(map[*windowsGenerationSealFile]bool), readers: make(map[int]*windowsGenerationSealFile), sealed: sealed}
	if len(snapshot.parentIdentities) == 0 && !sealed {
		for _, guard := range parent.guards {
			snapshot.parentIdentities = append(snapshot.parentIdentities, guard.identity)
		}
	}
	if ops.checkParent() != nil {
		_ = ops.close()
		return nil, ReasonStateIO
	}
	if err := ctx.Err(); err != nil {
		if ops.close() != nil {
			return nil, ReasonStateIO
		}
		return nil, err
	}
	return ops, nil
}

func (o *windowsGenerationSealOps) before(stage string, f *windowsGenerationSealFile) error {
	if o.hooks != nil && o.hooks.before != nil && o.hooks.before(stage, f) != nil {
		return ReasonStateIO
	}
	return nil
}

func (o *windowsGenerationSealOps) checkParent() error {
	if o.closeAttempted || o.parent == nil || len(o.parent.guards) == 0 || len(o.parent.guards) != len(o.snapshot.parentIdentities) || o.before("parent", nil) != nil {
		return ReasonStateIO
	}
	for i, g := range o.parent.guards {
		if g.identity != o.snapshot.parentIdentities[i] {
			return ReasonStateIO
		}
	}
	if o.parent.checkDirectory(false) != nil {
		return ReasonStateIO
	}
	return nil
}

func (o *windowsGenerationSealOps) registered(h privateGenerationSealHandle) (*windowsGenerationSealFile, error) {
	f, ok := h.(*windowsGenerationSealFile)
	if !ok || f == nil || f.owner != o || !o.files[f] || f.closeAttempted || f.file == nil || f.file.Fd() == uintptr(syscall.InvalidHandle) {
		return nil, ReasonStateIO
	}
	return f, nil
}

func (o *windowsGenerationSealOps) path(index int) string {
	return filepath.Join(o.snapshot.parentPath, o.snapshot.rootName, filepath.FromSlash(o.snapshot.nodes[index].name))
}

func (o *windowsGenerationSealOps) parentIndex(index int) int {
	name := o.snapshot.nodes[index].name
	parent := ""
	if at := strings.LastIndexByte(name, '/'); at >= 0 {
		parent = name[:at]
	}
	for i, node := range o.snapshot.nodes {
		if node.directory && node.name == parent {
			return i
		}
	}
	return -1
}

// Open existing leaves relative to retained directories with OBJ_DONT_REPARSE.
// The recovery handle requests no data-write, append, delete or owner right.
// WRITE_DAC/share compatibility requires native proof; an incompatible kernel
// refuses before any DACL mutation, without a broader-sharing fallback.
func (o *windowsGenerationSealOps) openNative(index int, access, share uint32) (syscall.Handle, error) {
	if index < 0 || index >= len(o.snapshot.nodes) || o.checkParent() != nil {
		return syscall.InvalidHandle, ReasonStateIO
	}
	parent := o.parent.guards[len(o.parent.guards)-1].handle
	name := o.snapshot.rootName
	if index != 0 {
		parentIndex := o.parentIndex(index)
		f := o.readers[parentIndex]
		if f == nil || o.checkTreeParent(parentIndex) != nil {
			return syscall.InvalidHandle, ReasonStateIO
		}
		parent = syscall.Handle(f.file.Fd())
		name = filepath.Base(filepath.FromSlash(o.snapshot.nodes[index].name))
	}
	chars, err := syscall.UTF16FromString(name)
	if err != nil || !windowsRecordLeafName(name) || windowsRecordNTCreate.Find() != nil {
		return syscall.InvalidHandle, ReasonStateIO
	}
	ntname := windowsRecordUnicodeString{Length: uint16((len(chars) - 1) * 2), MaximumLength: uint16(len(chars) * 2), Buffer: &chars[0]}
	oa := windowsRecordObjectAttributes{RootDirectory: parent, ObjectName: &ntname, Attributes: windowsRecordCaseInsensitive | windowsRecordDontReparse}
	oa.Length = uint32(unsafe.Sizeof(oa))
	options := uint32(windowsRecordSynchronousIO | windowsRecordOpenReparsePoint | windowsRecordNonDirectoryFile)
	if o.snapshot.nodes[index].directory {
		options = windowsRecordSynchronousIO | windowsRecordOpenReparsePoint | windowsRecordDirectoryFile
	}
	var handle syscall.Handle
	var iosb windowsRecordIOStatusBlock
	const fileOpen = 1
	if o.ctx.Err() != nil {
		return syscall.InvalidHandle, o.ctx.Err()
	}
	status, _, _ := windowsRecordNTCreate.Call(uintptr(unsafe.Pointer(&handle)), uintptr(access), uintptr(unsafe.Pointer(&oa)), uintptr(unsafe.Pointer(&iosb)), 0, 0, uintptr(share), fileOpen, uintptr(options), 0, 0)
	runtime.KeepAlive(chars)
	if uint32(status) != 0 {
		return syscall.InvalidHandle, ReasonStateIO
	}
	return handle, nil
}

func (o *windowsGenerationSealOps) checkTreeParent(index int) error {
	f := o.readers[index]
	if _, err := o.registered(f); err != nil {
		return ReasonStateIO
	}
	if index != 0 {
		if parent := o.parentIndex(index); parent < 0 || parent == index || o.checkTreeParent(parent) != nil {
			return ReasonStateIO
		}
	}
	observed, err := o.observeDirect(index, f, "ancestor")
	if err != nil || o.identity(index, observed) != nil {
		return ReasonStateIO
	}
	if index == 0 && o.sealed {
		return checkPrivateGenerationSealPolicy(observed.policy)
	}
	return checkPrivateRecordPolicy(observed.policy)
}

func (o *windowsGenerationSealOps) openRecovery() (privateGenerationSealHandle, []byte, error) {
	if o.recovery != nil || o.sealed || o.before("open-recovery", nil) != nil {
		return nil, nil, ReasonStateIO
	}
	access := uint32(windowsGenerationSealWriteDAC | windowsRecordReadControl | windowsRecordReadAttributes | syscall.SYNCHRONIZE)
	h, err := o.openNative(0, access, syscall.FILE_SHARE_READ)
	if err != nil {
		return nil, nil, ReasonStateIO
	}
	f := &windowsGenerationSealFile{owner: o, file: os.NewFile(uintptr(h), o.path(0)), index: 0, recovery: true, access: access, share: syscall.FILE_SHARE_READ}
	o.files[f], o.recovery = true, f
	original, err := windowsRecordSecurity(h)
	if err != nil {
		return f, nil, ReasonStateIO
	}
	p, err := windowsRecordPolicyFromSecurity(original)
	if err != nil || checkPrivateRecordPolicy(p) != nil {
		return f, nil, ReasonStateIO
	}
	return f, original, nil
}

func (o *windowsGenerationSealOps) openReader(index int) (privateGenerationSealHandle, error) {
	if index < 0 || index >= len(o.snapshot.nodes) || o.readers[index] != nil || o.before("open-reader", nil) != nil {
		return nil, ReasonStateIO
	}
	access := uint32(windowsGenerationSealRead)
	if o.snapshot.nodes[index].directory {
		access |= windowsRecordTraverse
	}
	// This exact sharing boundary is intentional, including for descendants
	// whose full-access DACLs otherwise permit write and delete opens.
	h, err := o.openNative(index, access, syscall.FILE_SHARE_READ)
	if err != nil {
		return nil, ReasonStateIO
	}
	f := &windowsGenerationSealFile{owner: o, file: os.NewFile(uintptr(h), o.path(index)), index: index, access: access, share: syscall.FILE_SHARE_READ}
	o.files[f], o.readers[index] = true, f
	return f, nil
}

func (o *windowsGenerationSealOps) projection(index int, h syscall.Handle) (privateGenerationSealObservation, error) {
	p, info, err := windowsRecordProjection(h, o.snapshot.nodes[index].directory)
	if err != nil {
		return privateGenerationSealObservation{}, ReasonStateIO
	}
	var raw syscall.ByHandleFileInformation
	if syscall.GetFileInformationByHandle(h, &raw) != nil || windowsRecordIdentity(raw) != p.identity || raw.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0 || (raw.FileAttributes&syscall.FILE_ATTRIBUTE_DIRECTORY != 0) != p.directory || (!p.directory && raw.NumberOfLinks != 1) {
		return privateGenerationSealObservation{}, ReasonStateIO
	}
	return privateGenerationSealObservation{policy: p, size: info.size, readOnly: raw.FileAttributes&syscall.FILE_ATTRIBUTE_READONLY != 0}, nil
}

func (o *windowsGenerationSealOps) identity(index int, observed privateGenerationSealObservation) error {
	node := o.snapshot.nodes[index]
	if observed.policy.identity != node.identity || observed.policy.observedIdentity != node.identity || observed.policy.reparse || observed.policy.ancestorReparse || observed.policy.directory != node.directory {
		return ReasonStateIO
	}
	return nil
}

// A named observation is freshly acquired and closed every time. It permits
// only read sharing and never supplies authority for a mutation by pathname.
func (o *windowsGenerationSealOps) observeDirect(index int, f *windowsGenerationSealFile, stage string) (privateGenerationSealObservation, error) {
	h, err := windowsRecordOpenMetadata(o.path(index), 0, syscall.FILE_SHARE_READ)
	if err != nil {
		return privateGenerationSealObservation{}, ReasonStateIO
	}
	named, observationErr := o.projection(index, h)
	closeErr := syscall.CloseHandle(h)
	if observationErr != nil || closeErr != nil || o.identity(index, named) != nil {
		return privateGenerationSealObservation{}, ReasonStateIO
	}
	observed := named
	if f != nil {
		if _, err := o.registered(f); err != nil || f.index != index {
			return privateGenerationSealObservation{}, ReasonStateIO
		}
		observed, err = o.projection(index, syscall.Handle(f.file.Fd()))
		if err != nil || o.identity(index, observed) != nil {
			return privateGenerationSealObservation{}, ReasonStateIO
		}
	}
	if o.hooks != nil && o.hooks.observe != nil {
		o.hooks.observe(stage, f, &observed)
	}
	return observed, nil
}

func (o *windowsGenerationSealOps) observe(index int, h privateGenerationSealHandle, stage string) (privateGenerationSealObservation, error) {
	if index < 0 || index >= len(o.snapshot.nodes) || o.checkParent() != nil || o.before(stage, nil) != nil {
		return privateGenerationSealObservation{}, ReasonStateIO
	}
	if index != 0 && o.checkTreeParent(o.parentIndex(index)) != nil {
		return privateGenerationSealObservation{}, ReasonStateIO
	}
	var f *windowsGenerationSealFile
	if h != nil {
		var err error
		f, err = o.registered(h)
		if err != nil {
			return privateGenerationSealObservation{}, err
		}
	}
	return o.observeDirect(index, f, stage)
}

func (o *windowsGenerationSealOps) inventory(ctx context.Context, index int, h privateGenerationSealHandle, limit int) ([]privateGenerationSealEntry, error) {
	f, err := o.registered(h)
	if err != nil || f.index != index || !o.snapshot.nodes[index].directory || limit < 0 || limit > privateGenerationMaxObjects || windowsGenerationSealQueryDir.Find() != nil {
		return nil, ReasonStateIO
	}
	entries := make([]privateGenerationSealEntry, 0)
	// FILE_DIRECTORY_INFORMATION: fixed 64-byte prefix followed by UTF-16
	// FileName. One result per synchronous call bounds work and allocation;
	// unknown or oversized names are rejected without entering that subtree.
	var buffer [1024]byte
	for call := 0; call < limit+3; call++ {
		if ctx.Err() != nil || o.checkTreeParent(index) != nil || o.checkParent() != nil {
			return nil, ReasonStateIO
		}
		var iosb windowsRecordIOStatusBlock
		restart := uintptr(0)
		if call == 0 {
			restart = 1
		}
		const fileDirectoryInformation = 1
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		status, _, _ := windowsGenerationSealQueryDir.Call(f.file.Fd(), 0, 0, 0, uintptr(unsafe.Pointer(&iosb)), uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)), fileDirectoryInformation, 1, 0, restart)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if uint32(status) == 0x80000006 { // STATUS_NO_MORE_FILES
			return entries, nil
		}
		if uint32(status) != 0 || iosb.Information < 64 || iosb.Information > uintptr(len(buffer)) || binary.LittleEndian.Uint32(buffer[:4]) != 0 {
			return nil, ReasonStateIO
		}
		length := int(binary.LittleEndian.Uint32(buffer[60:64]))
		attributes := binary.LittleEndian.Uint32(buffer[56:60])
		if length <= 0 || length%2 != 0 || length > int(iosb.Information)-64 || length > privateGenerationMaxComponent*2 {
			return nil, ReasonStateIO
		}
		encoded := make([]uint16, length/2)
		for i := range encoded {
			encoded[i] = binary.LittleEndian.Uint16(buffer[64+i*2 : 66+i*2])
		}
		name := string(utf16.Decode(encoded))
		if name == "." || name == ".." {
			continue
		}
		if attributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0 || !privateGenerationPath(name) || strings.Contains(name, "/") || len(entries) >= limit {
			return nil, ReasonStateIO
		}
		entries = append(entries, privateGenerationSealEntry{name: name, directory: attributes&syscall.FILE_ATTRIBUTE_DIRECTORY != 0})
	}
	return nil, ReasonStateIO
}

func windowsGenerationSealDescriptor(original []byte) ([]byte, error) {
	if len(original) < 20 || len(original) > 65536 {
		return nil, ReasonStateIO
	}
	p, err := windowsRecordPolicyFromSecurity(original)
	if err != nil || checkPrivateRecordPolicy(p) != nil {
		return nil, ReasonStateIO
	}
	sealed := append([]byte(nil), original...)
	acl := int(binary.LittleEndian.Uint32(sealed[16:20]))
	count := int(binary.LittleEndian.Uint16(sealed[acl+4 : acl+6]))
	position := acl + 8
	for range count {
		binary.LittleEndian.PutUint32(sealed[position+4:position+8], windowsGenerationSealRX)
		position += int(binary.LittleEndian.Uint16(sealed[position+2 : position+4]))
	}
	return sealed, nil
}

func (o *windowsGenerationSealOps) setRoot(h privateGenerationSealHandle, original []byte) error {
	f, err := o.registered(h)
	if err != nil || !f.recovery || f.index != 0 || f != o.recovery {
		return ReasonStateIO
	}
	restore := original != nil
	stage, post := "set-root", "post-set"
	if restore {
		stage, post = "restore-root", "post-restore"
	}
	if o.before(stage, f) != nil || o.checkParent() != nil {
		return ReasonStateIO
	}
	observed, err := o.observeDirect(0, f, "native-"+stage)
	if err != nil || o.identity(0, observed) != nil {
		return ReasonStateIO
	}
	descriptor := original
	if !restore {
		if checkPrivateRecordPolicy(observed.policy) != nil {
			return ReasonStateIO
		}
		current, readErr := windowsRecordSecurity(syscall.Handle(f.file.Fd()))
		if readErr != nil {
			return ReasonStateIO
		}
		descriptor, err = windowsGenerationSealDescriptor(current)
	} else {
		var policy privateRecordPolicy
		policy, err = windowsRecordPolicyFromSecurity(descriptor)
		if err == nil {
			err = checkPrivateRecordPolicy(policy)
		}
	}
	if err != nil || len(descriptor) < 20 || len(descriptor) > 65536 || windowsGenerationSealSetSecurity.Find() != nil {
		return ReasonStateIO
	}
	observed, err = o.observeDirect(0, f, "native-effect")
	if err != nil || o.identity(0, observed) != nil {
		return ReasonStateIO
	}
	if !restore && o.ctx.Err() != nil {
		return o.ctx.Err()
	}
	// Handle-only DACL write. Owner, attributes and every descendant are left
	// untouched. Restoration is allowed even after context cancellation.
	r, _, _ := windowsGenerationSealSetSecurity.Call(f.file.Fd(), windowsRecordDACLSecurity|windowsGenerationSealProtect, uintptr(unsafe.Pointer(&descriptor[0])))
	runtime.KeepAlive(descriptor)
	if r == 0 {
		return ReasonStateIO
	}
	o.sealed = !restore
	if o.hooks != nil && o.hooks.afterSet != nil && o.hooks.afterSet(f, restore) != nil {
		return ReasonStateIO
	}
	observed, err = o.observeDirect(0, f, post)
	if err != nil || o.identity(0, observed) != nil {
		return ReasonStateIO
	}
	if restore {
		return checkPrivateRecordPolicy(observed.policy)
	}
	// Isolated adapter post-set checker; a later readback cannot stand in for
	// this immediate observation of the actual setter result.
	if checkPrivateGenerationSealPolicy(observed.policy) != nil {
		return ReasonStateIO
	}
	return nil
}

func (f *windowsGenerationSealFile) ReadAt(b []byte, offset int64) (int, error) {
	if _, err := f.owner.registered(f); err != nil || f.recovery || f.owner.snapshot.nodes[f.index].directory || offset < 0 {
		return 0, ReasonStateIO
	}
	n, err := f.file.ReadAt(b, offset)
	if err != nil && err != io.EOF {
		return n, ReasonStateIO
	}
	return n, err
}

func (f *windowsGenerationSealFile) Close() error {
	if f.closeAttempted {
		return ReasonStateIO
	}
	f.closeAttempted = true
	if f.owner.before("close", f) != nil {
		return ReasonStateIO
	}
	if f.file.Close() != nil || f.owner.before("closed", f) != nil {
		return ReasonStateIO
	}
	f.closed = true
	return nil
}

func (o *windowsGenerationSealOps) close() error {
	if o.closeAttempted {
		return ReasonStateIO
	}
	o.closeAttempted = true
	hookErr := o.before("close-parent", nil)
	closeErr := o.parent.close()
	if hookErr != nil || closeErr != nil {
		return ReasonStateIO
	}
	return nil
}
