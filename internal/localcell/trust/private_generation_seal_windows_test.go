//go:build windows

package trust

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"unsafe"
)

// Native execution is an owner prerequisite. These fixtures use disposable
// fixed-NTFS trees only; they never exercise public Windows persistence.
type windowsSealFixture struct {
	g        *privateGeneration
	path     string
	expected []privateGenerationSealExpected
	payloads map[string][]byte
}

func windowsSealTestFixture(t *testing.T) windowsSealFixture {
	t.Helper()
	ctx := context.Background()
	g, ops, path := windowsGenerationTestBuilder(t, nil)
	if g.mkdir(ctx, "nested") != nil {
		t.Fatal("native seal nested fixture failed")
	}
	payloads := map[string][]byte{"data.bin": []byte("ordinary data"), "nested/tool.exe": []byte("synthetic executable label"), "private.json": []byte("private metadata")}
	for _, name := range []string{"data.bin", "nested/tool.exe", "private.json"} {
		b := payloads[name]
		if g.write(ctx, name, int64(len(b)), bytes.NewReader(b)) != nil {
			t.Fatal("native seal payload fixture failed")
		}
	}
	expected := make([]privateGenerationSealExpected, 0, len(g.nodes))
	for _, n := range g.nodes {
		e := privateGenerationSealExpected{name: n.name, directory: n.directory, size: n.size, mode: 0644}
		if n.directory {
			e.mode = 0700
			if n.name == "" {
				e.mode = 0500
			}
		} else {
			e.digest = sha256.Sum256(payloads[n.name])
			if n.name == "nested/tool.exe" {
				e.mode, e.executable = 0755, true
			}
			if n.name == "private.json" {
				e.mode, e.metadata = 0600, true
			}
		}
		expected = append(expected, e)
	}
	if g.complete(ctx) != nil || !g.completed || !g.terminal || g.result != nil {
		t.Fatal("native seal fixture did not complete D0A")
	}
	windowsGenerationTestClosed(t, ops)
	return windowsSealFixture{g: g, path: path, expected: expected, payloads: payloads}
}

func windowsSealTestStart(t *testing.T, f windowsSealFixture, hooks *windowsGenerationSealHooks) *sealedPrivateGeneration {
	t.Helper()
	installed := &windowsGenerationSealHooks{}
	if hooks != nil {
		*installed = *hooks
	}
	before := installed.before
	var owner *windowsGenerationSealOps
	installed.before = func(stage string, native *windowsGenerationSealFile) error {
		if native != nil {
			owner = native.owner
		}
		if before != nil {
			return before(stage, native)
		}
		return nil
	}
	t.Cleanup(func() {
		// Mutation witnesses may intentionally suppress actual native closure.
		// Rescue each exact Go owner, never a possibly reused raw handle number.
		if owner != nil {
			for native := range owner.files {
				if native.file.Fd() != uintptr(syscall.InvalidHandle) {
					_ = native.file.Close()
				}
			}
			if len(owner.parent.guards) != 0 {
				_ = owner.parent.close()
			}
		}
	})
	s, err := sealWindowsPrivateGeneration(context.Background(), f.g, f.expected, installed)
	if err != nil || s == nil {
		t.Fatal("native root seal or recovery/read sharing prerequisite failed")
	}
	t.Cleanup(func() { _ = s.close() })
	return s
}

func windowsSealTestProjection(t *testing.T, path string, directory bool) (privateRecordPolicy, privateRecordInfo, uint32, []byte) {
	t.Helper()
	h, err := windowsRecordOpenMetadata(path, 0, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE)
	if err != nil {
		t.Fatal("independent native seal readback open failed")
	}
	p, info, projectionErr := windowsRecordProjection(h, directory)
	var raw syscall.ByHandleFileInformation
	infoErr := syscall.GetFileInformationByHandle(h, &raw)
	descriptor, securityErr := windowsRecordSecurity(h)
	closeErr := syscall.CloseHandle(h)
	if projectionErr != nil || infoErr != nil || securityErr != nil || closeErr != nil {
		t.Fatal("independent native seal readback failed")
	}
	return p, info, raw.FileAttributes, descriptor
}

func windowsSealTestSecurity(t *testing.T, path string) []byte {
	t.Helper()
	h, err := windowsRecordOpenMetadata(path, 0, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE)
	if err != nil {
		t.Fatal("native security readback open failed")
	}
	b, readErr := windowsRecordSecurity(h)
	closeErr := syscall.CloseHandle(h)
	if readErr != nil || closeErr != nil {
		t.Fatal("native security readback failed")
	}
	return b
}

