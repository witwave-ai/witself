package trust

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type extractFixtureEntry struct {
	name string
	body []byte
	mode int64
	kind byte
}

func extractDigest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func extractGzip(t *testing.T, data []byte, compressed bool) []byte {
	t.Helper()
	var buffer bytes.Buffer
	level := gzip.NoCompression
	if compressed {
		level = gzip.BestCompression
	}
	w, err := gzip.NewWriterLevel(&buffer, level)
	if err != nil {
		t.Fatal("gzip writer")
	}
	if _, err = w.Write(data); err != nil {
		t.Fatal("gzip write")
	}
	if err = w.Close(); err != nil {
		t.Fatal("gzip close")
	}
	return buffer.Bytes()
}
func extractTarBytes(t *testing.T, entries []extractFixtureEntry) []byte {
	t.Helper()
	var buffer bytes.Buffer
	w := tar.NewWriter(&buffer)
	for _, entry := range entries {
		header := &tar.Header{Name: entry.name, Mode: entry.mode, Size: int64(len(entry.body)), Typeflag: entry.kind}
		if entry.kind != 0 && entry.kind != tar.TypeReg {
			header.Size = 0
			header.Linkname = "../../outside"
		}
		if err := w.WriteHeader(header); err != nil {
			t.Fatal("tar header")
		}
		if header.Size > 0 {
			if _, err := w.Write(entry.body); err != nil {
				t.Fatal("tar body")
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal("tar close")
	}
	return buffer.Bytes()
}
func extractZipBytes(t *testing.T, entries []extractFixtureEntry) []byte {
	t.Helper()
	var buffer bytes.Buffer
	w := zip.NewWriter(&buffer)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: zip.Store}
		mode := os.FileMode(entry.mode)
		if entry.kind == tar.TypeSymlink {
			mode |= os.ModeSymlink
		}
		if entry.kind == tar.TypeDir {
			mode |= os.ModeDir
		}
		header.SetMode(mode)
		part, err := w.CreateHeader(header)
		if err != nil {
			t.Fatal("zip header")
		}
		if _, err = part.Write(entry.body); err != nil {
			t.Fatal("zip body")
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal("zip close")
	}
	return buffer.Bytes()
}
func extractArchiveFixture(t *testing.T, format string, entries []extractFixtureEntry) (string, Component) {
	t.Helper()
	var data []byte
	if format == "zip" {
		data = extractZipBytes(t, entries)
	} else {
		data = extractGzip(t, extractTarBytes(t, entries), false)
	}
	file := filepath.Join(t.TempDir(), "fixture."+format)
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal("archive write")
	}
	component := Component{Archive: Archive{Name: filepath.Base(file), Size: uint64(len(data)), SHA256: extractDigest(data), Format: format}}
	for _, entry := range entries {
		mode := "0644"
		if entry.mode == 0755 {
			mode = "0755"
		}
		component.Inventory = append(component.Inventory, InventoryEntry{Path: entry.name, Size: uint64(len(entry.body)), SHA256: extractDigest(entry.body), Mode: mode, Executable: mode == "0755"})
	}
	return file, component
}
func extractAssertRefused(t *testing.T, file string, component Component, want error) {
	t.Helper()
	staging := filepath.Join(t.TempDir(), "stage")
	err := ExtractVerified(context.Background(), file, component, staging)
	if err == nil {
		t.Fatal("expected refusal")
	}
	if want != nil && err != want {
		t.Fatalf("reason got %s want %s", err, want)
	}
	if _, err := os.Lstat(staging); !os.IsNotExist(err) {
		t.Fatal("refused extraction retained staging")
	}
}

