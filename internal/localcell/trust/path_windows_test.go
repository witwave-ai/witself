package trust

import (
	"os"
	"syscall"
	"testing"
	"time"
)

type reparseFixtureInfo struct {
	attributes *syscall.Win32FileAttributeData
	mode       os.FileMode
}

func (r reparseFixtureInfo) Name() string       { return "fixture" }
func (r reparseFixtureInfo) Size() int64        { return 0 }
func (r reparseFixtureInfo) Mode() os.FileMode  { return r.mode }
func (r reparseFixtureInfo) ModTime() time.Time { return time.Time{} }
func (r reparseFixtureInfo) IsDir() bool        { return r.mode.IsDir() }
func (r reparseFixtureInfo) Sys() any           { return r.attributes }

func TestWindowsRejectsEveryReparseAttribute(t *testing.T) {
	for _, mode := range []os.FileMode{0644, os.ModeDir | 0700, os.ModeSymlink | 0644, os.ModeIrregular | 0644} {
		info := reparseFixtureInfo{attributes: &syscall.Win32FileAttributeData{FileAttributes: syscall.FILE_ATTRIBUTE_REPARSE_POINT}, mode: mode}
		if inventoryInfoSafe(info) {
			t.Fatal("reparse attribute accepted")
		}
	}
	if inventoryInfoSafe(reparseFixtureInfo{mode: 0644}) {
		t.Fatal("missing native attributes accepted")
	}
	if !inventoryInfoSafe(reparseFixtureInfo{attributes: &syscall.Win32FileAttributeData{}, mode: 0644}) {
		t.Fatal("plain file refused")
	}
}
