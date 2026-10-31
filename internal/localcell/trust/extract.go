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
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Extraction reasons are fixed codes and never contain archive paths or bytes.
const (
	ExtractInvalidInventory   Reason = "invalid_inventory"
	ExtractUnsafePath         Reason = "unsafe_path"
	ExtractUnsupportedArchive Reason = "unsupported_archive"
	ExtractArchiveSize        Reason = "archive_size_mismatch"
	ExtractArchiveDigest      Reason = "archive_digest_mismatch"
	ExtractArchiveInvalid     Reason = "archive_invalid"
	ExtractEntryType          Reason = "archive_entry_type"
	ExtractDuplicate          Reason = "archive_duplicate_entry"
	ExtractUndeclared         Reason = "archive_undeclared_entry"
	ExtractMissing            Reason = "archive_missing_entry"
	ExtractEntrySize          Reason = "entry_size_mismatch"
	ExtractEntryDigest        Reason = "entry_digest_mismatch"
	ExtractEntryMode          Reason = "entry_mode_mismatch"
	ExtractInventoryLimit     Reason = "inventory_limit"
	ExtractStagingNotFresh    Reason = "staging_not_fresh"
	ExtractFilesystem         Reason = "filesystem_error"
	ExtractCancelled          Reason = "cancelled"
)

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if r.ctx.Err() != nil {
		return 0, ExtractCancelled
	}
	return r.r.Read(p)
}

func extractionError(ctx context.Context, fallback Reason) error {
	if ctx.Err() != nil {
		return ExtractCancelled
	}
	return fallback
}
func validDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
func entryMode(s string) (os.FileMode, bool) {
	switch s {
	case "0644":
		return 0644, true
	case "0755":
		return 0755, true
	default:
		return 0, false
	}
}

func inventoryMap(component Component) (map[string]InventoryEntry, uint64, error) {
	if len(component.Inventory) == 0 || len(component.Inventory) > MaxInventoryEntries {
		return nil, 0, ExtractInvalidInventory
	}
	if component.Archive.Size > MaxArchiveBytes || component.Archive.Size == 0 {
		return nil, 0, ExtractArchiveSize
	}
	if component.Archive.Format != "tar.gz" && component.Archive.Format != "zip" {
		return nil, 0, ExtractUnsupportedArchive
	}
	if !validDigest(component.Archive.SHA256) {
		return nil, 0, ExtractArchiveDigest
	}
	entries := make(map[string]InventoryEntry, len(component.Inventory))
	var total uint64
	for _, entry := range component.Inventory {
		if !ValidPath(entry.Path) {
			return nil, 0, ExtractUnsafePath
		}
		mode, ok := entryMode(entry.Mode)
		if !ok || entry.Executable != (mode == 0755) {
			return nil, 0, ExtractEntryMode
		}
		if !validDigest(entry.SHA256) {
			return nil, 0, ExtractEntryDigest
		}
		if _, ok := entries[entry.Path]; ok {
			return nil, 0, ExtractDuplicate
		}
		if entry.Size > component.Archive.Size-total {
			return nil, 0, ExtractInventoryLimit
		}
		total += entry.Size
		entries[entry.Path] = entry
	}
	// A declared regular file cannot also serve as a directory for another file.
	for name := range entries {
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if _, ok := entries[parent]; ok {
				return nil, 0, ExtractInvalidInventory
			}
		}
	}
	return entries, total, nil
}