func TestExtractVerifiedRoundTrip(t *testing.T) {
	for _, format := range []string{"tar.gz", "zip"} {
		t.Run(format, func(t *testing.T) {
			entries := []extractFixtureEntry{{"bin/tool", []byte("synthetic executable"), 0755, tar.TypeReg}, {"share/license", []byte("synthetic license"), 0644, tar.TypeReg}}
			file, component := extractArchiveFixture(t, format, entries)
			archive, inventory, err := InspectArchive(context.Background(), file, format)
			if err != nil {
				t.Fatalf("inspect: %s", err)
			}
			if archive.Size != component.Archive.Size || archive.SHA256 != component.Archive.SHA256 || len(inventory) != 2 {
				t.Fatal("inventory mismatch")
			}
			stage := filepath.Join(t.TempDir(), "stage")
			if err = ExtractVerified(context.Background(), file, component, stage); err != nil {
				t.Fatalf("extract: %s", err)
			}
			if err = RecheckInventory(stage, component); err != nil {
				t.Fatalf("recheck: %s", err)
			}
			if runtime.GOOS != "windows" {
				for _, name := range []string{".", "bin", "share"} {
					info, err := os.Stat(filepath.Join(stage, name))
					if err != nil || info.Mode().Perm() != 0700 {
						t.Fatal("directory is not private")
					}
				}
			}
		})
	}
}

func TestExtractVerifiedArchiveAdversaries(t *testing.T) {
	body := []byte("fixture payload")
	for _, format := range []string{"tar.gz", "zip"} {
		for _, tc := range []struct {
			name    string
			entries []extractFixtureEntry
			mutate  func(*Component)
			reason  error
		}{
			{"symlink", []extractFixtureEntry{{"payload", nil, 0644, tar.TypeSymlink}}, nil, ExtractEntryType},
			{"duplicate", []extractFixtureEntry{{"payload", body, 0644, tar.TypeReg}, {"payload", body, 0644, tar.TypeReg}}, func(c *Component) { c.Inventory = c.Inventory[:1] }, ExtractDuplicate},
			// Nonempty extras exceed the aggregate inventory bound before the visitor.
			{"undeclared_aggregate_bound", []extractFixtureEntry{{"payload", body, 0644, tar.TypeReg}, {"extra", body, 0644, tar.TypeReg}}, func(c *Component) { c.Inventory = c.Inventory[:1] }, ExtractInventoryLimit},
			{"undeclared_executable_aggregate_bound", []extractFixtureEntry{{"payload", body, 0644, tar.TypeReg}, {"surprise", body, 0755, tar.TypeReg}}, func(c *Component) { c.Inventory = c.Inventory[:1] }, ExtractInventoryLimit},
			// Zero-byte extras isolate undeclared-entry refusal from that size bound.
			{"undeclared_zero_file", []extractFixtureEntry{{"payload", body, 0644, tar.TypeReg}, {"extra", nil, 0644, tar.TypeReg}}, func(c *Component) { c.Inventory = c.Inventory[:1] }, ExtractUndeclared},
			{"undeclared_zero_executable", []extractFixtureEntry{{"payload", body, 0644, tar.TypeReg}, {"surprise", nil, 0755, tar.TypeReg}}, func(c *Component) { c.Inventory = c.Inventory[:1] }, ExtractUndeclared},
			{"missing", []extractFixtureEntry{{"payload", body, 0644, tar.TypeReg}}, func(c *Component) {
				extra := c.Inventory[0]
				extra.Path = "missing"
				c.Inventory = append(c.Inventory, extra)
			}, ExtractMissing},
			{"size_shorter", []extractFixtureEntry{{"payload", body, 0644, tar.TypeReg}}, func(c *Component) { c.Inventory[0].Size++ }, ExtractEntrySize},
			{"size_longer", []extractFixtureEntry{{"payload", body, 0644, tar.TypeReg}}, func(c *Component) { c.Inventory[0].Size-- }, nil},
			{"digest", []extractFixtureEntry{{"payload", body, 0644, tar.TypeReg}}, func(c *Component) { c.Inventory[0].SHA256 = strings.Repeat("0", 64) }, ExtractEntryDigest},
			{"mode", []extractFixtureEntry{{"payload", body, 0644, tar.TypeReg}}, func(c *Component) { c.Inventory[0].Mode = "0755"; c.Inventory[0].Executable = true }, ExtractEntryMode},
			{"dotdot", []extractFixtureEntry{{"../outside", body, 0644, tar.TypeReg}}, func(c *Component) { c.Inventory[0].Path = "payload" }, ExtractUnsafePath},
			{"absolute", []extractFixtureEntry{{"/outside", body, 0644, tar.TypeReg}}, func(c *Component) { c.Inventory[0].Path = "payload" }, ExtractUnsafePath},
			{"directory", []extractFixtureEntry{{"folder/", nil, 0755, tar.TypeDir}}, func(c *Component) { c.Inventory[0].Path = "payload" }, ExtractEntryType},
		} {
			t.Run(format+"/"+tc.name, func(t *testing.T) {
				file, c := extractArchiveFixture(t, format, tc.entries)
				if tc.mutate != nil {
					tc.mutate(&c)
				}
				extractAssertRefused(t, file, c, tc.reason)
			})
		}
	}
	for _, tc := range []struct {
		name string
		kind byte
	}{{"hardlink", tar.TypeLink}, {"device", tar.TypeChar}, {"block_device", tar.TypeBlock}, {"fifo", tar.TypeFifo}} {
		t.Run("tar/"+tc.name, func(t *testing.T) {
			file, c := extractArchiveFixture(t, "tar.gz", []extractFixtureEntry{{"payload", nil, 0644, tc.kind}})
			extractAssertRefused(t, file, c, ExtractEntryType)
		})
	}
}

