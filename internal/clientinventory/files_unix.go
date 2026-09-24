//go:build darwin || linux

package clientinventory

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

type directory struct{ fd int }

// The absolute root is caller-owned custody. All descendants are opened one
// component at a time relative to held descriptors, without following links.
func openDirectory(path string) (*directory, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, closedFileError(err)
	}
	return &directory{fd: fd}, nil
}
func (d *directory) close() { _ = unix.Close(d.fd) }

func closedFileError(err error) error {
	if errors.Is(err, unix.ENOENT) {
		return errMissing
	}
	return errUnsafe
}

func (d *directory) parent(ctx context.Context, rel string) (int, string, error) {
	parts := strings.Split(rel, "/")
	if len(parts) > 8 {
		return -1, "", errUnsafe
	}
	for _, p := range parts {
		if p == "" || p == "." || p == ".." || strings.ContainsAny(p, "\\\x00") {
			return -1, "", errUnsafe
		}
	}
	fd, err := unix.Openat(d.fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, "", closedFileError(err)
	}
	for _, p := range parts[:len(parts)-1] {
		if err := ctx.Err(); err != nil {
			_ = unix.Close(fd)
			return -1, "", err
		}
		next, err := unix.Openat(fd, p, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
		_ = unix.Close(fd)
		if err != nil {
			return -1, "", closedFileError(err)
		}
		fd = next
	}
	return fd, parts[len(parts)-1], nil
}

func regularStat(fd int, name string) (unix.Stat_t, error) {
	var st unix.Stat_t
	if err := unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return st, closedFileError(err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 {
		return st, errUnsafe
	}
	return st, nil
}

func (d *directory) regular(ctx context.Context, rel string) error {
	fd, name, err := d.parent(ctx, rel)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err = regularStat(fd, name)
	return err
}

func (d *directory) read(ctx context.Context, rel string, limit int64) ([]byte, error) {
	fd, name, err := d.parent(ctx, rel)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	before, err := regularStat(fd, name)
	if err != nil {
		return nil, err
	}
	if before.Size < 0 || before.Size > limit || before.Mode&0444 == 0 {
		return nil, errUnsafe
	}
	opened, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, closedFileError(err)
	}
	f := os.NewFile(uintptr(opened), "inventory")
	defer f.Close()
	var current unix.Stat_t
	if unix.Fstat(opened, &current) != nil || !sameFile(before, current) {
		return nil, errUnsafe
	}
	// Chunked bounded reads provide cancellation checks even for a growing file.
	var raw []byte
	buf := make([]byte, 4096)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, err := f.Read(buf[:min(int64(len(buf)), limit+1-int64(len(raw)))])
		raw = append(raw, buf[:n]...)
		if int64(len(raw)) > limit {
			return nil, errUnsafe
		}
		if err == io.EOF {
			break
		}
		if err != nil || n == 0 {
			return nil, errUnsafe
		}
	}
	after, err := regularStat(fd, name)
	if err != nil || !sameFile(before, after) || unix.Fstat(opened, &current) != nil || !sameFile(before, current) {
		return nil, errUnsafe
	}
	return raw, nil
}

func sameFile(a, b unix.Stat_t) bool {
	return a.Dev == b.Dev && a.Ino == b.Ino && a.Mode == b.Mode && a.Nlink == b.Nlink && a.Size == b.Size && a.Mtim == b.Mtim && a.Ctim == b.Ctim
}
