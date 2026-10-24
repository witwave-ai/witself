//go:build windows

package runtimeconfig

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestLocalSpoolUnderProcessTemporaryDirectory(t *testing.T) {
	dir := privateDir(t)
	assertSpoolPrivateDescriptor(t, dir)
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal("cannot resolve private spool fixture")
	}
	temporary, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		t.Fatal("cannot resolve process temporary directory")
	}
	relative, err := filepath.Rel(temporary, resolved)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, `..\`) {
		t.Fatal("private spool fixture must be below the process temporary directory")
	}
	// EvalSymlinks normalizes the drive to upper case. This alternate spelling
	// always exercises normalization even when the runner uses no short names.
	for _, path := range []string{dir, strings.ToLower(resolved[:1]) + resolved[1:]} {
		if err := ValidateSpoolDir(path); err != nil {
			t.Error("private child of the temporary directory was refused")
			t.Fatal(err)
		}
		root, err := OpenSpoolRoot(path)
		if err != nil {
			t.Error("private child of the temporary directory could not be opened")
			t.Fatal(err)
		}
		if err := root.Close(); err != nil {
			t.Fatal("cannot close private spool root")
		}
	}
}

func TestLocalSpoolRejectsProcessTemporaryDirectory(t *testing.T) {
	t.Run("runner-temp", func(t *testing.T) {
		assertSpoolTemporaryRefused(t, os.TempDir())
	})
	t.Run("private-temp", func(t *testing.T) {
		dir := privateDir(t)
		assertSpoolPrivateDescriptor(t, dir)
		t.Setenv("TMP", dir)
		t.Setenv("TEMP", dir)
		actual, err := os.Stat(os.TempDir())
		if err != nil {
			t.Fatal("cannot read process temporary fixture")
		}
		expected, err := os.Stat(dir)
		if err != nil || !os.SameFile(actual, expected) {
			t.Fatal("process temporary fixture did not take effect")
		}
		assertSpoolTemporaryRefused(t, dir)
		assertSpoolTemporaryRefused(t, strings.ToLower(dir[:1])+dir[1:])
	})
}

func assertSpoolTemporaryRefused(t *testing.T, path string) {
	t.Helper()
	if err := ValidateSpoolDir(path); err != errSpoolTemporary {
		t.Fatal("temporary directory must have the specific refusal diagnostic")
	}
	root, err := OpenSpoolRoot(path)
	if root != nil {
		_ = root.Close()
		t.Fatal("temporary directory returned an open spool root")
	}
	if err != errSpoolTemporary {
		t.Fatal("temporary directory opening must retain the specific refusal diagnostic")
	}
}

func TestLocalSpoolWindowsRefusalDiagnostics(t *testing.T) {
	for _, test := range []struct {
		name  string
		cause error
		want  error
	}{
		{"private", errSpool, errSpool},
		{"temporary", errSpoolTemporary, errSpoolTemporary},
		{"identity", errSpoolIdentity, errSpoolIdentity},
		{"guard-identity", errWindowsPrivatePathChanged, errSpoolIdentity},
		{"unknown", errors.New("synthetic-private-value"), errSpool},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, err := openSpoolRoot("", func(string) (os.FileInfo, error) {
				return nil, test.cause
			})
			if root != nil || err != test.want {
				t.Fatal("spool refusal lost its value-free classification")
			}
		})
	}
}

func TestLocalSpoolOwnershipAndModes(t *testing.T) {
	dir := privateDir(t)
	assertSpoolPrivateDescriptor(t, dir)
	root, err := OpenSpoolRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	broadenSpoolDACL(t, dir)
	assertSpoolRefused(t, dir)
}

// Independently read the descriptor and compare binary SIDs, rather than
// relying on the production validator or localized account names.
func assertSpoolPrivateDescriptor(t *testing.T, path string) {
	t.Helper()
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil || descriptor == nil {
		t.Fatal("cannot read private spool security descriptor")
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatal(err)
	}
	owner, _, err := descriptor.Owner()
	if err != nil || owner == nil || !owner.Equals(user.User.Sid) {
		t.Fatal("spool owner differs from current user")
	}
	control, _, err := descriptor.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatal("spool DACL is not protected")
	}
	wantCount := uint16(2)
	if user.User.Sid.Equals(system) {
		wantCount = 1
	}
	dacl, _, err := descriptor.DACL()
	if err != nil || dacl == nil || dacl.AceCount != wantCount {
		t.Fatal("spool DACL must contain exactly the user and SYSTEM grants")
	}
	var sawUser, sawSystem bool
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			t.Fatal(err)
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags != 0 || ace.Mask != windows.ACCESS_MASK(0x1f01ff) {
			t.Fatal("spool contains an unexpected grant")
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		switch {
		case sid.Equals(user.User.Sid):
			sawUser = true
		case sid.Equals(system):
			sawSystem = true
		default:
			t.Fatal("spool contains a grant to another principal")
		}
	}
	if !sawUser || (!sawSystem && !user.User.Sid.Equals(system)) {
		t.Fatal("spool user or SYSTEM grant is absent")
	}
}

func broadenSpoolDACL(t *testing.T, path string) {
	t.Helper()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + user.User.Sid.String() + ")(A;;FA;;;SY)(A;;FR;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
}

func assertSpoolRefused(t *testing.T, path string) {
	t.Helper()
	if err := ValidateSpoolDir(path); err != errSpool {
		t.Fatal("spool validation must refuse with the fixed value-free diagnostic")
	}
	root, err := OpenSpoolRoot(path)
	if root != nil {
		_ = root.Close()
		t.Fatal("unsafe spool returned an open root")
	}
	if err != errSpool {
		t.Fatal("spool opening must refuse with the fixed value-free diagnostic")
	}
}

func TestLocalSpoolRejectsACLChangeAfterValidation(t *testing.T) {
	dir := privateDir(t)
	root, err := openSpoolRoot(dir, func(path string) (os.FileInfo, error) {
		before, err := validatedSpoolDirectory(path)
		if err == nil {
			broadenSpoolDACL(t, path)
		}
		return before, err
	})
	if root != nil {
		_ = root.Close()
		t.Fatal("spool descriptor was not revalidated before returning the root")
	}
	if err != errSpool {
		t.Fatal("changed ACL must produce the fixed value-free refusal")
	}
}

func TestLocalSpoolRejectsSameDirectoryJunctionReplacement(t *testing.T) {
	dir := privateDir(t)
	root, err := openSpoolRoot(dir, func(path string) (os.FileInfo, error) {
		before, err := validatedSpoolDirectory(path)
		if err != nil {
			return nil, err
		}
		retired := path + "-retired"
		if err := os.Rename(path, retired); err != nil {
			t.Fatal(err)
		}
		privateMkdir(t, path)
		makeSpoolJunction(t, path, retired)
		return before, nil
	})
	if root != nil {
		_ = root.Close()
		t.Fatal("junction replacement targeting the same directory escaped path validation")
	}
	if err != errSpool {
		t.Fatal("junction replacement must produce the fixed value-free refusal")
	}
}

func TestLocalSpoolRejectsJunctionComponents(t *testing.T) {
	parent := privateDir(t)
	target := filepath.Join(parent, "target")
	privateMkdir(t, target)
	child := filepath.Join(target, "child")
	privateMkdir(t, child)
	before, err := os.Stat(target)
	if err != nil {
		t.Fatal("cannot read junction target identity")
	}
	childBefore, err := os.Stat(child)
	if err != nil {
		t.Fatal("cannot read junction target child identity")
	}
	marker := filepath.Join(target, "preserved")
	file, err := WindowsPrivateOpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY)
	if err != nil {
		t.Fatal("cannot create junction target contents")
	}
	_, writeErr := file.WriteString("synthetic-preserved-content")
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatal("cannot write junction target contents")
	}
	assertSpoolPrivateDescriptor(t, target)
	junction := filepath.Join(parent, "junction")
	privateMkdir(t, junction)
	makeSpoolJunction(t, junction, target)
	for _, path := range []string{junction, filepath.Join(junction, "child")} {
		assertSpoolRefused(t, path)
	}
	after, err := os.Stat(target)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("junction rejection changed the target identity")
	}
	childAfter, err := os.Stat(child)
	if err != nil || !os.SameFile(childBefore, childAfter) {
		t.Fatal("junction rejection changed the target child identity")
	}
	contents, err := os.ReadFile(marker)
	if err != nil || string(contents) != "synthetic-preserved-content" {
		t.Fatal("junction rejection changed the target contents")
	}
	entries, err := os.ReadDir(target)
	if err != nil || len(entries) != 2 || entries[0].Name() != "child" || entries[1].Name() != "preserved" {
		t.Fatal("junction rejection changed the target entries")
	}
	assertSpoolPrivateDescriptor(t, target)
	if err := ValidateSpoolDir(target); err != nil {
		t.Error("unchanged private junction target was refused by spool validation")
		t.Fatal(err)
	}
}

// Mount-point reparse data needs no symlink privilege, so Windows CI always
// exercises ancestor and leaf reparse refusal even without Developer Mode.
func makeSpoolJunction(t *testing.T, path, target string) {
	t.Helper()
	buffer := spoolJunctionData(t, target)
	pathUTF16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(pathUTF16, windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	if err := setSpoolJunction(handle, buffer); err != nil {
		t.Fatal("junction fixture creation failed:", err)
	}
}

func spoolJunctionData(t *testing.T, target string) []byte {
	t.Helper()
	substitute, err := windows.UTF16FromString(`\??\` + target)
	if err != nil {
		t.Fatal(err)
	}
	printName, err := windows.UTF16FromString(target)
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 16+2*(len(substitute)+len(printName)))
	binary.LittleEndian.PutUint32(buffer[0:4], windows.IO_REPARSE_TAG_MOUNT_POINT)
	binary.LittleEndian.PutUint16(buffer[4:6], uint16(len(buffer)-8))
	binary.LittleEndian.PutUint16(buffer[10:12], uint16(2*(len(substitute)-1)))
	binary.LittleEndian.PutUint16(buffer[12:14], uint16(2*len(substitute)))
	binary.LittleEndian.PutUint16(buffer[14:16], uint16(2*(len(printName)-1)))
	for i, value := range append(substitute, printName...) {
		binary.LittleEndian.PutUint16(buffer[16+2*i:18+2*i], value)
	}
	return buffer
}

func setSpoolJunction(handle windows.Handle, buffer []byte) error {
	var returned uint32
	return windows.DeviceIoControl(handle, windows.FSCTL_SET_REPARSE_POINT, &buffer[0], uint32(len(buffer)), nil, 0, &returned, nil)
}

func TestLocalSpoolRejectsUNCAndReservedPaths(t *testing.T) {
	parent := privateDir(t)
	for _, path := range []string{`\\server.invalid\share\private-spool`, filepath.Join(parent, "NUL"), filepath.Join(parent, "COM1.txt")} {
		assertSpoolRefused(t, path)
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Fatal("invalid spool path validation changed the parent directory")
	}
}