func windowsSealTestDACL(t *testing.T, descriptor []byte) []byte {
	t.Helper()
	if len(descriptor) < 20 {
		t.Fatal("independent descriptor header is truncated")
	}
	offset := int(binary.LittleEndian.Uint32(descriptor[16:20]))
	if offset < 20 || offset > len(descriptor)-8 {
		t.Fatal("independent descriptor DACL offset is invalid")
	}
	size := int(binary.LittleEndian.Uint16(descriptor[offset+2 : offset+4]))
	if size < 8 || size > len(descriptor)-offset {
		t.Fatal("independent descriptor DACL length is invalid")
	}
	return append([]byte(nil), descriptor[offset:offset+size]...)
}

func windowsSealTestExactRoot(t *testing.T, path string, identity [3]uint64) {
	t.Helper()
	p, info, _, _ := windowsSealTestProjection(t, path, true)
	want := 2
	if p.userIsSystem {
		want = 1
	}
	if !p.valid || !p.ownerUser || !p.protected || !p.daclPresent || p.reparse || !p.directory || info.identity != identity || len(p.aces) != want {
		t.Fatal("independent root descriptor identity or structure differs")
	}
	seen := map[privateRecordPrincipal]bool{}
	for _, ace := range p.aces {
		if ace.rights != 0x001200a9 || ace.kind != 0 || ace.flags != 0 || (ace.principal != recordUser && ace.principal != recordSystem) || seen[ace.principal] {
			t.Fatal("root does not have exactly current-user/System read-and-execute entries")
		}
		seen[ace.principal] = true
	}
	if !seen[recordUser] || (!p.userIsSystem && !seen[recordSystem]) {
		t.Fatal("root sealed trustee set differs")
	}
}

func windowsSealTestGrantedAccess(t *testing.T, f *windowsGenerationSealFile) uint32 {
	t.Helper()
	query := windowsRecordNTDLL.NewProc("NtQueryObject")
	var raw [56]byte // OBJECT_BASIC_INFORMATION, including GrantedAccess at byte 4.
	var returned uint32
	status, _, _ := query.Call(f.file.Fd(), 0, uintptr(unsafe.Pointer(&raw[0])), uintptr(len(raw)), uintptr(unsafe.Pointer(&returned)))
	if uint32(status) != 0 || returned > uint32(len(raw)) {
		t.Fatal("NOT RUN (Windows): native granted-access observation unavailable")
	}
	return binary.LittleEndian.Uint32(raw[4:8])
}

func windowsSealTestSDDL(t *testing.T, mask string) string {
	t.Helper()
	return strings.ReplaceAll(windowsGenerationTestSDDL(t), ";;FA;;;", ";;"+mask+";;;")
}

func windowsSealTestHandleDACL(t *testing.T, f *windowsGenerationSealFile, sddl string) {
	t.Helper()
	encoded, err := syscall.UTF16PtrFromString(sddl)
	if err != nil {
		t.Fatal("fixture descriptor encoding failed")
	}
	var descriptor *byte
	var size uint32
	r, _, _ := windowsRecordConvertDescriptor.Call(uintptr(unsafe.Pointer(encoded)), 1, uintptr(unsafe.Pointer(&descriptor)), uintptr(unsafe.Pointer(&size)))
	if descriptor != nil {
		defer func() { _, _ = syscall.LocalFree(syscall.Handle(unsafe.Pointer(descriptor))) }()
	}
	if r == 0 || descriptor == nil || size < 20 || size > 65536 {
		t.Fatal("fixture descriptor construction failed")
	}
	setter := windowsRecordAdvapi32.NewProc("SetKernelObjectSecurity")
	r, _, _ = setter.Call(f.file.Fd(), windowsRecordDACLSecurity|0x80000000, uintptr(unsafe.Pointer(descriptor)))
	if r == 0 {
		t.Fatal("fixture handle-bound DACL mutation failed")
	}
}

func windowsSealTestOpenAccess(path string, access uint32) (syscall.Handle, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return syscall.InvalidHandle, err
	}
	return syscall.CreateFile(p, access, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE, nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_OPEN_REPARSE_POINT|syscall.FILE_FLAG_BACKUP_SEMANTICS, 0)
}

