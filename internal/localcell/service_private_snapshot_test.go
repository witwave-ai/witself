package localcell

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/runtimeconfig"
)

const (
	snapshotLost          = "private snapshot lost its complete object"
	snapshotAdmissionLost = "admission lost a private publication"
	snapshotUnproved      = "private snapshot admitted an unproved object"
	snapshotBudget        = "private read exceeded its admission budget"
)

// The staged file is created and synchronized just as it is before an atomic
// record publication. The rename callback runs inside the actual publisher.
func snapshotStage(t *testing.T, path string, data []byte) string {
	t.Helper()
	staged, err := createPrivateTemp(filepath.Dir(path))
	if err != nil {
		t.Fatal("cannot stage private snapshot fixture")
	}
	_, err = staged.Write(data)
	if err == nil {
		err = staged.Sync()
	}
	if closeErr := staged.Close(); err != nil || closeErr != nil {
		t.Fatal("cannot persist private snapshot fixture")
	}
	return staged.Name()
}

func TestServicePrivateSnapshot(t *testing.T) {
	for _, name := range []string{"before_replace", "after_replace", "retained_old"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(testPrivateRoot(t), "record")
			oldData, newData := bytes.Repeat([]byte{1}, 37), bytes.Repeat([]byte{2}, 83)
			if testWriteFile(path, oldData, 0o600) != nil {
				t.Fatal("cannot create private snapshot fixture")
			}
			staged := snapshotStage(t, path, newData)
			var reader *os.File
			if name == "retained_old" {
				var err error
				reader, err = openAdmissionRecord(t.Context(), path, ErrServiceControlRefused)
				if err != nil {
					t.Fatal(snapshotLost)
				}
				defer func() { _ = reader.Close() }()
			}
			calls := 0
			complete := false
			err := snapshotRename(staged, path, func(after bool) {
				if after != (name != "before_replace") {
					return
				}
				calls++
				f := reader
				if f == nil {
					var err error
					f, err = openAdmissionRecord(t.Context(), path, ErrServiceControlRefused)
					if err != nil {
						return
					}
					defer func() { _ = f.Close() }()
				}
				want := oldData
				if name == "after_replace" {
					want = newData
				}
				got, err := io.ReadAll(f)
				complete = err == nil && bytes.Equal(got, want)
			})
			if err != nil || calls != 1 || !complete {
				t.Fatal(snapshotLost)
			}
		})
	}

	for _, name := range []string{"wrong_type", "reparse_leaf", "changed_parent", "foreign_policy"} {
		t.Run(name, func(t *testing.T) {
			root := testPrivateRoot(t)
			path := filepath.Join(root, "record")
			switch name {
			case "wrong_type":
				if testMkdir(path, 0o700) != nil {
					t.Fatal("cannot prepare private snapshot directory")
				}
			case "reparse_leaf":
				snapshotLink(t, path, root)
			case "changed_parent":
				path = snapshotChangedParent(t, root)
			case "foreign_policy":
				if testWriteFile(path, nil, 0o600) != nil {
					t.Fatal("cannot prepare private snapshot policy fixture")
				}
				recoveryBoundaryNonPrivate(t, path, false)
			}
			f, err := openAdmissionRecord(t.Context(), path, ErrServiceControlRefused)
			if f != nil {
				_ = f.Close()
			}
			if err == nil || f != nil {
				t.Fatal(snapshotUnproved)
			}
		})
	}

	for _, row := range []string{"first_attempt", "terminal_policy"} {
		t.Run(row, func(t *testing.T) {
			root := testPrivateRoot(t)
			path := filepath.Join(root, "record")
			if testWriteFile(path, nil, 0o600) != nil {
				t.Fatal("cannot create private snapshot budget fixture")
			}
			calls, sleeps := 0, 0
			now := time.Unix(0, 0)
			clock := leasePublishHooks{
				now: func() time.Time { return now },
				sleep: func(time.Duration) {
					sleeps++
					now = now.Add(serviceLeasePublishBudget)
				},
			}
			check := func() error {
				return validateRootTree(root, func(root string) error {
					calls++
					if runtime.GOOS == "windows" {
						return validatePrivateTree(root)
					}
					f, err := openAdmissionRecord(t.Context(), path, ErrServiceControlRefused)
					if f != nil {
						_ = f.Close()
					}
					return err
				}, runtimeconfig.WindowsPrivateTransient, clock)
			}
			var readErr error
			if row == "terminal_policy" {
				recoveryBoundaryNonPrivate(t, path, false)
				readErr = check()
			} else {
				err := snapshotRename(snapshotStage(t, path, nil), path, func(after bool) {
					if after {
						readErr = check()
					}
				})
				if err != nil {
					t.Fatal(snapshotBudget)
				}
			}
			if (readErr != nil) != (row == "terminal_policy") || calls != 1 || sleeps != 0 {
				t.Fatal(snapshotBudget)
			}
		})
	}

	t.Run("already_cancelled", func(t *testing.T) {
		c := serviceCancellationReceiptCell(t)
		d := prepareSupervisorTest(t, c)
		lease, err := c.acquireServiceLease(d)
		if err != nil {
			t.Fatal("cannot acquire private snapshot control fixture")
		}
		defer func() { _ = lease.Close() }()
		listener, err := c.listenServiceControl()
		if err != nil {
			t.Fatal("cannot publish private snapshot control fixture")
		}
		defer func() {
			_ = listener.Close()
			_ = c.removeServiceControlEndpoint()
		}()
		walks := 0
		c.observeRootWalk = func(before bool) {
			if before {
				walks++
			}
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err = c.Control(ctx, "observe")
		// Both clients perform the authentication walk. Windows also checks
		// discovery before building the request. A dial would walk it again.
		wantWalks := 1
		if runtime.GOOS == "windows" {
			wantWalks++
		}
		if err == nil || err.Error() != "localcell: service_control_unavailable: transport:deadline" || walks != wantWalks {
			t.Fatal(snapshotBudget)
		}
	})

	snapshotAdmissionRows(t)
}

// A generated disposable credential lets both publication and the synthetic
// host agree without inspecting the contents of an existing credential file.
func snapshotAuthData(t *testing.T) []byte {
	t.Helper()
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		t.Fatal("cannot prepare private snapshot authentication fixture")
	}
	return base64.RawURLEncoding.AppendEncode(nil, key[:])
}

func snapshotRecordPath(c *Cell, row string) string {
	switch row {
	case "auth_record":
		return c.path("service", "control.secret")
	case "manifest":
		return c.path("manifest.json")
	case "descriptor":
		return c.path("service", "descriptor.json")
	case "lease":
		return c.path("service", "lease.json")
	default:
		return c.path("service", "control-endpoint.json")
	}
}
