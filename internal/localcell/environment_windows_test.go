//go:build windows

package localcell

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func environmentValues(t *testing.T, environment []string) map[string]string {
	t.Helper()
	values := make(map[string]string, len(environment))
	for _, entry := range environment {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || name == "" {
			t.Fatal("child environment contained an invalid entry")
		}
		name = strings.ToUpper(name)
		if _, exists := values[name]; exists {
			t.Fatal("child environment contained duplicate variable names")
		}
		values[name] = value
	}
	return values
}

func TestWindowsEnvironmentUsesTrustedDirectoriesAndPrivateProfile(t *testing.T) {
	root, err := windows.GetSystemWindowsDirectory()
	if err != nil {
		t.Fatal("cannot obtain trusted Windows directory for test")
	}
	system, err := windows.GetSystemDirectory()
	if err != nil {
		t.Fatal("cannot obtain trusted system directory for test")
	}
	c, _ := unitCell(t)
	c.cfg.Binaries.PostgresDir = t.TempDir()
	for _, name := range []string{"SystemRoot", "COMSPEC", "PATH", "HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "TEMP", "TMP", "TMPDIR", "PGHOST", "WITSELF_PROVISION_TOKEN"} {
		t.Setenv(name, "synthetic-parent-only")
	}
	for _, postgres := range []bool{false, true} {
		environment, err := c.baseEnvironment()
		if postgres {
			environment, err = c.postgresEnvironment()
		}
		if err != nil {
			t.Fatal("cannot construct Windows environment")
		}
		values := environmentValues(t, environment)
		want := map[string]string{
			"PATH": c.cfg.Binaries.PostgresDir, "HOME": c.path("home"),
			"USERPROFILE": c.path("home"), "TMPDIR": c.path("tmp"),
			"TEMP": c.path("tmp"), "TMP": c.path("tmp"), "SYSTEMROOT": root,
		}
		if postgres {
			want["COMSPEC"] = filepath.Join(system, "cmd.exe")
		}
		if len(values) != len(want) {
			t.Fatal("Windows environment differs from the exact allowlist")
		}
		for name, expected := range want {
			if values[name] != expected {
				t.Errorf("Windows environment has untrusted or nonprivate %s", name)
			}
		}
	}
}

func terminateEnvironmentFixture(record ProcessRecord) error {
	handle, alive, err := openVerifiedWindowsProcess(record, windows.PROCESS_TERMINATE)
	if err != nil || !alive {
		return err
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	return windows.TerminateProcess(handle, 1)
}

func TestWindowsEnvironmentDirectoryLookupFailsClosed(t *testing.T) {
	c, _ := unitCell(t)
	trusted := func() (string, error) { return `C:\Windows`, nil }
	failed := func() (string, error) { return "", errors.New("synthetic directory failure") }
	relative := func() (string, error) { return "relative-directory", nil }
	nul := func() (string, error) { return "C:\\Windows\x00suffix", nil }
	for _, lookup := range []func() (string, error){failed, relative, nul} {
		if environment, err := c.windowsEnvironment(false, lookup, trusted); err == nil || environment != nil {
			t.Fatal("untrusted Windows directory did not fail closed")
		}
		if environment, err := c.windowsEnvironment(true, trusted, lookup); err == nil || environment != nil {
			t.Fatal("untrusted system directory did not fail closed")
		}
	}
	if _, err := c.windowsEnvironment(false, trusted, failed); err != nil {
		t.Fatal("component environment unnecessarily required a command interpreter")
	}
}

func TestWindowsEnvironmentDeduplicatesNamesCaseInsensitively(t *testing.T) {
	got := appendEnvironment([]string{"Path=synthetic-first", "HOME=synthetic-home", "pAtH=synthetic-second"}, "PATH=synthetic-final", "home=synthetic-final-home")
	want := []string{"PATH=synthetic-final", "home=synthetic-final-home"}
	if !slices.Equal(got, want) {
		t.Fatal("Windows environment did not retain exactly the last explicit value per name")
	}
}

func TestWindowsPostgresToolsReceiveExactEnvironmentNames(t *testing.T) {
	binary := buildEnvironmentNamesChild(t)
	for _, name := range []string{"SystemRoot", "COMSPEC", "USERPROFILE", "LOCALCELL_PARENT_SENTINEL", "PGHOST", "LC_ALL", "LANG"} {
		t.Setenv(name, "synthetic-parent-only")
	}
	for _, database := range []bool{false, true} {
		name := "postgres"
		if database {
			name = "database"
		}
		t.Run(name, func(t *testing.T) {
			c, m := unitCell(t)
			c.cfg.Binaries.PostgresDir = filepath.Dir(binary)
			if err := c.makePassword(); err != nil {
				t.Fatal(err)
			}
			var output string
			var err error
			if database {
				output, err = c.databaseCommand(t.Context(), m, "environment-names", "exit")
			} else {
				err = c.pgCommand(t.Context(), "environment-names", "exit")
				if err == nil {
					var data []byte
					data, err = os.ReadFile(c.path("logs", "postgres.log"))
					output = string(data)
				}
			}
			if err != nil {
				t.Fatal("could not read PostgreSQL fixture environment names")
			}
			want := []string{"COMSPEC", "HOME", "LANG", "LC_ALL", "PATH", "SystemRoot", "TEMP", "TMP", "TMPDIR", "USERPROFILE"}
			if database {
				want = append(want, "PGCONNECT_TIMEOUT", "PGPASSWORD")
			}
			slices.Sort(want)
			if !slices.Equal(strings.Fields(output), want) || strings.Contains(output, "=") {
				t.Fatal("PostgreSQL fixture environment names differ from exact allowlist")
			}
		})
	}
}
