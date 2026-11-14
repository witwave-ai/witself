package localcell

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/runtimeconfig"
)

var syncFile = func(f *os.File) error { return f.Sync() }

// syncJournalDirectory syncs journal/ after an ordinary journal append creates
// operations.jsonl. Tests replace it; production always uses syncDirectory.
var syncJournalDirectory = syncDirectory

// syncPrivateDirParent persists a newly created directory's parent entry after
// privacy checks pass. Tests replace it; production always uses syncDirectory.
var syncPrivateDirParent = syncDirectory

// beforePasswordWrite observes the opened password temporary immediately before
// its first data write. It is nil outside tests.
var beforePasswordWrite func(f *os.File)

func privateDir(path string, create bool) error {
	created := false
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) && create {
		if err = makePrivateDir(path); err != nil {
			return errors.New("cannot create private local cell directory")
		}
		created = true
		info, err = os.Lstat(path)
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("local cell directory is missing or not a real directory")
	}
	if !privateOwnedDir(path, info) {
		return errors.New("local cell directory must be private and owned by the current user")
	}
	if created {
		if err := syncPrivateDirParent(filepath.Dir(path)); err != nil {
			return errors.New("cannot sync local cell parent directory")
		}
	}
	return nil
}

func (c *Cell) validateRoot(create bool) error {
	if err := validatePrivateTree(c.cfg.Root); err != nil {
		return err
	}
	// Resolve parent aliases once at the trust boundary; never follow a root or
	// layout symlink. Callers must supply an existing parent directory.
	parent, err := filepath.EvalSymlinks(filepath.Dir(c.cfg.Root))
	if err != nil {
		return errors.New("local cell parent directory is unavailable")
	}
	if filepath.Join(parent, filepath.Base(c.cfg.Root)) != c.cfg.Root {
		return errors.New("local cell parent directory identity changed")
	}
	if err := privateDir(c.cfg.Root, create); err != nil {
		return err
	}
	for _, name := range []string{"pg", "spool", "run", "journal", "logs", "tmp", "home", "recovery"} {
		if _, err := os.Lstat(c.path(name)); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if name == "pg" {
			if err := c.validatePostgresDirectory(); err != nil {
				return err
			}
			continue
		}
		if err := privateDir(c.path(name), false); err != nil {
			return err
		}
	}
	return nil
}

func (c *Cell) layout() error {
	for _, name := range []string{"spool", "run", "journal", "logs", "tmp", "home"} {
		if err := privateDir(c.path(name), true); err != nil {
			return err
		}
	}
	return nil
}

func readJSON(path string, dst any) error {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return os.ErrNotExist
		}
		return errors.New("cannot inspect local cell record")
	}
	if !info.Mode().IsRegular() {
		return errors.New("local cell record must be a regular file")
	}
	f, err := openPrivateFile(path, os.O_RDONLY, 0)
	if err != nil {
		return errors.New("cannot open local cell record")
	}
	defer func() { _ = f.Close() }()
	decoder := json.NewDecoder(io.LimitReader(f, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return errors.New("invalid local cell record")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("invalid trailing local cell record")
	}
	return nil
}

func readManifest(root string) (*Manifest, error) {
	var m Manifest
	if err := readJSON(filepath.Join(root, "manifest.json"), &m); err != nil {
		return nil, err
	}
	if m.LayoutVersion != LayoutVersion {
		return nil, errors.New("unsupported local cell layout version")
	}
	if err := validatePrincipalLayout(&m); err != nil {
		return nil, err
	}
	if m.InstallationID == "" || m.Database != localDatabaseName || m.Role != "localcell" {
		return nil, errors.New("invalid local cell manifest identity")
	}
	if err := validateRuntimeSettings(m.MigrationWait, m.JobRestartPolicy); err != nil {
		return nil, err
	}
	if m.Cleanup != nil && (m.Cleanup.Operation != "remove" || m.Cleanup.Step != "runtime_cleanup" || m.Cleanup.State != "started" || m.Cleanup.At.IsZero()) {
		return nil, errors.New("invalid local cell cleanup journal checkpoint")
	}
	seen := map[int]bool{}
	for _, port := range m.Ports.all() {
		if port < 1 || port > 65535 || seen[port] {
			return nil, errors.New("invalid local cell manifest ports")
		}
		seen[port] = true
	}
	return &m, nil
}

// writeAtomic publishes value as indented JSON with the same shared bounded
// publication retry as writeAtomicBytes.
func writeAtomic(path string, value any, beforeRename func() error) error {
	_, err := publishAtomic(func() error { return writeAtomicOnce(path, value, beforeRename) }, leasePublishHooks{})
	return err
}

