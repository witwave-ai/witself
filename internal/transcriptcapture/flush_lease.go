package transcriptcapture

import (
	"errors"
	"path/filepath"
	"sync"
)

var flushProcessLeases sync.Map

// acquireFlushLease serializes current-version owners without reclaiming or
// unlinking the lock inode. The separate legacy PID marker remains necessary
// while an older installed binary might still be draining the queue.
func acquireFlushLease(dir string) (func(), bool, error) {
	path, err := filepath.Abs(filepath.Join(dir, ".flush.lease"))
	if err != nil {
		return nil, false, err
	}
	value, _ := flushProcessLeases.LoadOrStore(path, &sync.Mutex{})
	mutex := value.(*sync.Mutex)
	if !mutex.TryLock() {
		return func() {}, false, nil
	}
	retained := false
	defer func() {
		if !retained {
			mutex.Unlock()
		}
	}()
	file, err := openFlushLease(path)
	if err != nil {
		return nil, false, err
	}
	defer func() {
		if !retained {
			_ = file.Close()
		}
	}()
	validate := func() error {
		info, err := file.Stat()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || !trustedPathIdentity(path, info) {
			return errors.New("capture flush lease is not a trusted regular file")
		}
		return nil
	}
	if err := validate(); err != nil {
		return nil, false, err
	}
	acquired, err := tryLockFlushLease(file)
	if err != nil || !acquired {
		return func() {}, false, err
	}
	if err := validate(); err != nil {
		return nil, false, err
	}
	retained = true
	var once sync.Once
	return func() {
		once.Do(func() {
			// Closing the retained descriptor releases its kernel lock, even
			// after process death. Never remove the permanent lease pathname.
			_ = file.Close()
			mutex.Unlock()
		})
	}, true, nil
}
