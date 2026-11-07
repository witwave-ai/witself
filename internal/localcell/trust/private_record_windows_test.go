//go:build windows

package trust

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// These fixtures exercise only the private record adapter. They do not assert
// StageGeneration, Activate, read-only payloads, or power-loss durability.
func windowsRecordTestHome(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, key := range []string{"WITSELF_HOME", "DSH_HOME", "HOME", "USERPROFILE"} {
		t.Setenv(key, root)
	}
	return root
}

func windowsRecordTestOps(t *testing.T, root string, hooks *windowsRecordHooks) *windowsRecordOps {
	t.Helper()
	ops, err := newWindowsPrivateRecordOps(filepath.Join(root, "records"), true, hooks)
	if err != nil {
		t.Fatal("private adapter construction failed")
	}
	t.Cleanup(func() {
		if ops.close() != nil {
			t.Error("private adapter close failed")
		}
	})
	return ops
}

func windowsRecordTestWrite(t *testing.T, ops privateRecordOps, name string, data []byte) {
	t.Helper()
	if withPrivateRecordTransaction(ops, ".record-fence", func() error {
		return writePrivateRecord(ops, name, ".record-tmp-", data, nil)
	}) != nil {
		t.Fatal("private record transaction failed")
	}
}

func windowsRecordTestRead(t *testing.T, ops privateRecordOps, name string, expected []byte) {
	t.Helper()
	got, err := readPrivateRecord(ops, name)
	if err != nil || !bytes.Equal(got, expected) {
		t.Fatal("private record content differs")
	}
}

func TestWindowsTrustPrivateRecordCreateAndReopen(t *testing.T) {
	root := windowsRecordTestHome(t)
	seen := map[string]int{}
	hooks := &windowsRecordHooks{observe: func(stage string, handle syscall.Handle, policy privateRecordPolicy) error {
		if checkPrivateRecordPolicy(policy) != nil {
			t.Error("native intermediate or final privacy policy differs")
			return ReasonStateIO
		}
		var raw syscall.ByHandleFileInformation
		if syscall.GetFileInformationByHandle(handle, &raw) != nil || policy.identity != [3]uint64{uint64(raw.VolumeSerialNumber), uint64(raw.FileIndexHigh), uint64(raw.FileIndexLow)} {
			t.Error("native observation identity differs from handle")
			return ReasonStateIO
		}
		seen[stage]++
		return nil
	}}
	ops := windowsRecordTestOps(t, root, hooks)
	state := emptyState()
	state.RootSequence, state.DelegationSequence = 17, 9
	state.Releases["stable"] = ReleaseRecord{Sequence: 23, Tag: "record-fixture", AllowedTargets: []uint64{18}}
	generation := Generation{Version: 1, Name: "record-generation", Sequence: 23, Document: []byte("signed-document-fixture"), Envelope: []byte("signed-envelope-fixture"), Trust: state}
	journal := HandoffJournal{Version: 1, Revision: 5, Generations: map[string]HandoffRecord{"record-generation": {State: "staged"}}}
	for _, fixture := range []struct {
		name  string
		value any
	}{
		{"trust-state.json", state},
		{"generation.json", generation},
		{"handoff.json", journal},
	} {
		payload, err := json.Marshal(fixture.value)
		if err != nil {
			t.Fatal("record fixture encoding failed")
		}
		windowsRecordTestWrite(t, ops, fixture.name, payload)
		reopened, err := newWindowsPrivateRecordOps(filepath.Join(root, "records"), false, hooks)
		if err != nil {
			t.Fatal("private adapter reopen failed")
		}
		got, readErr := readPrivateRecord(reopened, fixture.name)
		closeErr := reopened.close()
		if readErr != nil || closeErr != nil || !bytes.Equal(got, payload) {
			t.Fatal("record class did not reopen exactly")
		}
		decoded := reflect.New(reflect.TypeOf(fixture.value))
		if json.Unmarshal(got, decoded.Interface()) != nil || !reflect.DeepEqual(decoded.Elem().Interface(), fixture.value) {
			t.Fatal("decoded record fields differ")
		}
	}
	for _, stage := range []string{"directory-created", "directory", "temporary-created", "fence-created", "leaf-inspect", "leaf-open", "published"} {
		if seen[stage] == 0 {
			t.Error("required native observation stage was not exercised")
		}
	}
	t.Run("directory-creation-failure-cleanup", func(t *testing.T) {
		home := windowsRecordTestHome(t)
		path := filepath.Join(home, "failed-records")
		observed := false
		hooks := &windowsRecordHooks{observe: func(stage string, _ syscall.Handle, policy privateRecordPolicy) error {
			if stage == "directory-created" {
				observed = true
				if checkPrivateRecordPolicy(policy) != nil {
					t.Error("failed native creation exposed permissive directory")
				}
				return ReasonStateIO
			}
			return nil
		}}
		failed, err := newWindowsPrivateRecordOps(path, true, hooks)
		if failed != nil {
			_ = failed.close()
		}
		if !observed || err != ReasonStateIO || failed != nil {
			t.Fatal("directory creation failure did not refuse")
		}
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("failed directory creation leaked owned native object")
		}
	})

}

