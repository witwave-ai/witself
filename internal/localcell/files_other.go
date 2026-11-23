//go:build !windows

package localcell

import (
	"context"
	"errors"
	"os"
	"runtime"
	"syscall"
	"time"
)

// Preserve the existing Unix filesystem operations and permission checks.
func makePrivateDir(path string) error { return os.Mkdir(path, 0o700) }

func openPrivateFile(path string, flags int, mode os.FileMode) (*os.File, error) {
	return os.OpenFile(path, flags, mode)
}

// openPrivateReader opens path read-only without waiting on what stands at the
// name, so a FIFO opens at once, and refuses a link at its last element; a
// leased file is waited out by repeating that same open (waitOutLease).
func openPrivateReader(ctx context.Context, path string) (*os.File, error) {
	return waitOutLease(ctx, path, os.Lstat, func() (*os.File, error) {
		return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	})
}

// openPrivateWriter opens path for writing without waiting on what stands at
// the name, so a FIFO with no reader fails at once, and refuses a link at its
// last element; a leased file is waited out by repeating that same open
// (waitOutLease).
func openPrivateWriter(ctx context.Context, path string, flags int, mode os.FileMode) (*os.File, error) {
	return waitOutLease(ctx, path, os.Lstat, func() (*os.File, error) {
		return os.OpenFile(path, flags|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, mode)
	})
}

// leaseWait bounds waitOutLease, which repeats only on Linux. Its ceiling
// exceeds Linux's default lease-break time (/proc/sys/fs/lease-break-time, 45
// seconds), after which a holder that never lets go has lost a conflicting
// lease; a write lease met by a read is only downgraded then. Only tests
// change it.
var leaseWait = struct{ ceiling, first, longest time.Duration }{60 * time.Second, time.Millisecond, 100 * time.Millisecond}

// waitOutLease returns open's result unless open failed with EWOULDBLOCK and
// lstat shows a regular file at path: on Linux the refusal of a non-blocking
// open of a leased file, which has asked the holder to let go (open(2),
// fcntl(2)). It then sleeps and repeats the same open, the interval doubling
// from leaseWait.first to at most leaseWait.longest, while the next interval
// ends by leaseWait.ceiling; the last refusal is returned unchanged. It never
// opens otherwise and never blocks: anything at the name but a regular file is
// refused at once, on every attempt. Each sleep ends early at leaseWaitDone,
// and the wait then returns a leaseWaitCut of ctx's error with no further
// attempt; the first attempt is always made, so a done ctx never refuses an
// unleased file. A FUSE file system or a storage manager that answers
// would-block for a regular file is waited out like a lease. Other systems get
// the first result: macOS breaks a lease only for a blocking open, so a repeat
// there could only wait and then refuse.
func waitOutLease[T any](ctx context.Context, path string, lstat func(string) (os.FileInfo, error), open func() (T, error)) (T, error) {
	deadline, delay := time.Now().Add(leaseWait.ceiling), leaseWait.first
	for {
		result, err := open()
		if runtime.GOOS != "linux" || !errors.Is(err, syscall.EWOULDBLOCK) {
			return result, err
		}
		if info, statErr := lstat(path); statErr != nil || !info.Mode().IsRegular() || time.Now().Add(delay).After(deadline) {
			return result, err
		}
		pause := time.NewTimer(delay)
		select {
		case <-leaseWaitDone(ctx):
			pause.Stop()
			return result, &leaseWaitCut{cause: ctx.Err()}
		case <-pause.C:
		}
		if delay *= 2; delay > leaseWait.longest {
			delay = leaseWait.longest
		}
	}
}

// leaseWaitDone is what a lease wait races: ctx.Done(), or nil for a context
// leaseWaitDetached marked, so that no lease wait below it ends early.
func leaseWaitDone(ctx context.Context) <-chan struct{} {
	if ctx.Value(leaseWaitDetachedKey{}) != nil {
		return nil
	}
	return ctx.Done()
}

// blockingWriter clears openPrivateWriter's non-blocking flag: today's open
// leaves a regular file blocking (os/file_unix.go:204-223), and Fd would not
// clear a caller's flag for a child (os/file_unix.go:75-82).
func blockingWriter(f *os.File) error {
	raw, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var cleared error
	if err := raw.Control(func(fd uintptr) { cleared = syscall.SetNonblock(int(fd), false) }); err != nil {
		return err
	}
	return cleared
}

// A file below pg/ and a service manager file outside the root open the same
// way here; only Windows needs their different guarded or plain opens.
func openPostgresReader(ctx context.Context, path string) (*os.File, error) {
	return openPrivateReader(ctx, path)
}
func openPlainReader(ctx context.Context, path string) (*os.File, error) {
	return openPrivateReader(ctx, path)
}

func openAdmissionRecord(ctx context.Context, path string, open error) (*os.File, error) {
	return openServiceRecord(ctx, path, open)
}

func readServiceRecordJSON(ctx context.Context, path string, dst any) error {
	if err := servicePrivateRecord(path); err != nil {
		return err
	}
	return readJSON(ctx, path, dst)
}

func readManifestJSON(ctx context.Context, path string, dst any) error {
	return readJSON(ctx, path, dst)
}

func readPrivateFile(path string) ([]byte, error) { return os.ReadFile(path) }

func createPrivateTemp(dir string) (*os.File, error) { return os.CreateTemp(dir, ".pending-*") }
func privateTempName(name string) bool               { return unixPrivateTempName(name) }

func postgresPendingRegular(_ string, path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular()
}

func privatePassword(_ string, info os.FileInfo) bool { return info.Mode().Perm() == 0o600 }

func validateRootPath(string) error    { return nil }
func validatePrivateTree(string) error { return nil }
func validateWriteTarget(string) error { return nil }

func publishPrivateFile(oldPath, newPath string) error { return os.Rename(oldPath, newPath) }
func removePrivateFile(path string) error              { return os.Remove(path) }

func (c *Cell) preparePostgresDirectory() error    { return nil }
func (c *Cell) validatePostgresDirectory() error   { return privateDir(c.path("pg"), false) }
func readPostgresFile(path string) ([]byte, error) { return readPrivateFile(path) }
func (c *Cell) writePostgresConfig(data []byte) error {
	return writeAtomicBytes(c.path("pg", "postgresql.conf"), data, nil)
}