func windowsSealTestRootCreateDenied(t *testing.T, f windowsSealFixture) {
	t.Helper()
	h, err := windowsSealTestOpenAccess(f.path, 0x0002) // FILE_ADD_FILE, no creation effect.
	if err == nil {
		_ = syscall.CloseHandle(h)
		t.Fatal("root child-creation right remained available after every reader closed")
	}
	if err != syscall.ERROR_ACCESS_DENIED {
		t.Fatal("root child-creation refusal was not the DACL access-denied boundary")
	}
}

func TestWindowsTrustPrivateGenerationSealAndVerify(t *testing.T) {
	f := windowsSealTestFixture(t)
	s := windowsSealTestStart(t, f, nil)
	ctx := context.Background()
	for _, n := range s.snapshot.nodes {
		path := filepath.Join(f.path, filepath.FromSlash(n.name))
		if n.name == "" {
			windowsSealTestExactRoot(t, path, n.identity)
			continue
		}
		p, info, attrs, _ := windowsSealTestProjection(t, path, n.directory)
		if checkPrivateRecordPolicy(p) != nil || info.identity != n.identity || attrs&syscall.FILE_ATTRIBUTE_READONLY != 0 {
			t.Fatal("descendant identity, full-access DACL or writable projection changed")
		}
		if !n.directory {
			b, err := s.read(ctx, n.name)
			if err != nil || !bytes.Equal(b, f.payloads[n.name]) {
				t.Fatal("sealed session read did not match expected identity and bytes")
			}
		}
	}
	if s.verify(ctx) != nil {
		t.Fatal("retained sealed verification failed")
	}
	ops := s.ops.(*windowsGenerationSealOps)
	for native := range ops.files {
		if native.recovery {
			if !native.closed || native.file.Fd() != uintptr(syscall.InvalidHandle) {
				t.Fatal("privileged recovery handle escaped transition")
			}
			continue
		}
		access := windowsSealTestGrantedAccess(t, native)
		if access&uint32(0x0002|0x0004|0x0010|0x0040|0x0100|0x10000|0x40000|0x80000) != 0 {
			t.Fatal("session native handle retains mutation capability")
		}
	}
	fresh, err := openWindowsSealedPrivateGeneration(ctx, s.snapshot, nil)
	if err != nil || fresh == nil || fresh.verify(ctx) != nil || fresh.close() != nil {
		t.Fatal("independent fresh sealed verifier failed")
	}
	if again, err := sealWindowsPrivateGeneration(ctx, f.g, f.expected, nil); err != ReasonStateIO || again != nil {
		if again != nil {
			_ = again.close()
		}
		t.Fatal("already sealed builder passed unsealed-input preflight")
	}
	firstClose, secondClose := s.close(), s.close()
	if firstClose != nil || secondClose != nil || f.g.abort() != nil || !f.g.completed {
		t.Fatal("successful close or completed-builder abort changed terminal state")
	}
	windowsSealTestExactRoot(t, f.path, s.snapshot.nodes[0].identity)
	windowsSealTestRootCreateDenied(t, f)
	h, err := windowsSealTestOpenAccess(filepath.Join(f.path, "data.bin"), 0x0002)
	if err != nil || syscall.CloseHandle(h) != nil {
		t.Fatal("descendant did not regain ordinary writable access after session close")
	}
}