// ExtractVerified verifies the archive bytes before parsing entries. stagingDir
// must not exist; only a directory created by this call is removed on failure.
func ExtractVerified(ctx context.Context, archivePath string, component Component, stagingDir string) (result error) {
	entries, total, err := inventoryMap(component)
	if err != nil {
		return err
	}
	if err = os.Mkdir(stagingDir, 0700); err != nil {
		return ExtractStagingNotFresh
	}
	defer func() {
		if result != nil {
			_ = os.RemoveAll(stagingDir)
		}
	}()
	if err = os.Chmod(stagingDir, 0700); err != nil {
		return ExtractFilesystem
	}
	root, err := os.OpenRoot(stagingDir)
	if err != nil {
		return ExtractFilesystem
	}
	defer func() { _ = root.Close() }()
	file, err := openArchiveFile(archivePath)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	if err = verifyArchiveBytes(ctx, file, component.Archive); err != nil {
		return err
	}
	seen := make(map[string]bool, len(entries))
	visitor := func(name string, size uint64, mode os.FileMode, reader io.Reader) error {
		expected, ok := entries[name]
		if !ok {
			return ExtractUndeclared
		}
		if seen[name] {
			return ExtractDuplicate
		}
		seen[name] = true
		expectedMode, _ := entryMode(expected.Mode)
		if size != expected.Size {
			return ExtractEntrySize
		}
		if mode != expectedMode {
			return ExtractEntryMode
		}
		parent := path.Dir(name)
		if parent != "." {
			if err := root.MkdirAll(parent, 0700); err != nil {
				return ExtractFilesystem
			}
			for p := parent; p != "."; p = path.Dir(p) {
				if err := root.Chmod(p, 0700); err != nil {
					return ExtractFilesystem
				}
			}
		}
		output, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return ExtractFilesystem
		}
		sum := sha256.New()
		copyErr := consumeEntry(ctx, reader, size, io.MultiWriter(output, sum))
		modeErr := output.Chmod(expectedMode)
		closeErr := output.Close()
		if copyErr != nil {
			return copyErr
		}

		if hex.EncodeToString(sum.Sum(nil)) != expected.SHA256 {
			return ExtractEntryDigest
		}
		if modeErr != nil || closeErr != nil {
			return ExtractFilesystem
		}
		return nil
	}
	if err = walkArchive(ctx, file, component.Archive, total, visitor); err != nil {
		return err
	}
	if len(seen) != len(entries) {
		return ExtractMissing
	}
	return nil
}

func verifyArchiveBytes(ctx context.Context, file *os.File, archive Archive) error {
	if archive.Size == 0 || archive.Size > MaxArchiveBytes {
		return ExtractArchiveSize
	}
	info, err := file.Stat()
	if err != nil {
		return ExtractFilesystem
	}
	if !info.Mode().IsRegular() {
		return ExtractEntryType
	}
	if info.Size() != int64(archive.Size) {
		return ExtractArchiveSize
	}
	sum := sha256.New()
	n, err := io.Copy(sum, io.LimitReader(contextReader{ctx, file}, int64(archive.Size)+1))
	if err != nil {
		return extractionError(ctx, ExtractFilesystem)
	}
	if n != int64(archive.Size) {
		return ExtractArchiveSize
	}
	if hex.EncodeToString(sum.Sum(nil)) != archive.SHA256 {
		return ExtractArchiveDigest
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return ExtractFilesystem
	}
	return nil
}

type archiveVisitor func(name string, size uint64, mode os.FileMode, reader io.Reader) error

func walkArchive(ctx context.Context, file *os.File, archive Archive, total uint64, visit archiveVisitor) error {
	seen := map[string]bool{}
	var count int
	var consumed uint64
	checked := func(name string, size uint64, mode os.FileMode, reader io.Reader) error {
		if ctx.Err() != nil {
			return ExtractCancelled
		}
		if !ValidPath(name) {
			return ExtractUnsafePath
		}
		if seen[name] {
			return ExtractDuplicate
		}
		seen[name] = true
		count++
		if count > MaxInventoryEntries {
			return ExtractInventoryLimit
		}
		if mode != 0644 && mode != 0755 {
			return ExtractEntryMode
		}
		if size > total-consumed {
			return ExtractInventoryLimit
		}
		consumed += size
		return visit(name, size, mode, reader)
	}
	authenticatedBytes := io.NewSectionReader(file, 0, int64(archive.Size))
	switch archive.Format {
	case "tar.gz":
		return walkTar(ctx, authenticatedBytes, total, checked)
	case "zip":
		return walkZip(ctx, authenticatedBytes, archive.Size, checked)
	default:
		return ExtractUnsupportedArchive
	}
}

