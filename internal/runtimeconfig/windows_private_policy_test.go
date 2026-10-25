//go:build windows

package runtimeconfig

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// This oracle reads owner and DACL independently of the validator. Fixed
// mismatch literals survive the JSON checker's diagnostic allowlist without
// printing a path, SID, native error number, or descriptor.
func assertWindowsPrivateNativePolicy(t *testing.T, descriptor *windows.SECURITY_DESCRIPTOR) {
	t.Helper()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil {
		t.Fatal("cannot query native policy user")
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatal("cannot query native policy system")
	}
	admin, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		t.Fatal("cannot query native policy administrators")
	}
	if descriptor == nil {
		t.Fatal("cannot read native policy descriptor")
	}
	owner, _, err := descriptor.Owner()
	if err != nil || owner == nil || !owner.Equals(user.User.Sid) {
		if owner != nil && owner.Equals(admin) {
			t.Fatal("private Windows owner-is-admin-group")
		}
		t.Fatal("private Windows owner-mismatch")
	}
	control, _, err := descriptor.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatal("private Windows protected-missing")
	}
	dacl, _, err := descriptor.DACL()
	if err != nil || dacl == nil {
		t.Fatal("private Windows ace-count")
	}
	wantCount := uint16(2)
	if user.User.Sid.Equals(system) {
		wantCount = 1
	}
	seenUser, seenSystem := false, false
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if windows.GetAce(dacl, index, &ace) != nil || ace == nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			t.Fatal("native policy contains unsupported entry")
		}
		if ace.Header.AceFlags != 0 {
			t.Fatal("private Windows ace-flags")
		}
		// GENERIC_ALL and the mapped FILE_ALL_ACCESS are equivalent. No
		// unrelated standard, generic or specific access bit is permitted.
		if ace.Mask != 0x1f01ff && ace.Mask != windows.GENERIC_ALL && ace.Mask != windows.GENERIC_ALL|0x1f01ff {
			t.Fatal("private Windows ace-mask")
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		switch {
		case sid.Equals(user.User.Sid):
			if seenUser {
				t.Fatal("private Windows ace-count")
			}
			seenUser = true
		case sid.Equals(system):
			if seenSystem {
				t.Fatal("private Windows ace-count")
			}
			seenSystem = true
		default:
			t.Fatal("native policy grants another principal")
		}
	}
	if !seenSystem && !user.User.Sid.Equals(system) {
		t.Fatal("private Windows system-missing")
	}
	if !seenUser || dacl.AceCount != wantCount {
		t.Fatal("private Windows ace-count")
	}
}