func TestExtractVerifiedRawTarAdversaries(t *testing.T) {
	for _, tc := range []struct {
		name   string
		build  func([]byte) []byte
		reason error
	}{
		{"pax_longlink", func(raw []byte) []byte { raw[156] = tar.TypeXHeader; return raw }, ExtractEntryType},
		{"gnu_longlink", func(raw []byte) []byte { raw[156] = tar.TypeGNULongLink; return raw }, ExtractEntryType},
		{"gnu_longname", func(raw []byte) []byte { raw[156] = tar.TypeGNULongName; return raw }, ExtractEntryType},
		{"short_body", func(raw []byte) []byte { return raw[:515] }, ExtractEntrySize},
		{"long_body_lying_header", func(raw []byte) []byte { raw[512+len("fixture payload")] = 'X'; return raw }, ExtractArchiveInvalid},
		{"gzip_truncated", func(raw []byte) []byte { return raw[:512] }, ExtractEntrySize},
		{"gzip_bomb_tail", func(raw []byte) []byte { return append(raw, make([]byte, 1<<20)...) }, ExtractArchiveInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file, c := extractArchiveFixture(t, "tar.gz", []extractFixtureEntry{{"payload", []byte("fixture payload"), 0644, tar.TypeReg}})
			raw := extractTarBytes(t, []extractFixtureEntry{{"payload", []byte("fixture payload"), 0644, tar.TypeReg}})
			data := extractGzip(t, tc.build(raw), true)
			if err := os.WriteFile(file, data, 0600); err != nil {
				t.Fatal("fixture write")
			}
			c.Archive.Size = uint64(len(data))
			c.Archive.SHA256 = extractDigest(data)
			extractAssertRefused(t, file, c, tc.reason)
		})
	}
	t.Run("gzip_bomb_header", func(t *testing.T) {
		file, c := extractArchiveFixture(t, "tar.gz", []extractFixtureEntry{{"payload", []byte("tiny"), 0644, tar.TypeReg}})
		raw := extractTarBytes(t, []extractFixtureEntry{{"payload", make([]byte, 1<<20), 0644, tar.TypeReg}})
		data := extractGzip(t, raw, true)
		if err := os.WriteFile(file, data, 0600); err != nil {
			t.Fatal("fixture write")
		}
		c.Archive.Size = uint64(len(data))
		c.Archive.SHA256 = extractDigest(data)
		extractAssertRefused(t, file, c, ExtractInventoryLimit)
	})
}

