package localcell

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// Emit only fixed labels and native Windows error numbers. Never format an
// error, operation name or path; even a wrapper around a sentinel may be private.
func journalRepairErrorClass(err error) string {
	return journalRepairErrorClassForOS(err, runtime.GOOS)
}

// An explicit OS makes Windows errno handling testable on every host without
// mislabeling Unix EPIPE (32) or EIO (5) as Windows filesystem failures.
func journalRepairErrorClassForOS(err error, goos string) string {
	var matches []string
	for _, candidate := range []struct {
		err   error
		class string
	}{
		{ErrLocked, "locked"},
		{context.DeadlineExceeded, "deadline_exceeded"},
		{context.Canceled, "canceled"},
		{os.ErrNotExist, "not_exist"},
		{os.ErrPermission, "permission"},
		{ErrLocalMigrationPhaseRequired, "local_migration_phase_required"},
		{ErrLocalRuntimeLoginOwnsTables, "local_runtime_login_owns_tables"},
		{ErrLocalPrincipalCredentialsRefused, "local_principal_credentials_refused"},
		{ErrServiceHostActive, "service_host_active"},
		{ErrServiceControlRefused, "service_control_refused"},
		{ErrServiceManagerUnavailable, "service_manager_unavailable"},
		{ErrServiceOwnershipConflict, "service_ownership_conflict"},
		{ErrServiceRegistrationFailed, "service_registration_failed"},
		{ErrServiceRemovalFailed, "service_removal_failed"},
		{ErrServiceDesiredState, "service_desired_state"},
		{ErrServiceShutdownForced, "service_shutdown_forced"},
	} {
		if errors.Is(err, candidate.err) {
			matches = append(matches, candidate.class)
		}
	}
	sentinel := "other"
	if err == nil {
		sentinel = "nil"
	} else if len(matches) > 0 {
		sentinel = strings.Join(matches, ",")
	}

	windowsErrno := "other"
	if goos == "windows" {
		sharing := errors.Is(err, syscall.Errno(32))
		denied := errors.Is(err, syscall.Errno(5))
		switch {
		case sharing && denied:
			windowsErrno = "sharing_violation,access_denied"
		case sharing:
			windowsErrno = "sharing_violation"
		case denied:
			windowsErrno = "access_denied"
		default:
			var errno syscall.Errno
			if errors.As(err, &errno) && uint64(errno) <= math.MaxUint32 {
				windowsErrno = strconv.FormatUint(uint64(errno), 10)
			}
		}
	}

	pathClass := "other"
	step := ""
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		// Recognize either separator so the Windows cases run on Unix too.
		base := pathErr.Path[strings.LastIndexAny(pathErr.Path, `/\`)+1:]
		switch base {
		case "operations.jsonl":
			// A PathError identifies the journal operation, even when another
			// part of Repair performed it. Its Op is never copied to the log.
			switch pathErr.Op {
			case "open":
				step = "open"
			case "stat", "GetFileType", "GetFileInformationByHandle", "GetFileInformationByHandleEx":
				step = "stat"
			case "truncate":
				step = "truncate"
			case "sync":
				step = "flush"
			case "close":
				step = "close"
			}
			fallthrough
		case "journal":
			pathClass = "journal"
		case "manifest.json":
			pathClass = "manifest"
		case ".owner.lock", "operation.lock":
			pathClass = "lock"
		case "pg", "postmaster.pid", "postgres.json", "retained-data":
			pathClass = "pg"
		}
	}
	// Repair returns fresh errors.New values for these failures. Compare only
	// against those complete values without invoking an arbitrary Error method.
	for cause := err; cause != nil && step == ""; cause = errors.Unwrap(cause) {
		switch {
		case reflect.DeepEqual(cause, errors.New("cannot open torn local cell journal")):
			step = "open_or_identity"
		case reflect.DeepEqual(cause, errors.New("cannot truncate torn local cell journal")):
			step = "truncate"
		case reflect.DeepEqual(cause, errors.New("cannot sync repaired local cell journal")):
			step = "flush"
		case reflect.DeepEqual(cause, errors.New("local cell start: revert_stop failed")):
			step = "revert_stop"
		}
	}
	line := "sentinel=" + sentinel + " windows_errno=" + windowsErrno + " path=" + pathClass
	if step != "" {
		line += " step=" + step
	}
	return line
}

type journalErrorTextMustNotBeRead struct{}

func (journalErrorTextMustNotBeRead) Error() string {
	panic("journal repair classifier read private error text")
}

func TestJournalRepairErrorClass(t *testing.T) {
	const other = "sentinel=other windows_errno=other path=other"
	for _, test := range []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, "sentinel=nil windows_errno=other path=other"},
		{"private_text", errors.New("synthetic private error text"), other},
		{"unreadable_text", journalErrorTextMustNotBeRead{}, other},
		{"locked", ErrLocked, "sentinel=locked windows_errno=other path=other"},
		{"deadline", context.DeadlineExceeded, "sentinel=deadline_exceeded windows_errno=other path=other"},
		{"canceled", context.Canceled, "sentinel=canceled windows_errno=other path=other"},
		{"not_exist", os.ErrNotExist, "sentinel=not_exist windows_errno=other path=other"},
		{"permission", os.ErrPermission, "sentinel=permission windows_errno=other path=other"},
		{"migration", ErrLocalMigrationPhaseRequired, "sentinel=local_migration_phase_required windows_errno=other path=other"},
		{"runtime_login", ErrLocalRuntimeLoginOwnsTables, "sentinel=local_runtime_login_owns_tables windows_errno=other path=other"},
		{"principal_credentials", ErrLocalPrincipalCredentialsRefused, "sentinel=local_principal_credentials_refused windows_errno=other path=other"},
		{"host_active", ErrServiceHostActive, "sentinel=service_host_active windows_errno=other path=other"},
		{"control_refused", ErrServiceControlRefused, "sentinel=service_control_refused windows_errno=other path=other"},
		{"manager_unavailable", ErrServiceManagerUnavailable, "sentinel=service_manager_unavailable windows_errno=other path=other"},
		{"ownership_conflict", ErrServiceOwnershipConflict, "sentinel=service_ownership_conflict windows_errno=other path=other"},
		{"registration_failed", ErrServiceRegistrationFailed, "sentinel=service_registration_failed windows_errno=other path=other"},
		{"removal_failed", ErrServiceRemovalFailed, "sentinel=service_removal_failed windows_errno=other path=other"},
		{"desired_state", ErrServiceDesiredState, "sentinel=service_desired_state windows_errno=other path=other"},
		{"shutdown_forced", ErrServiceShutdownForced, "sentinel=service_shutdown_forced windows_errno=other path=other"},
		{"joined", errors.Join(ErrLocked, context.DeadlineExceeded, context.Canceled), "sentinel=locked,deadline_exceeded,canceled windows_errno=other path=other"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if journalRepairErrorClass(test.err) != test.want {
				t.Fatal("journal repair error classifier emitted a non-literal or incorrect class")
			}
			if test.err != nil && test.name != "unreadable_text" {
				if journalRepairErrorClass(fmt.Errorf("synthetic private wrapper: %w", test.err)) != test.want {
					t.Fatal("journal repair error classifier lost a wrapped class")
				}
			}
		})
	}
}

func TestJournalRepairErrorClassWindows(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want string
	}{
		{"sharing", syscall.Errno(32), "sharing_violation"},
		{"denied", syscall.Errno(5), "access_denied"},
		{"wrapped", fmt.Errorf("synthetic private wrapper: %w", syscall.Errno(32)), "sharing_violation"},
		{"joined", errors.Join(syscall.Errno(32), syscall.Errno(5)), "sharing_violation,access_denied"},
		{"unknown", syscall.Errno(12345), "12345"},
		{"wrapped_unknown", fmt.Errorf("private: %w", syscall.Errno(1224)), "1224"},
		{"joined_listed", errors.Join(syscall.Errno(33), syscall.Errno(32)), "sharing_violation"},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, goos := range []string{"windows", "linux", "darwin"} {
				want := " windows_errno=other path=other"
				if goos == "windows" {
					want = " windows_errno=" + test.want + " path=other"
				}
				// The host's syscall.Errno.Is can additionally match os.ErrPermission
				// on Windows; assert the numeric class independently of that mapping.
				if !strings.HasSuffix(journalRepairErrorClassForOS(test.err, goos), want) {
					t.Fatal("journal repair error classifier misclassified a Windows errno")
				}
			}
		})
	}
}

func TestJournalRepairErrorClassWindowsBounds(t *testing.T) {
	for _, row := range []struct {
		number        uint64
		want, failure string
	}{
		{math.MaxUint32, "4294967295", "classifier lost the largest Windows errno"},
		{math.MaxUint32 + 1, "other", "classifier accepted an oversized Windows errno"},
	} {
		t.Run(row.failure, func(t *testing.T) {
			errno := syscall.Errno(row.number)
			if uint64(errno) != row.number {
				t.Skip("native errno cannot represent this Windows test value")
			}
			if journalRepairErrorClassForOS(errno, "windows") != "sentinel=other windows_errno="+row.want+" path=other" {
				t.Fatal(row.failure)
			}
		})
	}
}

func TestJournalRepairErrorClassStep(t *testing.T) {
	const other = "sentinel=other windows_errno=other path=other"
	for _, row := range []struct{ message, want string }{
		{"cannot open torn local cell journal", other + " step=open_or_identity"},
		{"cannot truncate torn local cell journal", other + " step=truncate"},
		{"cannot sync repaired local cell journal", other + " step=flush"},
		{"local cell start: revert_stop failed", other + " step=revert_stop"},
		{"private: cannot truncate torn local cell journal", other},
	} {
		if journalRepairErrorClass(fmt.Errorf("private: %w", errors.New(row.message))) != row.want {
			t.Fatal("journal repair classifier lost or invented a fixed failure step")
		}
	}
	const journal = "sentinel=other windows_errno=1224 path=journal"
	for _, row := range []struct{ op, want string }{
		{"open", journal + " step=open"}, {"stat", journal + " step=stat"},
		{"truncate", journal + " step=truncate"}, {"sync", journal + " step=flush"},
		{"close", journal + " step=close"}, {"private operation", journal},
	} {
		err := &fs.PathError{Op: row.op, Path: `C:\private\operations.jsonl`, Err: syscall.Errno(1224)}
		if journalRepairErrorClassForOS(err, "windows") != row.want {
			t.Fatal("journal repair classifier lost or exposed a journal operation")
		}
	}
	for _, row := range []struct {
		op      string
		cause   error
		step    string
		failure string
	}{
		{"GetFileType", journalErrorTextMustNotBeRead{}, "stat", "classifier lost the GetFileType step"},
		{"GetFileInformationByHandle", journalErrorTextMustNotBeRead{}, "stat", "classifier lost the GetFileInformationByHandle step"},
		{"GetFileInformationByHandleEx", journalErrorTextMustNotBeRead{}, "stat", "classifier lost the GetFileInformationByHandleEx step"},
		{"open", errors.New("cannot sync repaired local cell journal"), "open", "classifier replaced the outer journal step"},
	} {
		t.Run(row.op, func(t *testing.T) {
			err := &fs.PathError{Op: row.op, Path: "operations.jsonl", Err: row.cause}
			if journalRepairErrorClassForOS(err, "windows") != "sentinel=other windows_errno=other path=journal step="+row.step {
				t.Fatal(row.failure)
			}
		})
	}
	err := &fs.PathError{Op: "truncate", Path: "/journal/private-file", Err: journalErrorTextMustNotBeRead{}}
	if journalRepairErrorClass(err) != other {
		t.Fatal("journal repair classifier attributed an unrelated file operation to the journal")
	}
}

func TestJournalRepairErrorClassPath(t *testing.T) {
	for _, test := range []struct{ name, path, want string }{
		{"journal", "/synthetic-private/operations.jsonl", "journal"},
		{"journal_directory", "/synthetic-private/journal", "journal"},
		{"manifest", `C:\synthetic-private\manifest.json`, "manifest"},
		{"owner_lock", "/synthetic-private/.owner.lock", "lock"},
		{"operation_lock", `C:\synthetic-private\run\operation.lock`, "lock"},
		{"pg_directory", "/synthetic-private/pg", "pg"},
		{"postmaster", `C:\synthetic-private\pg\postmaster.pid`, "pg"},
		{"postgres_record", "/synthetic-private/run/postgres.json", "pg"},
		{"retained", "/synthetic-private/pg/retained-data", "pg"},
		{"unknown", "/synthetic-private/unlisted-private-name", "other"},
		{"known_parent_only", "/operations.jsonl/private-child", "other"},
		{"suffix_only", "/synthetic-private/private-manifest.json", "other"},
		{"empty", "", "other"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := fmt.Errorf("synthetic private wrapper: %w", &fs.PathError{
				Op: "synthetic private operation", Path: test.path, Err: os.ErrPermission,
			})
			want := "sentinel=permission windows_errno=other path=" + test.want
			if journalRepairErrorClass(err) != want {
				t.Fatal("journal repair error classifier leaked or misclassified a path")
			}
		})
	}
	linkErr := &os.LinkError{Op: "private", Old: "manifest.json", New: "operations.jsonl", Err: os.ErrPermission}
	if journalRepairErrorClass(linkErr) != "sentinel=permission windows_errno=other path=other" {
		t.Fatal("journal repair error classifier treated a non-PathError as a path")
	}
	combined := &fs.PathError{Op: "private", Path: `C:\private\operations.jsonl`, Err: syscall.Errno(32)}
	if journalRepairErrorClassForOS(combined, "windows") != "sentinel=other windows_errno=sharing_violation path=journal" {
		t.Fatal("journal repair error classifier lost a path-wrapped Windows errno")
	}
}