func TestWindowsPrivateExplicitOwnerBeforeCreation(t *testing.T) {
	descriptor, err := windowsPrivateDescriptor()
	if err != nil {
		t.Fatal(err)
	}
	owner, defaulted, err := descriptor.Owner()
	if err != nil || owner == nil || defaulted {
		t.Fatal("creation owner must be explicit")
	}
	assertWindowsPrivateNativePolicy(t, descriptor)

	// Read TokenOwner as well as TokenUser. On an elevated runner they may
	// differ; an explicit descriptor must still produce TokenUser ownership.
	var size uint32
	token := windows.GetCurrentProcessToken()
	if err := windows.GetTokenInformation(token, windows.TokenOwner, nil, 0, &size); !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) || size < uint32(unsafe.Sizeof(uintptr(0))) {
		t.Fatal("cannot size token default owner")
	}
	data := make([]byte, size)
	if err := windows.GetTokenInformation(token, windows.TokenOwner, &data[0], size, &size); err != nil {
		t.Fatal("cannot query token default owner")
	}
	defaultOwner := *(**windows.SID)(unsafe.Pointer(&data[0]))
	if defaultOwner == nil || !defaultOwner.IsValid() {
		t.Fatal("token default owner is unavailable")
	}
	if !defaultOwner.Equals(owner) {
		t.Log("explicit user owner differs from token default owner")
	}
	runtime.KeepAlive(data)

	for _, directory := range []bool{true, false} {
		name := "file"
		if directory {
			name = "directory"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "created")
			pointer, err := windows.UTF16PtrFromString(path)
			if err != nil {
				t.Fatal("cannot prepare native policy path")
			}
			attributes := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: descriptor}
			if directory {
				if err := windows.CreateDirectory(pointer, &attributes); err != nil {
					t.Fatal(windowsPrivateFailure(err))
				}
			} else {
				handle, err := windows.CreateFile(pointer, windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
					&attributes, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
				if err != nil {
					t.Fatal(windowsPrivateFailure(err))
				}
				if err := windows.CloseHandle(handle); err != nil {
					t.Fatal("cannot close native policy file")
				}
			}
			runtime.KeepAlive(descriptor)
			actual, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
			if err != nil {
				t.Fatal("cannot read native policy descriptor")
			}
			assertWindowsPrivateNativePolicy(t, actual)
			if err := windowsPrivateValidateDescriptor(actual); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestWindowsPrivateProductionCreationPolicy(t *testing.T) {
	t.Run("directory", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "created")
		if err := windowsPrivateMkdir(path, func(created string) error {
			descriptor, err := windows.GetNamedSecurityInfo(created, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
			if err != nil {
				t.Fatal("cannot read native policy descriptor")
			}
			assertWindowsPrivateNativePolicy(t, descriptor)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "created")
		file, err := windowsPrivateOpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, func(handle windows.Handle) error {
			descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
			if err != nil {
				t.Fatal("cannot read native policy descriptor")
			}
			assertWindowsPrivateNativePolicy(t, descriptor)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal("cannot close native policy file")
		}
	})
}

func TestWindowsPrivatePolicyDiagnostics(t *testing.T) {
	user := windowsPrivateTestUser(t)
	owner := "O:" + user
	userAllow := "(A;;FA;;;" + user + ")"
	cases := []struct {
		name string
		sddl string
		want error
	}{
		{"owner-is-admin-group", "O:BAD:P" + userAllow + "(A;;FA;;;SY)", errWindowsPrivateOwnerAdmin},
		{"ace-count", owner + "D:P" + userAllow + "(A;;FA;;;SY)(A;;FR;;;WD)", errWindowsPrivateACECount},
		{"ace-mask", owner + "D:P(A;;FR;;;" + user + ")(A;;FA;;;SY)", errWindowsPrivateACEMask},
		{"ace-flags", owner + "D:P(A;OI;FA;;;" + user + ")(A;;FA;;;SY)", errWindowsPrivateACEFlags},
		{"protected-missing", owner + "D:" + userAllow + "(A;;FA;;;SY)", errWindowsPrivateProtected},
		{"system-missing", owner + "D:P" + userAllow, errWindowsPrivateSystem},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			err := windowsPrivateValidateDescriptor(windowsPrivateTestDescriptor(t, test.sddl))
			if !errors.Is(err, test.want) || err.Error() != test.want.Error() {
				t.Fatal("private descriptor diagnostic did not identify the mismatch")
			}
		})
	}
	for _, cause := range []error{windows.ERROR_SHARING_VIOLATION, windows.ERROR_ACCESS_DENIED, os.ErrNotExist, os.ErrExist} {
		if !errors.Is(windowsPrivateFailure(cause), cause) {
			t.Fatal("private diagnostic lost the native error cause")
		}
	}
}

func TestWindowsPrivateDescriptorCanonicalForms(t *testing.T) {
	user := windowsPrivateTestUser(t)
	owner := "O:" + user
	for _, sddl := range []string{
		owner + "D:P(A;;GA;;;" + user + ")(A;;GA;;;SY)",
		owner + "D:PAI(A;;FA;;;SY)(A;;FA;;;" + user + ")",
		owner + "D:P(A;;FA;;;" + user + ")(A;;FA;;;SY)S:(ML;;NW;;;ME)",
	} {
		if err := windowsPrivateValidateDescriptor(windowsPrivateTestDescriptor(t, sddl)); err != nil {
			t.Fatal("equivalent private descriptor rejected")
		}
	}
}