func windowsRecordTestSetDACL(t *testing.T, path string, sddl string, protected bool) {
	t.Helper()
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatal("fixture descriptor construction failed")
	}
	acl, _, err := descriptor.DACL()
	if err != nil || acl == nil {
		t.Fatal("fixture descriptor DACL unavailable")
	}
	security := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION)
	if !protected {
		security = windows.DACL_SECURITY_INFORMATION | windows.UNPROTECTED_DACL_SECURITY_INFORMATION
	}
	if windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, security, nil, nil, acl, nil) != nil {
		t.Fatal("fixture security mutation failed")
	}
}

// SetFileSecurityW preserves explicit ACE flags without the auto-inheritance
// processing performed by SetNamedSecurityInfo. Read the descriptor back before
// claiming that the inherited-ACE refusal has actually been exercised.
func windowsRecordTestSetInheritedDACL(t *testing.T, path, sddl string) {
	t.Helper()
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatal("fixture descriptor construction failed")
	}
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal("fixture security path conversion failed")
	}
	setSecurity := syscall.NewLazyDLL("advapi32.dll").NewProc("SetFileSecurityW")
	if setSecurity.Find() != nil {
		t.Fatal("raw inherited fixture security entry point unavailable")
	}
	const security = windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION
	result, _, _ := setSecurity.Call(uintptr(unsafe.Pointer(name)), uintptr(security), uintptr(unsafe.Pointer(descriptor)))
	if result == 0 {
		t.Fatal("raw inherited fixture security mutation failed")
	}
	handle, err := windowsRecordOpenMetadata(path, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE)
	if err != nil {
		t.Fatal("inherited fixture security readback open failed")
	}
	raw, readErr := windowsRecordSecurity(handle)
	closeErr := syscall.CloseHandle(handle)
	if readErr != nil || closeErr != nil {
		t.Fatal("inherited fixture security readback failed")
	}
	policy, err := windowsRecordPolicyFromSecurity(raw)
	if err != nil || !policy.protected || !policy.daclPresent {
		t.Fatal("inherited fixture protected DACL was not persisted")
	}
	inherited := false
	for i := range policy.aces {
		if policy.aces[i].flags&0x10 != 0 { // INHERITED_ACE
			inherited = true
			policy.aces[i].flags &^= 0x10
		}
	}
	if !inherited {
		t.Skip("NOT RUN — inherited ACE not persistable")
	}
	// Only change the in-memory projection: all other privacy checks must
	// accept it, so an unrelated invalid owner, trustee or right cannot mask
	// failure to reject INHERITED_ACE on the persisted native object.
	if checkPrivateRecordPolicy(policy) != nil {
		t.Fatal("inherited fixture has unrelated privacy violation")
	}
}

func windowsRecordTestSetOwner(path string, sid *windows.SID) error {
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION, sid, nil, nil, nil)
}

type windowsRecordReadCounter struct {
	privateRecordHandle
	reads *atomic.Int32
}

func (f *windowsRecordReadCounter) Read(b []byte) (int, error) {
	f.reads.Add(1)
	return f.privateRecordHandle.Read(b)
}

// This forwarding seam observes actual native reads/publication; it never
// substitutes creation, descriptors, handles, or the production policy checker.
type windowsRecordObservedOps struct {
	privateRecordOps
	reads           atomic.Int32
	publications    atomic.Int32
	committed       atomic.Int32
	completeFailure bool
	created         []*windowsRecordFile
	opened          []*windowsRecordFile
}

func (o *windowsRecordObservedOps) open(name string) (privateRecordHandle, error) {
	f, err := o.privateRecordOps.open(name)
	if err != nil {
		return nil, err
	}
	if native, ok := f.(*windowsRecordFile); ok {
		o.opened = append(o.opened, native)
	}
	return &windowsRecordReadCounter{privateRecordHandle: f, reads: &o.reads}, nil
}

func (o *windowsRecordObservedOps) create(name string, temporary bool) (privateRecordHandle, error) {
	f, err := o.privateRecordOps.create(name, temporary)
	if err == nil {
		if native, ok := f.(*windowsRecordFile); ok {
			o.created = append(o.created, native)
		}
	}
	return f, err
}

