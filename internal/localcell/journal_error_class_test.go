package localcell

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

// Only literal classes may leave this helper. Error text, operation names and
// paths can contain private data, including in wrappers around known sentinels.
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
		}
	}

	pathClass := "other"
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		// Recognize either separator so the Windows cases run on Unix too.
		base := pathErr.Path[strings.LastIndexAny(pathErr.Path, `/\`)+1:]
		switch base {
		case "operations.jsonl", "journal":
			pathClass = "journal"
		case "manifest.json":
			pathClass = "manifest"
		case ".owner.lock", "operation.lock":
			pathClass = "lock"
		case "pg", "postmaster.pid", "postgres.json", "retained-data":
			pathClass = "pg"
		}
	}
	return "sentinel=" + sentinel + " windows_errno=" + windowsErrno + " path=" + pathClass
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
		{"unknown", syscall.Errno(12345), "other"},
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
