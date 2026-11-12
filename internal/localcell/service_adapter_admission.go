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
	if owner == "" {
		return nil, ServiceDescriptor{}, nil, serviceSystemdError(ErrServiceOwnershipConflict, "apply:descriptor", "owner")
	}
	if err := serviceAdapterConfinement(d.Root, artifactDir); err != nil {
		return nil, ServiceDescriptor{}, nil, err
	}
	cell, err := New(Config{Root: d.Root, Timeout: 60 * time.Second})
	if err != nil {
		return nil, ServiceDescriptor{}, nil, serviceSystemdError(ErrServiceOwnershipConflict, "apply:root", "unavailable")
	}
	lock, err := cell.own(false)
	if err != nil {
		if errors.Is(err, ErrLocked) {
			return nil, ServiceDescriptor{}, nil, err
		}
		return nil, ServiceDescriptor{}, nil, serviceSystemdError(ErrServiceOwnershipConflict, "apply:root", "unavailable")
	}
	fresh, err := cell.serviceDescriptorForOwner(owner)
	if err != nil {
		_ = lock.Close()
		return nil, ServiceDescriptor{}, nil, serviceSystemdError(ErrServiceOwnershipConflict, "apply:descriptor", "unavailable")
	}
	if !reflect.DeepEqual(windowsTaskImmutable(fresh), windowsTaskImmutable(d)) {
		_ = lock.Close()
		return nil, ServiceDescriptor{}, nil, serviceSystemdError(ErrServiceOwnershipConflict, "apply:descriptor", "stale_identity")
	}
	return cell, fresh, func() { _ = lock.Close() }, nil
}

func serviceAdapterConfinement(root, artifactDir string) error {
	root, err := serviceAdapterCanonicalPath(root)
	if err != nil {
		return serviceSystemdError(ErrServiceOwnershipConflict, "apply:confinement", "unresolved")
	}
	artifactDir, err = serviceAdapterCanonicalPath(artifactDir)
	if err != nil {
		return serviceSystemdError(ErrServiceOwnershipConflict, "apply:confinement", "unresolved")
	}
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		root, artifactDir = strings.ToLower(root), strings.ToLower(artifactDir)
	}
	separator := string(filepath.Separator)
	if root == artifactDir || strings.HasPrefix(root, strings.TrimSuffix(artifactDir, separator)+separator) ||
		strings.HasPrefix(artifactDir, strings.TrimSuffix(root, separator)+separator) {
		return serviceSystemdError(ErrServiceOwnershipConflict, "apply:confinement", "overlap")
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
