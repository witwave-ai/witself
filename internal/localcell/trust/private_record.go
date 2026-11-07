package trust

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// privateRecordPolicy is the bounded, native-independent projection of a
// Windows security descriptor. Principals are classified by the native reader;
// no SID, descriptor, path, or record data enters an error.
type privateRecordPrincipal uint8

const (
	recordUnknown privateRecordPrincipal = iota
	recordUser
	recordSystem
)

type privateRecordACE struct {
	principal privateRecordPrincipal
	rights    uint32
	flags     uint8
	kind      uint8
}
type privateRecordPolicy struct {
	valid            bool
	ownerUser        bool
	protected        bool
	daclPresent      bool
	userIsSystem     bool
	aces             []privateRecordACE
	reparse          bool
	ancestorReparse  bool
	directory        bool
	wantDirectory    bool
	identity         [3]uint64
	observedIdentity [3]uint64
}

const privateRecordFullAccess uint32 = 0x1f01ff

func checkPrivateRecordPolicy(p privateRecordPolicy) error {
	if !p.valid || !p.ownerUser || !p.protected || !p.daclPresent || p.reparse || p.ancestorReparse || p.directory != p.wantDirectory || p.identity != p.observedIdentity {
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
		if ace.kind != 0 || ace.flags != 0 || ace.rights != privateRecordFullAccess || (ace.principal != recordUser && ace.principal != recordSystem) || seen[ace.principal] {
			return ReasonStateIO
		}
		seen[ace.principal] = true
	}
	if !seen[recordUser] || (!p.userIsSystem && !seen[recordSystem]) {
		return ReasonStateIO
	}
	return nil
}

type privateRecordInfo struct {
	identity [3]uint64
	size     int64
}
type privateRecordHandle interface {
	io.Reader
	io.Writer
	Sync() error
	Close() error
}

// Implementations own every returned handle. removeOwned must remain bound to
// the created object, even after a failed confinement check. Windows retains
// writers through handle-based publication; Unix closes before pathname rename.
type privateRecordOps interface {
	checkDirectory(write bool) error
	inspect(name string) (privateRecordInfo, error)
	open(name string) (privateRecordHandle, error)
	inspectHandle(privateRecordHandle) (privateRecordInfo, error)
	create(name string, temporary bool) (privateRecordHandle, error)
	removeOwned(privateRecordHandle) error
	closeBeforePublish() bool
	recheck(name string, source privateRecordHandle, prior *privateRecordInfo) error
	// publish reports whether the name was replaced, even when returning an
	// error. Once published, cleanup must preserve the complete next record.
	publish(source privateRecordHandle, name string) (bool, error)
	complete(source privateRecordHandle, name string) error
	close() error
}

func privateRecordName(name string) bool {
	return name != "" && name != "." && name != ".." && filepath.Base(name) == name && !strings.ContainsAny(name, `/\\:`)
}

func readPrivateRecord(ops privateRecordOps, name string) ([]byte, error) {
	if !privateRecordName(name) || ops.checkDirectory(false) != nil {
		return nil, ReasonStateIO
	}
	observed, err := ops.inspect(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, os.ErrNotExist
	}
	if err != nil || observed.size < 0 || observed.size > maxStateBytes {
		return nil, ReasonStateIO
	}
	f, err := ops.open(name)
	if err != nil {
		return nil, ReasonStateIO
	}
	defer func() { _ = f.Close() }()
	opened, err := ops.inspectHandle(f)
	if err != nil || opened.identity != observed.identity || opened.size < 0 || opened.size > maxStateBytes {
		return nil, ReasonStateIO
	}
	b, err := io.ReadAll(io.LimitReader(f, maxStateBytes+1))
	if err != nil || len(b) > maxStateBytes {
		return nil, ReasonStateIO
	}
	return b, nil
}

func withPrivateRecordTransaction(ops privateRecordOps, lockName string, fn func() error) (result error) {
	if !privateRecordName(lockName) || ops.checkDirectory(true) != nil {
		return ReasonStateIO
	}
	fence, err := ops.create(lockName, false)
	if err != nil {
		return ReasonStateIO
	}
	defer func() {
		removeErr := ops.removeOwned(fence)
		closeErr := fence.Close()
		if result == nil && (removeErr != nil || closeErr != nil) {
			result = ReasonStateIO
		}
	}()
	if _, err := ops.inspectHandle(fence); err != nil {
		return ReasonStateIO
	}
	return fn()
}

func writePrivateRecord(ops privateRecordOps, name, prefix string, b []byte, beforePublish func() error) (result error) {
	if !privateRecordName(name) || !privateRecordName(prefix) || ops.checkDirectory(true) != nil {
		return ReasonStateIO
	}
	if len(b) > maxStateBytes {
		return ReasonStateInvalid
	}
	var prior *privateRecordInfo
	info, err := ops.inspect(name)
	if err == nil {
		prior = &info
	} else if !errors.Is(err, os.ErrNotExist) {
		return ReasonStateIO
	}
	f, err := ops.create(prefix, true)
	if err != nil {
		return ReasonStateIO
	}
	published, closed := false, false
	defer func() {
		if !published && ops.removeOwned(f) != nil && result == nil {
			result = ReasonStateIO
		}
		if !closed && f.Close() != nil && result == nil {
			result = ReasonStateIO
		}
	}()
	if _, err := ops.inspectHandle(f); err != nil {
		return ReasonStateIO
	}
	if n, err := f.Write(b); err != nil || n != len(b) {
		return ReasonStateIO
	}
	if err := f.Sync(); err != nil {
		return ReasonStateIO
	}
	if ops.closeBeforePublish() {
		closed = true
		if err := f.Close(); err != nil {
			return ReasonStateIO
		}
	}
	if beforePublish != nil && beforePublish() != nil {
		return ReasonStateIO
	}
	if ops.recheck(name, f, prior) != nil {
		return ReasonStateIO
	}
	published, err = ops.publish(f, name)
	if err != nil || !published {
		return ReasonStateIO
	}
	if ops.complete(f, name) != nil {
		return ReasonStateIO
	}
	return nil
}
