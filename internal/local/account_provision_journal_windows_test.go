//go:build windows

package local

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsAccountProvisionJournalSyncBoundary(t *testing.T) {
	directory := t.TempDir()
	fileOK := t.Run("writable_file", func(t *testing.T) {
		file, err := os.OpenFile(filepath.Join(directory, "state.json"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = file.Close() }()
		if _, err := file.WriteString("{}\n"); err != nil {
			t.Fatal(err)
		}
		if err := file.Sync(); err != nil {
			t.Fatalf("writable file Sync: %v", err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	})
	t.Logf("writable_file_sync_passed=%t", fileOK)

	// Exercise this primitive directly even if the separate lifecycle test
	// rejects Windows permission metadata before reaching directory sync.
	directoryOK := t.Run("directory", func(t *testing.T) {
		if err := syncAccountProvisionJournalDirectory(directory); err != nil {
			t.Fatalf("journal directory Sync: %v", err)
		}
	})
	t.Logf("directory_sync_passed=%t", directoryOK)

	// This independent candidate only probes native local-NTFS feasibility.
	// It does not replace the blocking helper assertion above, verify private
	// ACLs, or establish publication/recovery or power-loss durability.
	candidateOK := t.Run("native_ntfs_directory_candidate", func(t *testing.T) {
		candidateDirectory := t.TempDir()
		before, err := os.Lstat(candidateDirectory)
		if err != nil || !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("candidate directory before open is unsafe: %v", err)
		}
		path, err := windows.UTF16PtrFromString(candidateDirectory)
		if err != nil {
			t.Fatal(err)
		}
		handle, err := windows.CreateFile(path,
			windows.GENERIC_READ|windows.GENERIC_WRITE,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
			nil, windows.OPEN_EXISTING,
			windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
		if err != nil {
			t.Fatalf("native candidate directory open: %v", err)
		}
		file := os.NewFile(uintptr(handle), candidateDirectory)
		if file == nil {
			if err := windows.CloseHandle(handle); err != nil {
				t.Errorf("native candidate handle close: %v", err)
			}
			t.Fatal("native candidate directory file is unavailable")
		}
		defer func() {
			if err := file.Close(); err != nil {
				t.Errorf("native candidate directory close: %v", err)
			}
		}()
		checkIdentity := func() {
			t.Helper()
			var information windows.ByHandleFileInformation
			if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
				t.Fatalf("native candidate directory information: %v", err)
			}
			opened, openErr := file.Stat()
			linked, linkErr := os.Lstat(candidateDirectory)
			if openErr != nil || linkErr != nil ||
				information.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 ||
				information.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 ||
				!opened.IsDir() || !linked.IsDir() ||
				opened.Mode()&os.ModeSymlink != 0 || linked.Mode()&os.ModeSymlink != 0 ||
				!os.SameFile(before, opened) || !os.SameFile(opened, linked) {
				t.Fatalf("native candidate directory identity changed or is unsafe: stat=%v path=%v", openErr, linkErr)
			}
		}
		checkIdentity()

		var filesystem [261]uint16
		var flags uint32
		if err := windows.GetVolumeInformationByHandle(handle, nil, 0, nil, nil, &flags,
			&filesystem[0], uint32(len(filesystem))); err != nil {
			t.Fatalf("native candidate handle filesystem: %v", err)
		}
		const required = windows.FILE_PERSISTENT_ACLS | windows.FILE_SUPPORTS_HARD_LINKS
		if windows.UTF16ToString(filesystem[:]) != "NTFS" || flags&required != required ||
			flags&windows.FILE_READ_ONLY_VOLUME != 0 {
			t.Fatal("native candidate requires writable NTFS with persistent ACL and hard-link support")
		}
		// VOLUME_NAME_GUID requires a local volume identity from this handle;
		// a drive-letter spelling or a successful flush alone is insufficient.
		const volumeNameGUID = 0x1
		var finalPath [32768]uint16
		n, err := windows.GetFinalPathNameByHandle(handle, &finalPath[0], uint32(len(finalPath)), volumeNameGUID)
		if err != nil || n == 0 || n >= uint32(len(finalPath)) {
			t.Fatalf("native candidate local volume identity: %v", err)
		}
		resolved := windows.UTF16ToString(finalPath[:n])
		end := strings.Index(resolved, `}\`)
		if !strings.HasPrefix(resolved, `\\?\Volume{`) || end != 47 {
			t.Fatal("native candidate directory does not resolve to a local volume GUID")
		}
		if _, err := windows.GUIDFromString(resolved[len(`\\?\Volume`) : end+1]); err != nil {
			t.Fatal("native candidate volume GUID is invalid")
		}
		if err := windows.FlushFileBuffers(handle); err != nil {
			t.Fatalf("native candidate directory FlushFileBuffers: %v", err)
		}
		checkIdentity()
	})
	t.Logf("native_ntfs_directory_candidate_passed=%t", candidateOK)
}
