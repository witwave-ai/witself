//go:build windows

package trust

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"unsafe"
)

// All native fixtures live under TempDir and exercise internal unsealed
// preparation only. No service, process, public persistence or real home is used.
func windowsGenerationTestParent(t *testing.T) string {
	t.Helper()
	home := windowsRecordTestHome(t)
	path := filepath.Join(home, "private-parent")
	parent, err := newWindowsPrivateRecordOps(path, true, nil)
	if err != nil || parent.close() != nil {
		t.Fatal("private generation parent fixture failed")
	}
	return path
}

func windowsGenerationTestBuilder(t *testing.T, hooks *windowsGenerationHooks) (*privateGeneration, *windowsGenerationOps, string) {
	t.Helper()
	parent := windowsGenerationTestParent(t)
	g, err := newWindowsPrivateGeneration(context.Background(), parent, "generation", hooks)
	if err != nil {
		t.Fatal("private generation fixture construction failed")
	}
	ops, ok := g.ops.(*windowsGenerationOps)
	if !ok {
		t.Fatal("native generation adapter absent")
	}
	windowsGenerationTestRescue(t, ops)
	return g, ops, filepath.Join(parent, "generation")
}

func windowsGenerationTestRescue(t *testing.T, ops *windowsGenerationOps) {
	t.Helper()
	t.Cleanup(func() {
		// A before-close injected error deliberately prevents confirmed closure.
		// The test owns the exact *os.File and rescues it independently of the
		// protocol; never close a possibly reused raw handle number.
		ops.hooks = nil
		for f := range ops.files {
			if f.file.Fd() != uintptr(syscall.InvalidHandle) {
				_ = f.file.Close()
			}
		}
		if len(ops.parent.guards) != 0 {
			_ = ops.parent.close()
		}
	})
}

func windowsGenerationTestClosed(t *testing.T, ops *windowsGenerationOps) {
	t.Helper()
	if len(ops.files) == 0 || !ops.closeAttempted || len(ops.parent.guards) != 0 {
		t.Fatal("generation ancestor closure was not confirmed")
	}
	for f := range ops.files {
		if !f.closeAttempted || f.file.Fd() != uintptr(syscall.InvalidHandle) {
			t.Fatal("generation retained a writable or read handle after success")
		}
	}
}

func windowsGenerationTestReadFresh(t *testing.T, path string, identity [3]uint64, expected []byte) {
	t.Helper()
	h, err := windowsRecordOpenMetadata(path, syscall.GENERIC_READ, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE)
	if err != nil {
		t.Fatal("completed unsealed file did not reopen")
	}
	f := os.NewFile(uintptr(h), path)
	p, info, policyErr := windowsRecordProjection(h, false)
	b, readErr := io.ReadAll(f)
	closeErr := f.Close()
	if policyErr != nil || checkPrivateRecordPolicy(p) != nil || info.identity != identity || readErr != nil || closeErr != nil || !bytes.Equal(b, expected) {
		t.Fatal("fresh unsealed readback bytes identity or writable private policy differ")
	}
}

