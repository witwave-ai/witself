//go:build windows

package runtimeconfig

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// The holds the journal repair takes: each property has its own text, and a
// fixture step that fails says it is the fixture.
func TestWindowsPrivateHold(t *testing.T) {
	check := func(ok bool, failure string) {
		t.Helper()
		if !ok {
			t.Fatal(failure)
		}
	}
	const fixture = "cannot create the hold fixture"
	dir := filepath.Join(t.TempDir(), "cell")
	file, junction := filepath.Join(dir, "journal", "log"), filepath.Join(dir, "junction")
	for _, made := range []string{dir, filepath.Join(dir, "journal"), junction} {
		check(WindowsPrivateMkdir(made) == nil, fixture)
	}
	makeSpoolJunction(t, junction, filepath.Join(dir, "journal"))
	writer, err := WindowsPrivateOpenFile(file, os.O_WRONLY|os.O_CREATE)
	check(err == nil && writer.Close() == nil, fixture)
	single, err := WindowsPrivateNameIdentity(file)
	check(err == nil, "the identity by name could not read a private file")
	check(single.Links == 1, "the identity by name misread a single-link file")
	check(!single.DeletePending, "the identity by name reported a pending deletion")
	check(single.Size == 0, "the identity by name misread the end of file")
	check(os.Link(file, filepath.Join(dir, "alias")) == nil, fixture)
	root, err := WindowsPrivateHoldPath(dir)
	check(err == nil, "the path hold refused an existing private directory")
	defer func() { _ = root.Close() }()
	check(WindowsPrivateValidateFile(root, true) == nil, "the path hold did not return the private directory")
	_, err = WindowsPrivateHoldPath(filepath.Join(dir, "missing"))
	check(errors.Is(err, os.ErrNotExist), "a path hold of a missing directory did not report absence")
	_, err = WindowsPrivateHoldPath(junction)
	check(err != nil, "a path hold followed or accepted a junction")
	check(os.Rename(dir, dir+".moved") != nil, "a held directory was renamed")
	reparse, err := WindowsPrivateHold(root, "junction", true)
	check(err == nil, "a hold refused to open a junction as itself")
	identity, err := WindowsPrivateIdentity(reparse)
	check(err == nil, "the identity by handle could not read a held junction")
	check(identity.Attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0, "a hold followed a junction instead of opening it as itself")
	check(WindowsPrivateValidateFile(reparse, true) != nil, "a held junction was validated as private")
	check(reparse.Close() == nil, fixture)
	journal, err := WindowsPrivateHold(root, "journal", true)
	check(err == nil, "cannot hold a directory relative to its held parent")
	defer func() { _ = journal.Close() }()
	// Nothing below journal/ is open yet: only its own hold can fence it.
	check(os.Rename(filepath.Join(dir, "journal"), filepath.Join(dir, "journal.moved")) != nil, "a directory hold let its name be renamed")
	for _, name := range []string{"", ".", "..", "a/b", `a\b`, "log:x", "CON", "log."} {
		if held, err := WindowsPrivateHold(journal, name, false); err == nil {
			_ = held.Close()
			t.Fatal("a malformed hold name was opened")
		}
	}
	_, err = WindowsPrivateHold(journal, "missing", false)
	check(errors.Is(err, windows.ERROR_FILE_NOT_FOUND), "a hold of a missing name did not report ERROR_FILE_NOT_FOUND")
	_, err = os.Lstat(filepath.Join(dir, "journal", "missing"))
	check(errors.Is(err, os.ErrNotExist), "a hold created a missing name")
	flush, err := WindowsPrivateOpenFlush(journal, "log")
	check(err == nil, "the flush open refused an existing journal")
	defer func() { _ = flush.Close() }()
	check(flush.Sync() == nil, "the flush open could not flush the journal")
	check(errors.Is(flush.Truncate(0), windows.ERROR_ACCESS_DENIED), "the flush open did not refuse truncation with access denied")
	peer, err := os.OpenFile(file, os.O_RDWR, 0)
	check(err == nil, "the flush open did not share reading and writing")
	check(peer.Close() == nil && flush.Close() == nil, fixture)
	refusal := func(parent *os.File, name string) error {
		unexpected, err := WindowsPrivateOpenFlush(parent, name)
		if err == nil {
			_ = unexpected.Close()
		}
		return err
	}
	check(refusal(root, "journal") != nil, "the flush open accepted a directory")
	check(errors.Is(refusal(journal, "missing"), windows.ERROR_FILE_NOT_FOUND), "the flush open did not report the missing journal")
	_, err = os.Lstat(filepath.Join(dir, "journal", "missing"))
	check(errors.Is(err, os.ErrNotExist), "the flush open created a missing journal")
	held, err := WindowsPrivateHold(journal, "log", false)
	check(err == nil, "cannot hold a file relative to its held parent")
	defer func() { _ = held.Close() }()
	if writer, err := WindowsPrivateOpenFile(file, os.O_WRONLY|os.O_APPEND); !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		if err == nil {
			_ = writer.Close()
		}
		t.Fatal("the file hold admitted a writer")
	}
	check(os.Rename(file, file+".moved") != nil, "the file hold let its name be renamed")
	if second, err := WindowsPrivateHold(journal, "log", false); !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		if err == nil {
			_ = second.Close()
		}
		t.Fatal("a second file hold was opened")
	}
	identity, err = WindowsPrivateIdentity(held)
	check(err == nil, "the identity by handle could not read a held file")
	named, err := WindowsPrivateNameIdentity(file)
	check(err == nil, "the identity by name could not read a held file")
	check(identity.Volume == named.Volume && identity.Index == named.Index, "the identity by handle and by name disagree")
	check(identity.Index == single.Index, "the identity by handle names another file than the name did")
	check(identity.Links == 2, "the link count by handle missed a second name")
}
