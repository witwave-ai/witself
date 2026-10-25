//go:build windows

package localcell

import (
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

const testFileAllAccess = 0x1f01ff

func currentTestSID(t *testing.T) *windows.SID {
	t.Helper()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal("cannot query current test user")
	}
	return user.User.Sid
}

func testSecurityDescriptor(t *testing.T, path string) *windows.SECURITY_DESCRIPTOR {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal("cannot query fixture security descriptor")
	}
	return sd
}

// Independently read the filesystem descriptor; do not use the validator under
// test as the oracle, and compare SIDs rather than localized account names.
func assertWindowsPrivate(t *testing.T, path string) {
	t.Helper()
	assertWindowsPrivateDescriptor(t, testSecurityDescriptor(t, path))
}

func assertWindowsPrivateDescriptor(t *testing.T, sd *windows.SECURITY_DESCRIPTOR) {
	t.Helper()
	user := currentTestSID(t)
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatal("cannot resolve SYSTEM SID")
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || !owner.Equals(user) {
		t.Fatal("created object does not have the current user owner")
	}
	control, _, err := sd.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatal("created object does not have a protected DACL")
	}
	dacl, _, err := sd.DACL()
	want := uint16(2)
	if user.Equals(system) {
		want = 1
	}
	if err != nil || dacl == nil || dacl.AceCount != want {
		t.Fatal("created object has missing, null, or unexpected DACL entries")
	}
	seenUser, seenSystem := false, false
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			t.Fatal("cannot inspect created object's ACE")
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags != 0 || ace.Mask != testFileAllAccess {
			t.Fatal("created object has an inherited, limited, or unsupported ACE")
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		switch {
		case sid.Equals(user):
			if seenUser {
				t.Fatal("created object repeats the user ACE")
			}
			seenUser = true
		case sid.Equals(system):
			if seenSystem {
				t.Fatal("created object repeats the SYSTEM ACE")
			}
			seenSystem = true
		default:
			t.Fatal("created object grants another principal access")
		}
	}
	if !seenUser || (!seenSystem && !user.Equals(system)) {
		t.Fatal("created object lacks a required principal")
	}
}

func setTestDACL(t *testing.T, path, extra string, inherited, null bool) bool {
	t.Helper()
	var flags byte
	if inherited {
		flags = windows.INHERITED_ACE
	}
	return setTestDACLFlags(t, path, extra, flags, null)
}

func setTestDACLFlags(t *testing.T, path, extra string, flags byte, null bool) bool {
	t.Helper()
	var dacl *windows.ACL
	if !null {
		sddl := "D:P(A;;FA;;;" + currentTestSID(t).String() + ")(A;;FA;;;SY)" + extra
		sd, err := windows.SecurityDescriptorFromString(sddl)
		if err != nil {
			t.Fatal("cannot build test DACL")
		}
		dacl, _, err = sd.DACL()
		if err != nil {
			t.Fatal("cannot extract test DACL")
		}
		var userACE *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, 0, &userACE); err != nil {
			t.Fatal("cannot inspect fixture user ACE")
		}
		// Plant the flag directly on this object. Parent inheritance is not
		// reliable for a protected DACL on every supported filesystem.
		userACE.Header.AceFlags = flags
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal("cannot install test DACL")
	}
	observed, _, err := testSecurityDescriptor(t, path).DACL()
	if err != nil || (null && observed != nil) || (!null && observed == nil) {
		t.Fatal("test DACL was not installed")
	}
	if flags != 0 {
		found := false
		for i := uint32(0); i < uint32(observed.AceCount); i++ {
			var ace *windows.ACCESS_ALLOWED_ACE
			if err := windows.GetAce(observed, i, &ace); err != nil {
				t.Fatal("cannot inspect inherited fixture ACE")
			}
			found = found || ace.Header.AceFlags&flags == flags
		}
		if !found {
			if flags != windows.INHERITED_ACE {
				t.Fatal("fixture did not retain its inheritable ACE flags")
			}
			// A protected DACL can lose the inherited flag or the inherited
			// user entry itself. Prove either exact result, then restore the
			// fixture for cleanup. Neither path changes production policy.
			assertStrippedInheritedFixture(t, path)
			setTestDACLFlags(t, path, "", 0, false)
			assertWindowsPrivate(t, path)
			return false
		}
	}
	return true
}

