//go:build windows

package localcell

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unsafe"

	"github.com/witwave-ai/witself/internal/runtimeconfig"
	"golang.org/x/sys/windows"
)

func TestWindowsPostgresOrdinaryFileCreation(t *testing.T) {
	if os.Getenv("WITSELF_PG_FILE_FIXTURE") != "1" {
		return
	}
	root := os.Args[len(os.Args)-1]
	pg := filepath.Join(root, "pg")
	if err := os.Mkdir(filepath.Join(pg, "base"), 0o700); err != nil {
		t.Fatal("cannot create inherited PG directory")
	}
	for _, relative := range []string{"postgresql.conf", "postmaster.pid", filepath.Join("base", "unicode-row")} {
		path := filepath.Join(pg, relative)
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal("cannot create inherited PG file")
		}
		// Inspect before the first byte. Post-creation ACL repair would not
		// prove that real PostgreSQL data is private when written.
		if err := runtimeconfig.WindowsPrivatePostgresValidate(pg, path, false); err != nil {
			_ = file.Close()
			t.Fatal("PG file was not private before writing")
		}
		if _, err := file.WriteString("synthetic é 東京"); err != nil {
			_ = file.Close()
			t.Fatal("cannot write inherited PG file")
		}
		if err := file.Close(); err != nil {
			t.Fatal("cannot close inherited PG file")
		}
	}
}

func windowsPostgresPrivateFixture(t *testing.T) *Cell {
	t.Helper()
	c, err := New(Config{Root: testPrivateRoot(t)})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.layout(); err != nil {
		t.Fatal(err)
	}
	if err := c.preparePostgresDirectory(); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal("cannot locate native file fixture")
	}
	cmd := exec.CommandContext(t.Context(), self, "-test.run=^TestWindowsPostgresOrdinaryFileCreation$", "--", c.cfg.Root)
	cmd.Dir = c.cfg.Root
	cmd.Env, err = c.postgresEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Env = appendEnvironment(cmd.Env, "WITSELF_PG_FILE_FIXTURE=1")
	release, err := configurePostgresCommand(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := cmd.Run(); err != nil {
		t.Fatal("restricted native PG file fixture failed")
	}
	return c
}

func TestWindowsPostgresInheritedPrivacy(t *testing.T) {
	c := windowsPostgresPrivateFixture(t)
	if err := c.validateRoot(false); err != nil {
		t.Fatal("private PostgreSQL descendants were refused")
	}
	if err := runtimeconfig.WindowsPrivatePostgresValidate(strings.ToLower(c.path("pg")), c.path("pg"), true); err != nil {
		t.Fatal("private PostgreSQL boundary casing changed its identity")
	}
	if err := runtimeconfig.WindowsPrivateValidate(c.path("pg"), true); err == nil {
		t.Fatal("PG inheritance weakened the ordinary strict directory policy")
	}
	for _, name := range []string{"run", "home", "spool", "tmp", "logs", "journal"} {
		assertWindowsPrivate(t, c.path(name))
	}
	data, err := readPostgresFile(c.path("pg", "postmaster.pid"))
	if err != nil || !bytes.Equal(data, []byte("synthetic é 東京")) {
		t.Fatal("private inherited PostgreSQL record could not be read")
	}
	if err := c.writePostgresConfig([]byte("synthetic configuration\n")); err != nil {
		t.Fatal("private PostgreSQL configuration replacement failed")
	}
	assertWindowsPrivate(t, c.path("pg", "postgresql.conf"))
	if err := c.validateRoot(false); err != nil {
		t.Fatal("private replacement changed PGDATA trust")
	}
}

func TestWindowsPostgresPrivacyRefusals(t *testing.T) {
	t.Run("broadened-descendant", func(t *testing.T) {
		c := windowsPostgresPrivateFixture(t)
		path := c.path("pg", "base", "unicode-row")
		setTestDACL(t, path, "(A;;FR;;;WD)", false, false)
		if err := c.validateRoot(false); err == nil {
			t.Fatal("PGDATA accepted a broadened descendant")
		}
	})
	t.Run("broadened-pidfile-before-read", func(t *testing.T) {
		c := windowsPostgresPrivateFixture(t)
		path := c.path("pg", "postmaster.pid")
		setTestDACL(t, path, "(A;;FR;;;WD)", false, false)
		if data, err := readPostgresFile(path); err == nil || len(data) != 0 {
			t.Fatal("PGDATA read an untrusted pidfile")
		}
	})
	t.Run("broadened-config-before-replace", func(t *testing.T) {
		c := windowsPostgresPrivateFixture(t)
		path := c.path("pg", "postgresql.conf")
		setTestDACL(t, path, "(A;;FR;;;WD)", false, false)
		if err := c.writePostgresConfig([]byte("changed")); err == nil {
			t.Fatal("PGDATA replaced an untrusted configuration")
		}
		data, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(data, []byte("synthetic é 東京")) {
			t.Fatal("refusal mutated untrusted configuration")
		}
	})
	t.Run("inherited-source-before-replace", func(t *testing.T) {
		c := windowsPostgresPrivateFixture(t)
		source, target := c.path("pg", "postmaster.pid"), c.path("pg", "postgresql.conf")
		if err := runtimeconfig.WindowsPrivatePostgresRename(c.path("pg"), source, target); err == nil {
			t.Fatal("PGDATA published an inherited replacement source")
		}
		for _, path := range []string{source, target} {
			data, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(data, []byte("synthetic é 東京")) {
				t.Fatal("refused PGDATA publication changed a file")
			}
		}
	})
	t.Run("junction-descendant", func(t *testing.T) {
		c := windowsPostgresPrivateFixture(t)
		outside := testPrivateRoot(t)
		createTestJunction(t, c.path("pg", "redirected"), outside)
		if err := c.validateRoot(false); err == nil {
			t.Fatal("PGDATA followed a descendant junction")
		}
	})
	t.Run("existing-boundary", func(t *testing.T) {
		c := windowsPostgresPrivateFixture(t)
		if err := c.preparePostgresDirectory(); err == nil {
			t.Fatal("PGDATA preparation adopted an existing tree")
		}
	})
	t.Run("outside-boundary", func(t *testing.T) {
		c := windowsPostgresPrivateFixture(t)
		if err := runtimeconfig.WindowsPrivatePostgresValidate(c.path("pg"), c.path("run"), true); err == nil {
			t.Fatal("PGDATA policy escaped its boundary")
		}
	})
}