// writeAtomicOnce is one complete attempt of writeAtomic: the record encoding
// and one atomic publication. Only the shared retry may repeat it.
func writeAtomicOnce(path string, value any, beforeRename func() error) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return errors.New("cannot encode local cell record")
	}
	return writeAtomicBytesOnce(path, append(data, '\n'), beforeRename)
}

// writeAtomicBytes publishes data as one private atomic record replacement. On
// Windows a concurrent private-tree validation holds each object it checks
// without delete sharing, so replacing the target or its pending source can be
// refused for a moment. Only that private sharing or access refusal is retried,
// within the lease publisher's bounds; every other failure, like every failure
// off Windows, is a single attempt that returns its unchanged error. Each
// attempt is a complete atomic publication with a fresh pending source.
func writeAtomicBytes(path string, data []byte, beforeRename func() error) error {
	_, err := publishAtomic(func() error { return writeAtomicBytesOnce(path, data, beforeRename) }, leasePublishHooks{})
	return err
}

// publishAtomic is the one bounded retry of a private atomic publication. Each
// attempt must be one complete publication; no caller may retry it again, so
// the lease budget can never compound.
func publishAtomic(attempt func() error, h leasePublishHooks) (int, error) {
	return retryPublish(attempt, func(err error) bool { return runtimeconfig.WindowsPrivateTransient(err) }, h)
}

// These recognisers describe createPrivateTemp's two platform generators.
// Keep both available everywhere so each platform tests the complete contract.
func unixPrivateTempName(name string) bool {
	suffix, ok := strings.CutPrefix(name, ".pending-")
	return ok && suffix != "" && strings.Trim(suffix, "0123456789") == ""
}

func windowsPrivateTempName(name string) bool {
	suffix, ok := strings.CutPrefix(name, ".pending-")
	return ok && len(suffix) == 24 && strings.Trim(suffix, "0123456789abcdef") == ""
}

func writeAtomicBytesOnce(path string, data []byte, beforeRename func() error) error {
	if err := validateWriteTarget(path); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return errors.New("local cell record must be a regular file")
	}
	f, err := createPrivateTemp(filepath.Dir(path))
	if err != nil {
		return errors.New("cannot create local cell record")
	}
	defer func() { _ = removePrivateFile(f.Name()) }()
	if (filepath.Base(path) == "postgres.password" || filepath.Base(filepath.Dir(path)) == "credentials") && beforePasswordWrite != nil {
		beforePasswordWrite(f)
	}
	if _, err = f.Write(data); err == nil {
		err = syncFile(f)
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return errors.New("cannot persist local cell record")
	}
	if beforeRename != nil {
		if err := beforeRename(); err != nil {
			return err
		}
	}
	if err := validateWriteTarget(path); err != nil {
		return err
	}
	if err := validateWriteTarget(f.Name()); err != nil {
		return err
	}
	if err := publishPrivateFile(f.Name(), path); err != nil {
		return &publishError{cause: err}
	}
	return syncDirectory(filepath.Dir(path))
}

func syncDirectory(path string) error {
	if runtime.GOOS == "windows" {
		return nil // File data is flushed; directory fsync has no portable Windows API.
	}
	d, err := os.Open(path)
	if err != nil {
		return errors.New("cannot open local cell directory for sync")
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return errors.New("cannot sync local cell directory")
	}
	return nil
}

type journalEntry struct {
	Operation string    `json:"operation"`
	Step      string    `json:"step"`
	State     string    `json:"state"`
	At        time.Time `json:"at"`
	Cause     string    `json:"cause,omitempty"`
}

// journal appends each value plus its newline in ONE write, then fsyncs before
// acknowledging success. An unterminated final line was never acknowledged;
// readJournal identifies the crash signatures that Repair may discard.
func (c *Cell) journal(operation, step, state string) error {
	return c.journalCause(operation, step, state, "")
}

