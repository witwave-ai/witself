//go:build windows

package trust

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unicode/utf16"
	"unsafe"
)

// Hooks observe actual native objects. They are per-adapter, unexported and nil
// in production; metadata can replace one observation for bounded race tests.
type windowsRecordHooks struct {
	before   func(string) error
	observe  func(string, syscall.Handle, privateRecordPolicy) error
	metadata func(string, *syscall.ByHandleFileInformation)
}
type windowsRecordGuard struct {
	path     string
	handle   syscall.Handle
	identity [3]uint64
}
type windowsRecordOps struct {
	dir    string
	guards []windowsRecordGuard
	hooks  *windowsRecordHooks
}
type windowsRecordFile struct {
	file           *os.File
	path           string
	identity       [3]uint64
	created        bool
	published      bool
	checked        bool
	expectedTarget *privateRecordInfo
}

func (f *windowsRecordFile) Read(b []byte) (int, error)  { return f.file.Read(b) }
func (f *windowsRecordFile) Write(b []byte) (int, error) { return f.file.Write(b) }
func (f *windowsRecordFile) Sync() error                 { return f.file.Sync() }
func (f *windowsRecordFile) Close() error                { return f.file.Close() }

// Native constants below retain their Win32/NT names in comments. The syscall
// package supplies the ordinary file and token operations; these bindings cover
// only the documented entry points and layouts it does not expose.
const (
	windowsRecordReadAttributes      = 0x0080     // FILE_READ_ATTRIBUTES
	windowsRecordReadControl         = 0x00020000 // READ_CONTROL
	windowsRecordTraverse            = 0x0020     // FILE_TRAVERSE
	windowsRecordDelete              = 0x00010000 // DELETE
	windowsRecordDriveFixed          = 3          // DRIVE_FIXED
	windowsRecordOwnerSecurity       = 0x00000001 // OWNER_SECURITY_INFORMATION
	windowsRecordDACLSecurity        = 0x00000004 // DACL_SECURITY_INFORMATION
	windowsRecordFileDispositionInfo = 4          // FileDispositionInfo
	windowsRecordFileRenameInfoEx    = 22         // FileRenameInfoEx
	windowsRecordCaseInsensitive     = 0x00000040 // OBJ_CASE_INSENSITIVE
	windowsRecordDontReparse         = 0x00001000 // OBJ_DONT_REPARSE
	windowsRecordDirectoryFile       = 0x00000001 // FILE_DIRECTORY_FILE
	windowsRecordSynchronousIO       = 0x00000020 // FILE_SYNCHRONOUS_IO_NONALERT
	windowsRecordNonDirectoryFile    = 0x00000040 // FILE_NON_DIRECTORY_FILE
	windowsRecordOpenReparsePoint    = 0x00200000 // FILE_OPEN_REPARSE_POINT
	windowsRecordFileCreate          = 2          // FILE_CREATE
)

// These are Windows KnownDLLs; only documented system exports are resolved.
var (
	windowsRecordKernel32          = syscall.NewLazyDLL("kernel32.dll")
	windowsRecordAdvapi32          = syscall.NewLazyDLL("advapi32.dll")
	windowsRecordNTDLL             = syscall.NewLazyDLL("ntdll.dll")
	windowsRecordGetSecurity       = windowsRecordAdvapi32.NewProc("GetKernelObjectSecurity")
	windowsRecordConvertDescriptor = windowsRecordAdvapi32.NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW")
	windowsRecordValidSID          = windowsRecordAdvapi32.NewProc("IsValidSid")
	windowsRecordSetInformation    = windowsRecordKernel32.NewProc("SetFileInformationByHandle")
	windowsRecordVolumeInformation = windowsRecordKernel32.NewProc("GetVolumeInformationByHandleW")
	windowsRecordDriveType         = windowsRecordKernel32.NewProc("GetDriveTypeW")
	windowsRecordNTCreate          = windowsRecordNTDLL.NewProc("NtCreateFile")
	windowsRecordNTError           = windowsRecordNTDLL.NewProc("RtlNtStatusToDosError")
)

