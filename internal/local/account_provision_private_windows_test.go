//go:build windows

package local

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestAccountProvisionWindowsPrivateCreationBeforeWrite(t *testing.T) {
	root := t.TempDir()
	directoryPath := filepath.Join(root, "private")
	directory, err := createAccountProvisionWindowsPrivateDirectory(directoryPath)
	if err != nil {
		t.Fatal(err)
	}
	defer closeAccountProvisionWindowsTestFile(t, directory)
	assertAccountProvisionWindowsPrivateDescriptor(t, directory)
	info, err := directory.Stat()
	if err != nil || validateAccountProvisionWindowsPrivateHandle(directory, directoryPath, info, true) != nil {
		t.Fatal("new private directory identity was not validated")
	}
	path := filepath.Join(directoryPath, "empty.json")
	file, err := createAccountProvisionWindowsPrivateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeAccountProvisionWindowsTestFile(t, file)
	info, err = file.Stat()
	if err != nil || info.Size() != 0 {
		t.Fatal("new private file was not empty before its first write")
	}
	// Inspect the real handle's owner and complete ACL before supplying bytes.
	assertAccountProvisionWindowsPrivateDescriptor(t, file)
	if _, err := file.WriteString("fixture\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := validateAccountProvisionWindowsPrivateHandle(file, path, info, false); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".moved"); err == nil {
		t.Fatal("private file handle allowed replacement while pinned")
	}
	if err := os.Rename(directoryPath, directoryPath+".moved"); err == nil {
		t.Fatal("private directory handle allowed replacement while pinned")
	}
}