// journalCause is journal with an optional fixed cause. When the append creates
// operations.jsonl, journal/ is synced after the record so the new file's
// directory entry is durable before success; appends to an existing file sync
// only the file. A failed directory sync leaves the complete, file-synced record
// in place for Repair, as if the process had stopped right after the append.
func (c *Cell) journalCause(operation, step, state, cause string) error {
	path := c.path("journal", "operations.jsonl")
	info, err := os.Lstat(path)
	if err == nil && !info.Mode().IsRegular() {
		return errors.New("local cell journal must be a regular file")
	}
	// Only an absent pathname can be created by the open below; an existing one
	// never reports not-exist. Journal writers hold the root operation lock, so
	// nothing else creates or unlinks the file between this Lstat and the open.
	created := errors.Is(err, os.ErrNotExist)
	f, err := openPrivateFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return errors.New("cannot open local cell journal")
	}
	defer func() { _ = f.Close() }()
	data, err := json.Marshal(journalEntry{Operation: operation, Step: step, State: state, At: time.Now().UTC(), Cause: cause})
	if err != nil {
		return errors.New("cannot encode local cell journal")
	}
	data = append(data, '\n')
	if n, err := f.Write(data); err != nil || n != len(data) {
		return errors.New("cannot append local cell journal")
	}
	if err := syncFile(f); err != nil {
		return errors.New("cannot sync local cell journal")
	}
	if created {
		if err := syncJournalDirectory(c.path("journal")); err != nil {
			return errors.New("cannot sync local cell journal directory")
		}
	}
	return nil
}

// journalProgress contains only validated, newline-terminated records. The
// durable prefix may be empty when the first append tears.
type journalProgress struct {
	pending        *Interruption
	cleanupOnly    bool // pending came from the manifest, not an open journal operation
	done           map[string]bool
	durableBytes   int64
	tornTail       bool
	updateID       string
	updateState    updateState
	updateMaxState updateState
	updateComplete bool
}

func (p journalProgress) installComplete(manifests ...*Manifest) bool {
	complete := p.pending != nil && p.pending.Operation == "install" &&
		p.done["initdb"] && p.done["configure"] && p.done["postgres"] && p.done["database"]
	if len(manifests) != 0 && manifests[0] != nil && manifests[0].PrincipalLayoutVersion != 0 {
		for _, step := range selectedInstallSteps {
			complete = complete && p.done[step]
		}
	}
	return complete
}

// readJournal relies on journal writing each value plus newline in ONE write
// followed by fsync. A final line without its newline was never acknowledged.
// After trimming trailing crash zero-fill, only an empty tail, a JSON prefix, or
// exactly one complete JSON value may be discarded, even at byte zero. Committed
// records and all other corruption remain fail-closed.
func (c *Cell) readJournal() (journalProgress, error) {
	var progress journalProgress
	path := c.path("journal", "operations.jsonl")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		progress.pending, err = c.cleanupInterruption()
		progress.cleanupOnly = progress.pending != nil
		return progress, err
	}
	if err != nil || !info.Mode().IsRegular() {
		return progress, errors.New("invalid local cell journal")
	}
	f, err := openPrivateFile(path, os.O_RDONLY, 0)
	if err != nil {
		return progress, errors.New("cannot read local cell journal")
	}
	defer func() { _ = f.Close() }()
	reader := bufio.NewReaderSize(f, 64*1024)
	for {
		line, readErr := reader.ReadSlice('\n')
		if len(line) == 0 && errors.Is(readErr, io.EOF) {
			break
		}
		if errors.Is(readErr, bufio.ErrBufferFull) && line[len(line)-1] == 0 {
			// Keep the record-size bound, but consume arbitrary crash zero-fill
			// without retaining it. A newline or non-NUL after it is corruption.
			line = bytes.Clone(bytes.TrimRight(line, "\x00"))
			for errors.Is(readErr, bufio.ErrBufferFull) {
				var padding []byte
				padding, readErr = reader.ReadSlice('\n')
				if len(bytes.TrimRight(padding, "\x00")) != 0 {
					return progress, errors.New("invalid local cell journal; manual recovery required")
				}
			}
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return progress, errors.New("cannot read local cell journal; manual recovery required")
		}
		if errors.Is(readErr, io.EOF) {
			tail := bytes.TrimRight(line, "\x00")
			decoder := json.NewDecoder(bytes.NewReader(tail))
			decodeErr := decoder.Decode(new(any))
			if len(tail) != 0 && !errors.Is(decodeErr, io.ErrUnexpectedEOF) &&
				(decodeErr != nil || decoder.Decode(new(any)) != io.EOF) {
				return progress, errors.New("invalid local cell journal; manual recovery required")
			}
			progress.tornTail = true
			break
		}
		var entry journalEntry
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.DisallowUnknownFields()
		decodeErr := decoder.Decode(&entry)
		if decodeErr != nil || decoder.Decode(new(any)) != io.EOF || (entry.State != "started" && entry.State != "done" && entry.State != "failed") || !validJournalStep(entry.Operation, entry.Step) || entry.At.IsZero() {
			return progress, errors.New("invalid local cell journal; manual recovery required")
		}
		if entry.Step == "operation" && entry.State == "started" {
			if progress.pending != nil {
				return progress, errors.New("overlapping local cell journal operations")
			}
			progress.pending = &Interruption{Operation: entry.Operation, Step: entry.Step}
			progress.done = make(map[string]bool)
		} else if progress.pending != nil {
			if entry.Operation != progress.pending.Operation {
				return progress, errors.New("mismatched local cell journal operation")
			}
			progress.pending.Step = entry.Step
			progress.done[entry.Step] = entry.State == "done"
			if entry.Step == "operation" && entry.State == "done" {
				progress.pending = nil
				progress.done = nil
			}
		} else {
			return progress, errors.New("unfenced local cell journal step")
		}
		progress.durableBytes += int64(len(line))
	}
	if progress.pending == nil {
		progress.pending, err = c.cleanupInterruption()
		progress.cleanupOnly = progress.pending != nil
	}
	return progress, err
}