// UNICODE_STRING, OBJECT_ATTRIBUTES and IO_STATUS_BLOCK follow their native
// pointer alignment on both 32-bit and 64-bit Windows.
type windowsRecordUnicodeString struct {
	Length        uint16
	MaximumLength uint16
	Buffer        *uint16
}
type windowsRecordObjectAttributes struct {
	Length                   uint32
	RootDirectory            syscall.Handle
	ObjectName               *windowsRecordUnicodeString
	Attributes               uint32
	SecurityDescriptor       *byte
	SecurityQualityOfService unsafe.Pointer
}
type windowsRecordIOStatusBlock struct {
	// Native union of NTSTATUS and a pointer; pointer width preserves alignment.
	Status      uintptr
	Information uintptr
}

func windowsRecordFailure(err error) error {
	if errors.Is(err, syscall.ERROR_FILE_NOT_FOUND) || errors.Is(err, syscall.ERROR_PATH_NOT_FOUND) {
		return os.ErrNotExist
	}
	return ReasonStateIO
}
func windowsRecordPrincipals() (*syscall.SID, *syscall.SID, error) {
	token, err := syscall.OpenCurrentProcessToken()
	if err != nil {
		return nil, nil, ReasonStateIO
	}
	user, userErr := token.GetTokenUser()
	closeErr := token.Close()
	if userErr != nil || closeErr != nil || user == nil || user.User.Sid == nil || windowsRecordValidSID.Find() != nil {
		return nil, nil, ReasonStateIO
	}
	valid, _, _ := windowsRecordValidSID.Call(uintptr(unsafe.Pointer(user.User.Sid)))
	if valid == 0 {
		return nil, nil, ReasonStateIO
	}
	// S-1-5-18 is the well-known LocalSystem SID, not an account lookup.
	system, err := syscall.StringToSid("S-1-5-18")
	if err != nil {
		return nil, nil, ReasonStateIO
	}
	return user.User.Sid, system, nil
}
func windowsRecordDescriptor() ([]byte, error) {
	user, system, err := windowsRecordPrincipals()
	if err != nil {
		return nil, err
	}
	userText, err := user.String()
	if err != nil {
		return nil, ReasonStateIO
	}
	text := "O:" + userText + "D:P(A;;FA;;;" + userText + ")"
	userBytes := unsafe.Slice((*byte)(unsafe.Pointer(user)), user.Len())
	systemBytes := unsafe.Slice((*byte)(unsafe.Pointer(system)), system.Len())
	if !bytes.Equal(userBytes, systemBytes) {
		text += "(A;;FA;;;SY)"
	}
	encoded, err := syscall.UTF16PtrFromString(text)
	if err != nil || windowsRecordConvertDescriptor.Find() != nil {
		return nil, ReasonStateIO
	}
	var native *byte
	var size uint32
	// SDDL_REVISION_1. The API owns this allocation until LocalFree; return a
	// Go-owned copy so no native allocation escapes this bounded operation.
	r, _, _ := windowsRecordConvertDescriptor.Call(uintptr(unsafe.Pointer(encoded)), 1, uintptr(unsafe.Pointer(&native)), uintptr(unsafe.Pointer(&size)))
	if native != nil {
		defer func() { _, _ = syscall.LocalFree(syscall.Handle(unsafe.Pointer(native))) }()
	}
	if r == 0 || native == nil || size < 20 || size > 65536 {
		return nil, ReasonStateIO
	}
	return append([]byte(nil), unsafe.Slice(native, int(size))...), nil
}

func windowsRecordSetFileInformation(handle syscall.Handle, class uint32, data *byte, size uint32) error {
	if windowsRecordSetInformation.Find() != nil {
		return ReasonStateIO
	}
	r, _, _ := windowsRecordSetInformation.Call(uintptr(handle), uintptr(class), uintptr(unsafe.Pointer(data)), uintptr(size))
	if r == 0 {
		return ReasonStateIO
	}
	return nil
}

