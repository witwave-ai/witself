//go:build windows

package transcriptcapture

import (
	"testing"

	"golang.org/x/sys/windows"
)

func TestTrustedWindowsOwnerMatchesCurrentUserAndRootEquivalents(t *testing.T) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	if !trustedWindowsOwner(user.User.Sid, user.User.Sid) {
		t.Fatal("current token user was not trusted as an owner")
	}
	for _, sidType := range []windows.WELL_KNOWN_SID_TYPE{
		windows.WinLocalSystemSid,
		windows.WinBuiltinAdministratorsSid,
	} {
		sid, err := windows.CreateWellKnownSid(sidType)
		if err != nil {
			t.Fatal(err)
		}
		if !trustedWindowsOwner(sid, user.User.Sid) {
			t.Fatalf("root-equivalent owner SID type %d was not trusted", sidType)
		}
	}
	users, err := windows.CreateWellKnownSid(windows.WinBuiltinUsersSid)
	if err != nil {
		t.Fatal(err)
	}
	if trustedWindowsOwner(users, user.User.Sid) {
		t.Fatal("ordinary local Users group was trusted as an owner")
	}
}