func assertStrippedInheritedFixture(t *testing.T, path string) {
	t.Helper()
	sd := testSecurityDescriptor(t, path)
	user := currentTestSID(t)
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || !owner.Equals(user) {
		t.Fatal("stripped inherited fixture changed its owner")
	}
	control, _, err := sd.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatal("stripped inherited fixture lost DACL protection")
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		t.Fatal("stripped inherited fixture lost its DACL")
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatal("cannot resolve stripped fixture SYSTEM SID")
	}
	if dacl.AceCount == 1 && !user.Equals(system) {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, 0, &ace); err != nil || ace == nil {
			t.Fatal("cannot inspect stripped fixture SYSTEM entry")
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags != 0 || ace.Mask != testFileAllAccess || !sid.Equals(system) {
			t.Fatal("stripped inherited fixture has an unexpected remaining entry")
		}
		return
	}
	assertWindowsPrivateDescriptor(t, sd)
}

type windowsTreeEntry struct {
	Mode fs.FileMode
	Size int64
}

func windowsTreeSnapshot(t *testing.T, root string) map[string]windowsTreeEntry {
	t.Helper()
	result := make(map[string]windowsTreeEntry)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		result[rel] = windowsTreeEntry{Mode: info.Mode(), Size: info.Size()}
		return nil
	})
	if err != nil {
		t.Fatal("cannot inspect test tree")
	}
	return result
}

func assertPrivateRefusal(t *testing.T, err error, path string) {
	t.Helper()
	if err == nil {
		t.Fatal("unsafe Windows filesystem object was accepted")
	}
	if strings.Contains(err.Error(), path) || strings.Contains(err.Error(), currentTestSID(t).String()) {
		t.Fatal("privacy refusal exposed a path or SID")
	}
}

