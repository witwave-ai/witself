package localcell

import (
	"strings"
	"testing"
)

func TestInstallationIdentitySupportsWindowsTaskNamespace(t *testing.T) {
	for _, platform := range []string{"windows", "linux", "darwin"} {
		t.Run(platform, func(t *testing.T) {
			first, err := newLocalCellInstallationID(platform)
			if err != nil {
				t.Fatal("cannot allocate installation identity fixture")
			}
			second, err := newLocalCellInstallationID(platform)
			if err != nil || first == second {
				t.Fatal("installation allocation did not retain independent randomness")
			}
			if platform == "windows" {
				if len(first) != 18 || !strings.HasPrefix(first, "lc") || !windowsTaskInstallationID.MatchString(first) {
					t.Fatal("fresh Windows installation cannot name a strict logon task")
				}
			} else if len(first) != 19 || !strings.HasPrefix(first, "lc_") {
				t.Fatal("Unix installation identity format changed")
			}
		})
	}
}

func TestWindowsTaskDoesNotAliasLegacyInstallationIdentity(t *testing.T) {
	d := windowsTaskTestDescriptor()
	d.InstallationID = "lc_abcdefghijklmnop"
	if _, err := WindowsLogonTaskName(d); err == nil {
		t.Fatal("legacy installation identity was silently normalized")
	}
	c, manifest := unitCell(t)
	before := manifest.InstallationID
	if _, err := newLocalCellInstallationID("windows"); err != nil {
		t.Fatal("cannot allocate new Windows identity")
	}
	after, err := readManifest(c.cfg.Root)
	if err != nil || after.InstallationID != before {
		t.Fatal("allocating a task identity rewrote an existing installation")
	}
}