func TestExtractVerifiedArchiveBindingAndFreshStaging(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Component)
		want   error
	}{{"longer", func(c *Component) { c.Archive.Size-- }, ExtractArchiveSize}, {"shorter", func(c *Component) { c.Archive.Size++ }, ExtractArchiveSize}, {"digest", func(c *Component) { c.Archive.SHA256 = strings.Repeat("0", 64) }, ExtractArchiveDigest}, {"inventory_sum", func(c *Component) { c.Inventory[0].Size = c.Archive.Size + 1 }, ExtractInventoryLimit}} {
		t.Run(tc.name, func(t *testing.T) {
			file, c := extractArchiveFixture(t, "tar.gz", []extractFixtureEntry{{"payload", []byte("fixture"), 0644, tar.TypeReg}})
			tc.mutate(&c)
			extractAssertRefused(t, file, c, tc.want)
		})
	}
	t.Run("existing_directory_retained", func(t *testing.T) {
		file, c := extractArchiveFixture(t, "tar.gz", []extractFixtureEntry{{"payload", []byte("fixture"), 0644, tar.TypeReg}})
		stage := t.TempDir()
		sentinel := filepath.Join(stage, "sentinel")
		if err := os.WriteFile(sentinel, []byte("preserve"), 0600); err != nil {
			t.Fatal("sentinel write")
		}
		if err := ExtractVerified(context.Background(), file, c, stage); err != ExtractStagingNotFresh {
			t.Fatal("existing staging accepted")
		}
		if _, err := os.Stat(sentinel); err != nil {
			t.Fatal("unowned staging removed")
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		file, c := extractArchiveFixture(t, "zip", []extractFixtureEntry{{"payload", []byte("fixture"), 0644, tar.TypeReg}})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		stage := filepath.Join(t.TempDir(), "stage")
		if err := ExtractVerified(ctx, file, c, stage); err != ExtractCancelled {
			t.Fatal("cancellation ignored")
		}
		if _, err := os.Stat(stage); !os.IsNotExist(err) {
			t.Fatal("cancelled staging retained")
		}
	})
}

func TestRecheckInventoryTampering(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*testing.T, string)
	}{
		{"byte_change", func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "bin/payload"), []byte("changed"), 0644); err != nil {
				t.Fatal("tamper write")
			}
		}},
		{"mode_change", func(t *testing.T, dir string) {
			mode := os.FileMode(0755)
			if runtime.GOOS == "windows" {
				mode = 0444
			}
			if err := os.Chmod(filepath.Join(dir, "bin/payload"), mode); err != nil {
				t.Fatal("tamper chmod")
			}
		}},
		{"extra_file", func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "extra"), nil, 0644); err != nil {
				t.Fatal("extra write")
			}
		}},
		{"extra_directory", func(t *testing.T, dir string) {
			if err := os.Mkdir(filepath.Join(dir, "extra"), 0700); err != nil {
				t.Fatal("extra mkdir")
			}
		}},
		{"missing_file", func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, "bin/payload")); err != nil {
				t.Fatal("remove payload")
			}
		}},
		{"symlink_swap", func(t *testing.T, dir string) {
			name := filepath.Join(dir, "bin/payload")
			outside := filepath.Join(t.TempDir(), "outside")
			if err := os.WriteFile(outside, []byte("fixture"), 0644); err != nil {
				t.Fatal("outside write")
			}
			if err := os.Remove(name); err != nil {
				t.Fatal("remove payload")
			}
			if err := os.Symlink(outside, name); err != nil {
				t.Fatal("symlink creation unavailable")
			}
		}},
		{"ancestor_symlink", func(t *testing.T, dir string) {
			if err := os.Rename(filepath.Join(dir, "bin"), filepath.Join(dir, "original")); err != nil {
				t.Fatal("move bin")
			}
			if err := os.Symlink(filepath.Join(dir, "original"), filepath.Join(dir, "bin")); err != nil {
				t.Fatal("symlink creation unavailable")
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file, c := extractArchiveFixture(t, "tar.gz", []extractFixtureEntry{{"bin/payload", []byte("fixture"), 0644, tar.TypeReg}})
			stage := filepath.Join(t.TempDir(), "stage")
			if err := ExtractVerified(context.Background(), file, c, stage); err != nil {
				t.Fatalf("extract: %s", err)
			}
			tc.change(t, stage)
			if err := RecheckInventory(stage, c); err == nil {
				t.Fatal("tampering accepted")
			}
		})
	}
}

