//go:build windows

package trust

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// These per-instance test hooks never supply a descriptor or native handle.
// afterCreate may change only the actual disposable object; observe runs after
// native readback and can isolate an otherwise unavailable race observation.
// Production leaves hooks nil.
type windowsGenerationHooks struct {
	before      func(string, *windowsGenerationFile) error
	afterCreate func(*windowsGenerationFile) error
	observe     func(string, *windowsGenerationFile, *privateRecordPolicy)
}

type windowsGenerationOps struct {
	parent         *windowsRecordOps
	hooks          *windowsGenerationHooks
	files          map[*windowsGenerationFile]bool
	closeAttempted bool
}

type windowsGenerationFile struct {
	owner           *windowsGenerationOps
	file            *os.File
	path            string
	parent          *windowsGenerationFile
	identity        [3]uint64
	identityKnown   bool
	directory       bool
	created         bool
	initialObserved bool
	closeAttempted  bool
	deleted         bool
}

// newWindowsPrivateGeneration opens an existing private NTFS parent and gives
// its retained guards to the shared operation protocol. Nothing here seals,
// stages, activates, publishes, or authorizes public Windows persistence.
func newWindowsPrivateGeneration(ctx context.Context, parentPath, root string, hooks *windowsGenerationHooks) (*privateGeneration, error) {
	parent, err := newWindowsPrivateRecordOps(parentPath, false, nil)
	if err != nil {
		return nil, ReasonStateIO
	}
	ops := &windowsGenerationOps{parent: parent, hooks: hooks, files: make(map[*windowsGenerationFile]bool)}
	return newPrivateGeneration(ctx, ops, root)
}

func (o *windowsGenerationOps) before(stage string, f *windowsGenerationFile) error {
	if o.hooks != nil && o.hooks.before != nil && o.hooks.before(stage, f) != nil {
		return ReasonStateIO
	}
	return nil
}

func (o *windowsGenerationOps) checkParent() error {
	if o.closeAttempted || o.parent == nil || o.before("parent", nil) != nil || o.parent.checkDirectory(false) != nil {
		return ReasonStateIO
	}
	return nil
}

func (o *windowsGenerationOps) registered(h privateGenerationHandle) (*windowsGenerationFile, error) {
	f, ok := h.(*windowsGenerationFile)
	if !ok || f == nil || f.owner != o || !o.files[f] || f.closeAttempted || f.deleted || f.file == nil || f.file.Fd() == uintptr(syscall.InvalidHandle) {
		return nil, ReasonStateIO
	}
	return f, nil
}

func (o *windowsGenerationOps) create(parent privateGenerationHandle, name string, directory bool) (privateGenerationHandle, error) {
	if !windowsRecordLeafName(name) || o.before("create", nil) != nil || o.checkParent() != nil {
		return nil, ReasonStateIO
	}
	var retained *windowsGenerationFile
	guard := o.parent.guards[len(o.parent.guards)-1]
	if parent != nil {
		var err error
		retained, err = o.registered(parent)
		if err != nil || !retained.directory || o.check(retained, true) != nil {
			return nil, ReasonStateIO
		}
		guard = windowsRecordGuard{path: retained.path, handle: syscall.Handle(retained.file.Fd()), identity: retained.identity}
	}
	path, err := windowsRecordCleanPath(filepath.Join(guard.path, name))
	if err != nil {
		return nil, ReasonStateIO
	}
	// Reuse only B1's creation primitive: FILE_CREATE, relative RootDirectory,
	// protected current-user/LocalSystem descriptor, and no-reparse flags.
	creator := windowsRecordOps{guards: []windowsRecordGuard{guard}}
	h, err := creator.createNative(name, directory)
	if err != nil {
		return nil, ReasonStateIO
	}
	f := &windowsGenerationFile{owner: o, file: os.NewFile(uintptr(h), path), path: path, parent: retained, directory: directory, created: true}
	o.files[f] = true
	var info syscall.ByHandleFileInformation
	if syscall.GetFileInformationByHandle(h, &info) != nil {
		// Ownership of the returned handle is known, identity is not. Shared
		// cleanup must preserve the object and attempt this handle's close.
		return f, ReasonStateIO
	}
	f.identity, f.identityKnown = windowsRecordIdentity(info), true
	if o.hooks != nil && o.hooks.afterCreate != nil && o.hooks.afterCreate(f) != nil {
		return f, ReasonStateIO
	}
	// The shared protocol registers f before its mandatory initial privacy
	// observation, so even a failed descriptor readback retains ownership.
	return f, nil
}

