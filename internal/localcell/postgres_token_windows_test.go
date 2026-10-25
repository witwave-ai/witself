//go:build windows

package localcell

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsPostgresCommandsUseRestrictedToken(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "token_fixture.go")
	// Check the effective child token, including under an elevated CI parent.
	// The fixture never emits a token, environment, identifier, or error value.
	const program = `package main
import ("fmt"; "os"; "unsafe"; "golang.org/x/sys/windows")
func main() {
  token := windows.GetCurrentProcessToken()
  groups, err := token.GetTokenGroups(); if err != nil { os.Exit(101) }
  for _, kind := range []windows.WELL_KNOWN_SID_TYPE{windows.WinBuiltinAdministratorsSid, windows.WinBuiltinPowerUsersSid} {
    sid, err := windows.CreateWellKnownSid(kind); if err != nil { os.Exit(102) }
    for _, group := range groups.AllGroups() {
      if group.Sid.Equals(sid) && (group.Attributes & windows.SE_GROUP_ENABLED != 0 || group.Attributes & windows.SE_GROUP_USE_FOR_DENY_ONLY == 0) { os.Exit(103) }
    }
  }
  var size uint32
  _ = windows.GetTokenInformation(token, windows.TokenPrivileges, nil, 0, &size)
  if size == 0 { os.Exit(104) }
  data := make([]byte, size)
  if windows.GetTokenInformation(token, windows.TokenPrivileges, &data[0], size, &size) != nil { os.Exit(105) }
  name, _ := windows.UTF16PtrFromString("SeChangeNotifyPrivilege")
  var traversal windows.LUID
  if windows.LookupPrivilegeValue(nil, name, &traversal) != nil { os.Exit(106) }
  for _, privilege := range (*windows.Tokenprivileges)(unsafe.Pointer(&data[0])).AllPrivileges() {
    if privilege.Attributes & windows.SE_PRIVILEGE_ENABLED != 0 && privilege.Luid != traversal { os.Exit(107) }
  }
  fmt.Println("postgres (PostgreSQL) 16.0")
}
`
	if err := os.WriteFile(source, []byte(program), 0o600); err != nil {
		t.Fatal("cannot write restricted-command fixture")
	}
	binary := filepath.Join(directory, "postgres.exe")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, source)
	build.Stdout, build.Stderr = io.Discard, io.Discard
	if err := build.Run(); err != nil {
		t.Fatal("cannot build restricted-command fixture")
	}
	data, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal("cannot copy restricted-command fixture")
	}
	tools := []string{"postgres", "initdb", "createdb", "pg_ctl", "psql", "future-postgres-tool"}
	for _, tool := range tools[1:] {
		if err := os.WriteFile(filepath.Join(directory, executable(tool)), data, 0o700); err != nil {
			t.Fatal("cannot prepare restricted-command fixture")
		}
	}
	c, manifest := unitCell(t)
	c.cfg.Binaries.PostgresDir = directory
	if err := c.makePassword(); err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools {
		t.Run(tool, func(t *testing.T) {
			if err := c.pgCommand(t.Context(), tool); err != nil {
				t.Fatal("PostgreSQL command did not receive its restricted token")
			}
			if _, err := c.databaseCommand(t.Context(), manifest, tool); err != nil {
				t.Fatal("database command did not receive its restricted token")
			}
			if _, err := c.reportedVersion(t.Context(), filepath.Join(directory, executable(tool))); err != nil {
				t.Fatal("PostgreSQL version probe did not receive its restricted token")
			}
		})
	}
	t.Run("proof-preflight", func(t *testing.T) {
		cmd := exec.CommandContext(t.Context(), binary, "--version")
		release, err := ConfigurePostgresProofCommand(cmd)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
		if err := cmd.Run(); err != nil {
			t.Fatal("proof preflight did not receive its restricted token")
		}
	})
}

func TestWindowsPostgresVersionTokenScope(t *testing.T) {
	const directory = `C:\fixture\postgres`
	for _, tc := range []struct {
		name, path string
		want       bool
	}{
		{"postgres", filepath.Join(directory, "postgres.exe"), true},
		{"future-tool", filepath.Join(directory, "future-tool.exe"), true},
		{"nested-tool", filepath.Join(directory, "helpers", "tool.exe"), true},
		{"case-insensitive", strings.ToUpper(filepath.Join(directory, "postgres.exe")), true},
		{"sibling", `C:\fixture\postgres-other\server.exe`, false},
		{"outside", `C:\fixture\worker.exe`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(tc.path)
			release, err := configurePostgresVersionCommand(cmd, directory)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			got := cmd.SysProcAttr != nil && cmd.SysProcAttr.Token != 0
			if got != tc.want {
				t.Fatal("version-command restriction selected the wrong distribution boundary")
			}
			if got {
				if _, err := windows.Token(cmd.SysProcAttr.Token).GetTokenUser(); err != nil {
					t.Fatal("version-command token was closed before execution")
				}
			}
		})
	}
}
