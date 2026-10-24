//go:build windows

package runtimeconfig

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func windowsPrivateTestRoot(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "private")
	if err := WindowsPrivateMkdir(path); err != nil {
		t.Fatal(err)
	}
	return path
}

func windowsPrivateTestDescriptor(t *testing.T, sddl string) *windows.SECURITY_DESCRIPTOR {
	t.Helper()
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatal("cannot construct test descriptor")
	}
	return descriptor
}

func windowsPrivateTestUser(t *testing.T) string {
	t.Helper()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil {
		t.Fatal("cannot obtain current user SID")
	}
	return user.User.Sid.String()
}

func windowsPrivateTestBroaden(t *testing.T, path string) {
	t.Helper()
	descriptor := windowsPrivateTestDescriptor(t, "D:P(A;;FA;;;"+windowsPrivateTestUser(t)+")(A;;FA;;;SY)(A;;FR;;;WD)")
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsPrivateDescriptorPolicy(t *testing.T) {
	user := windowsPrivateTestUser(t)
	owner := "O:" + user
	userAllow := "(A;;FA;;;" + user + ")"
	cases := []struct {
		name string
		sddl string
	}{
		{"inherited_user", owner + "D:P(A;ID;FA;;;" + user + ")(A;;FA;;;SY)"},
		{"everyone_replaces_system", owner + "D:P" + userAllow + "(A;;FA;;;WD)"},
		{"users_replaces_system", owner + "D:P" + userAllow + "(A;;FA;;;BU)"},
		{"administrators", owner + "D:P" + userAllow + "(A;;FA;;;BA)"},
		{"authenticated_users", owner + "D:P" + userAllow + "(A;;FA;;;AU)"},
		{"unprotected", owner + "D:" + userAllow + "(A;;FA;;;SY)"},
		{"inheritable", owner + "D:P(A;OICI;FA;;;" + user + ")(A;;FA;;;SY)"},
		{"wrong_owner", "O:WDD:P" + userAllow + "(A;;FA;;;SY)"},
		{"read_only_user", owner + "D:P(A;;FR;;;" + user + ")(A;;FA;;;SY)"},
		{"missing_system", owner + "D:P" + userAllow},
		{"deny_ace", owner + "D:P(D;;FA;;;SY)" + userAllow},
		{"empty_dacl", owner + "D:P"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			descriptor := windowsPrivateTestDescriptor(t, test.sddl)
			var private windowsPrivateError
			if err := windowsPrivateValidateDescriptor(descriptor); err == nil || !errors.As(err, &private) {
				t.Fatal("unsafe descriptor must fail with a value-free error")
			}
		})
	}
	for _, present := range []bool{true, false} {
		descriptor := windowsPrivateTestDescriptor(t, owner+"D:P"+userAllow+"(A;;FA;;;SY)")
		absolute, err := descriptor.ToAbsolute()
		if err != nil {
			t.Fatal(err)
		}
		if err := absolute.SetDACL(nil, present, false); err != nil {
			t.Fatal(err)
		}
		if err := windowsPrivateValidateDescriptor(absolute); err == nil {
			t.Fatal("null or missing DACL accepted")
		}
	}
	if err := windowsPrivateValidateDescriptor(nil); err == nil {
		t.Fatal("unavailable descriptor accepted")
	}
	descriptor, err := windowsPrivateDescriptor()
	if err != nil {
		t.Fatal(err)
	}
	if err := windowsPrivateValidateDescriptor(descriptor); err != nil {
		t.Fatal("canonical policy was rejected")
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(dacl, 0, &ace); err != nil {
		t.Fatal(err)
	}
	ace.Header.AceType = 17 // An unfamiliar access-bearing ACE must fail closed.
	if err := windowsPrivateValidateDescriptor(descriptor); err == nil {
		t.Fatal("unknown ACE accepted")
	}
}

func TestWindowsPrivateCreationVerifiedBeforeData(t *testing.T) {
	root := windowsPrivateTestRoot(t)
	t.Run("directory_post_create", func(t *testing.T) {
		path := filepath.Join(root, "directory")
		called := false
		err := windowsPrivateMkdir(path, func(created string) error {
			called = true
			assertSpoolPrivateDescriptor(t, created)
			windowsPrivateTestBroaden(t, created)
			return nil
		})
		if !called || err == nil {
			t.Fatal("directory creation bypassed post-create security verification")
		}
	})
	t.Run("password_before_write", func(t *testing.T) {
		path := filepath.Join(root, "database.password")
		called := false
		file, err := windowsPrivateOpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, func(handle windows.Handle) error {
			called = true
			// This point is immediately after CreateFile, before verification
			// or return to the caller that could publish password bytes.
			assertSpoolPrivateDescriptor(t, path)
			var info windows.ByHandleFileInformation
			if err := windows.GetFileInformationByHandle(handle, &info); err != nil || info.FileSizeHigh != 0 || info.FileSizeLow != 0 {
				t.Fatal("new password contains bytes before security validation")
			}
			windowsPrivateTestBroaden(t, path)
			return nil
		})
		if file != nil {
			_ = file.Close()
			t.Fatal("unverified password handle escaped to a writer")
		}
		if !called || err == nil {
			t.Fatal("password creation bypassed post-create security verification")
		}
		info, statErr := os.Stat(path)
		if statErr != nil || info.Size() != 0 {
			t.Fatal("failed password verification changed file contents")
		}
	})
}