func TestAccountProvisionWindowsPrivateDescriptorPolicy(t *testing.T) {
	user, err := accountProvisionWindowsUser()
	if err != nil {
		t.Fatal(err)
	}
	base, err := accountProvisionWindowsDescriptor()
	if err != nil {
		t.Fatal(err)
	}
	if !validAccountProvisionWindowsDescriptor(base, user) {
		t.Fatal("protected descriptor was rejected")
	}
	principals, err := accountProvisionWindowsPrincipals(user)
	if err != nil {
		t.Fatal(err)
	}
	var reversed strings.Builder
	reversed.WriteString("O:" + user.String() + "D:P")
	for i := len(principals) - 1; i >= 0; i-- {
		reversed.WriteString("(A;;FA;;;" + principals[i].String() + ")")
	}
	if !validAccountProvisionWindowsDescriptor(accountProvisionWindowsTestDescriptor(t, reversed.String()), user) {
		t.Fatal("equivalent ACE ordering was rejected")
	}
	// Owner policy is independent from the process's default TokenOwner. These
	// descriptor-only cases require no attempt to take ownership of real files.
	for _, owner := range []string{"SY", "BA"} {
		text := "O:" + owner + "D:P" + accountProvisionWindowsTestGrants(principals)
		if !validAccountProvisionWindowsDescriptor(accountProvisionWindowsTestDescriptor(t, text), user) {
			t.Fatal("root-equivalent descriptor owner was rejected")
		}
	}
	good := accountProvisionWindowsTestGrants(principals)
	ownerPrefix := "O:" + user.String()
	for name, sddl := range map[string]string{
		"everyone_read":       ownerPrefix + "D:P" + good + "(A;;FR;;;WD)",
		"users_read":          ownerPrefix + "D:P" + good + "(A;;FR;;;BU)",
		"everyone_write":      ownerPrefix + "D:P" + good + "(A;;FW;;;WD)",
		"everyone_delete":     ownerPrefix + "D:P" + good + "(A;;SD;;;WD)",
		"everyone_change_acl": ownerPrefix + "D:P" + good + "(A;;WD;;;WD)",
		"wrong_owner":         "O:WDD:P" + good,
		"unprotected":         ownerPrefix + "D:" + good,
		"empty":               ownerPrefix + "D:P",
		"duplicate":           ownerPrefix + "D:P" + good + "(A;;FA;;;" + user.String() + ")",
		"wrong_mask":          ownerPrefix + "D:P" + strings.Replace(good, ";FA;", ";FR;", 1),
		"unknown_trustee":     ownerPrefix + "D:P" + strings.Replace(good, principals[len(principals)-1].String(), "S-1-5-21-111-222-333-444", 1),
		"deny_ace":            ownerPrefix + "D:P" + strings.Replace(good, "(A;", "(D;", 1),
		"object_ace":          ownerPrefix + "D:P" + strings.Replace(good, "(A;;FA;;;", "(OA;;FA;00000000-0000-0000-0000-000000000001;;", 1),
		"inherited":           ownerPrefix + "D:P" + strings.Replace(good, "(A;;", "(A;ID;", 1),
		"inherit_only":        ownerPrefix + "D:P" + strings.Replace(good, "(A;;", "(A;OIIO;", 1),
		"inherit_children":    ownerPrefix + "D:P" + strings.Replace(good, "(A;;", "(A;OICI;", 1),
	} {
		t.Run(name, func(t *testing.T) {
			descriptor := accountProvisionWindowsTestDescriptor(t, sddl)
			if name == "object_ace" {
				dacl, _, err := descriptor.DACL()
				if err != nil || dacl == nil {
					t.Fatal("object ACE fixture has no DACL")
				}
				var ace *windows.ACCESS_ALLOWED_ACE
				if err := windows.GetAce(dacl, 0, &ace); err != nil || ace == nil || ace.Header.AceType != 0x5 {
					t.Fatal("native fixture was normalized instead of retaining an object ACE")
				}
			}
			if validAccountProvisionWindowsDescriptor(descriptor, user) {
				t.Fatal("unsafe descriptor accepted")
			}
		})
	}
	for _, present := range []bool{false, true} {
		name := "missing_dacl"
		if present {
			name = "null_dacl"
		}
		t.Run(name, func(t *testing.T) {
			descriptor, err := windows.NewSecurityDescriptor()
			if err != nil {
				t.Fatal(err)
			}
			if err := descriptor.SetOwner(user, false); err != nil {
				t.Fatal(err)
			}
			if err := descriptor.SetDACL(nil, present, false); err != nil {
				t.Fatal(err)
			}
			if err := descriptor.SetControl(windows.SE_DACL_PROTECTED, windows.SE_DACL_PROTECTED); err != nil {
				t.Fatal(err)
			}
			if validAccountProvisionWindowsDescriptor(descriptor, user) {
				t.Fatal("missing or NULL DACL accepted")
			}
			runtime.KeepAlive(user)
		})
	}
	for _, flag := range []windows.SECURITY_DESCRIPTOR_CONTROL{windows.SE_DACL_AUTO_INHERITED, windows.SE_DACL_AUTO_INHERIT_REQ} {
		descriptor, err := base.ToAbsolute()
		if err != nil {
			t.Fatal(err)
		}
		if err := descriptor.SetControl(flag, flag); err != nil {
			t.Fatal(err)
		}
		if validAccountProvisionWindowsDescriptor(descriptor, user) {
			t.Fatal("ambiguous inherited/defaulted DACL control accepted")
		}
	}
	t.Run("defaulted_dacl", func(t *testing.T) {
		descriptor, err := base.ToAbsolute()
		if err != nil {
			t.Fatal(err)
		}
		dacl, _, err := descriptor.DACL()
		if err != nil {
			t.Fatal(err)
		}
		if err := descriptor.SetDACL(dacl, true, true); err != nil {
			t.Fatal(err)
		}
		if validAccountProvisionWindowsDescriptor(descriptor, user) {
			t.Fatal("defaulted DACL accepted")
		}
	})
	t.Run("unknown_ace_type", func(t *testing.T) {
		descriptor, err := base.ToAbsolute()
		if err != nil {
			t.Fatal(err)
		}
		dacl, _, err := descriptor.DACL()
		if err != nil {
			t.Fatal(err)
		}
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, 0, &ace); err != nil || ace == nil {
			t.Fatal("native ACE fixture unavailable")
		}
		ace.Header.AceType = 0xff
		if validAccountProvisionWindowsDescriptor(descriptor, user) {
			t.Fatal("unknown ACE type accepted")
		}
	})
	if validAccountProvisionWindowsDescriptor(nil, user) || validAccountProvisionWindowsDescriptor(base, nil) {
		t.Fatal("missing descriptor or process user accepted")
	}
}