func TestWindowsTrustPrivateGenerationSealPolicyRefusals(t *testing.T) {
	ctx := context.Background()
	for _, kind := range []string{"root-full-access", "wrong-mask", "extra-trustee", "unprotected", "null", "inherited", "writable-attribute-mismatch"} {
		t.Run(kind, func(t *testing.T) {
			f := windowsSealTestFixture(t)
			s := windowsSealTestStart(t, f, nil)
			if s.close() != nil {
				t.Fatal("policy fixture session close failed")
			}
			bad := windowsSealTestSDDL(t, "0x001200a9")
			security := uint32(windowsRecordDACLSecurity | 0x80000000)
			switch kind {
			case "root-full-access":
				bad = windowsGenerationTestSDDL(t)
			case "wrong-mask":
				bad = windowsSealTestSDDL(t, "0x001200ab")
			case "extra-trustee":
				bad += "(A;;FR;;;WD)"
			case "unprotected":
				bad = strings.Replace(bad, "D:P", "D:", 1)
				security = windowsRecordDACLSecurity | 0x20000000
			case "null":
				bad = "D:NO_ACCESS_CONTROL"
			case "inherited":
				bad = strings.ReplaceAll(bad, "(A;;", "(A;ID;")
			}
			if kind == "writable-attribute-mismatch" {
				p, err := syscall.UTF16PtrFromString(filepath.Join(f.path, "data.bin"))
				if err != nil || syscall.SetFileAttributes(p, syscall.FILE_ATTRIBUTE_READONLY) != nil {
					t.Fatal("readonly attribute fixture failed")
				}
				t.Cleanup(func() { _ = syscall.SetFileAttributes(p, syscall.FILE_ATTRIBUTE_NORMAL) })
				_, _, attrs, _ := windowsSealTestProjection(t, filepath.Join(f.path, "data.bin"), false)
				if attrs&syscall.FILE_ATTRIBUTE_READONLY == 0 {
					t.Fatal("native readonly fixture prerequisite absent")
				}
			} else if !windowsGenerationTestSetSecurity(f.path, bad, security) {
				t.Fatal("NOT RUN (Windows): actual invalid root descriptor fixture unavailable")
			}
			before := windowsSealTestSecurity(t, f.path)
			if kind == "null" {
				if len(before) < 20 || binary.LittleEndian.Uint32(before[16:20]) != 0 || binary.LittleEndian.Uint16(before[2:4])&0x0004 == 0 {
					t.Fatal("actual null DACL fixture prerequisite absent")
				}
			} else {
				p, _, _, _ := windowsSealTestProjection(t, f.path, true)
				if kind != "writable-attribute-mismatch" && checkPrivateGenerationSealPolicy(p) == nil {
					t.Fatal("invalid native descriptor fixture prerequisite absent")
				}
				if kind == "inherited" && (len(p.aces) == 0 || p.aces[0].flags&0x10 == 0) {
					t.Fatal("NOT RUN (Windows): native inherited ACE fixture unavailable")
				}
			}
			opened, err := openWindowsSealedPrivateGeneration(ctx, s.snapshot, nil)
			if opened != nil {
				_ = opened.close()
			}
			after := windowsSealTestSecurity(t, f.path)
			if err != ReasonStateIO || opened != nil || !bytes.Equal(before, after) {
				t.Fatal("fresh verifier repaired or accepted invalid native policy")
			}
		})
	}
	for _, kind := range []string{"malformed", "wrong-owner"} {
		t.Run("injected-"+kind, func(t *testing.T) {
			f := windowsSealTestFixture(t)
			s := windowsSealTestStart(t, f, nil)
			if s.close() != nil {
				t.Fatal("injected policy fixture close failed")
			}
			if kind == "wrong-owner" && windowsGenerationTestSetSecurity(f.path, "O:SY", windowsRecordOwnerSecurity) {
				p, _, _, _ := windowsSealTestProjection(t, f.path, true)
				if !p.ownerUser {
					before := windowsSealTestSecurity(t, f.path)
					opened, err := openWindowsSealedPrivateGeneration(ctx, s.snapshot, nil)
					if opened != nil {
						_ = opened.close()
					}
					if err != ReasonStateIO || opened != nil || !bytes.Equal(before, windowsSealTestSecurity(t, f.path)) {
						t.Fatal("actual wrong-owner fixture was accepted or repaired")
					}
					t.Log("actual distinct native owner fixture exercised")
					return
				}
			}
			seen := false
			hooks := &windowsGenerationSealHooks{observe: func(_ string, native *windowsGenerationSealFile, p *privateGenerationSealObservation) {
				if native == nil || native.index != 0 || seen {
					return
				}
				if checkPrivateGenerationSealPolicy(p.policy) != nil {
					t.Fatal("native good projection prerequisite absent")
				}
				seen = true
				switch kind {
				case "malformed":
					p.policy.valid = false
				case "wrong-owner":
					p.policy.ownerUser = false
				}
			}}
			opened, err := openWindowsSealedPrivateGeneration(ctx, s.snapshot, hooks)
			if opened != nil {
				_ = opened.close()
			}
			if !seen || opened != nil || err != ReasonStateIO {
				t.Fatal("single injected native policy observation was accepted")
			}
			t.Log("bounded injected observation after real native readback; no actual malformed/owner mutation is claimed")
		})
	}
	t.Run("post-set-wrong-mask", func(t *testing.T) {
		f := windowsSealTestFixture(t)
		var bad privateGenerationSealObservation
		changed, captured, isolated := false, false, false
		hooks := &windowsGenerationSealHooks{afterSet: func(native *windowsGenerationSealFile, restore bool) error {
			if restore {
				return nil
			}
			windowsSealTestHandleDACL(t, native, windowsSealTestSDDL(t, "0x001200ab"))
			p, info, err := windowsRecordProjection(syscall.Handle(native.file.Fd()), true)
			if err != nil || len(p.aces) == 0 || p.aces[0].rights != 0x001200ab {
				t.Fatal("actual post-set wrong-mask fixture prerequisite failed")
			}
			bad = privateGenerationSealObservation{policy: p, size: info.size}
			changed, captured = true, true
			return nil
		}, observe: func(stage string, native *windowsGenerationSealFile, p *privateGenerationSealObservation) {
			if stage != "post-set" || !captured || isolated {
				return
			}
			windowsSealTestHandleDACL(t, native, windowsSealTestSDDL(t, "0x001200a9"))
			*p = bad
			isolated = true
		}}
		s, err := sealWindowsPrivateGeneration(ctx, f.g, f.expected, hooks)
		if s != nil {
			_ = s.close()
		}
		if !changed || !captured || !isolated || err != ReasonStateIO || s != nil {
			t.Fatal("isolated adapter post-set wrong-mask observation reached success")
		}
		p, _, _, _ := windowsSealTestProjection(t, f.path, true)
		if checkPrivateRecordPolicy(p) != nil {
			t.Fatal("post-set observation failure did not restore original root DACL")
		}
	})
}

