//go:build windows

package local

import (
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/sys/windows"
)

// This candidate is not adopted until native private-storage, flush and recovery
// tests pass. The journal owns these handles; unrelated local storage is unchanged.
type accountProvisionDirectoryPin struct {
	path    string
	file    *os.File
	info    os.FileInfo
	private bool
}

type accountProvisionDirectoryPins struct {
	directories []accountProvisionDirectoryPin
	anchor      int
}

func (pins *accountProvisionDirectoryPins) close() error {
	if pins == nil {
		return nil
	}
	var first error
	for i := len(pins.directories) - 1; i >= 0; i-- {
		if err := pins.directories[i].file.Close(); err != nil && first == nil {
			first = ErrAccountProvisionJournalStorage
		}
	}
	pins.directories = nil
	return first
}

func accountProvisionHome(home string) (string, error) {
	home, err := filepath.Abs(home)
	if err != nil {
		return "", ErrAccountProvisionJournalUnsafe
	}
	volume := filepath.VolumeName(home)
	if len(volume) != 2 || volume[1] != ':' || home == volume+string(filepath.Separator) {
		return "", ErrAccountProvisionJournalUnsafe
	}
	return filepath.Clean(home), nil
}

// FileInfo is structural metadata on Windows. Authorization always follows on
// the opened handle; mode bits are never substituted for the descriptor policy.
func privateAccountProvisionJournalDirectory(info os.FileInfo) bool {
	return info != nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0
}

func privateRegularAccountProvisionJournalFile(info os.FileInfo) bool {
	return info != nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0
}

func openAccountProvisionPrivateFile(path string, create bool) (*os.File, bool, error) {
	if create {
		file, err := createAccountProvisionWindowsPrivateFile(path)
		if err == nil {
			return file, true, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, false, err
		}
	}
	// OPEN_EXISTING retains the existing non-reparse/read-write/no-delete-share
	// behavior. Existing files are validated, never repaired.
	file, _, err := openLocalLockFileNoFollow(path, false)
	if err != nil {
		return nil, false, err
	}
	info, err := file.Stat()
	if err == nil {
		err = validateAccountProvisionPrivateFileHandle(file, path, info)
	}
	if err != nil {
		_ = file.Close()
		return nil, false, ErrAccountProvisionJournalUnsafe
	}
	return file, false, nil
}

func validateAccountProvisionPrivateFileHandle(file *os.File, path string, expected os.FileInfo) error {
	return validateAccountProvisionWindowsPrivateHandle(file, path, expected, false)
}

func validateAccountProvisionPrivatePath(path string, expected os.FileInfo) error {
	file, _, err := openAccountProvisionPrivateFile(path, false)
	if err != nil {
		return ErrAccountProvisionJournalUnsafe
	}
	err = validateAccountProvisionPrivateFileHandle(file, path, expected)
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return ErrAccountProvisionJournalStorage
	}
	return nil
}

