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
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Hooks observe actual native objects. They are per-adapter, unexported and nil
// in production; metadata can replace one observation for bounded race tests.
type windowsRecordHooks struct {
	before   func(string) error
	observe  func(string, windows.Handle, privateRecordPolicy) error
	metadata func(string, *windows.ByHandleFileInformation)
}
type windowsRecordGuard struct {
	path     string
	handle   windows.Handle
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

func windowsRecordFailure(err error) error {
	var status windows.NTStatus
	if errors.As(err, &status) {
		err = status.Errno()
	}
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		return os.ErrNotExist
	}
	return ReasonStateIO
}
func windowsRecordPrincipals() (*windows.SID, *windows.SID, error) {
	token, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || token == nil || token.User.Sid == nil || !token.User.Sid.IsValid() {
		return nil, nil, ReasonStateIO
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return nil, nil, ReasonStateIO
	}
	return token.User.Sid, system, nil
}
func windowsRecordDescriptor() (*windows.SECURITY_DESCRIPTOR, error) {
	user, system, err := windowsRecordPrincipals()
	if err != nil {
		return nil, err
	}
	text := "O:" + user.String() + "D:P(A;;FA;;;" + user.String() + ")"
	if !user.Equals(system) {
		text += "(A;;FA;;;SY)"
	}
	sd, err := windows.SecurityDescriptorFromString(text)
	if err != nil {
		return nil, ReasonStateIO
	}
	return sd, nil
}

var windowsRecordGetSecurity = windows.NewLazySystemDLL("advapi32.dll").NewProc("GetKernelObjectSecurity")

