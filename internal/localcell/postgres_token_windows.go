//go:build windows

package localcell

import (
	"errors"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var createRestrictedTokenProc = windows.NewLazySystemDLL("advapi32.dll").NewProc("CreateRestrictedToken")

// Match PostgreSQL's Windows launcher: disable admin/power-user groups and
// privileges before executing PostgreSQL. Keep TokenUser and explicitly choose
// it as the owner of newly created files; an elevated parent's TokenOwner can
// otherwise be Administrators. This is privilege reduction, not impersonation
// of a different user, and never grants service or account-management rights.
func windowsPostgresToken() (windows.Token, error) {
	var original windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_DUPLICATE|windows.TOKEN_QUERY|windows.TOKEN_ASSIGN_PRIMARY|windows.TOKEN_ADJUST_DEFAULT, &original); err != nil {
		return 0, errors.New("cannot open PostgreSQL launch token")
	}
	defer func() { _ = original.Close() }()
	user, err := original.GetTokenUser()
	if err != nil {
		return 0, errors.New("cannot identify PostgreSQL launch user")
	}
	drop := make([]windows.SIDAndAttributes, 2)
	for i, kind := range []windows.WELL_KNOWN_SID_TYPE{windows.WinBuiltinAdministratorsSid, windows.WinBuiltinPowerUsersSid} {
		drop[i].Sid, err = windows.CreateWellKnownSid(kind)
		if err != nil {
			return 0, errors.New("cannot construct PostgreSQL restricted groups")
		}
	}
	var token windows.Token
	const disableMaxPrivilege = 1
	result, _, _ := createRestrictedTokenProc.Call(uintptr(original), disableMaxPrivilege, uintptr(len(drop)), uintptr(unsafe.Pointer(&drop[0])), 0, 0, 0, 0, uintptr(unsafe.Pointer(&token)))
	runtime.KeepAlive(drop)
	if result == 0 {
		return 0, errors.New("cannot reduce PostgreSQL launch privileges")
	}
	owner := struct{ SID *windows.SID }{user.User.Sid}
	if err := windows.SetTokenInformation(token, windows.TokenOwner, (*byte)(unsafe.Pointer(&owner)), uint32(unsafe.Sizeof(owner))); err != nil {
		_ = token.Close()
		return 0, errors.New("cannot set PostgreSQL file owner")
	}
	// PostgreSQL's AddUserToTokenDacl addresses the same Administrator-account
	// default-DACL trap: after disabling Administrators, a default DACL containing
	// only Administrators/SYSTEM cannot authorize the child's own pipe/process
	// creation. Supply only the unchanged user and SYSTEM for new kernel objects.
	sddl := "D:(A;;GA;;;" + user.User.Sid.String() + ")"
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err == nil && !user.User.Sid.Equals(system) {
		sddl += "(A;;GA;;;SY)"
	}
	if err != nil {
		_ = token.Close()
		return 0, errors.New("cannot construct PostgreSQL object policy")
	}
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		_ = token.Close()
		return 0, errors.New("cannot construct PostgreSQL object policy")
	}
	acl, _, err := descriptor.DACL()
	defaultDACL := struct{ ACL *windows.ACL }{acl}
	if err != nil || acl == nil {
		_ = token.Close()
		return 0, errors.New("cannot construct PostgreSQL object policy")
	}
	if err := windows.SetTokenInformation(token, windows.TokenDefaultDacl, (*byte)(unsafe.Pointer(&defaultDACL)), uint32(unsafe.Sizeof(defaultDACL))); err != nil {
		_ = token.Close()
		return 0, errors.New("cannot set PostgreSQL object policy")
	}
	runtime.KeepAlive(descriptor)
	runtime.KeepAlive(user)
	return token, nil
}

func configurePostgresCommand(cmd *exec.Cmd) (func(), error) {
	token, err := windowsPostgresToken()
	if err != nil {
		return nil, err
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Token = syscall.Token(token)
	return func() { _ = token.Close() }, nil
}

// Version probes execute the same PostgreSQL binaries as lifecycle commands.
// Restrict every executable under the configured distribution, including tools
// added later, while leaving server/worker version probes unchanged.
func configurePostgresVersionCommand(cmd *exec.Cmd, directory string) (func(), error) {
	if filepath.IsAbs(directory) {
		relative, err := filepath.Rel(directory, cmd.Path)
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return configurePostgresCommand(cmd)
		}
	}
	return func() {}, nil
}