// discardJournalTail requires the operation lock. Sync the truncation before
// any recovery append so a second crash cannot turn the tail into an interior
// record. Inspection only reads the prefix; Repair performs this reconciliation.
func (c *Cell) discardJournalTail(progress journalProgress) error {
	if !progress.tornTail {
		return nil
	}
	f, err := openPrivateFile(c.path("journal", "operations.jsonl"), os.O_WRONLY, 0)
	if err != nil {
		return errors.New("cannot open torn local cell journal")
	}
	defer func() { _ = f.Close() }()
	if err := f.Truncate(progress.durableBytes); err != nil {
		return errors.New("cannot truncate torn local cell journal")
	}
	if err := syncFile(f); err != nil {
		return errors.New("cannot sync repaired local cell journal")
	}
	return nil
}

func (c *Cell) cleanupInterruption() (*Interruption, error) {
	m, err := readManifest(c.cfg.Root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if m.Cleanup != nil {
		return &Interruption{Operation: m.Cleanup.Operation, Step: m.Cleanup.Step}, nil
	}
	return nil, nil
}

func validJournalStep(operation, step string) bool {
	steps := map[string]map[string]bool{
		"install": {"manifest": true, "password": true, "initdb": true, "configure": true, "postgres": true, "database": true, "stop": true, "principal_credentials": true, "principals": true, "principal_migrations": true, "principal_grants": true, "principal_verified": true},
		"start":   {"ports": true, "postgres": true, "server": true, "server_ready": true, "account": true, "worker": true, "worker_ready": true, "principal_verified": true},
		"stop":    {"stop": true},
		"remove":  {"stop": true, "spool": true, "tmp": true, "logs": true, "purge": true, "runtime_cleanup": true},

		"reinstall": {"postgres": true, "verified": true, "stop": true, "readmitted": true},

		"checkpoint":        {"copy": true, "inventory": true, "witness": true},
		"checkpoint_delete": {"unseal": true, "remove": true},
	}
	allowed, ok := steps[operation]
	return ok && (step == "operation" || step == "revert_stop" || allowed[step])
}

var errInterrupted = errors.New("injected local cell interruption; repair required")

func (c *Cell) step(operation, step string, fn func() error) error {
	if err := c.journal(operation, step, "started"); err != nil {
		return err
	}
	if err := fn(); err != nil {
		var exit *readinessExitError
		if errors.As(err, &exit) {
			if journalErr := c.journalCause(operation, step, "failed", exit.Error()); journalErr != nil {
				return journalErr
			}
			return fmt.Errorf("local cell %s: %s failed: %w", operation, step, exit)
		}
		var cause fixedCause
		if errors.As(err, &cause) {
			if journalErr := c.journalCause(operation, step, "failed", cause.Error()); journalErr != nil {
				return journalErr
			}
			// Only the fixed cause crosses this boundary, never its wrappers.
			return fmt.Errorf("local cell %s: %s failed: %w", operation, step, cause)
		}
		_ = c.journal(operation, step, "failed")
		var readiness *readinessError
		if errors.As(err, &readiness) {
			// Unwrap only our fixed-message failure. A wrapper around it may
			// contain command output, paths, or other private values.
			return fmt.Errorf("local cell %s: %s failed: %w", operation, step, readiness)
		}
		return fmt.Errorf("local cell %s: %s failed", operation, step)
	}
	if c.cfg.Failpoint != nil && c.cfg.Failpoint(operation, step) != nil {
		return errInterrupted
	}
	return c.journal(operation, step, "done")
}

type updateJournalEntry struct {
	Operation string    `json:"operation"`
	Step      string    `json:"step"`
	State     string    `json:"state"`
	At        time.Time `json:"at"`
	Cause     string    `json:"cause,omitempty"`
	UpdateID  string    `json:"update_id,omitempty"`
}

// readUpdateJournal is reserved for private update helpers. Shipped lifecycle
// readers continue to reject update entries and never reconcile update records.
func (c *Cell) readUpdateJournal() (journalProgress, error) {
	var progress journalProgress
	path := c.path("journal", "operations.jsonl")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		progress.pending, err = c.cleanupInterruption()
		return progress, err
	}
	if err != nil || !info.Mode().IsRegular() {
		return progress, errors.New("invalid local cell journal")
	}
	f, err := openPrivateFile(path, os.O_RDONLY, 0)
	if err != nil {
		return progress, errors.New("cannot read local cell journal")
	}
	defer func() { _ = f.Close() }()
	reader := bufio.NewReaderSize(f, 64*1024)
	for {
		line, readErr := reader.ReadSlice('\n')
		if len(line) == 0 && errors.Is(readErr, io.EOF) {
			break
		}
		if errors.Is(readErr, bufio.ErrBufferFull) && line[len(line)-1] == 0 {
			// Keep the record-size bound, but consume arbitrary crash zero-fill
			// without retaining it. A newline or non-NUL after it is corruption.
			line = bytes.Clone(bytes.TrimRight(line, "\x00"))
			for errors.Is(readErr, bufio.ErrBufferFull) {
				var padding []byte
				padding, readErr = reader.ReadSlice('\n')
				if len(bytes.TrimRight(padding, "\x00")) != 0 {
					return progress, errors.New("invalid local cell journal; manual recovery required")
				}
			}
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return progress, errors.New("cannot read local cell journal; manual recovery required")
		}
		if errors.Is(readErr, io.EOF) {
			tail := bytes.TrimRight(line, "\x00")
			decoder := json.NewDecoder(bytes.NewReader(tail))
			decodeErr := decoder.Decode(new(any))
			if len(tail) != 0 && !errors.Is(decodeErr, io.ErrUnexpectedEOF) &&
				(decodeErr != nil || decoder.Decode(new(any)) != io.EOF) {
				return progress, errors.New("invalid local cell journal; manual recovery required")
			}
			progress.tornTail = true
			break
		}
		var entry updateJournalEntry
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.DisallowUnknownFields()
		decodeErr := decoder.Decode(&entry)
		if decodeErr != nil || decoder.Decode(new(any)) != io.EOF || (entry.State != "started" && entry.State != "done" && entry.State != "failed") || !validUpdateJournalStep(entry.Operation, entry.Step) || entry.At.IsZero() {
			return progress, errors.New("invalid local cell journal; manual recovery required")
		}
		if entry.Step == "operation" && entry.State == "started" {
			if progress.pending != nil {
				return progress, errors.New("overlapping local cell journal operations")
			}
			progress.pending = &Interruption{Operation: entry.Operation, Step: entry.Step}
			progress.done = make(map[string]bool)
			if entry.Operation == "update" {
				if !validUpdateOperationID(entry.UpdateID) {
					return progress, errUpdatePartialState
				}
				progress.updateID, progress.updateState, progress.updateMaxState, progress.updateComplete = entry.UpdateID, "", "", false
			} else if entry.UpdateID != "" {
				return progress, errUpdatePartialState
			}
		} else if progress.pending != nil {
			if entry.UpdateID != "" {
				return progress, errUpdatePartialState
			}
			if entry.Operation != progress.pending.Operation {
				return progress, errors.New("mismatched local cell journal operation")
			}
			progress.pending.Step = entry.Step
			progress.done[entry.Step] = entry.State == "done"
			if entry.Operation == "update" && updateState(entry.Step).valid() {
				if !validUpdateJournalTransition(progress.updateState, updateState(entry.Step)) {
					return progress, errUpdatePartialState
				}
				progress.updateState = updateState(entry.Step)
				if updateStateIndex(progress.updateState) > updateStateIndex(progress.updateMaxState) {
					progress.updateMaxState = progress.updateState
				}
			}
			if entry.Step == "operation" && entry.State == "done" {
				if entry.Operation == "update" {
					progress.updateComplete = true
				}
				progress.pending = nil
				progress.done = nil
			}
		} else {
			return progress, errors.New("unfenced local cell journal step")
		}
		progress.durableBytes += int64(len(line))
	}
	if progress.pending == nil {
		progress.pending, err = c.cleanupInterruption()
	}
	return progress, err
}

func validUpdateJournalStep(operation, step string) bool {
	if operation == "update" {
		return step == "operation" || step == "revert_stop" || updateState(step).valid()
	}
	return validJournalStep(operation, step)
}