func (o *windowsRecordObservedOps) assertHandlesClosed(t *testing.T) {
	t.Helper()
	if len(o.created) == 0 {
		t.Fatal("native handle closure observation absent")
	}
	for _, f := range append(o.created, o.opened...) {
		// File.Stat on Windows reports ERROR_INVALID_HANDLE after Close,
		// rather than os.ErrClosed. Inspect the os.File sentinel itself so a
		// recycled native handle number cannot make closure appear successful.
		if f.file.Fd() != uintptr(syscall.InvalidHandle) {
			t.Fatal("ordinary completion retained an open native record handle")
		}
	}
}

func (o *windowsRecordObservedOps) inspectHandle(f privateRecordHandle) (privateRecordInfo, error) {
	if counter, ok := f.(*windowsRecordReadCounter); ok {
		f = counter.privateRecordHandle
	}
	return o.privateRecordOps.inspectHandle(f)
}

func (o *windowsRecordObservedOps) publish(f privateRecordHandle, name string) (bool, error) {
	o.publications.Add(1)
	published, err := o.privateRecordOps.publish(f, name)
	if published {
		o.committed.Add(1)
	}
	return published, err
}

func (o *windowsRecordObservedOps) complete(f privateRecordHandle, name string) error {
	if err := o.privateRecordOps.complete(f, name); err != nil {
		return err
	}
	if o.completeFailure {
		return ReasonStateIO
	}
	return nil
}

func TestWindowsTrustPrivateRecordPrivacyRefusals(t *testing.T) {
	for _, name := range []string{"wrong-owner", "extra-trustee", "unprotected-dacl", "inherited-ace", "missing-system", "invalid-rights"} {
		t.Run(name, func(t *testing.T) {
			root := windowsRecordTestHome(t)
			ops := windowsRecordTestOps(t, root, nil)
			path := filepath.Join(root, "records", "record.json")
			windowsRecordTestWrite(t, ops, "record.json", []byte("private-fixture"))
			nativeUser, nativeSystem, err := windowsRecordPrincipals()
			user := (*windows.SID)(unsafe.Pointer(nativeUser))
			system := (*windows.SID)(unsafe.Pointer(nativeSystem))
			if err != nil {
				t.Fatal("fixture principals unavailable")
			}
			base := "D:P(A;;FA;;;" + user.String() + ")(A;;FA;;;SY)"
			// Restore disposable security for TempDir cleanup, never to allow an
			// adapter operation to proceed through a failed privacy check.
			t.Cleanup(func() { windowsRecordTestSetDACL(t, path, base, true) })
			switch name {
			case "wrong-owner":
				if user.Equals(system) {
					t.Skip("NOT RUN — distinct privileged owner prerequisite unavailable")
				}
				if windowsRecordTestSetOwner(path, system) != nil {
					t.Skip("NOT RUN — privileged wrong-owner fixture unavailable")
				}
				t.Cleanup(func() {
					if windowsRecordTestSetOwner(path, user) != nil {
						t.Error("fixture owner restoration failed")
					}
				})
			case "extra-trustee":
				windowsRecordTestSetDACL(t, path, base+"(A;;FR;;;WD)", true)
			case "unprotected-dacl":
				windowsRecordTestSetDACL(t, path, base, false)
			case "inherited-ace":
				windowsRecordTestSetInheritedDACL(t, path, "D:P(A;ID;FA;;;"+user.String()+")(A;;FA;;;SY)")
			case "missing-system":
				if user.Equals(system) {
					t.Skip("NOT RUN — separate system principal unavailable")
				}
				windowsRecordTestSetDACL(t, path, "D:P(A;;FA;;;"+user.String()+")", true)
			case "invalid-rights":
				windowsRecordTestSetDACL(t, path, "D:P(A;;FR;;;"+user.String()+")(A;;FA;;;SY)", true)
			}
			observed := &windowsRecordObservedOps{privateRecordOps: ops}
			if b, err := readPrivateRecord(observed, "record.json"); err != ReasonStateIO || b != nil || observed.reads.Load() != 0 {
				t.Fatal("unsafe record was not refused before payload read")
			}
			calls := 0
			err = withPrivateRecordTransaction(observed, ".record-fence", func() error {
				// The callback here is a record-level read/validate callback, as
				// in FileState.Update, rather than a user callback.
				if _, readErr := readPrivateRecord(observed, "record.json"); readErr != nil {
					return readErr
				}
				calls++
				return nil
			})
			if err != ReasonStateIO || calls != 0 || observed.reads.Load() != 0 {
				t.Fatal("unsafe record reached user write callback")
			}
			if _, err := readPrivateRecord(observed, "record.json"); err != ReasonStateIO {
				t.Fatal("unsafe record was normalized or repaired")
			}
		})
	}
	t.Run("unsafe-directory", func(t *testing.T) {
		root := windowsRecordTestHome(t)
		ops := windowsRecordTestOps(t, root, nil)
		windowsRecordTestWrite(t, ops, "record.json", []byte("private-fixture"))
		nativeUser, nativeSystem, err := windowsRecordPrincipals()
		user := (*windows.SID)(unsafe.Pointer(nativeUser))
		system := (*windows.SID)(unsafe.Pointer(nativeSystem))
		if err != nil {
			t.Fatal("fixture principals unavailable")
		}
		base := "D:P(A;;FA;;;" + user.String() + ")"
		if !user.Equals(system) {
			base += "(A;;FA;;;SY)"
		}
		dir := filepath.Join(root, "records")
		windowsRecordTestSetDACL(t, dir, base+"(A;;FR;;;WD)", true)
		t.Cleanup(func() { windowsRecordTestSetDACL(t, dir, base, true) })
		observed := &windowsRecordObservedOps{privateRecordOps: ops}
		if b, err := readPrivateRecord(observed, "record.json"); err != ReasonStateIO || b != nil || observed.reads.Load() != 0 {
			t.Fatal("unsafe directory reached native payload read")
		}
		callbacks := 0
		if withPrivateRecordTransaction(observed, ".record-fence", func() error { callbacks++; return nil }) != ReasonStateIO || callbacks != 0 || len(observed.created) != 0 {
			t.Fatal("unsafe directory reached fence creation or write callback")
		}
		if ops.checkDirectory(false) != ReasonStateIO {
			t.Fatal("unsafe directory was normalized or repaired")
		}
	})

}

