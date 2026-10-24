//go:build windows

package runtimeconfig

import (
	"golang.org/x/sys/windows"
	"os"
)

func privateSpoolOwner(path string, _ os.FileInfo) bool {
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil || descriptor == nil {
		return false
	}
	owner, _, err := descriptor.Owner()
	if err != nil || owner == nil {
		return false
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	return err == nil && user != nil && user.User.Sid != nil && owner.Equals(user.User.Sid)
}