func walkTar(ctx context.Context, file io.Reader, total uint64, visit archiveVisitor) error {
	compressed, err := gzip.NewReader(contextReader{ctx, file})
	if err != nil {
		return extractionError(ctx, ExtractArchiveInvalid)
	}
	defer func() { _ = compressed.Close() }()
	// USTAR permits at most 511 bytes of padding per file. This includes bounded
	// header overhead and at most 10 KiB of conventional terminal zero padding.
	bounded := &io.LimitedReader{R: contextReader{ctx, compressed}, N: int64(total) + MaxInventoryEntries*1024 + 10240}
	block := make([]byte, 512)
	for {
		if _, err = io.ReadFull(bounded, block); err != nil {
			return extractionError(ctx, ExtractArchiveInvalid)
		}
		if allZero(block) {
			if _, err = io.ReadFull(bounded, block); err != nil || !allZero(block) {
				return extractionError(ctx, ExtractArchiveInvalid)
			}
			// Consume the gzip trailer and reject nonzero or excessive trailing bytes.
			tail, err := io.ReadAll(io.LimitReader(bounded, 10240-1024+1))
			if err != nil {
				return extractionError(ctx, ExtractArchiveInvalid)
			}
			if len(tail) > 10240-1024 || len(tail)%512 != 0 || !allZero(tail) {
				return ExtractArchiveInvalid
			}
			var probe [1]byte
			n, err := bounded.Read(probe[:])
			if n != 0 || err != io.EOF {
				return extractionError(ctx, ExtractArchiveInvalid)
			}
			return nil
		}
		// Inspect the raw type before archive/tar can silently consume PAX or GNU
		// extended headers, including sparse and long-link records.
		if block[156] != tar.TypeReg && block[156] != 0 {
			return ExtractEntryType
		}
		header, err := tar.NewReader(bytes.NewReader(block)).Next()
		if err != nil {
			return ExtractArchiveInvalid
		}
		if header.Size < 0 || header.Linkname != "" || header.Mode != 0644 && header.Mode != 0755 {
			return ExtractEntryMode
		}
		if uint64(header.Size) > total {
			return ExtractInventoryLimit
		}
		body := &io.LimitedReader{R: bounded, N: header.Size}
		if err = visit(header.Name, uint64(header.Size), os.FileMode(header.Mode), body); err != nil {
			return err
		}
		if body.N != 0 {
			return ExtractEntrySize
		}
		padding := (512 - header.Size%512) % 512
		if _, err = io.ReadFull(bounded, block[:padding]); err != nil {
			return extractionError(ctx, ExtractArchiveInvalid)
		}
		if !allZero(block[:padding]) {
			return ExtractArchiveInvalid
		}
	}
}
func allZero(data []byte) bool {
	for _, b := range data {
		if b != 0 {
			return false
		}
	}
	return true
}

func walkZip(ctx context.Context, file io.ReaderAt, size uint64, visit archiveVisitor) error {
	if err := checkZipDirectory(file, size); err != nil {
		return err
	}
	archive, err := zip.NewReader(file, int64(size))
	if err != nil {
		return ExtractArchiveInvalid
	}
	if len(archive.File) > MaxInventoryEntries {
		return ExtractInventoryLimit
	}
	for _, entry := range archive.File {
		if ctx.Err() != nil {
			return ExtractCancelled
		}
		mode := entry.Mode()
		if !mode.IsRegular() || entry.Flags&1 != 0 || strings.HasSuffix(entry.Name, "/") {
			return ExtractEntryType
		}
		if mode&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
			return ExtractEntryMode
		}
		reader, err := entry.Open()
		if err != nil {
			return ExtractArchiveInvalid
		}
		readErr := visit(entry.Name, entry.UncompressedSize64, mode.Perm(), reader)
		closeErr := reader.Close()
		if readErr != nil {
			return readErr
		}
		if closeErr != nil {
			return ExtractArchiveInvalid
		}
	}
	return nil
}

