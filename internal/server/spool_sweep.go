package server

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

var archiveSpoolName = regexp.MustCompile(`^witself-(backup-validate|account-import|self-export)-[0-9]{1,10}\.tar\.gz$`)

// SweepStaleArchiveSpools runs before serving, when no archive jobs are active.
// Each server must own its temp directory; only direct, regular spool files are
// removed. In Kubernetes the directory is a per-pod emptyDir.
func SweepStaleArchiveSpools(dir string, w io.Writer) {
	if w == nil {
		w = io.Discard
	}
	var removed, failed int
	var bytes int64
	entries, err := os.ReadDir(dir)
	if err != nil {
		failed++
	}
	for _, entry := range entries {
		if !archiveSpoolName.MatchString(entry.Name()) {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			failed++
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if err := os.Remove(path); err != nil {
			failed++
			continue
		}
		removed++
		bytes += info.Size()
	}
	_, _ = fmt.Fprintf(w, "witself-server: stale archive spool sweep dir=%q removed=%d bytes=%d failed=%d\n", dir, removed, bytes, failed)
}
