package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
)

func TestFixturePublicationTransient(t *testing.T) {
	for _, code := range []syscall.Errno{5, 32, 33} {
		t.Run(fmt.Sprint(uintptr(code)), func(t *testing.T) {
			for _, err := range []error{code, &os.LinkError{Op: "rename", Old: "pending", New: "published", Err: code}, fmt.Errorf("wrapped: %w", code)} {
				if fixturePublicationTransient(err) != (runtime.GOOS == "windows") {
					t.Fatalf("errno %d transient classification differs from platform expectation", uintptr(code))
				}
			}
		})
	}
	for _, err := range []error{nil, errors.New("ordinary failure"), os.ErrNotExist, syscall.Errno(87)} {
		if fixturePublicationTransient(err) {
			t.Fatal("unlisted publication error classified as transient")
		}
	}
}

func TestFixturePublicationRenameRetries(t *testing.T) {
	transient := errors.New("synthetic sharing violation")
	clock := time.Unix(0, 0)
	calls, sleeps := 0, 0
	err := fixtureRenamePublication("pending", "published", func(oldpath, newpath string) error {
		if oldpath != "pending" || newpath != "published" {
			t.Fatal("rename retry changed publication paths")
		}
		calls++
		if calls <= 2 {
			return transient
		}
		return nil
	}, func(err error) bool { return errors.Is(err, transient) }, func() time.Time { return clock }, func(delay time.Duration) {
		sleeps++
		clock = clock.Add(delay)
	})
	if err != nil || calls != 3 || sleeps != 2 {
		t.Fatal("publication rename did not recover after two transient errors")
	}
}

func TestFixturePublicationRenameBound(t *testing.T) {
	original, later := errors.New("first sharing violation"), errors.New("later sharing violation")
	start := time.Unix(0, 0)
	clock := start
	calls := 0
	var totalSleep time.Duration
	err := fixtureRenamePublication("pending", "published", func(string, string) error {
		calls++
		if calls == 1 {
			return original
		}
		return later
	}, func(error) bool { return true }, func() time.Time { return clock }, func(delay time.Duration) {
		if delay <= 0 || delay > 10*time.Millisecond {
			t.Fatal("publication rename sleep is outside the short retry interval")
		}
		totalSleep += delay
		clock = clock.Add(delay)
	})
	if err != original {
		t.Fatal("publication rename exhaustion did not return the original error")
	}
	if calls <= 1 || totalSleep != fixturePublicationTimeout/10 || clock.Sub(start) != fixturePublicationTimeout/10 {
		t.Fatal("publication rename retry exceeded or bypassed its proportional bound")
	}
}

func TestFixturePublicationRenameImmediate(t *testing.T) {
	fatal := errors.New("permanent rename failure")
	for _, result := range []error{nil, fatal, syscall.Errno(87)} {
		calls := 0
		err := fixtureRenamePublication("pending", "published", func(string, string) error {
			calls++
			return result
		}, fixturePublicationTransient, time.Now, func(time.Duration) {
			t.Fatal("publication rename slept after an immediate result")
		})
		if err != result || calls != 1 {
			t.Fatal("publication rename changed a nontransient result")
		}
	}
}

func TestFixturePublicationRenameStopsAtFatal(t *testing.T) {
	transient, fatal := errors.New("sharing violation"), errors.New("permanent failure")
	clock := time.Unix(0, 0)
	calls := 0
	err := fixtureRenamePublication("pending", "published", func(string, string) error {
		calls++
		if calls == 1 {
			return transient
		}
		return fatal
	}, func(err error) bool { return errors.Is(err, transient) }, func() time.Time { return clock }, func(delay time.Duration) { clock = clock.Add(delay) })
	if err != fatal || calls != 2 {
		t.Fatal("publication rename retried or hid a permanent failure")
	}
}

func TestFixturePublicationRenameDeadlineBeforeRetry(t *testing.T) {
	transient := errors.New("sharing violation")
	for _, operationDelay := range []bool{false, true} {
		t.Run(fmt.Sprint(operationDelay), func(t *testing.T) {
			clock := time.Unix(0, 0)
			calls, sleeps := 0, 0
			err := fixtureRenamePublication("pending", "published", func(string, string) error {
				calls++
				if operationDelay {
					clock = clock.Add(fixturePublicationTimeout / 10)
				}
				return transient
			}, func(error) bool { return true }, func() time.Time { return clock }, func(time.Duration) {
				sleeps++
				clock = clock.Add(fixturePublicationTimeout / 10)
			})
			if err != transient || calls != 1 || (operationDelay && sleeps != 0) {
				t.Fatal("publication rename attempted another operation after its deadline")
			}
		})
	}
}

// Exercise the real empty-record publication path so bypassing the retry helper
// cannot leave its unit tests green while the fixture still renames just once.
func TestFixturePublicationServePostgresRetries(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	root := t.TempDir()
	t.Chdir(root)
	args := os.Args
	os.Args = []string{"postgres", "-D", root}
	t.Cleanup(func() { os.Args = args })
	if err := fixtureWriteFile(filepath.Join(root, "postgresql.conf"), []byte("port = 0\n")); err != nil {
		t.Fatal("cannot write publication fixture config")
	}
	if err := fixtureWriteFile("fixture-postgres-empty-record", nil); err != nil {
		t.Fatal("cannot stage publication fixture empty record")
	}
	transient := errors.New("synthetic sharing violation")
	calls := 0
	err := servePostgresWithPublication(func(oldpath, newpath string) error {
		calls++
		if calls <= 2 {
			return transient
		}
		return os.Rename(oldpath, newpath)
	}, func(err error) bool { return errors.Is(err, transient) })
	if err != nil || calls != 3 {
		t.Fatal("fixture publication path did not recover after two transient rename errors")
	}
	pidfile := filepath.Join(root, "postmaster.pid")
	if data, err := os.ReadFile(pidfile); err != nil || len(data) != 0 {
		t.Fatal("publication fixture did not publish its empty final record")
	}
	if _, err := os.Stat(pidfile + ".pending"); !os.IsNotExist(err) {
		t.Fatal("publication fixture retained its pending name after rename")
	}
}