// InspectArchive computes inventory through the same bounded archive reader as
// extraction. As specified by v1, total unpacked bytes cannot exceed archive size.
func InspectArchive(ctx context.Context, archivePath, format string) (Archive, []InventoryEntry, error) {
	archive := Archive{Name: filepath.Base(archivePath), Format: format}
	file, err := openArchiveFile(archivePath)
	if err != nil {
		return archive, nil, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return archive, nil, ExtractFilesystem
	}
	if info.Size() <= 0 || info.Size() > MaxArchiveBytes {
		return archive, nil, ExtractArchiveSize
	}
	archive.Size = uint64(info.Size())
	sum := sha256.New()
	n, err := io.Copy(sum, io.LimitReader(contextReader{ctx, file}, int64(archive.Size)+1))
	if err != nil {
		return archive, nil, extractionError(ctx, ExtractFilesystem)
	}
	if n != int64(archive.Size) {
		return archive, nil, ExtractArchiveSize
	}
	archive.SHA256 = hex.EncodeToString(sum.Sum(nil))
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return archive, nil, ExtractFilesystem
	}
	var inventory []InventoryEntry
	err = walkArchive(ctx, file, archive, archive.Size, func(name string, size uint64, mode os.FileMode, reader io.Reader) error {
		digest := sha256.New()
		if err := consumeEntry(ctx, reader, size, digest); err != nil {
			return err
		}
		modeText := "0644"
		if mode == 0755 {
			modeText = "0755"
		}
		inventory = append(inventory, InventoryEntry{Path: name, Size: size, SHA256: hex.EncodeToString(digest.Sum(nil)), Mode: modeText, Executable: mode == 0755})
		return nil
	})
	if err != nil {
		return archive, nil, err
	}
	sort.Slice(inventory, func(i, j int) bool { return inventory[i].Path < inventory[j].Path })
	if _, _, err = inventoryMap(Component{Archive: archive, Inventory: inventory}); err != nil {
		return archive, nil, err
	}
	return archive, inventory, nil
}

// RecheckInventory rejects missing/extra entries, links and altered file bytes or
// permissions immediately before the caller activates a verified component.
func RecheckInventory(dir string, component Component) error {
	entries, _, err := inventoryMap(component)
	if err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || !inventoryInfoSafe(info) {
		return ExtractFilesystem
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return ExtractFilesystem
	}
	defer func() { _ = root.Close() }()
	allowedDirs := map[string]bool{".": true}
	for name := range entries {
		for p := path.Dir(name); p != "."; p = path.Dir(p) {
			allowedDirs[p] = true
		}
	}
	seen := map[string]bool{}
	err = fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return ExtractFilesystem
		}
		entryInfo, infoErr := entry.Info()
		if infoErr != nil || !inventoryInfoSafe(entryInfo) {
			return ExtractEntryType
		}
		if entry.IsDir() {
			if !allowedDirs[name] {
				return ExtractUndeclared
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return ExtractEntryType
		}
		expected, ok := entries[name]
		if !ok {
			return ExtractUndeclared
		}
		// Refuse links in every ancestor even if a link resolves inside this root.
		for p := path.Dir(name); p != "."; p = path.Dir(p) {
			parent, err := root.Lstat(p)
			if err != nil || !parent.IsDir() || !inventoryInfoSafe(parent) {
				return ExtractEntryType
			}
		}
		file, err := openInventoryFile(root, name)
		if err != nil {
			return ExtractEntryType
		}
		stat, err := file.Stat()
		if err != nil {
			_ = file.Close()
			return ExtractFilesystem
		}
		expectedMode, _ := entryMode(expected.Mode)
		if !inventoryModeMatches(stat.Mode(), expectedMode) {
			_ = file.Close()
			return ExtractEntryMode
		}
		if stat.Size() != int64(expected.Size) {
			_ = file.Close()
			return ExtractEntrySize
		}
		digest := sha256.New()
		n, readErr := io.Copy(digest, io.LimitReader(file, int64(expected.Size)+1))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil {
			return ExtractFilesystem
		}
		if n != int64(expected.Size) {
			return ExtractEntrySize
		}
		if hex.EncodeToString(digest.Sum(nil)) != expected.SHA256 {
			return ExtractEntryDigest
		}
		seen[name] = true
		return nil
	})
	if err != nil {
		return err
	}
	if len(seen) != len(entries) {
		return ExtractMissing
	}
	return nil
}