// A directory junction needs no symlink privilege on supported NTFS volumes.
// Setup failure is an explicit native prerequisite skip, never a passing proof.
func windowsRecordTestJunction(t *testing.T, path, target string) {
	t.Helper()
	if os.Mkdir(path, 0700) != nil {
		t.Fatal("junction fixture directory failed")
	}
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal("junction fixture path conversion failed")
	}
	handle, err := syscall.CreateFile(pointer, windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		t.Skip("NOT RUN — native junction handle prerequisite unavailable")
	}
	defer func() { _ = syscall.CloseHandle(handle) }()
	substitute, err := windows.UTF16FromString(`\??\` + target)
	if err != nil {
		t.Fatal("junction substitute conversion failed")
	}
	printName, err := windows.UTF16FromString(target)
	if err != nil {
		t.Fatal("junction display conversion failed")
	}
	buffer := make([]byte, 16+2*(len(substitute)+len(printName)))
	binary.LittleEndian.PutUint32(buffer, windows.IO_REPARSE_TAG_MOUNT_POINT)
	binary.LittleEndian.PutUint16(buffer[4:], uint16(len(buffer)-8))
	binary.LittleEndian.PutUint16(buffer[8:], 0)
	binary.LittleEndian.PutUint16(buffer[10:], uint16(2*(len(substitute)-1)))
	binary.LittleEndian.PutUint16(buffer[12:], uint16(2*len(substitute)))
	binary.LittleEndian.PutUint16(buffer[14:], uint16(2*(len(printName)-1)))
	for i, char := range append(substitute, printName...) {
		binary.LittleEndian.PutUint16(buffer[16+2*i:], char)
	}
	var returned uint32
	if syscall.DeviceIoControl(handle, windows.FSCTL_SET_REPARSE_POINT, &buffer[0], uint32(len(buffer)), nil, 0, &returned, nil) != nil {
		t.Skip("NOT RUN — native junction creation prerequisite unavailable")
	}
	var info syscall.ByHandleFileInformation
	if syscall.GetFileInformationByHandle(handle, &info) != nil || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0 {
		t.Fatal("junction native reparse observation absent")
	}
}

func windowsRecordTestNativeMetadata(t *testing.T, path string) (syscall.Handle, syscall.ByHandleFileInformation) {
	t.Helper()
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal("native metadata path conversion failed")
	}
	h, err := syscall.CreateFile(pointer, windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		t.Fatal("native metadata open failed")
	}
	t.Cleanup(func() { _ = syscall.CloseHandle(h) })
	var raw syscall.ByHandleFileInformation
	if syscall.GetFileInformationByHandle(h, &raw) != nil {
		t.Fatal("native metadata observation failed")
	}
	return h, raw
}

func TestWindowsTrustPrivateRecordReparseAndReplacement(t *testing.T) {
	t.Run("reparse-leaf", func(t *testing.T) {
		root := windowsRecordTestHome(t)
		ops := windowsRecordTestOps(t, root, nil)
		outside := filepath.Join(root, "outside")
		if os.Mkdir(outside, 0700) != nil || os.WriteFile(filepath.Join(outside, "sentinel"), []byte("outside-fixture"), 0600) != nil {
			t.Fatal("outside fixture failed")
		}
		path := filepath.Join(root, "records", "record.json")
		windowsRecordTestJunction(t, path, outside)
		h, raw := windowsRecordTestNativeMetadata(t, path)
		policy, _, err := windowsRecordProjection(h, false)
		if err != nil || raw.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0 || !policy.reparse {
			t.Fatal("native projection must retain observed reparse attribute")
		}
		observed := &windowsRecordObservedOps{privateRecordOps: ops}
		if b, err := readPrivateRecord(observed, "record.json"); err != ReasonStateIO || b != nil || observed.reads.Load() != 0 {
			t.Fatal("reparse leaf reached payload read")
		}
		b, err := os.ReadFile(filepath.Join(outside, "sentinel"))
		if err != nil || !bytes.Equal(b, []byte("outside-fixture")) {
			t.Fatal("outside sentinel changed")
		}
	})
	t.Run("reparse-ancestor", func(t *testing.T) {
		root := windowsRecordTestHome(t)
		ops := windowsRecordTestOps(t, root, nil)
		windowsRecordTestWrite(t, ops, "record.json", []byte("original-fixture"))
		alias := filepath.Join(root, "alias")
		windowsRecordTestJunction(t, alias, root)
		other, err := newWindowsPrivateRecordOps(filepath.Join(alias, "records"), false, nil)
		if other != nil {
			_ = other.close()
		}
		if err != ReasonStateIO || other != nil {
			t.Fatal("reparse ancestor accepted")
		}
		windowsRecordTestRead(t, ops, "record.json", []byte("original-fixture"))
	})
	parentReplacement := func(t *testing.T, injectObservation bool) {
		root := windowsRecordTestHome(t)
		created := windowsRecordTestOps(t, root, nil)
		windowsRecordTestWrite(t, created, "record.json", []byte("original-fixture"))
		originalPath := filepath.Join(root, "records")
		retainedPath := filepath.Join(root, "retained-records")
		// The ordinary reopen path retains a guard without FILE_SHARE_DELETE.
		// Newly created directory guards permit delete sharing so they can
		// coexist with the exact rollback handle. Exercise the pinned
		// reopen path explicitly instead of assuming either sharing schedule.
		if created.close() != nil {
			t.Fatal("parent fixture initial adapter close failed")
		}
		hooks := &windowsRecordHooks{}
		ops, err := newWindowsPrivateRecordOps(originalPath, false, hooks)
		if err != nil {
			t.Fatal("pinned parent fixture reopen failed")
		}
		defer func() { _ = ops.close() }()
		replacementPath := filepath.Join(root, "replacement-records")
		replacement, err := newWindowsPrivateRecordOps(replacementPath, true, nil)
		if err != nil {
			t.Fatal("private parent replacement fixture unavailable")
		}
		defer func() { _ = replacement.close() }()
		windowsRecordTestWrite(t, replacement, "record.json", []byte("outside-fixture"))
		changed, injected, attempted := false, false, false
		publications := 0
		var renameErr error
		hooks.before = func(stage string) error {
			if stage == "publish" {
				publications++
			}
			if stage != "prepublication" {
				return nil
			}
			attempted = true
			renameErr = os.Rename(originalPath, retainedPath)
			if renameErr == nil {
				if os.Rename(retainedPath, originalPath) != nil {
					t.Error("parent fixture rollback failed")
				}
				return ReasonStateIO
			}
			if !errors.Is(renameErr, windows.ERROR_SHARING_VIOLATION) || !injectObservation {
				return ReasonStateIO
			}
			// Native pinning is established above. This single injected changed
			// observation isolates the prepublication identity comparison (W7).
			injected = true
			hooks.metadata = func(stage string, info *syscall.ByHandleFileInformation) {
				if stage == "parent-recheck" {
					prior := windowsRecordIdentity(*info)
					info.FileIndexLow ^= 1
					changed = prior != windowsRecordIdentity(*info)
				}
			}
			return nil
		}
		err = writePrivateRecord(ops, "record.json", ".record-tmp-", []byte("replacement-fixture"), nil)
		if !attempted {
			t.Fatal("parent replacement hook was not reached")
		}
		if renameErr == nil {
			t.Fatal("pinned parent fixture rename unexpectedly succeeded")
		}
		if !errors.Is(renameErr, windows.ERROR_SHARING_VIOLATION) {
			t.Fatal("parent fixture rename refused without sharing violation")
		}
		if injectObservation {
			if !injected {
				t.Fatal("changed parent observation hook was not installed")
			}
			if !changed {
				t.Fatal("changed parent observation hook was not reached")
			}
			t.Log("injected parent observation after native sharing refusal")
		} else {
			if injected || changed {
				t.Fatal("physical parent refusal unexpectedly used injected observation")
			}
			t.Log("physical parent replacement refused by native sharing guard")
		}
		if err != ReasonStateIO {
			t.Fatal("parent replacement did not return fixed refusal")
		}
		if publications != 0 {
			t.Fatal("parent replacement reached publication")
		}
		hooks.before, hooks.metadata = nil, nil
		windowsRecordTestRead(t, ops, "record.json", []byte("original-fixture"))
		windowsRecordTestRead(t, replacement, "record.json", []byte("outside-fixture"))
		windowsRecordTestNoArtifacts(t, root, 1)
	}
	t.Run("parent-replaced", func(t *testing.T) { parentReplacement(t, true) })
	t.Run("physical-parent-replaced", func(t *testing.T) { parentReplacement(t, false) })

	t.Run("leaf-replaced", func(t *testing.T) {
		root := windowsRecordTestHome(t)
		hooks := &windowsRecordHooks{}
		ops := windowsRecordTestOps(t, root, hooks)
		windowsRecordTestWrite(t, ops, "record.json", []byte("original-fixture"))
		windowsRecordTestWrite(t, ops, "replacement.json", []byte("substitute-fixture"))
		path := filepath.Join(root, "records", "record.json")
		old := filepath.Join(root, "records", "retained.json")
		didReplace := false
		hooks.before = func(stage string) error {
			if stage != "open" || didReplace {
				return nil
			}
			didReplace = true
			if os.Rename(path, old) != nil || os.Rename(filepath.Join(root, "records", "replacement.json"), path) != nil {
				return ReasonStateIO
			}
			return nil
		}
		observed := &windowsRecordObservedOps{privateRecordOps: ops}
		if b, err := readPrivateRecord(observed, "record.json"); !didReplace || err != ReasonStateIO || b != nil || observed.reads.Load() != 0 {
			t.Fatal("replaced leaf was not refused before payload read")
		}
		hooks.before = nil
		windowsRecordTestRead(t, ops, "retained.json", []byte("original-fixture"))
		windowsRecordTestRead(t, ops, "record.json", []byte("substitute-fixture"))
	})
	t.Run("target-replaced-before-publication", func(t *testing.T) {
		root := windowsRecordTestHome(t)
		hooks := &windowsRecordHooks{}
		ops := windowsRecordTestOps(t, root, hooks)
		windowsRecordTestWrite(t, ops, "record.json", []byte("original-fixture"))
		windowsRecordTestWrite(t, ops, "replacement.json", []byte("substitute-fixture"))
		changed := false
		observed := &windowsRecordObservedOps{privateRecordOps: ops}
		hooks.before = func(stage string) error {
			if stage != "publish" {
				return nil
			}
			before, err := ops.inspect("record.json")
			if err != nil {
				return ReasonStateIO
			}
			if os.Rename(filepath.Join(root, "records", "record.json"), filepath.Join(root, "records", "retained.json")) != nil || os.Rename(filepath.Join(root, "records", "replacement.json"), filepath.Join(root, "records", "record.json")) != nil {
				return ReasonStateIO
			}
			after, err := ops.inspect("record.json")
			if err != nil {
				return ReasonStateIO
			}
			changed = before.identity != after.identity
			return nil
		}
		if err := writePrivateRecord(observed, "record.json", ".record-tmp-", []byte("new-record-fixture"), nil); err != ReasonStateIO || !changed || observed.committed.Load() != 0 {
			t.Fatal("publication target replacement was not observed and refused")
		}
		hooks.before = nil
		windowsRecordTestRead(t, ops, "retained.json", []byte("original-fixture"))
		windowsRecordTestRead(t, ops, "record.json", []byte("substitute-fixture"))
		windowsRecordTestNoArtifacts(t, root, 2)
	})
	t.Run("synthetic-additional-reparse-tags", func(t *testing.T) {
		root := windowsRecordTestHome(t)
		hooks := &windowsRecordHooks{}
		ops := windowsRecordTestOps(t, root, hooks)
		windowsRecordTestWrite(t, ops, "record.json", []byte("original-fixture"))
		seen := false
		hooks.metadata = func(stage string, info *syscall.ByHandleFileInformation) {
			if stage == "projection" {
				info.FileAttributes |= windows.FILE_ATTRIBUTE_REPARSE_POINT
			}
		}
		hooks.observe = func(_ string, _ syscall.Handle, policy privateRecordPolicy) error {
			if policy.reparse {
				seen = true
			}
			return nil
		}
		observed := &windowsRecordObservedOps{privateRecordOps: ops}
		if b, err := readPrivateRecord(observed, "record.json"); err != ReasonStateIO || b != nil || !seen || observed.reads.Load() != 0 {
			t.Fatal("synthetic reparse metadata was not retained and refused")
		}
	})
}

func TestWindowsTrustPrivateRecordFenceAndPublication(t *testing.T) {
	t.Run("contending-writer", func(t *testing.T) {
		root := windowsRecordTestHome(t)
		first := windowsRecordTestOps(t, root, nil)
		windowsRecordTestWrite(t, first, "record.json", []byte("original-fixture"))
		second, err := newWindowsPrivateRecordOps(filepath.Join(root, "records"), false, nil)
		if err != nil {
			t.Fatal("second native adapter unavailable")
		}
		defer func() { _ = second.close() }()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
		go func() {
			done <- withPrivateRecordTransaction(first, ".record-fence", func() error {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
					return ReasonStateIO
				}
				return writePrivateRecord(first, "record.json", ".record-tmp-", []byte("replacement-fixture"), nil)
			})
		}()
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("writer handshake timed out")
		}
		callbacks := 0
		loserErr := withPrivateRecordTransaction(second, ".record-fence", func() error { callbacks++; return nil })
		close(release)
		select {
		case winnerErr := <-done:
			if winnerErr != nil || loserErr != ReasonStateIO || callbacks != 0 {
				t.Fatal("native fence allowed duplicate writer")
			}
		case <-ctx.Done():
			t.Fatal("writer completion timed out")
		}
		windowsRecordTestRead(t, second, "record.json", []byte("replacement-fixture"))
		windowsRecordTestNoArtifacts(t, root, 1)
	})
	t.Run("abandoned-fence", func(t *testing.T) {
		root := windowsRecordTestHome(t)
		ops := windowsRecordTestOps(t, root, nil)
		fence, err := ops.create(".record-fence", false)
		if err != nil || fence.Close() != nil {
			t.Fatal("abandoned native fence fixture failed")
		}
		callbacks := 0
		if withPrivateRecordTransaction(ops, ".record-fence", func() error { callbacks++; return nil }) != ReasonStateIO || callbacks != 0 {
			t.Fatal("abandoned fence was taken over")
		}
	})
	t.Run("reader-and-bounded-replacement", func(t *testing.T) {
		root := windowsRecordTestHome(t)
		ops := windowsRecordTestOps(t, root, nil)
		observed := &windowsRecordObservedOps{privateRecordOps: ops}
		original := bytes.Repeat([]byte("original-record"), 200)
		next := bytes.Repeat([]byte("replacement-record"), 200)
		windowsRecordTestWrite(t, observed, "record.json", original)
		reader, err := observed.open("record.json")
		if err != nil {
			t.Fatal("retained native reader unavailable")
		}
		for range 8 {
			windowsRecordTestWrite(t, observed, "record.json", next)
			windowsRecordTestRead(t, observed, "record.json", next)
		}
		old, readErr := io.ReadAll(reader)
		closeErr := reader.Close()
		if readErr != nil || closeErr != nil || !bytes.Equal(old, original) {
			t.Fatal("retained reader lost complete previous record")
		}
		windowsRecordTestNoArtifacts(t, root, 1)
		observed.assertHandlesClosed(t)
	})
	t.Run("creation-validation-failure", func(t *testing.T) {
		for _, stage := range []string{"temporary-created", "fence-created"} {
			t.Run(stage, func(t *testing.T) {
				root := windowsRecordTestHome(t)
				hooks := &windowsRecordHooks{}
				ops := windowsRecordTestOps(t, root, hooks)
				windowsRecordTestWrite(t, ops, "record.json", []byte("original-fixture"))
				seen := false
				hooks.observe = func(current string, _ syscall.Handle, policy privateRecordPolicy) error {
					if current == stage {
						seen = true
						if checkPrivateRecordPolicy(policy) != nil {
							t.Error("native creation exposed permissive intermediate")
						}
						return ReasonStateIO
					}
					return nil
				}
				callbacks := 0
				var err error
				if stage == "fence-created" {
					err = withPrivateRecordTransaction(ops, ".record-fence", func() error { callbacks++; return nil })
				} else {
					err = writePrivateRecord(ops, "record.json", ".record-tmp-", []byte("replacement-fixture"), nil)
				}
				if err != ReasonStateIO || !seen || callbacks != 0 {
					t.Fatal("native creation validation did not refuse")
				}
				hooks.observe = nil
				windowsRecordTestRead(t, ops, "record.json", []byte("original-fixture"))
				windowsRecordTestNoArtifacts(t, root, 1)
			})
		}
	})
	t.Run("created-object-confinement-failure", func(t *testing.T) {
		root := windowsRecordTestHome(t)
		hooks := &windowsRecordHooks{}
		ops := windowsRecordTestOps(t, root, hooks)
		windowsRecordTestWrite(t, ops, "record.json", []byte("original-fixture"))
		changed := false
		hooks.observe = func(stage string, _ syscall.Handle, _ privateRecordPolicy) error {
			if stage == "temporary-created" {
				hooks.metadata = func(stage string, info *syscall.ByHandleFileInformation) {
					if stage == "ancestor-recheck" {
						info.FileIndexLow ^= 1
						changed = true
					}
				}
			}
			return nil
		}
		if err := writePrivateRecord(ops, "record.json", ".record-tmp-", []byte("replacement-fixture"), nil); err != ReasonStateIO || !changed {
			t.Fatal("created-object confinement loss was not observed and refused")
		}
		hooks.observe, hooks.metadata = nil, nil
		windowsRecordTestRead(t, ops, "record.json", []byte("original-fixture"))
		windowsRecordTestNoArtifacts(t, root, 1)
	})
	t.Run("prepublication-failure", func(t *testing.T) {
		root := windowsRecordTestHome(t)
		ops := windowsRecordTestOps(t, root, nil)
		windowsRecordTestWrite(t, ops, "record.json", []byte("original-fixture"))
		observed := &windowsRecordObservedOps{privateRecordOps: ops}
		err := writePrivateRecord(observed, "record.json", ".record-tmp-", []byte("replacement-fixture"), func() error { return ReasonStateIO })
		if err != ReasonStateIO || observed.publications.Load() != 0 {
			t.Fatal("prepublication failure published bytes")
		}
		windowsRecordTestRead(t, ops, "record.json", []byte("original-fixture"))
		windowsRecordTestNoArtifacts(t, root, 1)
	})
	t.Run("postpublication-failure", func(t *testing.T) {
		root := windowsRecordTestHome(t)
		ops := windowsRecordTestOps(t, root, nil)
		windowsRecordTestWrite(t, ops, "record.json", []byte("original-fixture"))
		observed := &windowsRecordObservedOps{privateRecordOps: ops, completeFailure: true}
		err := writePrivateRecord(observed, "record.json", ".record-tmp-", []byte("replacement-fixture"), nil)
		if err != ReasonStateIO || observed.publications.Load() != 1 {
			t.Fatal("postpublication failure classification differs")
		}
		windowsRecordTestRead(t, ops, "record.json", []byte("replacement-fixture"))
		windowsRecordTestNoArtifacts(t, root, 1)
	})
}

func windowsRecordTestNoArtifacts(t *testing.T, root string, count int) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, "records"))
	if err != nil || len(entries) != count {
		t.Fatal("native record transaction leaked temporary or fence artifact")
	}
}

func TestWindowsTrustPublicPersistenceStillRefuses(t *testing.T) {
	root := windowsRecordTestHome(t)
	ops := windowsRecordTestOps(t, root, nil)
	windowsRecordTestWrite(t, ops, "record.json", []byte("private-fixture"))
	dir := filepath.Join(root, "records")
	before, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal("public boundary fixture inventory failed")
	}
	for _, selected := range []string{dir, filepath.Join(root, "absent")} {
		state, err := NewFileState(selected)
		if state != nil || err != ReasonStateIO {
			t.Fatal("public state must remain nil with exact Windows refusal")
		}
		if privateReadDirectory(selected) != ReasonStateIO || privateDirectory(selected, false) != ReasonStateIO || privateDirectory(selected, true) != ReasonStateIO {
			t.Fatal("public private-directory Windows refusal changed")
		}
	}
	calls := 0
	if withPrivateTransaction(context.Background(), dir, ".public-fence", func() error { calls++; return nil }) != ReasonStateIO || calls != 0 {
		t.Fatal("public transaction entered callback on Windows")
	}
	if writePrivateJSON(dir, "public.json", map[string]int{"version": 1}) != ReasonStateIO {
		t.Fatal("public JSON writer crossed Windows boundary")
	}
	var record map[string]any
	if readPrivateJSON(dir, "record.json", &record) != ReasonStateIO {
		t.Fatal("public JSON reader crossed Windows boundary")
	}
	for _, name := range []string{dir, filepath.Join(dir, "record.json")} {
		info, err := os.Stat(name)
		if err != nil || privateOwned(info) {
			t.Fatal("public ownership gate enabled on Windows")
		}
	}
	after, err := os.ReadDir(dir)
	if err != nil || len(after) != len(before) {
		t.Fatal("public refusal changed native fixture contents")
	}
	if _, err := os.Stat(filepath.Join(root, "absent")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("public refusal created a directory")
	}
	windowsRecordTestRead(t, ops, "record.json", []byte("private-fixture"))
}