// GetKernelObjectSecurity reports a bounded self-relative descriptor. Parsing
// offsets and lengths before forming any SID/ACE view avoids trusting embedded
// native pointers. The Windows allocation is not retained: this is a Go buffer.
func windowsRecordSecurity(handle windows.Handle) ([]byte, error) {
	const wanted = windows.OWNER_SECURITY_INFORMATION | windows.DACL_SECURITY_INFORMATION
	var needed uint32
	r, _, callErr := windowsRecordGetSecurity.Call(uintptr(handle), uintptr(wanted), 0, 0, uintptr(unsafe.Pointer(&needed)))
	if r != 0 || !errors.Is(callErr, windows.ERROR_INSUFFICIENT_BUFFER) || needed < 20 || needed > 65536 {
		return nil, ReasonStateIO
	}
	b := make([]byte, needed)
	r, _, _ = windowsRecordGetSecurity.Call(uintptr(handle), uintptr(wanted), uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)), uintptr(unsafe.Pointer(&needed)))
	if r == 0 || needed < 20 || int(needed) > len(b) {
		return nil, ReasonStateIO
	}
	return b[:needed], nil
}
func windowsRecordSID(b []byte, offset int) ([]byte, bool) {
	if offset < 0 || offset > len(b)-8 || b[offset] != 1 || b[offset+1] > 15 {
		return nil, false
	}
	length := 8 + int(b[offset+1])*4
	if length > len(b)-offset {
		return nil, false
	}
	return b[offset : offset+length], true
}
func windowsRecordPolicyFromSecurity(b []byte) (privateRecordPolicy, error) {
	p := privateRecordPolicy{}
	if len(b) < 20 || b[0] != 1 {
		return p, ReasonStateIO
	}
	control := binary.LittleEndian.Uint16(b[2:4])
	if control&uint16(windows.SE_SELF_RELATIVE) == 0 {
		return p, ReasonStateIO
	}
	ownerOffset := int(binary.LittleEndian.Uint32(b[4:8]))
	aclOffset := int(binary.LittleEndian.Uint32(b[16:20]))
	if ownerOffset < 20 || ownerOffset%4 != 0 || aclOffset < 20 || aclOffset%4 != 0 || aclOffset > len(b)-8 {
		return p, ReasonStateIO
	}
	owner, ok := windowsRecordSID(b, ownerOffset)
	if !ok {
		return p, ReasonStateIO
	}
	// Validate all optional offsets too, even though this query requests only
	// owner and DACL. No offset may point into the descriptor's fixed header.
	groupOffset := int(binary.LittleEndian.Uint32(b[8:12]))
	if groupOffset != 0 {
		if groupOffset < 20 || groupOffset%4 != 0 {
			return p, ReasonStateIO
		}
		if _, ok := windowsRecordSID(b, groupOffset); !ok {
			return p, ReasonStateIO
		}
	}
	if binary.LittleEndian.Uint32(b[12:16]) != 0 {
		return p, ReasonStateIO
	}
	aclSize := int(binary.LittleEndian.Uint16(b[aclOffset+2 : aclOffset+4]))
	count := int(binary.LittleEndian.Uint16(b[aclOffset+4 : aclOffset+6]))
	if (b[aclOffset] != 2 && b[aclOffset] != 4) || aclSize < 8 || aclSize > len(b)-aclOffset || count > 1024 {
		return p, ReasonStateIO
	}
	if ownerOffset < aclOffset+aclSize && ownerOffset+len(owner) > aclOffset {
		return p, ReasonStateIO
	}
	user, system, err := windowsRecordPrincipals()
	if err != nil {
		return p, err
	}
	userBytes := unsafe.Slice((*byte)(unsafe.Pointer(user)), user.Len())
	systemBytes := unsafe.Slice((*byte)(unsafe.Pointer(system)), system.Len())
	p.ownerUser = bytes.Equal(owner, userBytes)
	p.userIsSystem = bytes.Equal(userBytes, systemBytes)
	p.protected = control&uint16(windows.SE_DACL_PROTECTED) != 0
	p.daclPresent = control&uint16(windows.SE_DACL_PRESENT) != 0
	end, pos := aclOffset+aclSize, aclOffset+8
	for range count {
		if pos > end-8 {
			return privateRecordPolicy{}, ReasonStateIO
		}
		size := int(binary.LittleEndian.Uint16(b[pos+2 : pos+4]))
		if size < 8 || size%4 != 0 || size > end-pos {
			return privateRecordPolicy{}, ReasonStateIO
		}
		ace := privateRecordACE{kind: b[pos], flags: b[pos+1], rights: binary.LittleEndian.Uint32(b[pos+4 : pos+8])}
		if ace.kind == windows.ACCESS_ALLOWED_ACE_TYPE {
			sid, ok := windowsRecordSID(b[pos:pos+size], 8)
			if !ok || len(sid)+8 != size {
				return privateRecordPolicy{}, ReasonStateIO
			}
			switch {
			case bytes.Equal(sid, userBytes):
				ace.principal = recordUser
			case bytes.Equal(sid, systemBytes):
				ace.principal = recordSystem
			}
		}
		p.aces = append(p.aces, ace)
		pos += size
	}
	if pos > end {
		return privateRecordPolicy{}, ReasonStateIO
	}
	p.valid = true
	runtime.KeepAlive(user)
	runtime.KeepAlive(system)
	return p, nil
}
func windowsRecordIdentity(info windows.ByHandleFileInformation) [3]uint64 {
	return [3]uint64{uint64(info.VolumeSerialNumber), uint64(info.FileIndexHigh), uint64(info.FileIndexLow)}
}
func windowsRecordProjection(handle windows.Handle, directory bool) (privateRecordPolicy, privateRecordInfo, error) {
	var info windows.ByHandleFileInformation
	if windows.GetFileInformationByHandle(handle, &info) != nil {
		return privateRecordPolicy{}, privateRecordInfo{}, ReasonStateIO
	}
	b, err := windowsRecordSecurity(handle)
	if err != nil {
		return privateRecordPolicy{}, privateRecordInfo{}, err
	}
	p, err := windowsRecordPolicyFromSecurity(b)
	p.directory = info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0
	p.wantDirectory = directory
	p.reparse = info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0
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
func (o *windowsRecordOps) project(stage string, h windows.Handle, directory bool) (privateRecordInfo, error) {
	p, info, err := windowsRecordProjection(h, directory)
	if err != nil {
		return privateRecordInfo{}, ReasonStateIO
	}
	if o.hooks != nil && o.hooks.metadata != nil {
		var observed windows.ByHandleFileInformation
		if windows.GetFileInformationByHandle(h, &observed) != nil {
			return privateRecordInfo{}, ReasonStateIO
		}
		o.hooks.metadata("projection", &observed)
		p.reparse = observed.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0
		p.directory = observed.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0
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
func windowsRecordOpenMetadata(path string, access, share uint32) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return windows.InvalidHandle, ReasonStateIO
	}
	h, err := windows.CreateFile(p, access|windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL, share, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return windows.InvalidHandle, windowsRecordFailure(err)
	}
	return h, nil
}
func (o *windowsRecordOps) appendGuard(path string) error {
	h, err := windowsRecordOpenMetadata(path, windows.FILE_TRAVERSE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE)
	if err != nil {
		return err
	}
	var info windows.ByHandleFileInformation
	if windows.GetFileInformationByHandle(h, &info) != nil || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		_ = windows.CloseHandle(h)
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
	ptr, _ := windows.UTF16PtrFromString(root)
	if windows.GetDriveType(ptr) != windows.DRIVE_FIXED {
		return nil, ReasonStateIO
	}
	if o.appendGuard(root) != nil {
		return nil, ReasonStateIO
	}
	var filesystem [32]uint16
	if windows.GetVolumeInformationByHandle(o.guards[0].handle, nil, 0, nil, nil, nil, &filesystem[0], uint32(len(filesystem))) != nil || windows.UTF16ToString(filesystem[:]) != "NTFS" {
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
			_ = windows.CloseHandle(h)
			return nil, ReasonStateIO
		}
		// Acquire a compatible metadata guard while the exact rollback handle
		// is still live. The selected-directory guard permits delete sharing;
		// ancestors deny it. Neither prevents in-place reparse conversion, so
		// verify the retained and current observations at every boundary.
		guard, openErr := windowsRecordOpenMetadata(current, windows.FILE_TRAVERSE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE)
		var currentInfo windows.ByHandleFileInformation
		if openErr == nil {
			openErr = windows.GetFileInformationByHandle(guard, &currentInfo)
		}
		if openErr != nil || windowsRecordIdentity(currentInfo) != observed.identity {
			if guard != windows.InvalidHandle {
				_ = windows.CloseHandle(guard)
			}
			_ = windowsRecordDeleteHandle(h)
			_ = windows.CloseHandle(h)
			return nil, ReasonStateIO
		}
		o.guards = append(o.guards, windowsRecordGuard{path: current, handle: guard, identity: observed.identity})
		if o.verifyGuards(false) != nil {
			_ = windowsRecordDeleteHandle(h)
			_ = windows.CloseHandle(h)
			return nil, ReasonStateIO
		}
		if windows.CloseHandle(h) != nil {
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
func (o *windowsRecordOps) createNative(name string, directory bool) (windows.Handle, error) {
	if len(o.guards) == 0 || !windowsRecordLeafName(name) {
		return windows.InvalidHandle, ReasonStateIO
	}
	sd, err := windowsRecordDescriptor()
	if err != nil {
		return windows.InvalidHandle, err
	}
	ntname, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return windows.InvalidHandle, ReasonStateIO
	}
	oa := windows.OBJECT_ATTRIBUTES{RootDirectory: o.guards[len(o.guards)-1].handle, ObjectName: ntname, Attributes: windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE, SecurityDescriptor: sd}
	oa.Length = uint32(unsafe.Sizeof(oa))
	options := uint32(windows.FILE_SYNCHRONOUS_IO_NONALERT | windows.FILE_OPEN_REPARSE_POINT | windows.FILE_NON_DIRECTORY_FILE)
	access := uint32(windows.GENERIC_READ | windows.GENERIC_WRITE | windows.READ_CONTROL | windows.DELETE | windows.SYNCHRONIZE)
	if directory {
		options = windows.FILE_SYNCHRONOUS_IO_NONALERT | windows.FILE_OPEN_REPARSE_POINT | windows.FILE_DIRECTORY_FILE
	}
	var h windows.Handle
	var iosb windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(&h, access, &oa, &iosb, nil, windows.FILE_ATTRIBUTE_NORMAL, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_CREATE, options, 0, 0)
	runtime.KeepAlive(sd)
	runtime.KeepAlive(ntname)
	if err != nil {
		return windows.InvalidHandle, windowsRecordFailure(err)
	}
	return h, nil
}
func (o *windowsRecordOps) verifyGuards(prepublication bool) error {
	for i, g := range o.guards {
		var retained windows.ByHandleFileInformation
		if windows.GetFileInformationByHandle(g.handle, &retained) != nil || retained.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || retained.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 || windowsRecordIdentity(retained) != g.identity {
			return ReasonStateIO
		}
		h, err := windowsRecordOpenMetadata(g.path, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE)
		if err != nil {
			return ReasonStateIO
		}
		var observed windows.ByHandleFileInformation
		err = windows.GetFileInformationByHandle(h, &observed)
		closeErr := windows.CloseHandle(h)
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
		if observed.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || observed.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 || windowsRecordIdentity(observed) != g.identity {
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
	h, err := windowsRecordOpenMetadata(filepath.Join(o.dir, name), 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE)
	if err != nil {
		return privateRecordInfo{}, err
	}
	defer func() { _ = windows.CloseHandle(h) }()
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
	h, err := windowsRecordOpenMetadata(path, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE)
	if err != nil {
		return nil, err
	}
	info, err := o.project("leaf-open", h, false)
	if err == nil {
		err = o.verifyGuards(false)
	}
	if err != nil {
		_ = windows.CloseHandle(h)
		return nil, ReasonStateIO
	}
	return &windowsRecordFile{file: os.NewFile(uintptr(h), path), path: path, identity: info.identity}, nil
}
func (o *windowsRecordOps) inspectHandle(handle privateRecordHandle) (privateRecordInfo, error) {
	f, ok := handle.(*windowsRecordFile)
	if !ok {
		return privateRecordInfo{}, ReasonStateIO
	}
	info, err := o.project("handle", windows.Handle(f.file.Fd()), false)
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
		_ = windows.CloseHandle(h)
		return nil, ReasonStateIO
	}
	path := filepath.Join(o.dir, name)
	return &windowsRecordFile{file: os.NewFile(uintptr(h), path), path: path, identity: info.identity, created: true}, nil
}
func windowsRecordDeleteHandle(h windows.Handle) error {
	remove := byte(1)
	if windows.SetFileInformationByHandle(h, windows.FileDispositionInfo, &remove, 1) != nil {
		return ReasonStateIO
	}
	return nil
}
func (*windowsRecordOps) removeOwned(handle privateRecordHandle) error {
	f, ok := handle.(*windowsRecordFile)
	if !ok || !f.created || f.published {
		return ReasonStateIO
	}
	return windowsRecordDeleteHandle(windows.Handle(f.file.Fd()))
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
	RootDirectory  windows.Handle
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
	target, err := windowsRecordOpenMetadata(filepath.Join(o.dir, name), windows.DELETE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE)
	if err == nil {
		defer func() { _ = windows.CloseHandle(target) }()
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
	chars, err := windows.UTF16FromString(path)
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
	if windows.SetFileInformationByHandle(windows.Handle(f.file.Fd()), windows.FileRenameInfoEx, &b[0], uint32(len(b))) != nil {
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
	opened, err := o.project("published", windows.Handle(f.file.Fd()), false)
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
		if windows.CloseHandle(o.guards[i].handle) != nil {
			result = ReasonStateIO
		}
	}
	o.guards = nil
	return result
}