func TestAccountProvisionWindowsPrivateHandleRejectsUnsafeACLWithoutRepair(t *testing.T) {
	user, err := accountProvisionWindowsUser()
	if err != nil {
		t.Fatal(err)
	}
	principals, err := accountProvisionWindowsPrincipals(user)
	if err != nil {
		t.Fatal(err)
	}
	for _, grant := range []string{"(A;;FR;;;WD)", "(A;;FR;;;BU)", "(A;;FW;;;WD)", "(A;;SD;;;WD)", "(A;;WD;;;WD)"} {
		t.Run(grant, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "unsafe.json")
			descriptor := accountProvisionWindowsTestDescriptor(t, "O:"+user.String()+"D:P"+accountProvisionWindowsTestGrants(principals)+grant)
			file := accountProvisionWindowsRawTestFile(t, path, descriptor, false)
			defer closeAccountProvisionWindowsTestFile(t, file)
			if _, err := file.WriteString("unchanged"); err != nil {
				t.Fatal(err)
			}
			before := accountProvisionWindowsTestHandleDescriptor(t, file).String()
			info, err := file.Stat()
			if err != nil {
				t.Fatal(err)
			}
			if err := validateAccountProvisionWindowsPrivateHandle(file, path, info, false); !errors.Is(err, ErrAccountProvisionJournalUnsafe) {
				t.Fatalf("unsafe ACL result: %v", err)
			}
			if before != accountProvisionWindowsTestHandleDescriptor(t, file).String() {
				t.Fatal("validation repaired or changed an unsafe ACL")
			}
			raw, err := os.ReadFile(path)
			if err != nil || string(raw) != "unchanged" {
				t.Fatal("validation changed existing content")
			}
		})
	}
}

func TestAccountProvisionWindowsPrivateCreationPreservesExistingObjects(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "existing.json")
	if err := os.WriteFile(path, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeAccountProvisionWindowsTestFile(t, file)
	before := accountProvisionWindowsTestHandleDescriptor(t, file).String()
	if candidate, err := createAccountProvisionWindowsPrivateFile(path); candidate != nil || !errors.Is(err, os.ErrExist) {
		t.Fatalf("existing file creation result: %v", err)
	}
	if before != accountProvisionWindowsTestHandleDescriptor(t, file).String() {
		t.Fatal("exclusive creation changed existing file ACL")
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != "unchanged" {
		t.Fatal("exclusive creation changed existing file bytes")
	}
	directoryPath := filepath.Join(root, "existing-directory")
	if err := os.Mkdir(directoryPath, 0o700); err != nil {
		t.Fatal(err)
	}
	directory, err := os.Open(directoryPath)
	if err != nil {
		t.Fatal(err)
	}
	defer closeAccountProvisionWindowsTestFile(t, directory)
	before = accountProvisionWindowsTestHandleDescriptor(t, directory).String()
	if candidate, err := createAccountProvisionWindowsPrivateDirectory(directoryPath); candidate != nil || !errors.Is(err, os.ErrExist) {
		t.Fatalf("existing directory creation result: %v", err)
	}
	if before != accountProvisionWindowsTestHandleDescriptor(t, directory).String() {
		t.Fatal("exclusive creation changed existing directory ACL")
	}
}

func TestAccountProvisionWindowsPrivateHandleIdentityFences(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "original.json")
	descriptor, err := accountProvisionWindowsDescriptor()
	if err != nil {
		t.Fatal(err)
	}
	// Only this adversarial fixture permits delete sharing, so the path can be
	// rebound while its original handle stays alive. Production creators do not.
	file := accountProvisionWindowsRawTestFile(t, path, descriptor, true)
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { closeAccountProvisionWindowsTestFile(t, file) }()
	otherPath := filepath.Join(root, "other.json")
	other, err := createAccountProvisionWindowsPrivateFile(otherPath)
	if err != nil {
		t.Fatal(err)
	}
	defer closeAccountProvisionWindowsTestFile(t, other)
	otherInfo, err := other.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if validateAccountProvisionWindowsPrivateHandle(file, path, otherInfo, false) == nil ||
		validateAccountProvisionWindowsPrivateHandle(file, path, info, true) == nil ||
		validateAccountProvisionWindowsPrivateHandle(nil, path, info, false) == nil {
		t.Fatal("wrong identity, type or absent handle accepted")
	}
	alias := filepath.Join(root, "owned-link.json")
	if err := os.Link(path, alias); err != nil {
		t.Fatal(err)
	}
	if err := validateAccountProvisionWindowsPrivateHandle(file, alias, info, false); err != nil {
		t.Fatal("valid handle identity with an additional link was rejected")
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	replacement, err := createAccountProvisionWindowsPrivateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeAccountProvisionWindowsTestFile(t, replacement)
	if err := validateAccountProvisionWindowsPrivateHandle(file, path, info, false); !errors.Is(err, ErrAccountProvisionJournalUnsafe) {
		t.Fatalf("rebound path accepted: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	file = nil
	closed, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	if validateAccountProvisionWindowsPrivateHandle(closed, path, info, false) == nil {
		t.Fatal("closed handle accepted")
	}
}

func TestAccountProvisionWindowsPrivateHandleRejectsReparsePath(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "target.json")
	file, err := createAccountProvisionWindowsPrivateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeAccountProvisionWindowsTestFile(t, file)
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatalf("native reparse fixture unavailable: %v", err)
	}
	if err := validateAccountProvisionWindowsPrivateHandle(file, link, info, false); !errors.Is(err, ErrAccountProvisionJournalUnsafe) {
		t.Fatalf("reparse path accepted: %v", err)
	}
}

func accountProvisionWindowsTestDescriptor(t *testing.T, sddl string) *windows.SECURITY_DESCRIPTOR {
	t.Helper()
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatal("native security descriptor fixture is invalid")
	}
	return descriptor
}