func TestBoundedEntryReaderDoesNotWriteOverflow(t *testing.T) {
	for _, size := range []int{0, 1, 4, 32} {
		reader := bytes.NewReader(bytes.Repeat([]byte{'x'}, 100))
		var written bytes.Buffer
		err := consumeEntry(context.Background(), reader, uint64(size), &written)
		if err != ExtractEntrySize || written.Len() != size || reader.Len() != 100-size-1 {
			t.Fatal("reader exceeded size-plus-one or wrote overflow")
		}
	}
	var written bytes.Buffer
	if err := consumeEntry(context.Background(), strings.NewReader("x"), 2, &written); err != ExtractEntrySize {
		t.Fatal("short body accepted")
	}
}

func TestInspectArchiveUsesExtractionBoundary(t *testing.T) {
	for _, format := range []string{"tar.gz", "zip"} {
		t.Run(format, func(t *testing.T) {
			file, _ := extractArchiveFixture(t, format, []extractFixtureEntry{{"../outside", []byte("fixture"), 0644, tar.TypeReg}})
			if _, _, err := InspectArchive(context.Background(), file, format); err != ExtractUnsafePath {
				t.Fatal("inspection accepted unsafe path")
			}
		})
	}
	file, _ := extractArchiveFixture(t, "tar.gz", []extractFixtureEntry{{"payload", bytes.Repeat([]byte{'z'}, 10000), 0644, tar.TypeReg}})
	raw := extractTarBytes(t, []extractFixtureEntry{{"payload", bytes.Repeat([]byte{'z'}, 10000), 0644, tar.TypeReg}})
	if err := os.WriteFile(file, extractGzip(t, raw, true), 0600); err != nil {
		t.Fatal("compressed archive write")
	}
	if _, _, err := InspectArchive(context.Background(), file, "tar.gz"); err != ExtractInventoryLimit {
		t.Fatal("inspection accepted expanded-size excess")
	}
}

func TestUnixNoFollowInventoryOpen(t *testing.T) {
	// This also exercises the documented Lstat rejection on Windows.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "target"), nil, 0644); err != nil {
		t.Fatal("target write")
	}
	if err := os.Symlink("target", filepath.Join(dir, "link")); err != nil {
		t.Fatal("symlink creation unavailable")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal("open root")
	}
	defer func() { _ = root.Close() }()
	file, err := openInventoryFile(root, "link")
	if err == nil {
		_ = file.Close()
		t.Fatal("followed inventory symlink")
	}
}

func TestZIPDirectoryPreflightBounds(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func([]byte, int, int)
	}{
		{"entry_count", func(data []byte, end, _ int) {
			binary.LittleEndian.PutUint16(data[end+10:], MaxInventoryEntries+1)
		}},
		{"central_size", func(data []byte, end, _ int) { binary.LittleEndian.PutUint32(data[end+12:], 8<<20+1) }},
		{"zip64", func(data []byte, _, central int) { binary.LittleEndian.PutUint32(data[central+24:], 0xffffffff) }},
		{"multidisk", func(data []byte, end, _ int) { binary.LittleEndian.PutUint16(data[end+4:], 1) }},
		{"directory_count_mismatch", func(data []byte, end, _ int) { binary.LittleEndian.PutUint16(data[end+8:], 0) }},
		{"filename_bound", func(data []byte, _, central int) { binary.LittleEndian.PutUint16(data[central+28:], 1025) }},
		{"extra_bound", func(data []byte, _, central int) { binary.LittleEndian.PutUint16(data[central+30:], 4097) }},
		{"comment_bound", func(data []byte, _, central int) { binary.LittleEndian.PutUint16(data[central+32:], 4097) }},
		{"directory_gap", func(data []byte, end, central int) { binary.LittleEndian.PutUint32(data[end+16:], uint32(central-1)) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file, c := extractArchiveFixture(t, "zip", []extractFixtureEntry{{"payload", []byte("fixture"), 0644, tar.TypeReg}})
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal("read fixture")
			}
			end := len(data) - 22
			central := int(binary.LittleEndian.Uint32(data[end+16:]))
			tc.change(data, end, central)
			if err = os.WriteFile(file, data, 0600); err != nil {
				t.Fatal("write fixture")
			}
			c.Archive.SHA256 = extractDigest(data)
			extractAssertRefused(t, file, c, nil)
			if _, _, err = InspectArchive(context.Background(), file, "zip"); err == nil {
				t.Fatal("inspection accepted malformed metadata")
			}
		})
	}
}

