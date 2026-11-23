//go:build windows

package localcell

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/witwave-ai/witself/internal/runtimeconfig"
)

func snapshotRename(source, target string, boundary func(bool)) error {
	return runtimeconfig.WindowsPrivateRenameObserved(source, target, boundary)
}

func snapshotLink(t *testing.T, path, _ string) {
	t.Helper()
	// This mandatory reparse fixture needs no symbolic-link privilege.
	createTestJunction(t, path, testPrivateRoot(t))
}

func snapshotChangedParent(t *testing.T, root string) string {
	t.Helper()
	parent := filepath.Join(root, "parent")
	path := filepath.Join(parent, "record")
	original := filepath.Join(root, "original")
	if testMkdir(parent, 0o700) != nil || testWriteFile(path, nil, 0o600) != nil ||
		os.Rename(parent, original) != nil {
		t.Fatal("cannot replace private snapshot parent")
	}
	// The same private regular file is reachable under the original spelling.
	// Its policy still cannot authenticate a changed, reparse parent.
	createTestJunction(t, parent, original)
	return path
}

func snapshotAdmissionRows(t *testing.T) {
	for _, row := range []string{"tree_entry", "manifest", "descriptor", "endpoint", "lease", "auth_tree", "auth_record"} {
		t.Run(row, func(t *testing.T) {
			var data []byte
			host := serviceWorkerConfigured(t, "", false, func(c *Cell, _ ServiceDescriptor) {
				if row == "auth_record" {
					data = snapshotAuthData(t)
					if writeAtomicBytes(snapshotRecordPath(c, row), data, nil) != nil {
						t.Fatal("cannot install private snapshot authentication fixture")
					}
				}
			})
			host.ready(t)
			path := snapshotRecordPath(host.cell, row)
			if row != "auth_record" {
				var err error
				data, err = os.ReadFile(path)
				if err != nil {
					t.Fatal("cannot stage selected snapshot record")
				}
			}
			// Auth is the first walk. Preflight and dial each validate a
			// descriptor tree. For the dial case require preflight to finish
			// normally within this very request before activating the hold.
			passes := []int{2, 3}
			if row == "auth_tree" || row == "auth_record" {
				passes = []int{1}
			}
			for _, pass := range passes {
				snapshotObserveHeld(t, host.cell.cfg, path, data, pass, row == "tree_entry" || row == "auth_tree")
			}
		})
	}
}

func snapshotObserveHeld(t *testing.T, cfg Config, path string, data []byte, pass int, holdBeforeWalk bool) {
	t.Helper()
	c, err := New(cfg)
	if err != nil {
		t.Fatal("cannot create independent snapshot client")
	}
	ctx, cancel := context.WithTimeout(t.Context(), serviceControlBudget(c.cfg.Timeout, 3))
	atCrossing, resume, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(resume) })
	var walks atomic.Int32
	c.observeRootWalk = func(before bool) {
		if before {
			walks.Add(1)
		}
		if int(walks.Load()) == pass && before == holdBeforeWalk {
			close(atCrossing)
			select {
			case <-resume:
			case <-ctx.Done():
			}
		}
	}
	var status ServiceStatus
	var readErr error
	go func() {
		defer close(done)
		status, readErr = c.Control(ctx, "observe")
	}()
	defer func() {
		unblock()
		cancel()
		<-done
	}()
	select {
	case <-atCrossing:
	case <-done:
		t.Fatal(snapshotAdmissionLost)
	case <-ctx.Done():
		t.Fatal(snapshotAdmissionLost)
	}
	observed := false
	err = snapshotRename(snapshotStage(t, path, data), path, func(after bool) {
		if !after {
			return
		}
		unblock()
		select {
		case <-done:
			observed = readErr == nil && status.Healthy && status.State == "running" && !status.WorkInFlight && walks.Load() == 3
		case <-ctx.Done():
		}
	})
	if err != nil || !observed {
		t.Fatal(snapshotAdmissionLost)
	}
}
