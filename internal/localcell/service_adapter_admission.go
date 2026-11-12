package localcell

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"time"
)

func serviceAdapterAdmission(owner string, d ServiceDescriptor, artifactDir string) (*Cell, ServiceDescriptor, func(), error) {
	return serviceAdapterAdmissionForStep(owner, d, artifactDir, "apply")
}

func serviceAdapterRemovalAdmission(owner string, d ServiceDescriptor, artifactDir string) (*Cell, ServiceDescriptor, func(), error) {
	return serviceAdapterAdmissionForStep(owner, d, artifactDir, "remove")
}

func serviceAdapterAdmissionForStep(owner string, d ServiceDescriptor, artifactDir, step string) (*Cell, ServiceDescriptor, func(), error) {
	if owner == "" {
		return nil, ServiceDescriptor{}, nil, serviceSystemdError(ErrServiceOwnershipConflict, step+":descriptor", "owner")
	}
	if err := serviceAdapterConfinementForStep(d.Root, artifactDir, step); err != nil {
		return nil, ServiceDescriptor{}, nil, err
	}
	cell, err := New(Config{Root: d.Root, Timeout: 60 * time.Second})
	if err != nil {
		return nil, ServiceDescriptor{}, nil, serviceSystemdError(ErrServiceOwnershipConflict, step+":root", "unavailable")
	}
	lock, err := cell.own(false)
	if err != nil {
		if errors.Is(err, ErrLocked) {
			return nil, ServiceDescriptor{}, nil, err
		}
		return nil, ServiceDescriptor{}, nil, serviceSystemdError(ErrServiceOwnershipConflict, step+":root", "unavailable")
	}
	fresh, err := cell.serviceDescriptorForOwner(owner)
	if err != nil {
		_ = lock.Close()
		return nil, ServiceDescriptor{}, nil, serviceSystemdError(ErrServiceOwnershipConflict, step+":descriptor", "unavailable")
	}
	if !reflect.DeepEqual(windowsTaskImmutable(fresh), windowsTaskImmutable(d)) {
		_ = lock.Close()
		return nil, ServiceDescriptor{}, nil, serviceSystemdError(ErrServiceOwnershipConflict, step+":descriptor", "stale_identity")
	}
	return cell, fresh, func() { _ = lock.Close() }, nil
}

func serviceAdapterConfinementForStep(root, artifactDir, step string) error {
	root, err := serviceAdapterCanonicalPath(root)
	if err != nil {
		return serviceSystemdError(ErrServiceOwnershipConflict, step+":confinement", "unresolved")
	}
	artifactDir, err = serviceAdapterCanonicalPath(artifactDir)
	if err != nil {
		return serviceSystemdError(ErrServiceOwnershipConflict, step+":confinement", "unresolved")
	}
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		root, artifactDir = strings.ToLower(root), strings.ToLower(artifactDir)
	}
	separator := string(filepath.Separator)
	if root == artifactDir || strings.HasPrefix(root, strings.TrimSuffix(artifactDir, separator)+separator) ||
		strings.HasPrefix(artifactDir, strings.TrimSuffix(root, separator)+separator) {
		return serviceSystemdError(ErrServiceOwnershipConflict, step+":confinement", "overlap")
	}
	return nil
}

func serviceAdapterCanonicalPath(path string) (string, error) {
	path, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	var tail []string
	for {
		if _, err := os.Lstat(path); err == nil {
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil {
				return "", err
			}
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			return resolved, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", os.ErrNotExist
		}
		tail = append(tail, filepath.Base(path))
		path = parent
	}
}

// The caller holds the operation lock. Keep the stable host lock until all
// registration effects and absence proofs finish; never publish a host lease.
func serviceAdapterRemovalHostLock(c *Cell) (func(), error) {
	if err := servicePrivateRecord(c.path(".service-host.lock")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	file, err := openLockFile(c.path(".service-host.lock"), true)
	if err != nil {
		return nil, err
	}
	if err := lockFile(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return func() { _ = file.Close() }, nil
}