func TestWindowsPrivateRefusesBeforeTruncation(t *testing.T) {
	path := filepath.Join(windowsPrivateTestRoot(t), "existing")
	file, err := WindowsPrivateOpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("synthetic-existing-data"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	windowsPrivateTestBroaden(t, path)
	file, err = WindowsPrivateOpenFile(path, os.O_WRONLY|os.O_TRUNC)
	if file != nil {
		_ = file.Close()
		t.Fatal("non-private existing file returned")
	}
	if err == nil {
		t.Fatal("owner-only validation accepted broadened ACL")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "synthetic-existing-data" {
		t.Fatal("refusal changed pre-existing data")
	}
}

func TestWindowsPrivatePathSyntax(t *testing.T) {
	for _, path := range []string{
		`relative`, `C:relative`, `\rooted`, `\\server\share\cell`, `\\?\C:\cell`, `\\.\C:\cell`,
		`C:\NUL`, `C:\CON.txt`, `C:\PRN`, `C:\AUX`, `C:\COM1`, `C:\LPT9.txt`, `C:\COM¹`,
		`C:\parent\nul\..\cell`, `C:\CONIN$`, `C:\file:stream`, `C:\trailing.`, `C:\trailing `,
		`C:\bad?name`, "C:\\bad\x00name", `C:\` + strings.Repeat("x", 257), `C:\` + strings.Repeat("🙂", 129),
	} {
		if _, err := windowsPrivateCleanPath(path); err == nil {
			t.Fatal("unsafe Windows path accepted")
		}
	}
	for _, path := range []string{`C:\space and é\cell`, `c:/space/../cell`, `C:\COM10`, `C:\LPT10.txt`, `C:\safe&name`} {
		if _, err := windowsPrivateCleanPath(path); err != nil {
			t.Fatal("valid Windows path spelling rejected")
		}
	}
	// Exercise the public boundary too: a UNC path must be rejected without
	// contacting a share or creating any object.
	if err := WindowsPrivateMkdir(`\\server\share\cell`); err == nil {
		t.Fatal("UNC root creation accepted")
	}
}

func TestWindowsPrivateTempAndAppend(t *testing.T) {
	root := windowsPrivateTestRoot(t)
	file, err := WindowsPrivateCreateTemp(root)
	if err != nil {
		t.Fatal(err)
	}
	path := file.Name()
	assertSpoolPrivateDescriptor(t, path)
	if !strings.HasPrefix(filepath.Base(path), ".pending-") {
		t.Fatal("temporary file has unexpected name")
	}
	if _, err := file.WriteString("first"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	file, err = WindowsPrivateOpenFile(path, os.O_WRONLY|os.O_APPEND)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("second"); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal("private append handle cannot flush journal bytes")
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "firstsecond" {
		t.Fatal("append did not preserve existing private file bytes")
	}
	if _, err := WindowsPrivateOpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY); !errors.Is(err, os.ErrExist) {
		t.Fatal("exclusive creation did not preserve the existing file")
	}
}