// GetKernelObjectSecurity reports a bounded self-relative descriptor. Parsing
// offsets and lengths before forming any SID/ACE view avoids trusting embedded
// native pointers. The Windows allocation is not retained: this is a Go buffer.
func windowsRecordSecurity(handle syscall.Handle) ([]byte, error) {
	if windowsRecordGetSecurity.Find() != nil {
		return nil, ReasonStateIO
	}
	const wanted = windowsRecordOwnerSecurity | windowsRecordDACLSecurity
	var needed uint32
	r, _, callErr := windowsRecordGetSecurity.Call(uintptr(handle), uintptr(wanted), 0, 0, uintptr(unsafe.Pointer(&needed)))
	if r != 0 || !errors.Is(callErr, syscall.ERROR_INSUFFICIENT_BUFFER) || needed < 20 || needed > 65536 {
		return nil, ReasonStateIO
	}
	b := make([]byte, needed)
	r, _, _ = windowsRecordGetSecurity.Call(uintptr(handle), uintptr(wanted), uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)), uintptr(unsafe.Pointer(&needed)))
	if r == 0 || needed < 20 || int(needed) > len(b) {
		return nil, ReasonStateIO
	}
	return b[:needed], nil
}
func windowsRecordPolicyFromSecurity(b []byte) (privateRecordPolicy, error) {
	user, system, err := windowsRecordPrincipals()
	if err != nil {
		return privateRecordPolicy{}, err
	}
	userBytes := unsafe.Slice((*byte)(unsafe.Pointer(user)), user.Len())
	systemBytes := unsafe.Slice((*byte)(unsafe.Pointer(system)), system.Len())
	policy, err := privateRecordPolicyFromSecurity(b, userBytes, systemBytes)
	runtime.KeepAlive(user)
	runtime.KeepAlive(system)
	return policy, err
}
func windowsRecordIdentity(info syscall.ByHandleFileInformation) [3]uint64 {
	return [3]uint64{uint64(info.VolumeSerialNumber), uint64(info.FileIndexHigh), uint64(info.FileIndexLow)}
}
func windowsRecordProjection(handle syscall.Handle, directory bool) (privateRecordPolicy, privateRecordInfo, error) {
	var info syscall.ByHandleFileInformation
	if syscall.GetFileInformationByHandle(handle, &info) != nil {
		return privateRecordPolicy{}, privateRecordInfo{}, ReasonStateIO
	}
	b, err := windowsRecordSecurity(handle)
	if err != nil {
		return privateRecordPolicy{}, privateRecordInfo{}, err
	}
	p, err := windowsRecordPolicyFromSecurity(b)
	p.directory = info.FileAttributes&syscall.FILE_ATTRIBUTE_DIRECTORY != 0
	p.wantDirectory = directory
	p.reparse = info.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0
	p.identity = windowsRecordIdentity(info)
	p.observedIdentity = p.identity
	size := uint64(info.FileSizeHigh)<<32 | uint64(info.FileSizeLow)
	if size > uint64(^uint64(0)>>1) {
		return p, privateRecordInfo{}, ReasonStateIO
	}
	return p, privateRecordInfo{identity: p.identity, size: int64(size)}, err
}
func (o *windowsRecordOps) before(stage string) error {
	if o.hooks != nil && o.hooks.before != nil && o.hooks.before(stage) != nil {
		return ReasonStateIO
	}
	return nil
}
func (o *windowsRecordOps) project(stage string, h syscall.Handle, directory bool) (privateRecordInfo, error) {
	p, info, err := windowsRecordProjection(h, directory)
	if err != nil {
		return privateRecordInfo{}, ReasonStateIO
	}
	if o.hooks != nil && o.hooks.metadata != nil {
		var observed syscall.ByHandleFileInformation
		if syscall.GetFileInformationByHandle(h, &observed) != nil {
			return privateRecordInfo{}, ReasonStateIO
		}
		o.hooks.metadata("projection", &observed)
		p.reparse = observed.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0
		p.directory = observed.FileAttributes&syscall.FILE_ATTRIBUTE_DIRECTORY != 0
		p.observedIdentity = windowsRecordIdentity(observed)
	}
	if o.hooks != nil && o.hooks.observe != nil && o.hooks.observe(stage, h, p) != nil {
		return privateRecordInfo{}, ReasonStateIO
	}
	if checkPrivateRecordPolicy(p) != nil {
		return privateRecordInfo{}, ReasonStateIO
	}
	return info, nil
}
func windowsRecordCleanPath(path string) (string, error) {
	if len(path) < 4 {
		return "", ReasonStateIO
	}
	drive := path[0] | 0x20
	if path[1] != ':' || path[2] != '\\' || drive < 'a' || drive > 'z' || filepath.Clean(path) != path || len(utf16.Encode([]rune(path))) >= 260 {
		return "", ReasonStateIO
	}
	for _, part := range strings.Split(path[3:], `\`) {
		if !privateRecordName(part) || strings.TrimRight(part, ". ") != part || strings.ContainsAny(part, `<>"|?*`) {
			return "", ReasonStateIO
		}
		for _, c := range part {
			if c < 32 {
				return "", ReasonStateIO
			}
		}
		base, _, _ := strings.Cut(strings.ToUpper(part), ".")
		switch base {
		case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$", "CLOCK$":
			return "", ReasonStateIO
		}
		if strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT") {
			suffix := []rune(base[3:])
			if len(suffix) == 1 && strings.ContainsRune("123456789¹²³", suffix[0]) {
				return "", ReasonStateIO
			}
		}
	}
	return path, nil
}
func windowsRecordLeafName(name string) bool {
	if !privateRecordName(name) {
		return false
	}
	_, err := windowsRecordCleanPath(`C:\` + name)
	return err == nil
}
func windowsRecordOpenMetadata(path string, access, share uint32) (syscall.Handle, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return syscall.InvalidHandle, ReasonStateIO
	}
	h, err := syscall.CreateFile(p, access|windowsRecordReadAttributes|windowsRecordReadControl, share, nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_BACKUP_SEMANTICS|syscall.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return syscall.InvalidHandle, windowsRecordFailure(err)
	}
	return h, nil
}
func (o *windowsRecordOps) appendGuard(path string) error {
	h, err := windowsRecordOpenMetadata(path, windowsRecordTraverse, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE)
	if err != nil {
		return err
	}
	var info syscall.ByHandleFileInformation
	if syscall.GetFileInformationByHandle(h, &info) != nil || info.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0 || info.FileAttributes&syscall.FILE_ATTRIBUTE_DIRECTORY == 0 {
		_ = syscall.CloseHandle(h)
		return ReasonStateIO
	}
	o.guards = append(o.guards, windowsRecordGuard{path: path, handle: h, identity: windowsRecordIdentity(info)})
	return nil
}
func newPrivateRecordOps(dir string, _ bool) (privateRecordOps, error) {
	return newWindowsPrivateRecordOps(dir, false, nil)
}
func newWindowsPrivateRecordOps(dir string, create bool, hooks *windowsRecordHooks) (*windowsRecordOps, error) {
	clean, err := windowsRecordCleanPath(dir)
	if err != nil {
		return nil, err
	}
	o := &windowsRecordOps{dir: clean, hooks: hooks}
	success := false
	defer func() {
		if !success {
			_ = o.close()
		}
	}()
	root := clean[:3]
	ptr, _ := syscall.UTF16PtrFromString(root)
	if windowsRecordDriveType.Find() != nil {
		return nil, ReasonStateIO
	}
	driveType, _, _ := windowsRecordDriveType.Call(uintptr(unsafe.Pointer(ptr)))
	if driveType != windowsRecordDriveFixed {
		return nil, ReasonStateIO
	}
	if o.appendGuard(root) != nil {
		return nil, ReasonStateIO
	}
	var filesystem [32]uint16
	if windowsRecordVolumeInformation.Find() != nil {
		return nil, ReasonStateIO
	}
	volumeOK, _, _ := windowsRecordVolumeInformation.Call(uintptr(o.guards[0].handle), 0, 0, 0, 0, 0, uintptr(unsafe.Pointer(&filesystem[0])), uintptr(len(filesystem)))
	if volumeOK == 0 || syscall.UTF16ToString(filesystem[:]) != "NTFS" {
		return nil, ReasonStateIO
	}
	current := root
	parts := strings.Split(clean[3:], `\`)
	for i, part := range parts {
		current = filepath.Join(current, part)
		err := o.appendGuard(current)
		if err == nil {
			continue
		}
		if i != len(parts)-1 || !create || !errors.Is(err, os.ErrNotExist) {
			return nil, ReasonStateIO
		}
		if o.before("create-directory") != nil {
			return nil, ReasonStateIO
		}
		h, err := o.createNative(part, true)
		if err != nil {
			return nil, ReasonStateIO
		}
		observed, err := o.project("directory-created", h, true)
		if err == nil {
			err = o.verifyGuards(false)
		}
		if err != nil {
			_ = windowsRecordDeleteHandle(h)
			_ = syscall.CloseHandle(h)
			return nil, ReasonStateIO
		}
		// Acquire a compatible metadata guard while the exact rollback handle
		// is still live. The selected-directory guard permits delete sharing;
		// ancestors deny it. Neither prevents in-place reparse conversion, so
		// verify the retained and current observations at every boundary.
		guard, openErr := windowsRecordOpenMetadata(current, windowsRecordTraverse, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE)
		var currentInfo syscall.ByHandleFileInformation
		if openErr == nil {
			openErr = syscall.GetFileInformationByHandle(guard, &currentInfo)
		}
		if openErr != nil || windowsRecordIdentity(currentInfo) != observed.identity {
			if guard != syscall.InvalidHandle {
				_ = syscall.CloseHandle(guard)
			}
			_ = windowsRecordDeleteHandle(h)
			_ = syscall.CloseHandle(h)
			return nil, ReasonStateIO
		}
		o.guards = append(o.guards, windowsRecordGuard{path: current, handle: guard, identity: observed.identity})
		if o.verifyGuards(false) != nil {
			_ = windowsRecordDeleteHandle(h)
			_ = syscall.CloseHandle(h)
			return nil, ReasonStateIO
		}
		if syscall.CloseHandle(h) != nil {
			return nil, ReasonStateIO
		}
	}
	if o.checkDirectory(false) != nil {
		return nil, ReasonStateIO
	}
	success = true
	return o, nil
}

// NtCreateFile is a documented ntdll export. A relative name and retained
// RootDirectory bind creation to that directory, and return the exact created
// handle for rollback. Every object receives its security before any payload.
func (o *windowsRecordOps) createNative(name string, directory bool) (syscall.Handle, error) {
	if len(o.guards) == 0 || !windowsRecordLeafName(name) {
		return syscall.InvalidHandle, ReasonStateIO
	}
	sd, err := windowsRecordDescriptor()
	if err != nil {
		return syscall.InvalidHandle, err
	}
	chars, err := syscall.UTF16FromString(name)
	if err != nil || len(chars)*2 > 65535 || windowsRecordNTCreate.Find() != nil || windowsRecordNTError.Find() != nil {
		return syscall.InvalidHandle, ReasonStateIO
	}
	ntname := windowsRecordUnicodeString{Length: uint16((len(chars) - 1) * 2), MaximumLength: uint16(len(chars) * 2), Buffer: &chars[0]}
	oa := windowsRecordObjectAttributes{RootDirectory: o.guards[len(o.guards)-1].handle, ObjectName: &ntname, Attributes: windowsRecordCaseInsensitive | windowsRecordDontReparse, SecurityDescriptor: &sd[0]}
	oa.Length = uint32(unsafe.Sizeof(oa))
	options := uint32(windowsRecordSynchronousIO | windowsRecordOpenReparsePoint | windowsRecordNonDirectoryFile)
	access := uint32(syscall.GENERIC_READ | syscall.GENERIC_WRITE | windowsRecordReadControl | windowsRecordDelete | syscall.SYNCHRONIZE)
	if directory {
		options = windowsRecordSynchronousIO | windowsRecordOpenReparsePoint | windowsRecordDirectoryFile
	}
	var h syscall.Handle
	var iosb windowsRecordIOStatusBlock
	status, _, _ := windowsRecordNTCreate.Call(uintptr(unsafe.Pointer(&h)), uintptr(access), uintptr(unsafe.Pointer(&oa)), uintptr(unsafe.Pointer(&iosb)), 0, syscall.FILE_ATTRIBUTE_NORMAL, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE, windowsRecordFileCreate, uintptr(options), 0, 0)
	runtime.KeepAlive(sd)
	runtime.KeepAlive(chars)
	if uint32(status) != 0 {
		errno, _, _ := windowsRecordNTError.Call(uintptr(uint32(status)))
		return syscall.InvalidHandle, windowsRecordFailure(syscall.Errno(errno))
	}
	return h, nil
}
func (o *windowsRecordOps) verifyGuards(prepublication bool) error {
	for i, g := range o.guards {
		var retained syscall.ByHandleFileInformation
		if syscall.GetFileInformationByHandle(g.handle, &retained) != nil || retained.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0 || retained.FileAttributes&syscall.FILE_ATTRIBUTE_DIRECTORY == 0 || windowsRecordIdentity(retained) != g.identity {
			return ReasonStateIO
		}
		h, err := windowsRecordOpenMetadata(g.path, 0, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE)
		if err != nil {
			return ReasonStateIO
		}
		var observed syscall.ByHandleFileInformation
		err = syscall.GetFileInformationByHandle(h, &observed)
		closeErr := syscall.CloseHandle(h)
		if err != nil || closeErr != nil {
			return ReasonStateIO
		}
		if o.hooks != nil && o.hooks.metadata != nil {
			stage := "ancestor-recheck"
			if prepublication && i == len(o.guards)-1 {
				stage = "parent-recheck"
			}
			o.hooks.metadata(stage, &observed)
		}
		if observed.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0 || observed.FileAttributes&syscall.FILE_ATTRIBUTE_DIRECTORY == 0 || windowsRecordIdentity(observed) != g.identity {
			return ReasonStateIO
		}
	}
	return nil
}
func (o *windowsRecordOps) checkDirectory(_ bool) error {
	if len(o.guards) == 0 || o.verifyGuards(false) != nil {
		return ReasonStateIO
	}
	_, err := o.project("directory", o.guards[len(o.guards)-1].handle, true)
	return err
}
func (o *windowsRecordOps) inspect(name string) (privateRecordInfo, error) {
	if !windowsRecordLeafName(name) || o.checkDirectory(false) != nil {
		return privateRecordInfo{}, ReasonStateIO
	}
	h, err := windowsRecordOpenMetadata(filepath.Join(o.dir, name), 0, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE)
	if err != nil {
		return privateRecordInfo{}, err
	}
	defer func() { _ = syscall.CloseHandle(h) }()
	info, err := o.project("leaf-inspect", h, false)
	if err == nil {
		err = o.verifyGuards(false)
	}
	return info, err
}
func (o *windowsRecordOps) open(name string) (privateRecordHandle, error) {
	if !windowsRecordLeafName(name) || o.before("open") != nil || o.checkDirectory(false) != nil {
		return nil, ReasonStateIO
	}
	path := filepath.Join(o.dir, name)
	h, err := windowsRecordOpenMetadata(path, syscall.GENERIC_READ, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE)
	if err != nil {
		return nil, err
	}
	info, err := o.project("leaf-open", h, false)
	if err == nil {
		err = o.verifyGuards(false)
	}
	if err != nil {
		_ = syscall.CloseHandle(h)
		return nil, ReasonStateIO
	}
	return &windowsRecordFile{file: os.NewFile(uintptr(h), path), path: path, identity: info.identity}, nil
}
func (o *windowsRecordOps) inspectHandle(handle privateRecordHandle) (privateRecordInfo, error) {
	f, ok := handle.(*windowsRecordFile)
	if !ok {
		return privateRecordInfo{}, ReasonStateIO
	}
	info, err := o.project("handle", syscall.Handle(f.file.Fd()), false)
	if err != nil || info.identity != f.identity || o.verifyGuards(false) != nil {
		return privateRecordInfo{}, ReasonStateIO
	}
	return info, nil
}
func (o *windowsRecordOps) create(name string, temporary bool) (privateRecordHandle, error) {
	if !windowsRecordLeafName(name) || o.checkDirectory(true) != nil {
		return nil, ReasonStateIO
	}
	stage := "fence"
	if temporary {
		stage = "temporary"
		var suffix [12]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			return nil, ReasonStateIO
		}
		name += hex.EncodeToString(suffix[:])
	}
	if o.before("create-"+stage) != nil {
		return nil, ReasonStateIO
	}
	h, err := o.createNative(name, false)
	if err != nil {
		return nil, ReasonStateIO
	}
	info, err := o.project(stage+"-created", h, false)
	if err == nil {
		err = o.verifyGuards(false)
	}
	if err != nil {
		_ = windowsRecordDeleteHandle(h)
		_ = syscall.CloseHandle(h)
		return nil, ReasonStateIO
	}
	path := filepath.Join(o.dir, name)
	return &windowsRecordFile{file: os.NewFile(uintptr(h), path), path: path, identity: info.identity, created: true}, nil
}
func windowsRecordDeleteHandle(h syscall.Handle) error {
	remove := byte(1)
	if windowsRecordSetFileInformation(h, windowsRecordFileDispositionInfo, &remove, 1) != nil {
		return ReasonStateIO
	}
	return nil
}
func (*windowsRecordOps) removeOwned(handle privateRecordHandle) error {
	f, ok := handle.(*windowsRecordFile)
	if !ok || !f.created || f.published {
		return ReasonStateIO
	}
	return windowsRecordDeleteHandle(syscall.Handle(f.file.Fd()))
}
func (*windowsRecordOps) closeBeforePublish() bool { return false }
func (o *windowsRecordOps) recheck(name string, source privateRecordHandle, prior *privateRecordInfo) error {
	if o.before("prepublication") != nil || o.verifyGuards(true) != nil {
		return ReasonStateIO
	}
	if _, err := o.inspectHandle(source); err != nil {
		return ReasonStateIO
	}
	info, err := o.inspect(name)
	if prior == nil {
		if !errors.Is(err, os.ErrNotExist) {
			return ReasonStateIO
		}
	} else if err != nil || info.identity != prior.identity {
		return ReasonStateIO
	}
	f, ok := source.(*windowsRecordFile)
	if !ok {
		return ReasonStateIO
	}
	f.checked = true
	f.expectedTarget = nil
	if prior != nil {
		copyInfo := *prior
		f.expectedTarget = &copyInfo
	}
	return nil
}

type windowsRecordRenameInfo struct {
	Flags          uint32
	RootDirectory  syscall.Handle
	FileNameLength uint32
	FileName       [1]uint16
}

// FileRenameInfoEx POSIX replacement lets existing delete-sharing readers keep
// the previous complete object. File data is flushed first; this primitive does
// not establish directory durability or power-loss persistence. Retained parent
// handles do not prevent in-place reparse conversion; postchecks detect it.
func (o *windowsRecordOps) publish(source privateRecordHandle, name string) (bool, error) {
	f, ok := source.(*windowsRecordFile)
	if !ok || !f.created || f.published || !f.checked || !windowsRecordLeafName(name) || o.before("publish") != nil {
		return false, ReasonStateIO
	}
	// Probe DELETE access on an existing target before the atomic replacement.
	target, err := windowsRecordOpenMetadata(filepath.Join(o.dir, name), windowsRecordDelete, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE)
	if err == nil {
		defer func() { _ = syscall.CloseHandle(target) }()
		observed, err := o.project("target", target, false)
		if err != nil || f.expectedTarget == nil || observed.identity != f.expectedTarget.identity {
			return false, ReasonStateIO
		}
	} else if !errors.Is(err, os.ErrNotExist) || f.expectedTarget != nil {
		return false, ReasonStateIO
	}
	if o.verifyGuards(true) != nil {
		return false, ReasonStateIO
	}
	path := filepath.Join(o.dir, name)
	chars, err := syscall.UTF16FromString(path)
	if err != nil {
		return false, ReasonStateIO
	}
	var layout windowsRecordRenameInfo
	offset := int(unsafe.Offsetof(layout.FileName))
	b := make([]byte, max(int(unsafe.Sizeof(layout)), offset+len(chars)*2))
	info := (*windowsRecordRenameInfo)(unsafe.Pointer(&b[0]))
	info.Flags = 1 | 2 // FILE_RENAME_FLAG_REPLACE_IF_EXISTS | FILE_RENAME_FLAG_POSIX_SEMANTICS
	info.FileNameLength = uint32((len(chars) - 1) * 2)
	for i, c := range chars {
		binary.LittleEndian.PutUint16(b[offset+i*2:], c)
	}
	if windowsRecordSetFileInformation(syscall.Handle(f.file.Fd()), windowsRecordFileRenameInfoEx, &b[0], uint32(len(b))) != nil {
		return false, ReasonStateIO
	}
	f.published = true
	f.path = path
	return true, nil
}
func (o *windowsRecordOps) complete(source privateRecordHandle, name string) error {
	if o.before("completion") != nil || o.verifyGuards(false) != nil {
		return ReasonStateIO
	}
	f, ok := source.(*windowsRecordFile)
	if !ok || !f.published {
		return ReasonStateIO
	}
	opened, err := o.project("published", syscall.Handle(f.file.Fd()), false)
	if err != nil || opened.identity != f.identity {
		return ReasonStateIO
	}
	observed, err := o.inspect(name)
	if err != nil || observed.identity != opened.identity {
		return ReasonStateIO
	}
	return nil
}
func (o *windowsRecordOps) close() error {
	var result error
	for i := len(o.guards) - 1; i >= 0; i-- {
		if syscall.CloseHandle(o.guards[i].handle) != nil {
			result = ReasonStateIO
		}
	}
	o.guards = nil
	return result
}