func TestWindowsFreshCellObjectsHaveExactPrivateDACL(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	root := filepath.Join(t.TempDir(), "Cell space é")
	c, err := New(Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.validateRoot(true); err != nil {
		t.Fatal(err)
	}
	if err := c.layout(); err != nil {
		t.Fatal(err)
	}
	if err := privateDir(c.path("pg"), true); err != nil {
		t.Fatal(err)
	}
	if err := c.makePassword(); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(c.path("manifest.json"), map[string]string{"fixture": "synthetic"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(c.path("run", "server.json"), map[string]string{"fixture": "synthetic"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.journal("install", "operation", "started"); err != nil {
		t.Fatal(err)
	}
	log, err := c.logFile("server")
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	lock, err := acquireLock(root, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	pending, err := createPrivateTemp(c.path("tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if err := pending.Close(); err != nil {
		t.Fatal(err)
	}
	err = filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		assertWindowsPrivate(t, path)
		return nil
	})
	if err != nil {
		t.Fatal("cannot inspect complete private layout")
	}
}

func TestWindowsRootPrivacyRefusedBeforeMutation(t *testing.T) {
	for _, test := range []struct {
		name        string
		extra       string
		inherited   bool
		inheritable bool
		null        bool
	}{
		{name: "inherited-user-ACE", inherited: true},
		{name: "inheritable-flag", inheritable: true},
		{name: "Everyone", extra: "(A;;FR;;;WD)"},
		{name: "Users", extra: "(A;;FR;;;BU)"},
		{name: "Administrators", extra: "(A;;FA;;;BA)"},
		{name: "Authenticated-Users", extra: "(A;;FR;;;AU)"},
		// Standard CI cannot assign a foreign owner. This crafted foreign SID
		// grants access while retaining the current owner, catching owner-only
		// validation; it is not a second-user or foreign-owner access witness.
		{name: "foreign-SID-surrogate", extra: "(A;;FA;;;S-1-5-21-1-2-3-4242)"},
		{name: "null-DACL", null: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := testPrivateRoot(t)
			c, err := New(Config{Root: root})
			if err != nil {
				t.Fatal(err)
			}
			if test.inheritable {
				setTestDACLFlags(t, root, test.extra, windows.OBJECT_INHERIT_ACE|windows.CONTAINER_INHERIT_ACE, false)
			} else if !setTestDACL(t, root, test.extra, test.inherited, test.null) {
				t.Log("filesystem stripped inherited ACE flag; inheritable flag case covers filesystem refusal")
				return
			}
			before := windowsTreeSnapshot(t, root)
			assertPrivateRefusal(t, c.validateRoot(true), root)
			lock, err := acquireLock(root, true)
			if lock != nil {
				_ = lock.Close()
			}
			assertPrivateRefusal(t, err, root)
			if !reflect.DeepEqual(before, windowsTreeSnapshot(t, root)) {
				t.Fatal("privacy refusal changed the existing root")
			}
		})
	}
}

func TestWindowsExistingObjectsRefusedBeforeMutation(t *testing.T) {
	for _, name := range []string{"run", "spool", "logs", "tmp", "journal", "home", "manifest.json", "run/postgres.password", "run/server.json", "journal/operations.jsonl", "logs/server.log", ".owner.lock", "run/operation.lock"} {
		t.Run(strings.ReplaceAll(name, "/", "_"), func(t *testing.T) {
			c, _ := unitCell(t)
			lock, err := acquireLock(c.cfg.Root, true)
			if err != nil {
				t.Fatal(err)
			}
			if err := lock.Close(); err != nil {
				t.Fatal(err)
			}
			for _, file := range []string{"run/postgres.password", "run/server.json", "journal/operations.jsonl", "logs/server.log"} {
				if err := testWriteFile(c.path(file), []byte("synthetic unchanged fixture bytes"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			path := c.path(name)
			setTestDACL(t, path, "(A;;FR;;;WD)", false, false)
			before := windowsTreeSnapshot(t, c.cfg.Root)
			assertPrivateRefusal(t, c.validateRoot(true), path)
			lock, err = acquireLock(c.cfg.Root, true)
			if lock != nil {
				_ = lock.Close()
			}
			assertPrivateRefusal(t, err, path)
			if !reflect.DeepEqual(before, windowsTreeSnapshot(t, c.cfg.Root)) {
				t.Fatal("object privacy refusal changed the existing layout")
			}
		})
	}
}

func TestWindowsPasswordReplacementACLRefusedBeforeReadOrWrite(t *testing.T) {
	c, _ := unitCell(t)
	path := c.path("run", "postgres.password")
	const witness = "synthetic password replacement witness"
	if err := testWriteFile(path, []byte(witness), 0o600); err != nil {
		t.Fatal(err)
	}
	setTestDACL(t, path, "(A;;FR;;;WD)", false, false)
	password, err := c.password()
	assertPrivateRefusal(t, err, path)
	if password != "" {
		t.Fatal("password with broadened ACL was exposed")
	}
	assertPrivateRefusal(t, c.makePassword(), path)
	data, err := os.ReadFile(path)
	if err != nil || string(data) != witness {
		t.Fatal("password refusal changed the replacement file")
	}
	pending, err := filepath.Glob(c.path("run", ".pending-*"))
	if err != nil || len(pending) != 0 {
		t.Fatal("password refusal left temporary state")
	}
}

func TestWindowsPasswordTemporaryPrivateBeforeWrite(t *testing.T) {
	c, _ := unitCell(t)
	previous := beforePasswordWrite
	t.Cleanup(func() { beforePasswordWrite = previous })
	observed := 0
	beforePasswordWrite = func(file *os.File) {
		observed++
		// Read the actual opened object, independently of the production
		// validator and pathname; repairing its ACL after Write is too late.
		t.Cleanup(func() { _ = file.Close() })
		info, err := file.Stat()
		if err != nil || info.Size() != 0 {
			t.Fatal("password privacy observation did not precede the first data write")
		}
		sd, err := windows.GetSecurityInfo(windows.Handle(file.Fd()), windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal("cannot query opened password temporary security descriptor")
		}
		assertWindowsPrivateDescriptor(t, sd)
	}
	if err := c.makePassword(); err != nil {
		t.Fatal(err)
	}
	if observed != 1 {
		t.Fatal("password publisher bypassed the pre-write privacy observation")
	}
}

func TestWindowsPasswordTemporaryPrivateBeforePersistence(t *testing.T) {
	c, _ := unitCell(t)
	previous := syncFile
	t.Cleanup(func() { syncFile = previous })
	observed := false
	syncFile = func(file *os.File) error {
		if strings.HasPrefix(filepath.Base(file.Name()), ".pending-") {
			observed = true
			assertWindowsPrivate(t, file.Name())
		}
		return previous(file)
	}
	if err := c.makePassword(); err != nil {
		t.Fatal(err)
	}
	if !observed {
		t.Fatal("password publisher bypassed the checked temporary file")
	}
	assertWindowsPrivate(t, c.path("run", "postgres.password"))
}

// Junctions do not require the symbolic-link privilege. A native reparse witness
// remains mandatory even when a CI account cannot create a symbolic link.
func createTestJunction(t *testing.T, path, target string) {
	t.Helper()
	if err := testMkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal("cannot encode junction path")
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		t.Fatal("cannot open junction fixture")
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	substitute := utf16.Encode([]rune(`\??\` + target))
	printName := utf16.Encode([]rune(target))
	pathData := append(append(append(substitute, 0), printName...), 0)
	buffer := make([]byte, 16+2*len(pathData))
	binary.LittleEndian.PutUint32(buffer[0:4], windows.IO_REPARSE_TAG_MOUNT_POINT)
	binary.LittleEndian.PutUint16(buffer[4:6], uint16(len(buffer)-8))
	binary.LittleEndian.PutUint16(buffer[10:12], uint16(2*len(substitute)))
	binary.LittleEndian.PutUint16(buffer[12:14], uint16(2*(len(substitute)+1)))
	binary.LittleEndian.PutUint16(buffer[14:16], uint16(2*len(printName)))
	for i, value := range pathData {
		binary.LittleEndian.PutUint16(buffer[16+2*i:], value)
	}
	var returned uint32
	if err := windows.DeviceIoControl(handle, windows.FSCTL_SET_REPARSE_POINT, &buffer[0], uint32(len(buffer)), nil, 0, &returned, nil); err != nil {
		t.Fatal("cannot create mandatory junction fixture")
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0 {
		t.Fatal("fixture is not a reparse point")
	}
}

func TestWindowsJunctionRunRefusedBeforeMutation(t *testing.T) {
	root, outside := testPrivateRoot(t), testPrivateRoot(t)
	createTestJunction(t, filepath.Join(root, "run"), outside)
	before := windowsTreeSnapshot(t, outside)
	lock, err := acquireLock(root, true)
	if lock != nil {
		_ = lock.Close()
	}
	assertPrivateRefusal(t, err, root)
	if _, err := os.Lstat(filepath.Join(root, ".owner.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("junction refusal created the stable lock first")
	}
	if !reflect.DeepEqual(before, windowsTreeSnapshot(t, outside)) {
		t.Fatal("junction refusal mutated its outside target")
	}
}

func TestWindowsJunctionAncestorAndRootRefused(t *testing.T) {
	parent, outside := testPrivateRoot(t), testPrivateRoot(t)
	alias := filepath.Join(parent, "alias")
	createTestJunction(t, alias, outside)
	for _, path := range []string{alias, filepath.Join(alias, "cell")} {
		_, err := New(Config{Root: path})
		assertPrivateRefusal(t, err, path)
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatal("junction root validation mutated the target")
	}
}

func TestWindowsSymlinkStateFileRefused(t *testing.T) {
	c, _ := unitCell(t)
	path := c.path("manifest.json")
	witness, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("cannot read synthetic manifest fixture")
	}
	outside := filepath.Join(testPrivateRoot(t), "manifest.json")
	if err := testWriteFile(outside, witness, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		if errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD) {
			t.Skip("LC-S2-PW3: Windows state symlink fixture privilege unavailable; mandatory junction coverage remains active")
		}
		t.Fatal("cannot create state symlink fixture")
	}
	_, err = readManifest(c.cfg.Root)
	assertPrivateRefusal(t, err, path)
	assertPrivateRefusal(t, writeAtomic(path, map[string]bool{"changed": true}, nil), path)
	data, err := os.ReadFile(outside)
	if err != nil || string(data) != string(witness) {
		t.Fatal("symlink refusal changed outside state")
	}
}

func TestWindowsUnsupportedRootsRefusedBeforeMutation(t *testing.T) {
	parent := testPrivateRoot(t)
	paths := []string{
		`\\localhost\C$\localcell-test`, `\\?\C:\localcell-test`, `\\.\C:\localcell-test`,
		`relative-cell`, `C:drive-relative-cell`, filepath.Join(parent, strings.Repeat("x", 260)),
	}
	for _, name := range []string{"CON", "con.txt", "PRN", "AUX", "NUL", "COM1", "LPT9", "COM¹", "LPT²", "cell.", "cell ", "cell:stream"} {
		paths = append(paths, filepath.Join(parent, name))
	}
	before := windowsTreeSnapshot(t, parent)
	for _, path := range paths {
		_, err := New(Config{Root: path})
		assertPrivateRefusal(t, err, path)
	}
	if !reflect.DeepEqual(before, windowsTreeSnapshot(t, parent)) {
		t.Fatal("invalid root validation changed the parent directory")
	}
}

func TestWindowsAtomicPublicationRevalidatesTargetAndTemporary(t *testing.T) {
	for _, boundary := range []string{"broadened-target", "replaced-target", "broadened-temporary"} {
		t.Run(boundary, func(t *testing.T) {
			c, _ := unitCell(t)
			path := c.path("run", "state.json")
			const before = "synthetic original state"
			const replacement = "synthetic replacement state"
			if err := testWriteFile(path, []byte(before), 0o600); err != nil {
				t.Fatal(err)
			}
			want := before
			err := writeAtomicBytes(path, []byte("synthetic attempted publication"), func() error {
				target := path
				switch boundary {
				case "replaced-target":
					if err := os.Remove(path); err != nil {
						return err
					}
					if err := testWriteFile(path, []byte(replacement), 0o600); err != nil {
						return err
					}
					want = replacement
				case "broadened-temporary":
					pending, err := filepath.Glob(c.path("run", ".pending-*"))
					if err != nil || len(pending) != 1 {
						t.Fatal("publication did not stage exactly one temporary")
					}
					target = pending[0]
				}
				setTestDACL(t, target, "(A;;FR;;;WD)", false, false)
				return nil
			})
			assertPrivateRefusal(t, err, path)
			data, err := os.ReadFile(path)
			if err != nil || string(data) != want {
				t.Fatal("publication replaced a changed or exposed object")
			}
		})
	}
}

func TestWindowsAtomicPublicationRefusesRunJunctionReplacement(t *testing.T) {
	c, _ := unitCell(t)
	outside := testPrivateRoot(t)
	path := c.path("run", "state.json")
	const before = "synthetic original state"
	const witness = "synthetic outside witness"
	if err := testWriteFile(path, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	var outsidePending string
	err := writeAtomicBytes(path, []byte("synthetic attempted publication"), func() error {
		pending, err := filepath.Glob(c.path("run", ".pending-*"))
		if err != nil || len(pending) != 1 {
			t.Fatal("publication did not stage exactly one temporary")
		}
		outsidePending = filepath.Join(outside, filepath.Base(pending[0]))
		if err := testWriteFile(outsidePending, []byte(witness), 0o600); err != nil {
			return err
		}
		if err := testWriteFile(filepath.Join(outside, "state.json"), []byte(witness), 0o600); err != nil {
			return err
		}
		if err := os.Rename(c.path("run"), c.path("retained-run")); err != nil {
			return err
		}
		createTestJunction(t, c.path("run"), outside)
		return nil
	})
	assertPrivateRefusal(t, err, path)
	data, err := os.ReadFile(c.path("retained-run", "state.json"))
	if err != nil || string(data) != before {
		t.Fatal("run replacement changed the retained original state")
	}
	for _, path := range []string{outsidePending, filepath.Join(outside, "state.json")} {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != witness {
			t.Fatal("publication or temporary cleanup followed the replaced run junction")
		}
	}
}

func TestWindowsAtomicPublicationWithOpenReaders(t *testing.T) {
	for _, shareDelete := range []bool{true, false} {
		name := "sharing-denied-preserves-state"
		if shareDelete {
			name = "open-reader-retains-old-state"
		}
		t.Run(name, func(t *testing.T) {
			c, _ := unitCell(t)
			path := c.path("run", "state.json")
			const before = "synthetic old reader state"
			const after = "synthetic new published state"
			if err := testWriteFile(path, []byte(before), 0o600); err != nil {
				t.Fatal(err)
			}
			pointer, err := windows.UTF16PtrFromString(path)
			if err != nil {
				t.Fatal("cannot encode reader fixture path")
			}
			share := uint32(windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE)
			if shareDelete {
				share |= windows.FILE_SHARE_DELETE
			}
			handle, err := windows.CreateFile(pointer, windows.GENERIC_READ, share, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
			if err != nil {
				t.Fatal("cannot create native reader fixture")
			}
			reader := os.NewFile(uintptr(handle), path)
			defer func() { _ = reader.Close() }()
			err = writeAtomicBytes(path, []byte(after), nil)
			want := after
			if shareDelete {
				if err != nil {
					t.Error("publication failed with a reader permitting replacement")
					t.Fatal(err)
				}
			} else {
				assertPrivateRefusal(t, err, path)
				want = before
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != want {
				t.Fatal("publication or sharing denial lost the expected durable state")
			}
			data, err = io.ReadAll(reader)
			if err != nil || string(data) != before {
				t.Fatal("publication changed bytes visible through an existing reader")
			}
			pending, err := filepath.Glob(c.path("run", ".pending-*"))
			if err != nil || len(pending) != 0 {
				t.Fatal("publication or sharing denial left a temporary record")
			}
		})
	}
}