func (o *windowsGenerationOps) snapshot(f *windowsGenerationFile, h syscall.Handle, privacy bool, stage string) error {
	var p privateRecordPolicy
	if privacy {
		var err error
		p, _, err = windowsRecordProjection(h, f.directory)
		if err != nil {
			return ReasonStateIO
		}
	} else {
		var info syscall.ByHandleFileInformation
		if syscall.GetFileInformationByHandle(h, &info) != nil {
			return ReasonStateIO
		}
		p.identity = windowsRecordIdentity(info)
		p.observedIdentity = p.identity
		p.directory = info.FileAttributes&syscall.FILE_ATTRIBUTE_DIRECTORY != 0
		p.wantDirectory = f.directory
		p.reparse = info.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0
	}
	if o.hooks != nil && o.hooks.observe != nil {
		o.hooks.observe(stage, f, &p)
	}
	// Native mutation boundaries: retain observed reparse/type/identity and
	// refuse the initial created-object privacy snapshot before any payload.
	if !f.identityKnown || p.identity != f.identity || p.observedIdentity != f.identity || p.reparse || p.directory != f.directory {
		return ReasonStateIO
	}
	if privacy && checkPrivateRecordPolicy(p) != nil {
		return ReasonStateIO
	}
	return nil
}

func (o *windowsGenerationOps) checkFile(f *windowsGenerationFile, privacy bool) error {
	if _, err := o.registered(f); err != nil {
		return err
	}
	if f.parent != nil && o.checkFile(f.parent, privacy) != nil {
		return ReasonStateIO
	}
	stage := "retained"
	if privacy && f.created && !f.initialObserved {
		stage = "created"
		f.initialObserved = true
	}
	if o.snapshot(f, syscall.Handle(f.file.Fd()), privacy, stage) != nil {
		return ReasonStateIO
	}
	h, err := windowsRecordOpenMetadata(f.path, 0, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE)
	if err != nil {
		return ReasonStateIO
	}
	observedErr := o.snapshot(f, h, privacy, "named")
	closeErr := syscall.CloseHandle(h)
	if observedErr != nil || closeErr != nil {
		return ReasonStateIO
	}
	return nil
}

func (o *windowsGenerationOps) check(h privateGenerationHandle, privacy bool) error {
	f, err := o.registered(h)
	if err != nil || o.before("check", f) != nil || o.checkParent() != nil || o.checkFile(f, privacy) != nil {
		return ReasonStateIO
	}
	return nil
}

func (o *windowsGenerationOps) reopen(h privateGenerationHandle) (privateGenerationHandle, error) {
	f, err := o.registered(h)
	if err != nil || !f.created || f.directory || o.before("reopen", f) != nil || o.check(f, true) != nil {
		return nil, ReasonStateIO
	}
	opened, err := windowsRecordOpenMetadata(f.path, syscall.GENERIC_READ, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE)
	if err != nil {
		return nil, ReasonStateIO
	}
	reader := &windowsGenerationFile{owner: o, file: os.NewFile(uintptr(opened), f.path), path: f.path, parent: f.parent, identity: f.identity, identityKnown: true}
	o.files[reader] = true
	if o.check(reader, true) != nil {
		// The shared protocol owns and closes every returned handle, including
		// one whose readback failed. No raw handle is retried after Close.
		return reader, ReasonStateIO
	}
	return reader, nil
}

func (o *windowsGenerationOps) remove(h privateGenerationHandle) error {
	f, err := o.registered(h)
	if err != nil || !f.created || o.before("remove", f) != nil || o.check(f, false) != nil {
		return ReasonStateIO
	}
	if windowsRecordDeleteHandle(syscall.Handle(f.file.Fd())) != nil {
		return ReasonStateIO
	}
	f.deleted = true
	return nil
}

func (o *windowsGenerationOps) close() error {
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

func (f *windowsGenerationFile) Read(b []byte) (int, error) {
	if f.owner.before("read", f) != nil || f.owner.check(f, true) != nil {
		return 0, ReasonStateIO
	}
	n, err := f.file.Read(b)
	if err != nil && err != io.EOF {
		return n, ReasonStateIO
	}
	return n, err
}

func (f *windowsGenerationFile) Write(b []byte) (int, error) {
	if !f.created || f.directory || f.owner.before("write", f) != nil || f.owner.check(f, true) != nil {
		return 0, ReasonStateIO
	}
	n, err := f.file.Write(b)
	if err != nil {
		return n, ReasonStateIO
	}
	return n, nil
}

func (f *windowsGenerationFile) Sync() error {
	if !f.created || f.directory || f.owner.before("sync", f) != nil || f.owner.check(f, true) != nil || f.file.Sync() != nil {
		return ReasonStateIO
	}
	return nil
}

func (f *windowsGenerationFile) Close() error {
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
	if !f.created {
		delete(f.owner.files, f)
	}
	return nil
}

// Full-access descriptors intentionally remain writable. Share modes and
// repeated observations do not defeat every installing-user in-place reparse
// race, nor make a pathname check atomic with a later effect. File Sync and
// observed reopen do not establish durable directory entries after power loss.
