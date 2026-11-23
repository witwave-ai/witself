//go:build !windows

package localcell

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func snapshotRename(source, target string, boundary func(bool)) error {
	boundary(false)
	if err := publishPrivateFile(source, target); err != nil {
		return err
	}
	boundary(true)
	return nil
}

func snapshotLink(t *testing.T, path, root string) {
	t.Helper()
	target := filepath.Join(root, "private")
	if testWriteFile(target, nil, 0o600) != nil || os.Symlink(target, path) != nil {
		t.Fatal("cannot prepare private snapshot link")
	}
}

func snapshotChangedParent(t *testing.T, root string) string {
	t.Helper()
	parent := filepath.Join(root, "parent")
	path := filepath.Join(parent, "record")
	if testMkdir(parent, 0o700) != nil || testWriteFile(path, nil, 0o600) != nil ||
		os.Rename(parent, filepath.Join(root, "original")) != nil || testWriteFile(parent, nil, 0o600) != nil {
		t.Fatal("cannot replace private snapshot parent")
	}
	return path
}

// Unix transport uses a socket and does not consult the published Windows
// endpoint. Exercise its portable admission chain with the actual descriptor,
// lease and endpoint readers and a synthetic boot identity.
func snapshotAdmissionRows(t *testing.T) {
	t.Run("auth_record", func(t *testing.T) {
		c, _ := descriptorTestCell(t)
		fresh := snapshotAuthData(t)
		path := snapshotRecordPath(c, "auth_record")
		observed := false
		err := snapshotRename(snapshotStage(t, path, fresh), path, func(after bool) {
			if after {
				got, readErr := c.readServiceControlSecret()
				observed = readErr == nil && bytes.Equal(got, fresh)
			}
		})
		if err != nil || !observed {
			t.Fatal(snapshotAdmissionLost)
		}
	})
	for _, row := range []string{"manifest", "descriptor", "endpoint", "lease"} {
		t.Run(row, func(t *testing.T) {
			c, d := descriptorTestCell(t)
			boot := func() (string, error) { return "fixture-boot", nil }
			lease, err := c.acquireServiceLeaseWithBoot(d, boot)
			if err != nil {
				t.Fatal("cannot prepare private snapshot lease")
			}
			defer func() { _ = lease.Close() }()
			name, err := newServiceControlPipeName(d.InstallationID)
			if err != nil {
				t.Fatal("cannot prepare private snapshot endpoint")
			}
			want, err := c.publishServiceControlEndpointWithBoot(name, boot)
			if err != nil {
				t.Fatal("cannot publish private snapshot endpoint")
			}
			path := snapshotRecordPath(c, row)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal("cannot stage selected snapshot record")
			}
			observed := false
			err = snapshotRename(snapshotStage(t, path, data), path, func(after bool) {
				if after {
					got, readErr := c.readServiceControlEndpointWithBoot(boot)
					observed = readErr == nil && got == want
				}
			})
			if err != nil || !observed {
				t.Fatal(snapshotAdmissionLost)
			}
		})
	}
}
