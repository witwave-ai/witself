//go:build !windows

package trust

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

type unixPrivateRecordOps struct {
	dir    string
	strict bool
	parent os.FileInfo
}
type unixPrivateRecordFile struct {
	*os.File
	identity os.FileInfo
}

func newPrivateRecordOps(dir string, strict bool) (privateRecordOps, error) {
	ops := &unixPrivateRecordOps{dir: dir, strict: strict}
	if err := ops.checkDirectory(false); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, ReasonStateIO
	}
	ops.parent = info
	return ops, nil
}

func (o *unixPrivateRecordOps) checkDirectory(write bool) error {
	if o.strict {
		if write {
			if privateDirectory(o.dir, false) != nil {
				return ReasonStateIO
			}
		} else if privateReadDirectory(o.dir) != nil {
			return ReasonStateIO
		}
	} else {
		info, err := os.Lstat(o.dir)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0700 {
			return ReasonStateIO
		}
	}
	if o.parent != nil {
		info, err := os.Lstat(o.dir)
		if err != nil || !os.SameFile(o.parent, info) {
			return ReasonStateIO
		}
	}
	return nil
}

func unixPrivateRecordInfo(info os.FileInfo) (privateRecordInfo, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return privateRecordInfo{}, ReasonStateIO
	}
	return privateRecordInfo{identity: [3]uint64{uint64(stat.Dev), uint64(stat.Ino), 0}, size: info.Size()}, nil
}

func (o *unixPrivateRecordOps) validate(info os.FileInfo) (privateRecordInfo, error) {
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || (o.strict && !privateOwned(info)) {
		return privateRecordInfo{}, ReasonStateIO
	}
	return unixPrivateRecordInfo(info)
}

func (o *unixPrivateRecordOps) inspect(name string) (privateRecordInfo, error) {
	if !privateRecordName(name) || o.checkDirectory(false) != nil {
		return privateRecordInfo{}, ReasonStateIO
	}
	info, err := os.Lstat(filepath.Join(o.dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return privateRecordInfo{}, os.ErrNotExist
	}
	if err != nil {
		return privateRecordInfo{}, ReasonStateIO
	}
	return o.validate(info)
}
func (o *unixPrivateRecordOps) open(name string) (privateRecordHandle, error) {
	if !privateRecordName(name) || o.checkDirectory(false) != nil {
		return nil, ReasonStateIO
	}
	f, err := openArchiveFile(filepath.Join(o.dir, name))
	if err != nil {
		return nil, ReasonStateIO
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, ReasonStateIO
	}
	return &unixPrivateRecordFile{File: f, identity: info}, nil
}
func (o *unixPrivateRecordOps) inspectHandle(handle privateRecordHandle) (privateRecordInfo, error) {
	f, ok := handle.(*unixPrivateRecordFile)
	if !ok {
		return privateRecordInfo{}, ReasonStateIO
	}
	info, err := f.Stat()
	if err != nil {
		return privateRecordInfo{}, ReasonStateIO
	}
	if !os.SameFile(f.identity, info) {
		return privateRecordInfo{}, ReasonStateIO
	}
	return o.validate(info)
}
func (o *unixPrivateRecordOps) create(name string, temporary bool) (privateRecordHandle, error) {
	if !privateRecordName(name) || o.checkDirectory(true) != nil {
		return nil, ReasonStateIO
	}
	var f *os.File
	var err error
	if temporary {
		f, err = os.CreateTemp(o.dir, name)
	} else {
		f, err = os.OpenFile(filepath.Join(o.dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	}
	if err != nil {
		return nil, ReasonStateIO
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, ReasonStateIO
	}
	result := &unixPrivateRecordFile{File: f, identity: info}
	if temporary {
		err = f.Chmod(0600)
	}
	if err == nil {
		_, err = o.inspectHandle(result)
	}
	if err != nil {
		_ = o.removeOwned(result)
		_ = f.Close()
		return nil, ReasonStateIO
	}
	return result, nil
}
func (o *unixPrivateRecordOps) removeOwned(handle privateRecordHandle) error {
	f, ok := handle.(*unixPrivateRecordFile)
	if !ok || o.checkDirectory(true) != nil {
		return ReasonStateIO
	}
	info, err := os.Lstat(f.Name())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !os.SameFile(f.identity, info) {
		return ReasonStateIO
	}
	if os.Remove(f.Name()) != nil {
		return ReasonStateIO
	}
	return nil
}
func (*unixPrivateRecordOps) closeBeforePublish() bool { return true }
func (o *unixPrivateRecordOps) recheck(name string, handle privateRecordHandle, prior *privateRecordInfo) error {
	f, ok := handle.(*unixPrivateRecordFile)
	if !ok || o.checkDirectory(true) != nil {
		return ReasonStateIO
	}
	source, err := os.Lstat(f.Name())
	if err != nil || !os.SameFile(source, f.identity) {
		return ReasonStateIO
	}
	if _, err := o.validate(source); err != nil {
		return err
	}
	target, err := o.inspect(name)
	if prior == nil {
		if !errors.Is(err, os.ErrNotExist) {
			return ReasonStateIO
		}
	} else if err != nil || target.identity != prior.identity {
		return ReasonStateIO
	}
	return nil
}
func (o *unixPrivateRecordOps) publish(handle privateRecordHandle, name string) (bool, error) {
	f, ok := handle.(*unixPrivateRecordFile)
	if !ok || !privateRecordName(name) {
		return false, ReasonStateIO
	}
	if os.Rename(f.Name(), filepath.Join(o.dir, name)) != nil {
		return false, ReasonStateIO
	}
	return true, nil
}
func (o *unixPrivateRecordOps) complete(handle privateRecordHandle, name string) error {
	f, ok := handle.(*unixPrivateRecordFile)
	if !ok || o.checkDirectory(true) != nil {
		return ReasonStateIO
	}
	info, err := os.Lstat(filepath.Join(o.dir, name))
	if err != nil || !os.SameFile(f.identity, info) {
		return ReasonStateIO
	}
	if _, err := o.validate(info); err != nil {
		return err
	}
	d, err := os.Open(o.dir)
	if err != nil {
		return ReasonStateIO
	}
	syncErr := d.Sync()
	closeErr := d.Close()
	if syncErr != nil || closeErr != nil {
		return ReasonStateIO
	}
	return nil
}
func (*unixPrivateRecordOps) close() error { return nil }