func TestWindowsTrustPrivateGenerationSealHandleTransitions(t *testing.T) {
	ctx := context.Background()
	t.Run("recovery-reader-compatible", func(t *testing.T) {
		f := windowsSealTestFixture(t)
		seen := false
		hooks := &windowsGenerationSealHooks{before: func(stage string, native *windowsGenerationSealFile) error {
			if stage == "set-root" {
				access := windowsSealTestGrantedAccess(t, native)
				if access&uint32(0x40000|0x20000|0x80) != 0x40000|0x20000|0x80 || access&^uint32(0x40000|0x20000|0x80|0x100000) != 0 || len(native.owner.readers) != len(f.expected) {
					t.Fatal("actual minimal recovery and complete reader set did not coexist")
				}
				for _, reader := range native.owner.readers {
					if reader.share != syscall.FILE_SHARE_READ || reader.file.Fd() == uintptr(syscall.InvalidHandle) {
						t.Fatal("compatibility used widened sharing or a closed reader")
					}
				}
				seen = true
			}
			return nil
		}}
		s := windowsSealTestStart(t, f, hooks)
		if !seen || s.close() != nil {
			t.Fatal("native recovery/read compatibility was not observed")
		}
	})
	t.Run("existing-writer", func(t *testing.T) {
		f := windowsSealTestFixture(t)
		writer, err := windowsSealTestOpenAccess(filepath.Join(f.path, "data.bin"), 0x0002)
		if err != nil {
			t.Fatal("native pre-existing writer fixture unavailable")
		}
		defer func() { _ = syscall.CloseHandle(writer) }()
		sets := 0
		hooks := &windowsGenerationSealHooks{before: func(stage string, _ *windowsGenerationSealFile) error {
			if stage == "set-root" {
				sets++
			}
			return nil
		}}
		s, err := sealWindowsPrivateGeneration(ctx, f.g, f.expected, hooks)
		if s != nil {
			_ = s.close()
		}
		p, _, _, _ := windowsSealTestProjection(t, f.path, true)
		if err != ReasonStateIO || s != nil || sets != 0 || checkPrivateRecordPolicy(p) != nil {
			t.Fatal("pre-existing writer reached root mutation or success")
		}
	})
	t.Run("conflicting-opens", func(t *testing.T) {
		f := windowsSealTestFixture(t)
		s := windowsSealTestStart(t, f, nil)
		path := filepath.Join(f.path, "data.bin")
		p, _, attrs, _ := windowsSealTestProjection(t, path, false)
		if checkPrivateRecordPolicy(p) != nil || attrs&syscall.FILE_ATTRIBUTE_READONLY != 0 {
			t.Fatal("conflicting-open fixture is not an ordinarily writable descendant")
		}
		for _, access := range []uint32{0x0002, 0x10000} {
			h, err := windowsSealTestOpenAccess(path, access)
			if err == nil {
				_ = syscall.CloseHandle(h)
				t.Fatal("session allowed new descendant WRITE_DATA or DELETE capability")
			}
			if err != syscall.Errno(32) {
				t.Fatal("conflicting open did not reach strict reader-sharing refusal")
			}
		}
		if s.close() != nil {
			t.Fatal("conflicting-open session close failed")
		}
		for _, access := range []uint32{0x0002, 0x10000} {
			h, err := windowsSealTestOpenAccess(path, access)
			if err != nil || syscall.CloseHandle(h) != nil {
				t.Fatal("ordinary descendant access did not return after reader closure")
			}
		}
	})
	t.Run("privileged-closed", func(t *testing.T) {
		f := windowsSealTestFixture(t)
		var recovery *windowsGenerationSealFile
		closed := false
		hooks := &windowsGenerationSealHooks{before: func(stage string, native *windowsGenerationSealFile) error {
			if native != nil && native.recovery {
				recovery = native
				if stage == "closed" {
					closed = native.file.Fd() == uintptr(syscall.InvalidHandle)
				}
			}
			return nil
		}}
		s := windowsSealTestStart(t, f, hooks)
		if recovery == nil || !closed || recovery.file.Fd() != uintptr(syscall.InvalidHandle) {
			t.Fatal("success preceded actual privileged recovery close")
		}
		if s.close() != nil {
			t.Fatal("privileged-closed fixture session close failed")
		}
	})
	t.Run("child-delete-after-close", func(t *testing.T) {
		f := windowsSealTestFixture(t)
		s := windowsSealTestStart(t, f, nil)
		if s.close() != nil {
			t.Fatal("child-delete limit session close failed")
		}
		windowsSealTestExactRoot(t, f.path, s.snapshot.nodes[0].identity)
		windowsSealTestRootCreateDenied(t, f)
		path := filepath.Join(f.path, "data.bin")
		p, original, _, _ := windowsSealTestProjection(t, path, false)
		if checkPrivateRecordPolicy(p) != nil {
			t.Fatal("child-delete limit requires direct-child full access")
		}
		h, err := windowsSealTestOpenAccess(path, 0x10000|windowsRecordReadAttributes)
		if err != nil {
			t.Fatal("documented child DELETE capability did not return after close")
		}
		var observed syscall.ByHandleFileInformation
		infoErr, closeErr := syscall.GetFileInformationByHandle(h, &observed), syscall.CloseHandle(h)
		if infoErr != nil || closeErr != nil || windowsRecordIdentity(observed) != original.identity {
			t.Fatal("child DELETE capability did not bind the same retained identity")
		}
		fresh, err := openWindowsSealedPrivateGeneration(ctx, s.snapshot, nil)
		if err != nil || fresh == nil || fresh.verify(ctx) != nil || fresh.close() != nil {
			t.Fatal("non-mutating child DELETE capability changed fresh sealed verification")
		}
		t.Log("root-only seal does not protect child DELETE capability after readers close; no deletion or rename syscall is exercised")
	})
}