// Probe the size-plus-one byte without ever writing beyond the declared size.
func consumeEntry(ctx context.Context, reader io.Reader, size uint64, writer io.Writer) error {
	bounded := contextReader{ctx, reader}
	n, err := io.Copy(writer, io.LimitReader(bounded, int64(size)))
	if err != nil {
		return extractionError(ctx, ExtractArchiveInvalid)
	}
	if n != int64(size) {
		return ExtractEntrySize
	}
	var extra [1]byte
	nExtra, err := bounded.Read(extra[:])
	if nExtra != 0 {
		return ExtractEntrySize
	}
	if err != io.EOF {
		return extractionError(ctx, ExtractArchiveInvalid)
	}
	return nil
}

// Bound the central directory before archive/zip allocates its entry slice.
// ZIP64 and multidisk archives are outside this v1 extraction profile.
func checkZipDirectory(file io.ReaderAt, size uint64) error {
	const maxDirectory = 8 << 20
	if size < 22 {
		return ExtractArchiveInvalid
	}
	tailSize := min(size, 65535+22)
	tail := make([]byte, int(tailSize))
	if _, err := file.ReadAt(tail, int64(size-tailSize)); err != nil {
		return ExtractArchiveInvalid
	}
	offset := -1
	for i := len(tail) - 22; i >= 0; i-- {
		if binary.LittleEndian.Uint32(tail[i:]) == 0x06054b50 && i+22+int(binary.LittleEndian.Uint16(tail[i+20:])) == len(tail) {
			offset = i
			break
		}
	}
	if offset < 0 {
		return ExtractArchiveInvalid
	}
	end := tail[offset:]
	count := uint64(binary.LittleEndian.Uint16(end[10:]))
	directorySize := uint64(binary.LittleEndian.Uint32(end[12:]))
	directoryOffset := uint64(binary.LittleEndian.Uint32(end[16:]))
	if count > MaxInventoryEntries || directorySize > maxDirectory {
		return ExtractInventoryLimit
	}
	if count == 0 || binary.LittleEndian.Uint16(end[4:]) != 0 || binary.LittleEndian.Uint16(end[6:]) != 0 || uint64(binary.LittleEndian.Uint16(end[8:])) != count || binary.LittleEndian.Uint16(end[20:]) > 4096 {
		return ExtractArchiveInvalid
	}
	endOffset := size - tailSize + uint64(offset)
	if directoryOffset > endOffset || directorySize != endOffset-directoryOffset {
		return ExtractArchiveInvalid
	}
	cursor := directoryOffset
	header := make([]byte, 46)
	for range count {
		if cursor+46 > endOffset {
			return ExtractArchiveInvalid
		}
		if _, err := file.ReadAt(header, int64(cursor)); err != nil || binary.LittleEndian.Uint32(header) != 0x02014b50 {
			return ExtractArchiveInvalid
		}
		nameSize := uint64(binary.LittleEndian.Uint16(header[28:]))
		extraSize := uint64(binary.LittleEndian.Uint16(header[30:]))
		commentSize := uint64(binary.LittleEndian.Uint16(header[32:]))
		if nameSize == 0 || nameSize > 1024 || extraSize > 4096 || commentSize > 4096 {
			return ExtractInventoryLimit
		}
		if binary.LittleEndian.Uint32(header[20:]) == 0xffffffff || binary.LittleEndian.Uint32(header[24:]) == 0xffffffff || binary.LittleEndian.Uint16(header[34:]) != 0 || binary.LittleEndian.Uint32(header[42:]) == 0xffffffff {
			return ExtractArchiveInvalid
		}
		cursor += 46 + nameSize + extraSize + commentSize
		if cursor > endOffset {
			return ExtractArchiveInvalid
		}
		extra := make([]byte, int(extraSize))
		if _, err := file.ReadAt(extra, int64(cursor-commentSize-extraSize)); err != nil {
			return ExtractArchiveInvalid
		}
		for len(extra) > 0 {
			if len(extra) < 4 {
				return ExtractArchiveInvalid
			}
			tag := binary.LittleEndian.Uint16(extra)
			length := int(binary.LittleEndian.Uint16(extra[2:]))
			if tag == 1 || length > len(extra)-4 {
				return ExtractArchiveInvalid
			}
			extra = extra[4+length:]
		}
	}
	if cursor != endOffset {
		return ExtractArchiveInvalid
	}
	return nil
}