func accountProvisionWindowsTestGrants(principals []*windows.SID) string {
	var grants strings.Builder
	for _, sid := range principals {
		grants.WriteString("(A;;FA;;;" + sid.String() + ")")
	}
	return grants.String()
}

func accountProvisionWindowsTestHandleDescriptor(t *testing.T, file *os.File) *windows.SECURITY_DESCRIPTOR {
	t.Helper()
	descriptor, err := windows.GetSecurityInfo(windows.Handle(file.Fd()), windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil || descriptor == nil {
		t.Fatal("read fixture security descriptor failed")
	}
	return descriptor
}

func assertAccountProvisionWindowsPrivateDescriptor(t *testing.T, file *os.File) {
	t.Helper()
	user, err := accountProvisionWindowsUser()
	if err != nil {
		t.Fatal(err)
	}
	descriptor := accountProvisionWindowsTestHandleDescriptor(t, file)
	if !validAccountProvisionWindowsDescriptor(descriptor, user) {
		t.Fatal("real handle lacks the complete private descriptor before first write")
	}
}

func accountProvisionWindowsRawTestFile(t *testing.T, path string, descriptor *windows.SECURITY_DESCRIPTOR, deleteSharing bool) *os.File {
	t.Helper()
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	attributes := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: descriptor}
	sharing := uint32(windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE)
	if deleteSharing {
		sharing |= windows.FILE_SHARE_DELETE
	}
	handle, err := windows.CreateFile(pointer, windows.GENERIC_READ|windows.GENERIC_WRITE|windows.READ_CONTROL,
		sharing, attributes, windows.CREATE_NEW, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	runtime.KeepAlive(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		if err := windows.CloseHandle(handle); err != nil {
			t.Fatal(err)
		}
		t.Fatal("native fixture handle unavailable")
	}
	return file
}

func closeAccountProvisionWindowsTestFile(t *testing.T, file *os.File) {
	t.Helper()
	if file != nil {
		if err := file.Close(); err != nil {
			t.Error("close native fixture handle failed")
		}
	}
}