func TestWindowsTrustPrivateGenerationSealReparseAndReplacement(t *testing.T) {
	ctx := context.Background()
	t.Run("native-hardlink", func(t *testing.T) {
		f := windowsSealTestFixture(t)
		path := filepath.Join(f.path, "data.bin")
		alias := filepath.Join(filepath.Dir(f.path), "outside-link")
		if os.Link(path, alias) != nil {
			t.Fatal("NOT RUN (Windows): actual hardlink fixture unavailable")
		}
		h, err := windowsSealTestOpenAccess(path, windowsRecordReadAttributes)
		if err != nil {
			t.Fatal("hardlink fixture readback open failed")
		}
		var raw syscall.ByHandleFileInformation
		infoErr, closeErr := syscall.GetFileInformationByHandle(h, &raw), syscall.CloseHandle(h)
		if infoErr != nil || closeErr != nil || raw.NumberOfLinks != 2 {
			t.Fatal("native hardlink fixture did not expose the actual link count")
		}
		s, err := sealWindowsPrivateGeneration(ctx, f.g, f.expected, nil)
		if s != nil {
			_ = s.close()
		}
		b, readErr := os.ReadFile(alias)
		if err != ReasonStateIO || s != nil || readErr != nil || !bytes.Equal(b, f.payloads["data.bin"]) {
			t.Fatal("native hardlink refusal changed linked bytes or accepted an external alias")
		}
	})
	t.Run("replacement-before-acquisition", func(t *testing.T) {
		f := windowsSealTestFixture(t)
		path := filepath.Join(f.path, "data.bin")
		original := f.g.names["data.bin"].handle.(*windowsGenerationFile).identity
		if os.Rename(path, filepath.Join(f.path, "retained.bin")) != nil || os.WriteFile(path, []byte("foreign sentinel"), 0600) != nil {
			t.Fatal("native replacement fixture unavailable")
		}
		_, changed, _, _ := windowsSealTestProjection(t, path, false)
		if changed.identity == original {
			t.Fatal("replacement fixture did not acquire a distinct native identity")
		}
		s, err := sealWindowsPrivateGeneration(ctx, f.g, f.expected, nil)
		if s != nil {
			_ = s.close()
		}
		b, readErr := os.ReadFile(path)
		if err != ReasonStateIO || s != nil || readErr != nil || string(b) != "foreign sentinel" {
			t.Fatal("replacement seal refusal changed the foreign sentinel")
		}
	})
	t.Run("native-junction", func(t *testing.T) {
		f := windowsSealTestFixture(t)
		outside := filepath.Join(filepath.Dir(f.path), "outside")
		if os.Mkdir(outside, 0700) != nil || os.WriteFile(filepath.Join(outside, "sentinel"), []byte("foreign"), 0600) != nil || os.Rename(filepath.Join(f.path, "nested"), filepath.Join(f.path, "retained")) != nil {
			t.Fatal("native junction fixture setup failed")
		}
		junction := filepath.Join(f.path, "nested")
		windowsGenerationTestJunction(t, junction, outside)
		t.Cleanup(func() { _ = os.Remove(junction) })
		s, err := sealWindowsPrivateGeneration(ctx, f.g, f.expected, nil)
		if s != nil {
			_ = s.close()
		}
		b, readErr := os.ReadFile(filepath.Join(outside, "sentinel"))
		if err != ReasonStateIO || s != nil || readErr != nil || string(b) != "foreign" {
			t.Fatal("junction seal refusal changed foreign sentinel")
		}
	})
	for _, kind := range []string{"identity", "reparse", "type"} {
		t.Run("observed-"+kind+"-before-set", func(t *testing.T) {
			f := windowsSealTestFixture(t)
			hit, sets := false, 0
			hooks := &windowsGenerationSealHooks{observe: func(stage string, native *windowsGenerationSealFile, p *privateGenerationSealObservation) {
				if stage != "pre-set" || native.index != 0 || hit {
					return
				}
				if checkPrivateRecordPolicy(p.policy) != nil {
					t.Fatal("real native pre-set projection prerequisite absent")
				}
				hit = true
				switch kind {
				case "identity":
					p.policy.observedIdentity[2] ^= 1
				case "reparse":
					p.policy.reparse = true
				case "type":
					p.policy.directory = false
				}
			}, before: func(stage string, _ *windowsGenerationSealFile) error {
				if stage == "set-root" {
					sets++
				}
				return nil
			}}
			s, err := sealWindowsPrivateGeneration(ctx, f.g, f.expected, hooks)
			if s != nil {
				_ = s.close()
			}
			if !hit || err != ReasonStateIO || s != nil || sets != 0 {
				t.Fatal("injected confirmed pre-set observation reached root mutation")
			}
			t.Log("bounded observation injection after native readback; no elimination of unobserved in-place races is claimed")
		})
	}
}

