//go:build windows

package localcell

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/runtimeconfig"
	"golang.org/x/sys/windows"
)

// This proof uses the actual filesystem publication and Windows error chain.
// Only observation and the lifetime of the foreign reader are controlled.
func TestPublishServiceLeaseForeignHandle(t *testing.T) {
	for _, release := range []bool{true, false} {
		name := "held-past-budget"
		if release {
			name = "released-after-second-sleep"
		}
		t.Run(name, func(t *testing.T) {
			c, path, before, after := leasePublishWindowsFixture(t)
			pointer, err := windows.UTF16PtrFromString(path)
			if err != nil {
				t.Fatal("cannot encode lease reader fixture")
			}
			handle, err := windows.CreateFile(pointer, windows.GENERIC_READ,
				windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil,
				windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
			if err != nil {
				t.Fatal("cannot open lease reader without delete sharing")
			}
			reader := os.NewFile(uintptr(handle), path)
			t.Cleanup(func() {
				if reader != nil {
					_ = reader.Close()
				}
			})
			attempts, sleeps := 0, 0
			previousHooks := serviceLeasePublishTestHooks
			t.Cleanup(func() { serviceLeasePublishTestHooks = previousHooks })
			serviceLeasePublishTestHooks = leasePublishHooks{
				// Count each complete attempt of the shared publication retry.
				write: func(path string, value any, beforeRename func() error) error {
					attempts++
					return writeAtomicOnce(path, value, beforeRename)
				},
				sleep: func(delay time.Duration) {
					sleeps++
					time.Sleep(delay)
					if release && sleeps == 2 {
						if err := reader.Close(); err != nil {
							t.Fatal("cannot release lease reader")
						}
						reader = nil
					}
				},
			}
			started := time.Now()
			err = c.publishServiceLease(path, after)
			elapsed := time.Since(started)
			want := before
			if release {
				if err != nil || attempts < 3 || sleeps != 2 {
					t.Fatalf("released reader publication: success=%t attempts=%d sleeps=%d", err == nil, attempts, sleeps)
				}
				want = leasePublishWindowsBytes(t, after)
			} else {
				class := runtimeconfig.WindowsPrivateFailureClass(err)
				if err == nil || (class != "sharing_violation" && class != "access_denied") {
					t.Fatalf("held reader publication: failure=%t class=%s", err != nil, class)
				}
				if attempts < 2 || sleeps < 1 || elapsed < serviceLeasePublishBudget {
					t.Fatalf("held reader did not exhaust budget: attempts=%d sleeps=%d elapsed_ms=%d", attempts, sleeps, elapsed.Milliseconds())
				}
				t.Logf("held reader failure class=%s attempts=%d", class, attempts)
			}
			data, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(data, want) {
				t.Fatal("lease publication did not preserve the expected record bytes")
			}
		})
	}

	t.Run("validation-refusal", func(t *testing.T) {
		c, path, before, after := leasePublishWindowsFixture(t)
		// A readable but non-private target must fail validation before retrying.
		if !setTestDACL(t, path, "(A;;FR;;;WD)", false, false) {
			t.Fatal("cannot install lease validation refusal fixture")
		}
		attempts, sleeps := 0, 0
		previousHooks := serviceLeasePublishTestHooks
		t.Cleanup(func() { serviceLeasePublishTestHooks = previousHooks })
		serviceLeasePublishTestHooks = leasePublishHooks{
			write: func(path string, value any, beforeRename func() error) error {
				attempts++
				return writeAtomicOnce(path, value, beforeRename)
			},
			sleep: func(time.Duration) { sleeps++ },
		}
		err := c.publishServiceLease(path, after)
		class := runtimeconfig.WindowsPrivateFailureClass(err)
		if err == nil || class != "validation" || attempts != 1 || sleeps != 0 {
			t.Fatalf("validation refusal: failure=%t class=%s attempts=%d sleeps=%d", err != nil, class, attempts, sleeps)
		}
		data, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(data, before) {
			t.Fatal("validation refusal changed the existing lease bytes")
		}
	})
}

func leasePublishWindowsFixture(t *testing.T) (*Cell, string, []byte, serviceLeaseRecord) {
	t.Helper()
	c, _ := unitCell(t)
	if err := privateDir(c.path("service"), true); err != nil {
		t.Fatal("cannot create private lease fixture directory")
	}
	path := c.path("service", "lease.json")
	record := serviceLeaseRecord{
		Version: 1, BootID: "synthetic-boot", StartIdentity: "synthetic-old-process",
		PID: 101, Owner: "synthetic-owner", InstallationID: "lc_test",
	}
	if err := writeAtomic(path, record, nil); err != nil {
		t.Fatal("cannot publish initial lease fixture")
	}
	before := leasePublishWindowsBytes(t, record)
	record.StartIdentity = "synthetic-new-process"
	record.PID = 102
	return c, path, before, record
}

func leasePublishWindowsBytes(t *testing.T, record serviceLeaseRecord) []byte {
	t.Helper()
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		t.Fatal("cannot encode expected lease fixture")
	}
	return append(data, '\n')
}
