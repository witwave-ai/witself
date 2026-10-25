//go:build windows

package localcell

import (
	"errors"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func (c *Cell) baseEnvironment() ([]string, error) {
	return c.windowsEnvironment(false, windows.GetSystemWindowsDirectory, windows.GetSystemDirectory)
}

func (c *Cell) postgresEnvironment() ([]string, error) {
	return c.windowsEnvironment(true, windows.GetSystemWindowsDirectory, windows.GetSystemDirectory)
}

// No parent variable supplies a path, including SystemRoot and COMSPEC. PATH
// contains only the PostgreSQL bundle's bin directory; Windows resolves required
// system DLLs through its trusted system search locations, not an inherited PATH.
// PW3 must prove any additional bundle dependency directories before adding them.
func (c *Cell) windowsEnvironment(postgres bool, windowsDirectory, systemDirectory func() (string, error)) ([]string, error) {
	root, err := windowsDirectory()
	if err != nil || !filepath.IsAbs(root) || strings.ContainsRune(root, 0) {
		return nil, errors.New("local cell trusted Windows directory unavailable")
	}
	environment := []string{
		"PATH=" + c.cfg.Binaries.PostgresDir,
		"HOME=" + c.path("home"),
		"TMPDIR=" + c.path("tmp"),
		"TEMP=" + c.path("tmp"),
		"TMP=" + c.path("tmp"),
		"SystemRoot=" + root,
		"USERPROFILE=" + c.path("home"),
	}
	if postgres {
		system, err := systemDirectory()
		if err != nil || !filepath.IsAbs(system) || strings.ContainsRune(system, 0) {
			return nil, errors.New("local cell trusted Windows system directory unavailable")
		}
		// pg_ctl uses cmd.exe for its shell redirection. Components never get a
		// command interpreter or real user-profile settings in their allowlist.
		environment = append(environment, "COMSPEC="+filepath.Join(system, "cmd.exe"))
	}
	return appendEnvironment(nil, environment...), nil
}

// Windows names are case-insensitive. Keep only the last explicitly supplied
// value, including when an existing entry differs in casing, with no os.Environ
// fallback. The retained entry uses its caller's spelling.
func appendEnvironment(environment []string, entries ...string) []string {
	combined := append(append([]string(nil), environment...), entries...)
	last := make(map[string]int, len(combined))
	for i, entry := range combined {
		name, _, _ := strings.Cut(entry, "=")
		last[strings.ToUpper(name)] = i
	}
	result := make([]string, 0, len(last))
	for i, entry := range combined {
		name, _, _ := strings.Cut(entry, "=")
		if last[strings.ToUpper(name)] == i {
			result = append(result, entry)
		}
	}
	return result
}