func createAccountProvisionPrivateTemp(directory, pattern string) (*os.File, error) {
	star := strings.LastIndexByte(pattern, '*')
	if star < 0 || strings.ContainsAny(pattern, `/\`) {
		return nil, ErrAccountProvisionJournalUnsafe
	}
	for range 100 {
		path := filepath.Join(directory, pattern[:star]+rand.Text()+pattern[star+1:])
		file, err := createAccountProvisionWindowsPrivateFile(path)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		return file, err
	}
	return nil, ErrAccountProvisionJournalStorage
}

func prepareAccountProvisionPrivateTemp(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return ErrAccountProvisionJournalStorage
	}
	return validateAccountProvisionPrivateFileHandle(file, file.Name(), info)
}

// Delete through the exact validated handle, not a later pathname lookup. This
// removes only the opened link, including an owned temporary hard-link name.
func removeAccountProvisionPrivateFile(path string, expected os.FileInfo) error {
	if expected == nil {
		return ErrAccountProvisionJournalUnsafe
	}
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return ErrAccountProvisionJournalUnsafe
	}
	handle, err := windows.CreateFile(pointer, windows.DELETE|windows.GENERIC_READ|windows.READ_CONTROL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return ErrAccountProvisionJournalStorage
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return ErrAccountProvisionJournalStorage
	}
	if err := validateAccountProvisionPrivateFileHandle(file, path, expected); err != nil {
		_ = file.Close()
		return err
	}
	// FILE_DISPOSITION_INFO is one BOOLEAN. No POSIX/delete-ignore flags or ACL
	// changes are used. All ordinary readers/writers must have closed first.
	remove := byte(1)
	err = windows.SetFileInformationByHandle(handle, windows.FileDispositionInfo, &remove, 1)
	runtime.KeepAlive(file)
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return ErrAccountProvisionJournalStorage
	}
	return nil
}

func accountProvisionNTFSVolume(file *os.File) (string, error) {
	defer func() { runtime.KeepAlive(file) }()
	var filesystem [261]uint16
	var flags uint32
	handle := windows.Handle(file.Fd())
	if err := windows.GetVolumeInformationByHandle(handle, nil, 0, nil, nil, &flags, &filesystem[0], uint32(len(filesystem))); err != nil {
		return "", ErrAccountProvisionJournalUnsafe
	}
	const required = windows.FILE_PERSISTENT_ACLS | windows.FILE_SUPPORTS_HARD_LINKS
	if windows.UTF16ToString(filesystem[:]) != "NTFS" || flags&required != required || flags&windows.FILE_READ_ONLY_VOLUME != 0 {
		return "", ErrAccountProvisionJournalUnsafe
	}
	var resolved [32768]uint16
	n, err := windows.GetFinalPathNameByHandle(handle, &resolved[0], uint32(len(resolved)), 0x1)
	if err != nil || n == 0 || n >= uint32(len(resolved)) {
		return "", ErrAccountProvisionJournalUnsafe
	}
	path := windows.UTF16ToString(resolved[:n])
	end := strings.Index(path, `}\`)
	if !strings.HasPrefix(path, `\\?\Volume{`) || end != 47 {
		return "", ErrAccountProvisionJournalUnsafe
	}
	if _, err := windows.GUIDFromString(path[len(`\\?\Volume`) : end+1]); err != nil {
		return "", ErrAccountProvisionJournalUnsafe
	}
	return strings.ToLower(path[:end+1]), nil
}

func validateAccountProvisionDirectoryPin(pin accountProvisionDirectoryPin) error {
	defer func() { runtime.KeepAlive(pin.file) }()
	opened, err := pin.file.Stat()
	linked, linkErr := os.Lstat(pin.path)
	var information windows.ByHandleFileInformation
	if err != nil || linkErr != nil || !privateAccountProvisionJournalDirectory(opened) ||
		!privateAccountProvisionJournalDirectory(linked) || !os.SameFile(pin.info, opened) || !os.SameFile(opened, linked) ||
		windows.GetFileInformationByHandle(windows.Handle(pin.file.Fd()), &information) != nil ||
		information.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 || information.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return ErrAccountProvisionJournalUnsafe
	}
	if pin.private {
		return validateAccountProvisionWindowsPrivateHandle(pin.file, pin.path, pin.info, true)
	}
	return nil
}

func openAccountProvisionDirectoryPin(path string, private bool) (accountProvisionDirectoryPin, error) {
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return accountProvisionDirectoryPin{}, ErrAccountProvisionJournalUnsafe
	}
	// MS-FSA sharing checks exclude metadata-only handles. FILE_TRAVERSE
	// participates without requesting directory listing or mutation rights.
	handle, err := windows.CreateFile(pointer, windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES|windows.FILE_TRAVERSE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return accountProvisionDirectoryPin{}, err
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return accountProvisionDirectoryPin{}, ErrAccountProvisionJournalStorage
	}
	info, err := file.Stat()
	pin := accountProvisionDirectoryPin{path: path, file: file, info: info, private: private}
	if err == nil {
		err = validateAccountProvisionDirectoryPin(pin)
	}
	if err != nil {
		_ = file.Close()
		return accountProvisionDirectoryPin{}, ErrAccountProvisionJournalUnsafe
	}
	return pin, nil
}

func syncAccountProvisionDirectoryPin(pin accountProvisionDirectoryPin) error {
	if err := validateAccountProvisionDirectoryPin(pin); err != nil {
		return err
	}
	pointer, err := windows.UTF16PtrFromString(pin.path)
	if err != nil {
		return ErrAccountProvisionJournalUnsafe
	}
	// The structural pin remains open. Only this exact durability boundary
	// requires write access; no write handle is requested for higher ancestors.
	handle, err := windows.CreateFile(pointer, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return ErrAccountProvisionJournalStorage
	}
	file := os.NewFile(uintptr(handle), pin.path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return ErrAccountProvisionJournalStorage
	}
	flushPin := accountProvisionDirectoryPin{path: pin.path, file: file, info: pin.info, private: pin.private}
	err = validateAccountProvisionDirectoryPin(flushPin)
	if err == nil {
		_, err = accountProvisionNTFSVolume(file)
	}
	if err == nil {
		err = windows.FlushFileBuffers(handle)
		runtime.KeepAlive(file)
	}
	if err == nil {
		err = validateAccountProvisionDirectoryPin(flushPin)
	}
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return ErrAccountProvisionJournalStorage
	}
	return validateAccountProvisionDirectoryPin(pin)
}

func syncAccountProvisionJournalDirectory(directory string) error {
	pin, err := openAccountProvisionDirectoryPin(directory, false)
	if err != nil {
		return err
	}
	err = syncAccountProvisionDirectoryPin(pin)
	closeErr := pin.file.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func pinAccountProvisionDirectories(home, directory string, create bool) (*accountProvisionDirectoryPins, error) {
	return pinAccountProvisionDirectoriesWithSync(home, directory, create, syncAccountProvisionDirectoryPin)
}

// The fixed anchor is always the selected home's immediate parent. Its existence
// is a Windows prerequisite, independent of which children a prior attempt made.
// Per-call sync injection exercises ambiguous creation without a global hook.
func pinAccountProvisionDirectoriesWithSync(home, directory string, create bool,
	syncPin func(accountProvisionDirectoryPin) error,
) (*accountProvisionDirectoryPins, error) {
	home, err := accountProvisionHome(home)
	if err != nil {
		return nil, err
	}
	directory, err = filepath.Abs(directory)
	if err != nil {
		return nil, ErrAccountProvisionJournalUnsafe
	}
	directory = filepath.Clean(directory)
	relative, err := filepath.Rel(home, directory)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, ErrAccountProvisionJournalUnsafe
	}
	volume := filepath.VolumeName(directory)
	if !strings.EqualFold(volume, filepath.VolumeName(home)) {
		return nil, ErrAccountProvisionJournalUnsafe
	}
	root := volume + string(filepath.Separator)
	paths := []string{root}
	current := root
	for _, part := range strings.Split(strings.TrimPrefix(directory, root), string(filepath.Separator)) {
		if part == "" || strings.ContainsAny(part, ":/") {
			return nil, ErrAccountProvisionJournalUnsafe
		}
		current = filepath.Join(current, part)
		paths = append(paths, current)
	}
	homeIndex := -1
	for i, path := range paths {
		if strings.EqualFold(path, home) {
			homeIndex = i
			break
		}
	}
	if homeIndex < 1 {
		return nil, ErrAccountProvisionJournalUnsafe
	}
	pins := &accountProvisionDirectoryPins{anchor: homeIndex - 1}
	accepted := false
	defer func() {
		if !accepted {
			_ = pins.close()
		}
	}()
	var expectedVolume string
	for i, path := range paths {
		private := i >= homeIndex
		pin, openErr := openAccountProvisionDirectoryPin(path, private)
		created := false
		if errors.Is(openErr, os.ErrNotExist) {
			if !private {
				return nil, ErrAccountProvisionJournalUnsafe
			}
			if !create {
				return nil, os.ErrNotExist
			}
			file, createErr := createAccountProvisionWindowsPrivateDirectory(path)
			if errors.Is(createErr, os.ErrExist) {
				pin, openErr = openAccountProvisionDirectoryPin(path, true)
			} else if createErr != nil {
				return nil, createErr
			} else {
				info, statErr := file.Stat()
				pin = accountProvisionDirectoryPin{path: path, file: file, info: info, private: true}
				openErr = statErr
				created = true
				if openErr == nil {
					openErr = validateAccountProvisionDirectoryPin(pin)
				}
				if openErr != nil {
					_ = file.Close()
				}
			}
		}
		if openErr != nil {
			return nil, ErrAccountProvisionJournalUnsafe
		}
		pins.directories = append(pins.directories, pin)
		identity, volumeErr := accountProvisionNTFSVolume(pin.file)
		if volumeErr != nil {
			return nil, volumeErr
		}
		if expectedVolume == "" {
			expectedVolume = identity
		} else if expectedVolume != identity {
			return nil, ErrAccountProvisionJournalUnsafe
		}
		if created {
			if err := syncPin(pin); err != nil {
				return nil, ErrAccountProvisionJournalStorage
			}
			if err := syncPin(pins.directories[i-1]); err != nil {
				return nil, ErrAccountProvisionJournalStorage
			}
		}
	}
	// Reestablish the whole fixed chain on replay, including an already visible
	// home whose publication previously failed its external parent's flush.
	for i := len(pins.directories) - 1; i >= pins.anchor; i-- {
		if err := syncPin(pins.directories[i]); err != nil {
			return nil, ErrAccountProvisionJournalStorage
		}
	}
	for _, pin := range pins.directories {
		if err := validateAccountProvisionDirectoryPin(pin); err != nil {
			return nil, err
		}
	}
	accepted = true
	return pins, nil
}

func pinAccountProvisionCredentialDirectories(home, directory string) (*accountProvisionDirectoryPins, error) {
	return pinAccountProvisionDirectories(home, directory, true)
}

func ensureAccountProvisionJournalDirectories(home, directory string) error {
	pins, err := pinAccountProvisionDirectories(home, directory, true)
	if err != nil {
		return err
	}
	return pins.close()
}

func validateAccountProvisionJournalDirectories(home, directory string) error {
	// Validation is not an extra durability acknowledgement. In particular,
	// publication's post-rename validation must not bypass its injectable final
	// directory-sync boundary by flushing here first. The owning operation pins
	// and syncs its fixed chain before locking or returning a usable candidate.
	pins, err := pinAccountProvisionDirectoriesWithSync(home, directory, false, func(accountProvisionDirectoryPin) error { return nil })
	if err != nil {
		return err
	}
	return pins.close()
}

func ensurePrivateProvisionDirectory(home, directory string) error {
	return ensureAccountProvisionJournalDirectories(home, directory)
}

func syncAccountProvisionLockPublication(file *os.File, directory string, created bool) error {
	return syncAccountProvisionLockPublicationWith(file, directory, created, (*os.File).Sync, syncAccountProvisionJournalDirectory)
}

func syncAccountProvisionLockPublicationWith(file *os.File, directory string, _ bool,
	syncFile func(*os.File) error, syncDirectory func(string) error,
) error {
	// An existing lock can be the result of a failed first file/directory flush.
	// Reopening it never substitutes for the two durability acknowledgements.
	if err := syncFile(file); err != nil {
		return ErrAccountProvisionJournalStorage
	}
	if err := syncDirectory(directory); err != nil {
		return ErrAccountProvisionJournalStorage
	}
	return nil
}

// AvailableAccountProvisionName performs signup's existing name check through
// private managed paths. General local.Load/Available/Save behavior is unchanged.
func AvailableAccountProvisionName(name string) error {
	home, _, err := accountProvisionJournalLocation(name)
	if err != nil {
		return err
	}
	pins, err := pinAccountProvisionDirectories(home, home, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = pins.close() }()
	config, _, err := readProvisionConfig(home)
	if err != nil {
		return err
	}
	if _, exists := config.Accounts[name]; exists {
		return ErrNameTaken
	}
	accounts := filepath.Join(home, "tokens", "accounts")
	accountPins, err := pinAccountProvisionDirectories(home, accounts, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = accountPins.close() }()
	if _, err := os.Lstat(filepath.Join(accounts, name+".token")); err == nil {
		return ErrNameTaken
	} else if !errors.Is(err, os.ErrNotExist) {
		return ErrAccountProvisionJournalStorage
	}
	namedPins, err := pinAccountProvisionDirectories(home, filepath.Join(accounts, name), false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = namedPins.close() }()
	raw, exists, err := readPrivateProvisionFile(filepath.Join(accounts, name, "owner.token"), maxAccountProvisionJournalBytes)
	clear(raw)
	if err != nil {
		return err
	}
	if exists {
		return ErrNameTaken
	}
	return nil
}