func TestWindowsTrustPrivateGenerationCreateAndReopen(t *testing.T) {
	ctx := context.Background()
	directories, files := 0, 0
	var closed []*windowsGenerationFile
	hooks := &windowsGenerationHooks{before: func(stage string, f *windowsGenerationFile) error {
		if stage == "close" {
			closed = append(closed, f)
		}
		return nil
	}, observe: func(stage string, f *windowsGenerationFile, p *privateRecordPolicy) {
		if stage != "created" {
			return
		}
		if checkPrivateRecordPolicy(*p) != nil {
			t.Error("created native generation object lacks protected full-access policy")
		}
		var info syscall.ByHandleFileInformation
		if syscall.GetFileInformationByHandle(syscall.Handle(f.file.Fd()), &info) != nil || windowsRecordIdentity(info) != p.identity {
			t.Error("created native generation identity differs from retained handle")
		}
		if f.directory {
			directories++
		} else {
			files++
		}
	}}
	g, ops, path := windowsGenerationTestBuilder(t, hooks)
	if g.mkdir(ctx, "nested") != nil {
		t.Fatal("private nested directory creation failed")
	}
	payloads := map[string][]byte{"first.bin": []byte("first synthetic payload"), "nested/second.bin": {0, 1, 2, 3, 255}}
	identities := make(map[string][3]uint64)
	for name, payload := range payloads {
		if g.write(ctx, name, int64(len(payload)), bytes.NewReader(payload)) != nil {
			t.Fatal("bounded native write and sync failed")
		}
		for range 3 {
			got, err := g.read(ctx, name)
			if err != nil || !bytes.Equal(got, payload) {
				t.Fatal("registered native reopen did not return exact payload")
			}
		}
		f := g.names[name].handle.(*windowsGenerationFile)
		identities[name] = f.identity
	}
	if directories != 2 || files != 2 || len(ops.files) != 4 || g.complete(ctx) != nil || !g.completed {
		t.Fatal("all created kinds were not verified before successful completion")
	}
	windowsGenerationTestClosed(t, ops)
	if len(closed) != 10 {
		t.Fatal("successful completion did not close all created objects and read handles")
	}
	for _, f := range closed {
		if f.file.Fd() != uintptr(syscall.InvalidHandle) {
			t.Fatal("successful completion retained a native handle")
		}
	}
	if g.complete(ctx) != nil || g.abort() != nil {
		t.Fatal("successful generation terminal operations were not idempotent")
	}
	for name, payload := range payloads {
		windowsGenerationTestReadFresh(t, filepath.Join(path, filepath.FromSlash(name)), identities[name], payload)
	}
}

// This fixture uses standard Windows bindings and never exposes the descriptor
// or SID in an error. It is not a production ACL repair operation.
func windowsGenerationTestSetSecurity(path, text string, security uint32) bool {
	encoded, err := syscall.UTF16PtrFromString(text)
	if err != nil || windowsRecordConvertDescriptor.Find() != nil {
		return false
	}
	var descriptor *byte
	var size uint32
	r, _, _ := windowsRecordConvertDescriptor.Call(uintptr(unsafe.Pointer(encoded)), 1, uintptr(unsafe.Pointer(&descriptor)), uintptr(unsafe.Pointer(&size)))
	if descriptor != nil {
		defer func() { _, _ = syscall.LocalFree(syscall.Handle(unsafe.Pointer(descriptor))) }()
	}
	name, err := syscall.UTF16PtrFromString(path)
	setSecurity := windowsRecordAdvapi32.NewProc("SetFileSecurityW")
	if r == 0 || descriptor == nil || err != nil || setSecurity.Find() != nil {
		return false
	}
	r, _, _ = setSecurity.Call(uintptr(unsafe.Pointer(name)), uintptr(security), uintptr(unsafe.Pointer(descriptor)))
	return r != 0
}

func windowsGenerationTestSDDL(t *testing.T) string {
	t.Helper()
	user, _, err := windowsRecordPrincipals()
	if err != nil {
		t.Fatal("native fixture principal unavailable")
	}
	text, err := user.String()
	if err != nil {
		t.Fatal("native fixture principal encoding failed")
	}
	base := "O:" + text + "D:P(A;;FA;;;" + text + ")"
	if text != "S-1-5-18" {
		base += "(A;;FA;;;SY)"
	}
	return base
}