func TestWindowsTrustPrivateGenerationSealRollbackAndCloseUncertainty(t *testing.T) {
	ctx := context.Background()
	for _, boundary := range []string{"set", "readback", "inventory", "foreign-root", "restore-failure", "close-before-confirmation", "close-after-actual-close"} {
		t.Run(boundary, func(t *testing.T) {
			f := windowsSealTestFixture(t)
			originalDACL := windowsSealTestDACL(t, windowsSealTestSecurity(t, f.path))
			sentinel := filepath.Join(filepath.Dir(f.path), "foreign-sentinel")
			if os.WriteFile(sentinel, []byte("foreign"), 0600) != nil {
				t.Fatal("rollback sentinel fixture failed")
			}
			armed, reached, restores, recoveryCloses := false, false, 0, 0
			var uncertain *windowsGenerationSealFile
			closes := map[*windowsGenerationSealFile]int{}
			var owner *windowsGenerationSealOps
			hooks := &windowsGenerationSealHooks{afterSet: func(_ *windowsGenerationSealFile, restore bool) error {
				if restore {
					return nil
				}
				armed = true
				if boundary == "set" || boundary == "foreign-root" || boundary == "restore-failure" {
					reached = true
					return ReasonStateIO
				}
				return nil
			}, observe: func(stage string, native *windowsGenerationSealFile, p *privateGenerationSealObservation) {
				if !armed {
					return
				}
				if boundary == "foreign-root" && stage == "rollback" {
					p.policy.observedIdentity[2] ^= 1
				}
				if boundary == "readback" && stage == "post-set" && !reached {
					reached = true
					p.policy.valid = false
				}
				if boundary == "inventory" && stage == "pre-inventory" && native.index == 0 && !reached {
					reached = true
					p.policy.valid = false
				}
			}, before: func(stage string, native *windowsGenerationSealFile) error {
				if native != nil {
					owner = native.owner
					if stage == "close" {
						closes[native]++
					}
				}
				if stage == "restore-root" {
					restores++
					if boundary == "restore-failure" {
						return ReasonStateIO
					}
				}
				if native != nil && native.recovery && stage == "close" {
					recoveryCloses++
				}
				if !armed || native == nil || !native.recovery {
					return nil
				}
				if (boundary == "close-before-confirmation" && stage == "close") || (boundary == "close-after-actual-close" && stage == "closed") {
					reached, uncertain = true, native
					return ReasonStateIO
				}
				return nil
			}}
			s, err := sealWindowsPrivateGeneration(ctx, f.g, f.expected, hooks)
			if uncertain != nil {
				t.Cleanup(func() {
					// Rescue the exact Go owner, never retry an ambiguous raw number.
					if uncertain.file.Fd() != uintptr(syscall.InvalidHandle) {
						_ = uncertain.file.Close()
					}
				})
			}
			if s != nil {
				_ = s.close()
			}
			if !armed || !reached || err != ReasonStateIO || s != nil || recoveryCloses != 1 {
				t.Fatal("failed native transition succeeded or retried recovery close")
			}
			if owner == nil || len(owner.parent.guards) != 0 {
				t.Fatal("native failure retained ancestor guards")
			}
			for native := range owner.files {
				if closes[native] != 1 || !native.closeAttempted || (native != uncertain && native.file.Fd() != uintptr(syscall.InvalidHandle)) {
					t.Fatal("native failure did not attempt all owned closes exactly once")
				}
			}
			unclear := boundary == "foreign-root" || boundary == "restore-failure" || boundary == "close-before-confirmation" || boundary == "close-after-actual-close"
			p, rootInfo, _, rootDescriptor := windowsSealTestProjection(t, f.path, true)
			if rootInfo.identity != f.g.nodes[0].handle.(*windowsGenerationFile).identity {
				t.Fatal("rollback or uncertain close replaced the completed root")
			}
			if unclear {
				if checkPrivateGenerationSealPolicy(p) != nil {
					t.Fatal("uncertain failure did not preserve possible sealed residue")
				}
			} else if restores != 1 || checkPrivateRecordPolicy(p) != nil || !bytes.Equal(originalDACL, windowsSealTestDACL(t, rootDescriptor)) {
				t.Fatal("native failed mutation did not restore exact original root policy")
			}
			if boundary == "foreign-root" && restores != 0 {
				t.Fatal("changed observed identity reached rollback DACL setter")
			}
			if boundary == "foreign-root" {
				t.Log("bounded changed-identity observation after native readback; strict reader sharing prevents an actual root rename while retained")
			}
			if uncertain != nil {
				actuallyClosed := uncertain.file.Fd() == uintptr(syscall.InvalidHandle)
				if actuallyClosed != (boundary == "close-after-actual-close") {
					t.Fatal("native close uncertainty did not distinguish before/after actual closure")
				}
			}
			for name, want := range f.payloads {
				got, readErr := os.ReadFile(filepath.Join(f.path, filepath.FromSlash(name)))
				if readErr != nil || !bytes.Equal(got, want) {
					t.Fatal("rollback or uncertain close changed completed descendant bytes")
				}
				_, info, _, _ := windowsSealTestProjection(t, filepath.Join(f.path, filepath.FromSlash(name)), false)
				if info.identity != f.g.names[name].handle.(*windowsGenerationFile).identity {
					t.Fatal("rollback or uncertain close replaced a completed descendant")
				}
			}
			if f.g.abort() != nil || !f.g.completed {
				t.Fatal("seal failure changed completed-builder terminal state")
			}
			if b, err := os.ReadFile(sentinel); err != nil || string(b) != "foreign" {
				t.Fatal("native rollback changed the foreign sentinel")
			}
		})
	}
}