func TestExtractVerifiedValidExtendedTarHeadersRefused(t *testing.T) {
	for _, format := range []tar.Format{tar.FormatPAX, tar.FormatGNU} {
		t.Run(format.String(), func(t *testing.T) {
			var raw bytes.Buffer
			writer := tar.NewWriter(&raw)
			header := &tar.Header{Name: "payload", Mode: 0644, Size: 7, Format: format}
			if format == tar.FormatPAX {
				header.PAXRecords = map[string]string{"comment": "extended fixture"}
			} else {
				header.Name = strings.Repeat("x", 120)
			}
			if err := writer.WriteHeader(header); err != nil {
				t.Fatal("extended header write")
			}
			if _, err := writer.Write([]byte("fixture")); err != nil {
				t.Fatal("extended body write")
			}
			if err := writer.Close(); err != nil {
				t.Fatal("extended writer close")
			}
			if raw.Bytes()[156] != tar.TypeXHeader && raw.Bytes()[156] != tar.TypeGNULongName {
				t.Fatal("fixture did not encode an extension header")
			}
			file, c := extractArchiveFixture(t, "tar.gz", []extractFixtureEntry{{"payload", []byte("fixture"), 0644, tar.TypeReg}})
			data := extractGzip(t, raw.Bytes(), false)
			if err := os.WriteFile(file, data, 0600); err != nil {
				t.Fatal("extended fixture write")
			}
			c.Archive.Size = uint64(len(data))
			c.Archive.SHA256 = extractDigest(data)
			extractAssertRefused(t, file, c, ExtractEntryType)
		})
	}
}

func TestArchiveSecondPassUsesDeclaredByteBoundary(t *testing.T) {
	for _, optionalFlag := range []byte{8, 16} {
		t.Run(fmt.Sprintf("gzip_flag_%d", optionalFlag), func(t *testing.T) {
			full := extractGzip(t, extractTarBytes(t, []extractFixtureEntry{{"payload", []byte("fixture"), 0644, tar.TypeReg}}), false)
			header := append([]byte(nil), full[:10]...)
			header[3] = optionalFlag
			name := filepath.Join(t.TempDir(), "archive.tar.gz")
			if err := os.WriteFile(name, header, 0600); err != nil {
				t.Fatal("write authenticated prefix")
			}
			file, err := os.OpenFile(name, os.O_RDWR, 0600)
			if err != nil {
				t.Fatal("open archive")
			}
			defer func() { _ = file.Close() }()
			archive := Archive{Size: uint64(len(header)), SHA256: extractDigest(header), Format: "tar.gz"}
			if err = verifyArchiveBytes(context.Background(), file, archive); err != nil {
				t.Fatal("prefix byte verification failed")
			}
			suffix := append([]byte("appended optional header\x00"), full[10:]...)
			if _, err = file.WriteAt(suffix, int64(len(header))); err != nil {
				t.Fatal("append archive after verification")
			}
			count := 0
			visit := func(_ string, size uint64, _ os.FileMode, reader io.Reader) error {
				count++
				return consumeEntry(context.Background(), reader, size, io.Discard)
			}
			// The appended bytes form a valid gzip/tar stream. An unbounded pass would
			// accept it, despite all but ten bytes being outside the authenticated range.
			if _, err = file.Seek(0, io.SeekStart); err != nil {
				t.Fatal("rewind control")
			}
			if err = walkTar(context.Background(), file, 7, visit); err != nil || count != 1 {
				t.Fatal("unbounded control fixture was not valid")
			}
			count = 0
			if err = walkArchive(context.Background(), file, archive, 7, visit); err != ExtractArchiveInvalid || count != 0 {
				t.Fatal("second pass consumed appended unauthenticated bytes")
			}
		})
	}
}