func TestWindowsTrustPrivateGenerationPrivacyRefusals(t *testing.T) {
	ctx := context.Background()
	for _, fixture := range []string{"null-dacl", "unprotected-dacl", "permissive-dacl", "wrong-owner"} {
		t.Run(fixture, func(t *testing.T) {
			parent := windowsGenerationTestParent(t)
			base := windowsGenerationTestSDDL(t)
			security := uint32(windowsRecordDACLSecurity | 0x80000000) // PROTECTED_DACL_SECURITY_INFORMATION
			bad := base + "(A;;FR;;;WD)"
			switch fixture {
			case "null-dacl":
				bad = "D:NO_ACCESS_CONTROL"
			case "unprotected-dacl":
				bad = strings.Replace(base, "D:P", "D:", 1)
				security = windowsRecordDACLSecurity | 0x20000000 // UNPROTECTED_DACL_SECURITY_INFORMATION
			case "wrong-owner":
				bad, security = "O:SY", windowsRecordOwnerSecurity
			}
			actual := windowsGenerationTestSetSecurity(parent, bad, security)
			if actual {
				t.Cleanup(func() {
					if !windowsGenerationTestSetSecurity(parent, base, windowsRecordOwnerSecurity|windowsRecordDACLSecurity|0x80000000) {
						t.Error("disposable parent security restoration failed")
					}
				})
			}
			// A LocalSystem runner has no distinct wrong-owner fixture. A
			// nonprivileged runner may be unable to assign LocalSystem ownership.
			if fixture == "wrong-owner" {
				h, err := windowsRecordOpenMetadata(parent, 0, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE)
				if err == nil {
					p, _, readErr := windowsRecordProjection(h, true)
					closeErr := syscall.CloseHandle(h)
					actual = actual && readErr == nil && closeErr == nil && !p.ownerUser
				} else {
					actual = false
				}
				if !actual {
					t.Log("bounded wrong-owner observation: native distinct owner requires unavailable privilege or identity")
					g, _, _ := windowsGenerationTestBuilder(t, &windowsGenerationHooks{observe: func(stage string, _ *windowsGenerationFile, p *privateRecordPolicy) {
						if stage == "retained" {
							p.ownerUser = false
						}
					}})
					if g.mkdir(ctx, "blocked") != ReasonStateIO {
						t.Fatal("wrong owner observation did not refuse before creation")
					}
					return
				}
			}
			if !actual {
				t.Fatal("native privacy fixture could not be constructed")
			}
			h, err := windowsRecordOpenMetadata(parent, 0, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE)
			if err != nil {
				t.Fatal("native unsafe parent observation open failed")
			}
			raw, securityErr := windowsRecordSecurity(h)
			policy, _, policyErr := windowsRecordProjection(h, true)
			if syscall.CloseHandle(h) != nil || securityErr != nil {
				t.Fatal("native unsafe parent observation failed")
			}
			switch fixture {
			case "null-dacl":
				if len(raw) < 20 || binary.LittleEndian.Uint32(raw[16:20]) != 0 || binary.LittleEndian.Uint16(raw[2:4])&privateRecordDACLPresent == 0 || policyErr != ReasonStateIO {
					t.Fatal("native null DACL fixture was not observed")
				}
			case "unprotected-dacl":
				if policyErr != nil || policy.protected {
					t.Fatal("native unprotected DACL fixture was not observed")
				}
				policy.protected = true
				if checkPrivateRecordPolicy(policy) != nil {
					t.Fatal("unprotected DACL fixture had another privacy violation")
				}
			case "wrong-owner":
				if policyErr != nil || policy.ownerUser {
					t.Fatal("native wrong owner fixture was not observed")
				}
				policy.ownerUser = true
				if checkPrivateRecordPolicy(policy) != nil {
					t.Fatal("wrong owner fixture had another privacy violation")
				}
			case "permissive-dacl":
				if policyErr != nil || checkPrivateRecordPolicy(policy) != ReasonStateIO {
					t.Fatal("native permissive DACL fixture was not observed")
				}
			}
			g, err := newWindowsPrivateGeneration(ctx, parent, "refused", nil)
			if err != ReasonStateIO || g != nil {
				if g != nil {
					_ = g.abort()
				}
				t.Fatal("native unsafe parent did not return fixed refusal")
			}
			if _, err := os.Lstat(filepath.Join(parent, "refused")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("unsafe parent reached root creation")
			}
		})
	}
	t.Run("initial-owned-file-privacy", func(t *testing.T) {
		base := windowsGenerationTestSDDL(t)
		changed, observed, writes := false, false, 0
		hooks := &windowsGenerationHooks{}
		hooks.afterCreate = func(f *windowsGenerationFile) error {
			if !f.directory {
				changed = windowsGenerationTestSetSecurity(f.path, base+"(A;;FR;;;WD)", windowsRecordDACLSecurity|0x80000000)
				if !changed {
					return ReasonStateIO
				}
			}
			return nil
		}
		hooks.observe = func(stage string, f *windowsGenerationFile, p *privateRecordPolicy) {
			if stage == "created" && !f.directory {
				observed = changed && checkPrivateRecordPolicy(*p) == ReasonStateIO
				// Restore only after taking the actual native bad snapshot. This
				// isolates its refusal: bypassing that check reaches the write,
				// rather than being accidentally killed by a later observation.
				if !windowsGenerationTestSetSecurity(f.path, base, windowsRecordDACLSecurity|0x80000000) {
					t.Error("disposable file privacy restoration failed")
				}
			}
		}
		hooks.before = func(stage string, _ *windowsGenerationFile) error {
			if stage == "write" {
				writes++
			}
			return nil
		}
		g, ops, path := windowsGenerationTestBuilder(t, hooks)
		result := g.write(ctx, "blocked.bin", 1, bytes.NewReader([]byte{1}))
		if !changed || !observed {
			t.Fatal("native fixture prerequisite failed: initial privacy observation")
		}
		if result != ReasonStateIO || writes != 0 || g.completed {
			t.Fatal("initial native privacy refusal did not precede first payload write")
		}
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("initial privacy refusal did not remove only its owned tree")
		}
		windowsGenerationTestClosed(t, ops)
	})
	t.Run("unsupported-parent-type-and-path", func(t *testing.T) {
		home := windowsRecordTestHome(t)
		file := filepath.Join(home, "regular-parent")
		if os.WriteFile(file, []byte("foreign sentinel"), 0600) != nil {
			t.Fatal("wrong parent type fixture failed")
		}
		for _, parent := range []string{file, `\\server\share`, `\\?\C:\device`} {
			if g, err := newWindowsPrivateGeneration(ctx, parent, "refused", nil); g != nil || err != ReasonStateIO {
				t.Fatal("unsupported native parent was accepted")
			}
		}
		got, err := os.ReadFile(file)
		if err != nil || string(got) != "foreign sentinel" {
			t.Fatal("unsupported parent changed foreign sentinel")
		}
	})
	t.Run("existing-root", func(t *testing.T) {
		g, _, path := windowsGenerationTestBuilder(t, nil)
		if g.write(ctx, "sentinel", 7, bytes.NewReader([]byte("foreign"))) != nil || g.complete(ctx) != nil {
			t.Fatal("existing root fixture failed")
		}
		if other, err := newWindowsPrivateGeneration(ctx, filepath.Dir(path), "generation", nil); err != ReasonStateIO || other != nil {
			t.Fatal("existing native root was adopted")
		}
		got, err := os.ReadFile(filepath.Join(path, "sentinel"))
		if err != nil || string(got) != "foreign" {
			t.Fatal("competing constructor changed existing root")
		}
	})
}

