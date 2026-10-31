//go:build !windows

package trust

import (
	"archive/tar"
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestArchiveSourceFIFORefusesWithoutWriter(t *testing.T) {
	file, component := extractArchiveFixture(t, "tar.gz", []extractFixtureEntry{{"payload", []byte("fixture"), 0644, tar.TypeReg}})
	if err := os.Remove(file); err != nil {
		t.Fatal("remove regular fixture")
	}
	if err := syscall.Mkfifo(file, 0600); err != nil {
		t.Fatal("create FIFO fixture")
	}
	for _, cancelled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		if cancelled {
			cancel()
		}
		stage := filepath.Join(t.TempDir(), "stage")
		err := ExtractVerified(ctx, file, component, stage)
		cancel()
		if err != ExtractEntryType {
			t.Fatal("FIFO source was not refused")
		}
		if _, err := os.Lstat(stage); !os.IsNotExist(err) {
			t.Fatal("FIFO refusal retained staging")
		}
	}
	if _, _, err := InspectArchive(context.Background(), file, "tar.gz"); err != ExtractEntryType {
		t.Fatal("inspection accepted FIFO source")
	}
}
