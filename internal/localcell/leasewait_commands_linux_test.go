//go:build linux

package localcell

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// leaseCommandRow is one LC-LEASE-2D declaration on a cell leaseCommandPlant
// prepared, met by a lease never let go on leased ("credential": the server's;
// empty: taken at the credential seam's first open, after the call wrote every
// credential); refusal is HEAD's text at the ceiling (empty for the diagnostic,
// which returns none); kept checks what the cut left from the bytes before;
// finish, where set, must then finish it (R7).
type leaseCommandRow struct {
	name, prepare, leased, refusal string
	lease                          int
	call                           func(ctx context.Context, c *Cell, m *Manifest) (any, error)
	kept                           func(c *Cell, m *Manifest, before map[string][]byte) bool
	finish                         func(c *Cell, m *Manifest) bool
}

// leaseCommandRead reads what a row compares before and after its call.
func leaseCommandRead(c *Cell, leased string) map[string][]byte {
	files := map[string][]byte{}
	for _, name := range []string{"manifest.json", "journal/operations.jsonl", "run/postgres.password", "logs/server.log", "pg/PG_VERSION", leased} {
		files[name], _ = os.ReadFile(c.path(filepath.FromSlash(name)))
	}
	return files
}

func leaseCommandRows() []leaseCommandRow {
	journal, invalid, refused := "cannot open local cell journal", "local cell password file is invalid", ErrLocalPrincipalCredentialsRefused.Error()
	same := func(c *Cell, _ *Manifest, before map[string][]byte) bool {
		for name, data := range before {
			if now, _ := os.ReadFile(c.path(filepath.FromSlash(name))); !bytes.Equal(now, data) {
				return false
			}
		}
		return true
	}
	open := func(operation string, written func(c *Cell, m, disk *Manifest, before map[string][]byte) bool) func(*Cell, *Manifest, map[string][]byte) bool {
		return func(c *Cell, m *Manifest, before map[string][]byte) bool {
			disk, err := readManifest(c.cfg.Root)
			now, readErr := os.ReadFile(c.path("journal", "operations.jsonl"))
			progress, journalErr := c.readJournal(context.Background())
			return err == nil && readErr == nil && journalErr == nil && bytes.Equal(before["journal/operations.jsonl"], now) &&
				progress.pending != nil && progress.pending.Operation == operation && written(c, m, disk, before)
		}
	}
	repaired := func(c *Cell, _ *Manifest) bool {
		if c.Repair(context.Background()) != nil {
			return false
		}
		inspection, err := c.Inspect(context.Background())
		return err == nil && inspection.Interrupted == nil && !inspection.JournalTailIncomplete
	}
	credentials := func(c *Cell) map[string][]byte {
		files := map[string][]byte{}
		for _, purpose := range principalCredentialPurposes {
			path, _ := c.credentialPath(purpose)
			if info, err := os.Lstat(path); err == nil && privatePrincipalCredential(path, info) {
				files[path], _ = os.ReadFile(path)
			}
		}
		return files
	}
	return []leaseCommandRow{
		{"password", "", "run/postgres.password", invalid, unix.F_WRLCK, func(ctx context.Context, c *Cell, _ *Manifest) (any, error) { return c.password(ctx) }, same, nil},
		{"logFile", "", "logs/server.log", "cannot open local cell log", unix.F_RDLCK, func(ctx context.Context, c *Cell, _ *Manifest) (any, error) { return c.logFile(ctx, "server") }, same, nil},
		{"childEnvironment", "", "run/postgres.password", invalid, unix.F_WRLCK, func(ctx context.Context, c *Cell, m *Manifest) (any, error) {
			return c.childEnvironment(ctx, m, "server")
		}, same, nil},
		{"credential", "selected", "credential", refused, unix.F_WRLCK, func(ctx context.Context, c *Cell, m *Manifest) (any, error) {
			return c.credentialPassword(ctx, m, credentialServer)
		}, same, nil},
		{"validate", "selected", "credential", refused, unix.F_WRLCK, func(ctx context.Context, c *Cell, m *Manifest) (any, error) {
			return nil, c.validatePrincipalPasswords(ctx, m)
		}, same, nil},
		// The draft's partial set is refused before any write (by design); a cell
		// holding none writes all six, then the first validation open meets the lease.
		{"make_after_write", "fresh", "", refused, unix.F_WRLCK, func(ctx context.Context, c *Cell, m *Manifest) (any, error) {
			return nil, c.makePrincipalPasswords(ctx, m)
		}, func(c *Cell, _ *Manifest, _ map[string][]byte) bool {
			return len(credentials(c)) == len(principalCredentialPurposes)
		}, func(c *Cell, m *Manifest) bool {
			written := credentials(c)
			if c.makePrincipalPasswords(context.Background(), m) != nil || len(written) != len(principalCredentialPurposes) {
				return false
			}
			entries, err := os.ReadDir(c.path("run", "credentials"))
			now := credentials(c)
			for path, data := range written {
				if !bytes.Equal(now[path], data) {
					return false
				}
			}
			return err == nil && len(entries) == len(written)
		}},
		{"reinstall_available", "retained", "pg/PG_VERSION", errReinstallState.Error(), unix.F_WRLCK, func(ctx context.Context, c *Cell, m *Manifest) (any, error) {
			progress, _ := c.readJournal(context.Background())
			return nil, c.reinstallAvailable(ctx, m, progress)
		}, same, nil},
		{"admit_reinstall", "retained", "pg/PG_VERSION", errReinstallState.Error(), unix.F_WRLCK, func(ctx context.Context, c *Cell, m *Manifest) (any, error) {
			return c.admitReinstall(ctx, ReinstallOptions{InstallationID: m.InstallationID})
		}, same, nil},
		{"finish_reinstall", "reinstall", "journal/operations.jsonl", journal, unix.F_RDLCK, func(ctx context.Context, c *Cell, m *Manifest) (any, error) {
			return nil, c.finishReinstall(ctx, m)
		}, open("reinstall", func(_ *Cell, _, disk *Manifest, _ map[string][]byte) bool { return disk.LastOperation == "reinstall" }), repaired},
		{"ports_after_manifest", "start", "journal/operations.jsonl", journal, unix.F_RDLCK, func(ctx context.Context, c *Cell, m *Manifest) (any, error) {
			return nil, c.prepareComponentPortsFor(ctx, "start", m, "server", false, true)
		}, open("start", func(c *Cell, m, disk *Manifest, before map[string][]byte) bool {
			now, err := os.ReadFile(c.path("manifest.json"))
			return err == nil && disk.Ports == m.Ports && !bytes.Equal(now, before["manifest.json"])
		}), repaired},
		{"diagnostic", "", "logs/server.log", "", unix.F_WRLCK, func(ctx context.Context, c *Cell, _ *Manifest) (any, error) {
			return c.lastValueFreeDiagnostic(ctx, "server", 0), nil
		}, same, nil},
	}
}