// A mount-point junction needs no symlink privilege on the NTFS volume already
// required by the adapter. Fixture errors fail this test, never silently skip a
// required native group. Only this disposable directory receives the reparse tag.
func windowsGenerationTestJunction(t *testing.T, path, target string) {
	t.Helper()
	if os.Mkdir(path, 0700) != nil {
		t.Fatal("generation junction fixture directory failed")
	}
	pointer, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal("generation junction fixture path conversion failed")
	}
	h, err := syscall.CreateFile(pointer, syscall.GENERIC_WRITE, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE, nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_OPEN_REPARSE_POINT|syscall.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		t.Fatal("generation junction fixture handle unavailable")
	}
	defer func() {
		if syscall.CloseHandle(h) != nil {
			t.Error("generation junction fixture close failed")
		}
	}()
	substitute, err := syscall.UTF16FromString(`\??\` + target)
	if err != nil {
		t.Fatal("generation junction substitute conversion failed")
	}
	printName, err := syscall.UTF16FromString(target)
	if err != nil {
		t.Fatal("generation junction display conversion failed")
	}
	buffer := make([]byte, 16+2*(len(substitute)+len(printName)))
	binary.LittleEndian.PutUint32(buffer, 0xa0000003) // IO_REPARSE_TAG_MOUNT_POINT
	binary.LittleEndian.PutUint16(buffer[4:], uint16(len(buffer)-8))
	binary.LittleEndian.PutUint16(buffer[10:], uint16(2*(len(substitute)-1)))
	binary.LittleEndian.PutUint16(buffer[12:], uint16(2*len(substitute)))
	binary.LittleEndian.PutUint16(buffer[14:], uint16(2*(len(printName)-1)))
	for i, char := range append(substitute, printName...) {
		binary.LittleEndian.PutUint16(buffer[16+2*i:], char)
	}
	var returned uint32
	if syscall.DeviceIoControl(h, 0x000900a4, &buffer[0], uint32(len(buffer)), nil, 0, &returned, nil) != nil { // FSCTL_SET_REPARSE_POINT
		t.Fatal("generation junction fixture creation unavailable")
	}
	var info syscall.ByHandleFileInformation
	if syscall.GetFileInformationByHandle(h, &info) != nil || info.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT == 0 {
		t.Fatal("generation junction fixture native reparse observation absent")
	}
}

func TestWindowsTrustPrivateGenerationReparseAndReplacement(t *testing.T) {
	ctx := context.Background()
	t.Run("native-file-replacement", func(t *testing.T) {
		reopens := 0
		hooks := &windowsGenerationHooks{before: func(stage string, _ *windowsGenerationFile) error {
			if stage == "reopen" {
				reopens++
			}
			return nil
		}}
		g, _, path := windowsGenerationTestBuilder(t, hooks)
		if g.write(ctx, "payload", 8, bytes.NewReader([]byte("original"))) != nil {
			t.Fatal("replacement fixture write failed")
		}
		f := g.names["payload"].handle.(*windowsGenerationFile)
		if os.Rename(f.path, filepath.Join(path, "retained")) != nil || os.WriteFile(f.path, []byte("foreign sentinel"), 0600) != nil {
			t.Fatal("actual native file replacement fixture failed")
		}
		h, err := windowsRecordOpenMetadata(f.path, 0, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE)
		if err != nil {
			t.Fatal("replacement identity fixture open failed")
		}
		var native syscall.ByHandleFileInformation
		getErr, closeErr := syscall.GetFileInformationByHandle(h, &native), syscall.CloseHandle(h)
		if getErr != nil || closeErr != nil || windowsRecordIdentity(native) == f.identity {
			t.Fatal("replacement did not have a distinct retained native identity")
		}
		if b, err := g.read(ctx, "payload"); err != ReasonStateIO || b != nil || reopens != 0 {
			t.Fatal("same-path replacement reached readback reopen")
		}
		got, err := os.ReadFile(f.path)
		if err != nil || string(got) != "foreign sentinel" {
			t.Fatal("rollback deleted a foreign replacement")
		}
	})
	t.Run("native-nested-junction", func(t *testing.T) {
		g, _, path := windowsGenerationTestBuilder(t, nil)
		if g.mkdir(ctx, "nested") != nil {
			t.Fatal("junction fixture directory creation failed")
		}
		outside := filepath.Join(filepath.Dir(path), "outside")
		if os.Mkdir(outside, 0700) != nil || os.WriteFile(filepath.Join(outside, "sentinel"), []byte("foreign"), 0600) != nil || os.Rename(filepath.Join(path, "nested"), filepath.Join(path, "retained")) != nil {
			t.Fatal("native junction replacement fixture failed")
		}
		junction := filepath.Join(path, "nested")
		windowsGenerationTestJunction(t, junction, outside)
		t.Cleanup(func() { _ = os.Remove(junction) })
		if g.mkdir(ctx, "nested/blocked") != ReasonStateIO {
			t.Fatal("native reparse replacement reached nested creation")
		}
		if _, err := os.Lstat(filepath.Join(outside, "blocked")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("native reparse replacement created outside the tree")
		}
		got, err := os.ReadFile(filepath.Join(outside, "sentinel"))
		if err != nil || string(got) != "foreign" {
			t.Fatal("native reparse cleanup changed outside sentinel")
		}
	})
	for _, kind := range []string{"reparse", "type", "identity"} {
		t.Run("observed-"+kind, func(t *testing.T) {
			observed, writes := false, 0
			hooks := &windowsGenerationHooks{observe: func(stage string, f *windowsGenerationFile, p *privateRecordPolicy) {
				if stage != "created" || f.directory {
					return
				}
				observed = true
				switch kind {
				case "reparse":
					p.reparse = true
				case "type":
					p.directory = true
				case "identity":
					p.observedIdentity[2] ^= 1
				}
			}, before: func(stage string, _ *windowsGenerationFile) error {
				if stage == "write" {
					writes++
				}
				return nil
			}}
			g, _, _ := windowsGenerationTestBuilder(t, hooks)
			result := g.write(ctx, "blocked", 1, bytes.NewReader([]byte{1}))
			if !observed {
				t.Fatal("native fixture prerequisite failed: mismatch observation")
			}
			if result != ReasonStateIO || writes != 0 {
				t.Fatal("observed native mismatch reached a payload effect")
			}
			t.Log("bounded single native observation injection isolates the refusal before write")
		})
	}
}

func TestWindowsTrustPrivateGenerationFailureCleanupAndHandles(t *testing.T) {
	ctx := context.Background()
	for _, boundary := range []string{"write", "sync", "reopen", "remove", "close", "closed", "close-parent"} {
		t.Run(boundary, func(t *testing.T) {
			armed, hit, closes := false, false, 0
			var uncertain *windowsGenerationFile
			hooks := &windowsGenerationHooks{before: func(stage string, f *windowsGenerationFile) error {
				if stage == "close" && f != nil && !f.directory {
					closes++
				}
				if !armed {
					return nil
				}
				if boundary == "remove" && stage == "write" {
					return ReasonStateIO
				}
				if stage == boundary && (f == nil || !f.directory) {
					hit = true
					uncertain = f
					return ReasonStateIO
				}
				return nil
			}}
			g, ops, path := windowsGenerationTestBuilder(t, hooks)
			armed = boundary == "write" || boundary == "sync" || boundary == "remove"
			result := g.write(ctx, "payload", 7, bytes.NewReader([]byte("payload")))
			if !armed {
				if result != nil {
					t.Fatal("failure boundary setup write failed")
				}
				armed = true
				if boundary == "reopen" {
					_, result = g.read(ctx, "payload")
				} else {
					result = g.complete(ctx)
				}
			}
			if result != ReasonStateIO || !hit || g.completed || g.complete(ctx) != ReasonStateIO || g.abort() != ReasonStateIO {
				t.Fatal("native failed transition became successful or changed terminal classification")
			}
			if closes != 1 {
				t.Fatal("native failure did not attempt each payload close exactly once")
			}
			switch boundary {
			case "close", "closed":
				if uncertain == nil || !g.names["payload"].attempted || g.names["payload"].closed {
					t.Fatal("ambiguous close was reported confirmed")
				}
				actuallyClosed := uncertain.file.Fd() == uintptr(syscall.InvalidHandle)
				if actuallyClosed != (boundary == "closed") {
					t.Fatal("native close fixture did not distinguish closure timing")
				}
				if _, err := os.Lstat(path); err != nil {
					t.Fatal("ambiguous completion incorrectly claimed clean residue")
				}
			case "remove", "close-parent":
				if _, err := os.Lstat(path); err != nil {
					t.Fatal("failed cleanup or terminal closure did not preserve ambiguous residue")
				}
			default:
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("native owned rollback left an ordinary interrupted tree")
				}
				windowsGenerationTestClosed(t, ops)
			}
		})
	}
	t.Run("two-creators", func(t *testing.T) {
		parent := windowsGenerationTestParent(t)
		start := make(chan struct{})
		var wg sync.WaitGroup
		var builders [2]*privateGeneration
		var results [2]error
		for i := range builders {
			wg.Go(func() {
				<-start
				builders[i], results[i] = newWindowsPrivateGeneration(ctx, parent, "exclusive", nil)
			})
		}
		close(start)
		wg.Wait()
		winners := 0
		for i, g := range builders {
			if g == nil {
				if results[i] != ReasonStateIO {
					t.Fatal("competing creator did not return fixed refusal")
				}
				continue
			}
			winners++
			ops := g.ops.(*windowsGenerationOps)
			windowsGenerationTestRescue(t, ops)
			if results[i] != nil || g.write(ctx, "payload", 1, bytes.NewReader([]byte{7})) != nil || g.complete(ctx) != nil || g.abort() != nil {
				t.Fatal("exclusive native creator did not preserve its successful tree")
			}
			windowsGenerationTestClosed(t, ops)
		}
		if winners != 1 {
			t.Fatal("FILE_CREATE did not select exactly one root owner")
		}
	})
}