func windowsTokenInformation(t *testing.T, token windows.Token, class uint32) []byte {
	t.Helper()
	var size uint32
	if err := windows.GetTokenInformation(token, class, nil, 0, &size); !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) || size == 0 {
		t.Fatal("cannot size launch token information")
	}
	data := make([]byte, size)
	if err := windows.GetTokenInformation(token, class, &data[0], size, &size); err != nil {
		t.Fatal("cannot read launch token information")
	}
	return data
}

func TestWindowsPostgresTokenIsRestrictedSameUser(t *testing.T) {
	token, err := windowsPostgresToken()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = token.Close() }()
	current, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal("cannot inspect current user")
	}
	user, err := token.GetTokenUser()
	if err != nil || !user.User.Sid.Equals(current.User.Sid) {
		t.Fatal("restricted token changed the PostgreSQL user")
	}
	ownerData := windowsTokenInformation(t, token, windows.TokenOwner)
	owner := *(**windows.SID)(unsafe.Pointer(&ownerData[0]))
	if owner == nil || !owner.Equals(current.User.Sid) {
		t.Fatal("restricted PostgreSQL token retained an administrative file owner")
	}
	runtime.KeepAlive(ownerData)
	daclData := windowsTokenInformation(t, token, windows.TokenDefaultDacl)
	dacl := *(**windows.ACL)(unsafe.Pointer(&daclData[0]))
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatal("cannot identify SYSTEM")
	}
	wantCount := uint16(2)
	if current.User.Sid.Equals(system) {
		wantCount = 1
	}
	if dacl == nil || dacl.AceCount != wantCount {
		t.Fatal("restricted PostgreSQL token lacks its explicit private object DACL")
	}
	var foundUser, foundSystem bool
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if windows.GetAce(dacl, i, &ace) != nil || ace == nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Mask != windows.GENERIC_ALL {
			t.Fatal("invalid PostgreSQL default object grant")
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		switch {
		case sid.Equals(current.User.Sid):
			foundUser = true
		case sid.Equals(system):
			foundSystem = true
		default:
			t.Fatal("PostgreSQL default object policy grants another principal")
		}
	}
	if !foundUser || (!current.User.Sid.Equals(system) && !foundSystem) {
		t.Fatal("PostgreSQL default object policy omitted its user or SYSTEM")
	}
	runtime.KeepAlive(daclData)
	groups, err := token.GetTokenGroups()
	if err != nil {
		t.Fatal("cannot inspect restricted groups")
	}
	for _, kind := range []windows.WELL_KNOWN_SID_TYPE{windows.WinBuiltinAdministratorsSid, windows.WinBuiltinPowerUsersSid} {
		sid, err := windows.CreateWellKnownSid(kind)
		if err != nil {
			t.Fatal("cannot identify administrative group")
		}
		for _, group := range groups.AllGroups() {
			if group.Sid.Equals(sid) && (group.Attributes&windows.SE_GROUP_ENABLED != 0 || group.Attributes&windows.SE_GROUP_USE_FOR_DENY_ONLY == 0) {
				t.Fatal("PostgreSQL token retained an administrative grant")
			}
		}
	}
	privilegeData := windowsTokenInformation(t, token, windows.TokenPrivileges)
	privileges := (*windows.Tokenprivileges)(unsafe.Pointer(&privilegeData[0]))
	name, err := windows.UTF16PtrFromString("SeChangeNotifyPrivilege")
	if err != nil {
		t.Fatal("cannot name traversal privilege")
	}
	var traversal windows.LUID
	if err := windows.LookupPrivilegeValue(nil, name, &traversal); err != nil {
		t.Fatal("cannot identify traversal privilege")
	}
	for _, privilege := range privileges.AllPrivileges() {
		if privilege.Attributes&windows.SE_PRIVILEGE_ENABLED != 0 && privilege.Luid != traversal {
			t.Fatal("PostgreSQL token retained a privileged operation")
		}
	}
	runtime.KeepAlive(privilegeData)
}