// Each LC-LEASE-2D declaration on a lease never let go (ceiling 2 s here) whose
// context is cancelled 200 ms in returns the cut well before the ceiling, never
// HEAD's refusal (the diagnostic: none, as OQ-F rules), and leaves what a crash
// at that point leaves (R7); then Repair, or the next install, finishes it.
// background: context.Background() with the kept holder (ceiling 300 ms) keeps
// HEAD's refusal at the ceiling.
func TestLeaseWaitCommands(t *testing.T) {
	for _, row := range leaseCommandRows() {
		t.Run(row.name, func(t *testing.T) {
			t.Run("background", func(t *testing.T) { leaseCommandRun(t, row, true) })
			leaseCommandRun(t, row, false)
		})
	}
}

func leaseCommandRun(t *testing.T, row leaseCommandRow, background bool) {
	c, m := leaseCommandPlant(t, row.prepare)
	leased := row.leased
	if role, _ := credentialRole(credentialServer); leased == "credential" {
		leased = "run/credentials/" + role
	}
	before, l := leaseCommandRead(c, leased), leaseHolder(t, "kept", row.lease)
	if !background {
		leaseWait.ceiling = 2 * time.Second
	}
	if leased != "" {
		leaseTaken(t, l, c.path(filepath.FromSlash(leased)))
	} else {
		opener, taken := openPrincipalCredentialFile, false
		t.Cleanup(func() { openPrincipalCredentialFile = opener })
		openPrincipalCredentialFile = func(ctx context.Context, name string, flags int, mode os.FileMode) (*os.File, error) {
			if !taken {
				taken = true
				l.take(name)
			}
			return opener(ctx, name, flags, mode)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	point := make(chan time.Time, 1)
	if background {
		ctx = context.Background()
	} else {
		time.AfterFunc(200*time.Millisecond, func() { point <- time.Now(); cancel() })
	}
	value, started := any(nil), time.Now()
	err := readerWatchdog(t, func() (err error) { value, err = row.call(ctx, c, m); return err }, l.release)
	returned, cut := time.Now(), (*leaseWaitCut)(nil)
	if f, ok := value.(*os.File); ok && f != nil {
		_ = f.Close()
	}
	if !l.do(func(uintptr) {}) {
		t.Fatal("this kernel or sandbox refused the lease the row needs")
	}
	if _, broken := leaseContextBroken(l); !broken {
		t.Fatal("the holder never saw the break of the command row lease")
	}
	l.close() // a plain read of a write-leased file would wait out the kernel's break
	if row.refusal == "" && (err != nil || value != "") {
		t.Fatal("the leased diagnostic row gave a diagnostic or an error")
	}
	if background {
		if errors.As(err, &cut) || row.refusal != "" && (err == nil || err.Error() != row.refusal) {
			t.Fatal("the placeholder context did not keep the command row refusal")
		}
		if returned.Sub(started) < leaseKeptAfter {
			t.Fatal("the placeholder context did not wait for the command row lease")
		}
		return
	}
	if row.refusal != "" && !errors.As(err, &cut) {
		t.Fatal("the command row declaration did not return the lease wait cut")
	} else if row.refusal != "" && !errors.Is(err, context.Canceled) {
		t.Fatal("the command row cut does not carry the context error")
	}
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, os.ErrNotExist) {
		t.Fatal("the command row cut reads as a lease refusal or an absent file")
	}
	if at := <-point; returned.Before(at) || returned.After(at.Add(leaseWait.ceiling/2)) {
		t.Fatal("the command row declaration did not return soon after its context was done")
	}
	if !row.kept(c, m, before) {
		t.Fatal("the command row cut did not leave what a crash at that point leaves")
	}
	if row.finish != nil && !row.finish(c, m) {
		t.Fatal("repair or the next install did not finish what the command row cut left")
	}
}
