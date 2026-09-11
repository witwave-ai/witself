//go:build windows

package local

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"golang.org/x/sys/windows"
)

func privateAccountProvisionTestHome(t *testing.T) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), ".witself")
	file, err := createAccountProvisionWindowsPrivateDirectory(home)
	if err != nil {
		t.Fatal(err)
	}
	closeAccountProvisionWindowsTestFile(t, file)
	return home
}

func missingAccountProvisionTestHome(t *testing.T) string {
	t.Helper()
	// Windows requires the immediate home parent to preexist. This parent is
	// external fixture setup; the production operation still creates the home.
	parent := filepath.Join(t.TempDir(), "missing")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(parent, ".witself")
}

func assertAccountProvisionPrivateTestPath(t *testing.T, path string, directory bool) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(pointer, windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		t.Fatal(err)
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		t.Fatal("native private fixture handle unavailable")
	}
	defer closeAccountProvisionWindowsTestFile(t, file)
	if err := validateAccountProvisionWindowsPrivateHandle(file, path, info, directory); err != nil {
		t.Fatalf("private fixture path validation failed: %v", err)
	}
}

func makeAccountProvisionTestPathReadable(t *testing.T, path string) {
	t.Helper()
	user, err := accountProvisionWindowsUser()
	if err != nil {
		t.Fatal(err)
	}
	principals, err := accountProvisionWindowsPrincipals(user)
	if err != nil {
		t.Fatal(err)
	}
	// Actual broad read access replaces the POSIX group-read attack. Only this
	// adversarial test fixture changes an ACL; production never repairs one.
	descriptor := accountProvisionWindowsTestDescriptor(t,
		"O:"+user.String()+"D:P"+accountProvisionWindowsTestGrants(principals)+"(A;;GR;;;WD)")
	defer func() { runtime.KeepAlive(descriptor) }()
	dacl, _, err := descriptor.DACL()
	if err != nil || dacl == nil {
		t.Fatal("unsafe read fixture DACL unavailable")
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil); err != nil {
		t.Fatal("unsafe read fixture DACL application failed")
	}
}

func writeAccountProvisionPrivateTestFile(path string, raw []byte) error {
	file, err := createAccountProvisionWindowsPrivateFile(path)
	if err != nil {
		return err
	}
	n, writeErr := file.Write(raw)
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if n != len(raw) {
		return io.ErrShortWrite
	}
	return closeErr
}

func createAccountProvisionPrivateTestTokenDirs(t *testing.T, home, name string) {
	t.Helper()
	path := home
	for _, component := range []string{"tokens", "accounts", name} {
		path = filepath.Join(path, component)
		file, err := createAccountProvisionWindowsPrivateDirectory(path)
		if err != nil {
			t.Fatal(err)
		}
		closeAccountProvisionWindowsTestFile(t, file)
	}
}
