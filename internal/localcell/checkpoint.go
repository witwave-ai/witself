package localcell

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/witwave-ai/witself/internal/id"
	"github.com/witwave-ai/witself/internal/runtimeconfig"
)

// CheckpointOptions names the installation whose cold state Checkpoint copies.
// The caller reads InstallationID from Inspect; its exact value is the consent.
type CheckpointOptions struct{ InstallationID string }

// DeleteCheckpointOptions names the one checkpoint DeleteCheckpoint removes.
// The caller reads CheckpointID from Inspect; its exact value is the consent.
type DeleteCheckpointOptions struct{ CheckpointID string }

// CheckpointSummary is Inspect's value-free view of one checkpoint; an
// incomplete one reports only its identifier. Call Printable before rendering
// witness metadata; completeness alone does not establish its safe shape.
type CheckpointSummary struct {
	ID             string    `json:"id"`
	InstallationID string    `json:"installation_id,omitempty"`
	CreatedAt      time.Time `json:"created_at,omitzero"`
	Bytes          int64     `json:"bytes,omitempty"`
	Complete       bool      `json:"complete"`
}

// checkpointStateError carries only a closed reason, never a refused name or
// OS error. Unwrap preserves the existing fixed cause and journal vocabulary.
type checkpointStateError struct{ reason string }

func (e checkpointStateError) Error() string { return "local_checkpoint_state_unsupported" }
func (e checkpointStateError) Unwrap() error { return errCheckpointState }

// CheckpointFailure is safe to render: Class and Reason contain only closed
// constants. A nil failure has the zero value. CleanupIncomplete means that
// the failed operation could not remove its own witness-less directory.
type CheckpointFailure struct {
	Class             string
	Reason            string
	CleanupIncomplete bool
}

var checkpointFailureClasses = []struct {
	cause error
	class string
}{
	{errCheckpointState, "local_checkpoint_state_unsupported"},
	{errCheckpointIdentity, "local_checkpoint_identity_mismatch"},
	{errCheckpointSource, "local_checkpoint_source_unsupported"},
	{errCheckpointStorage, "local_checkpoint_storage_insufficient"},
	{errCheckpointLimit, "local_checkpoint_limit_reached"},
	{errCheckpointIncomplete, "local_checkpoint_incomplete"},
	{errCheckpointWitness, "local_checkpoint_witness_mismatch"},
	{errCheckpointCopy, "local_checkpoint_copy_failed"},
	{errMaintenanceFenceActive, "local_maintenance_fence_active"},
	{ErrLocked, "local_cell_locked"},
	{context.Canceled, "local_checkpoint_interrupted"},
	{context.DeadlineExceeded, "local_checkpoint_interrupted"},
	{errPrivateLayout, "local_cell_layout_refused"},
	{errLocalDirectory, "local_cell_layout_refused"},
	{errLocalDirectoryPrivate, "local_cell_layout_refused"},
	{errLocalParentUnavailable, "local_cell_layout_refused"},
	{errLocalParentChanged, "local_cell_layout_refused"},
}

// ClassifyCheckpointFailure classifies identities, including wrapped causes,
// without reading error text. Unknown failures expose no original information.
func ClassifyCheckpointFailure(err error) CheckpointFailure {
	if err == nil {
		return CheckpointFailure{}
	}
	result := CheckpointFailure{Class: "local_checkpoint_operation_failed"}
	var cleanup checkpointCleanupError
	if errors.As(err, &cleanup) {
		result.CleanupIncomplete = true
		err = cleanup.Unwrap()
	}
	for _, entry := range checkpointFailureClasses {
		if errors.Is(err, entry.cause) {
			result.Class = entry.class
			break
		}
	}
	var state checkpointStateError
	if errors.As(err, &state) {
		switch state.reason {
		case "boundary_unreadable", "boundary_noncanonical", "boundary_not_private",
			"descendant_refused", "tree_unreadable", "unknown_entries":
			result.Reason = state.reason
		}
	}
	return result
}

// Printable checks only metadata shape, not completeness, integrity or restore
// readiness. It does not affect admission, counting, or exact-ID deletion.
func (s CheckpointSummary) Printable() bool {
	return checkpointIDPattern.MatchString(s.ID) && s.Bytes >= 0 &&
		(len(s.InstallationID) <= 64 && strings.Trim(s.InstallationID, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-") == "") &&
		(s.InstallationID != "" || !s.Complete) &&
		(!s.CreatedAt.IsZero() || !s.Complete) && s.CreatedAt.Year() >= 1 && s.CreatedAt.Year() <= 9999
}

var checkpointIDPattern = regexp.MustCompile(`^ckpt_[a-z2-7]{16}$`)

const (
	checkpointLimit         = 2       // complete checkpoints kept per root (option A)
	checkpointReserve       = 1 << 30 // free space required beyond 2*S
	checkpointBlock         = 4 << 10
	checkpointControlLimit  = 16 << 10
	checkpointInventoryName = "inventory.json"
	checkpointWitnessName   = "witness.json"
	checkpointRetryFloor    = 10 * time.Second
	checkpointRetryPerFile  = 100 * time.Millisecond
)

var errCheckpointAllocation = errors.New("cannot allocate local cell checkpoint identity")
var errCheckpointCleanup = errors.New("local cell checkpoint failed; cleanup incomplete")

// checkpointCleanupError: the own witness-less directory could not be removed.
type checkpointCleanupError struct{ cause error }

func (e checkpointCleanupError) Error() string { return errCheckpointCleanup.Error() }
func (e checkpointCleanupError) Unwrap() error { return e.cause }

// checkpointOperations replaces Checkpoint's external boundaries in tests.
type checkpointOperations struct {
	control     func(context.Context) (pgControlData, error)
	free        func(string) (uint64, uint64, error)
	afterCopy   func(string)
	beforeClose func()
	newID       func() (string, error)
	publish     func(string, string) error
	open        func(string) (*os.File, error)
	transient   func(error) bool
	sleep       func(context.Context, time.Duration)
	now         func() time.Time
	beforeStep  func(step, path string) // observes attempts without replacing operations
}

func (c *Cell) checkpointOps() checkpointOperations {
	ops := checkpointOperations{control: c.checkpointControl, free: checkpointFreeSpace}
	if c.checkpointTest != nil {
		ops = *c.checkpointTest
	}
	if ops.control == nil {
		ops.control = c.checkpointControl
	}
	if ops.free == nil {
		ops.free = checkpointFreeSpace
	}
	if ops.newID == nil {
		ops.newID = func() (string, error) { return id.New("ckpt") }
	}
	if ops.publish == nil {
		ops.publish = publishPrivateFile
	}
	if ops.open == nil {
		ops.open = func(path string) (*os.File, error) { return openPrivateReader(context.Background(), path) }
	}
	if ops.transient == nil {
		ops.transient = runtimeconfig.WindowsPrivateTransient
	}
	if ops.now == nil {
		ops.now = time.Now
	}
	if ops.sleep == nil {
		ops.sleep = func(ctx context.Context, delay time.Duration) {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-ctx.Done():
			case <-timer.C:
			}
		}
	}
	return ops
}

type checkpointRetryBudget struct{ slept, limit time.Duration }

// retryCheckpointStep adds call-wide sleep and cancellation bounds to the
// shared per-step engine. Cancellation never starts another file operation.
func retryCheckpointStep(ctx context.Context, budget *checkpointRetryBudget, ops checkpointOperations, attempt func() error) error {
	_, err := retryPublish(func() error {
		if ctx.Err() != nil {
			return errCheckpointCopy
		}
		return attempt()
	}, func(err error) bool {
		return ctx.Err() == nil && budget.slept < budget.limit && ops.transient(err)
	}, leasePublishHooks{now: ops.now, sleep: func(delay time.Duration) {
		delay = min(delay, budget.limit-budget.slept)
		budget.slept += delay
		ops.sleep(ctx, delay)
	}})
	return err
}

// recoveryDir is the one production spelling of <root>/recovery.
func (c *Cell) recoveryDir() string { return c.path("recovery") }

// checkpointDir is the only source of a path below recovery/ that is created,
// published, synced or deleted: one exact identifier directly below it.
func (c *Cell) checkpointDir(checkpointID string) (string, error) {
	if !checkpointIDPattern.MatchString(checkpointID) {
		return "", errCheckpointIdentity
	}
	dir := filepath.Join(c.recoveryDir(), checkpointID)
	if filepath.Dir(dir) != c.recoveryDir() || filepath.Base(dir) != checkpointID {
		return "", errCheckpointIdentity
	}
	return dir, nil
}

// Checkpoint copies the PGDATA, manifest and retained custody of the named,
// proven-cold installation byte for byte into a new recovery/<id>/ directory,
// re-reads the copy, re-proves the cold state and publishes the witness last;
// only a witnessed checkpoint is complete, and at most two are kept. Nothing
// is written to PGDATA, custody or the manifest, and no server or postmaster
// is started: pg_controldata runs at admission and again at the witness step.
// A failed call removes only the witness-less directory it created.
func (c *Cell) Checkpoint(ctx context.Context, options CheckpointOptions) error {
	if err := c.refuseBeforeLockNames(func() error { return c.admitCheckpointDry(ctx, options) }); err != nil {
		return err
	}
	lock, err := c.own(false)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	if err := c.maintenanceFenceAllows(0); err != nil {
		return err
	}
	return c.checkpointLocked(ctx, options)
}

// DeleteCheckpoint deletes exactly the checkpoint its identifier names: the
// witness first, so an interruption leaves it incomplete, then the directory.
func (c *Cell) DeleteCheckpoint(ctx context.Context, options DeleteCheckpointOptions) error {
	if err := c.refuseBeforeLockNames(func() error { return c.admitDeleteCheckpoint(options) }); err != nil {
		return err
	}
	lock, err := c.own(false)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	if err := c.maintenanceFenceAllows(0); err != nil {
		return err
	}
	return c.deleteCheckpointLocked(ctx, options)
}

// errCheckpointDryAdmitted ends admitCheckpointDry at the control boundary; it
// is never returned outside this file.
var errCheckpointDryAdmitted = errors.New("local cell checkpoint dry admission passed")

// refuseBeforeLockNames lets a verb whose own(false) would create a missing
// .owner.lock, run/ or run/operation.lock run its admission first, read-only,
// with only the locks whose names exist, so a refusal decided by what stood in
// the root creates none of them. It never admits: nil leads to own(false), the
// fence and the admission, unchanged under the full lock. It never reads the
// maintenance fence and never runs pg_controldata (admitCheckpointDry).
func (c *Cell) refuseBeforeLockNames(admit func() error) error {
	// The gate: with both names present own(false) creates nothing, so today's
	// path alone runs. A not-exist here only selects the checks below.
	_, owner := os.Lstat(c.path(".owner.lock"))
	_, run := os.Lstat(c.path("run", "operation.lock"))
	// Ownership refusals recur in own(false), which classifies them before it
	// creates anything; here they defer and are never classified.
	if (owner == nil && run == nil) || c.validateRoot(false) != nil {
		return nil
	}
	// No create. Its not-exists (no owner lock, no run/, no run lock) stand on
	// validateRoot just above: the root and any run are real private
	// directories, so a file at run was refused there, on Windows too.
	held, err := acquireLock(context.Background(), c.cfg.Root, false)
	if err != nil {
		return nil
	}
	defer func() { _ = held.Close() }()
	if err := c.validateRoot(false); err != nil {
		return nil
	}
	// Only the fence's approved owners read it, under the full lock: a leaf not
	// proven absent defers. validateRoot just above proved journal absent or a
	// real private directory, so this not-exist is an absence.
	if _, err := os.Lstat(c.path("journal", "maintenance.json")); !errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	// Without the owner lock, admit runs only if recovery/ was absent at this
	// Lstat (as Inspect lists it), and an existing run lock is left to
	// own(false), after the owner lock. The same validateRoot proved recovery
	// absent or a real private directory. One window remains: a concurrent owner
	// that creates recovery/ after this Lstat. The dry admission's own recovery
	// checks then read it without the owner lock; they write nothing, and a dry
	// pass can only refuse, never admit.
	if _, err := os.Lstat(c.recoveryDir()); held == nil && (run == nil || !errors.Is(err, fs.ErrNotExist)) {
		return nil
	}
	return admit()
}

// admitCheckpointDry is the unchanged admitCheckpoint with its control
// boundary replaced, so it returns before pg_controldata or free space.
func (c *Cell) admitCheckpointDry(ctx context.Context, options CheckpointOptions) error {
	ops := c.checkpointOps()
	ops.control = func(context.Context) (pgControlData, error) { return pgControlData{}, errCheckpointDryAdmitted }
	if _, err := c.admitCheckpoint(ctx, options, ops); !errors.Is(err, errCheckpointDryAdmitted) {
		return err
	}
	return nil
}

// checkpointEntry is one planned directory or file, by slash path below the
// checkpoint directory.
type checkpointEntry struct {
	path      string
	directory bool
	size      int64
	custody   bool
	purpose   credentialPurpose
	digest    [sha256.Size]byte
}

type checkpointPlan struct {
	manifest       *Manifest
	entries        []checkpointEntry
	createRecovery bool
	control        pgControlData
	inventory      [sha256.Size]byte
}

func (c *Cell) checkpointLocked(ctx context.Context, options CheckpointOptions) error {
	ops := c.checkpointOps()
	plan, err := c.admitCheckpoint(ctx, options, ops)
	if err != nil {
		return err
	}
	if err := c.layout(); err != nil {
		return err
	}
	// A generator error, a malformed or an existing identifier writes nothing.
	checkpointID, err := ops.newID()
	dir, dirErr := c.checkpointDir(checkpointID)
	if _, statErr := os.Lstat(dir); err != nil || dirErr != nil || !errors.Is(statErr, fs.ErrNotExist) {
		return errCheckpointAllocation
	}
	if err := c.closeCheckpointLeftover(); err != nil {
		return err
	}
	if err := c.begin("checkpoint"); err != nil {
		return err
	}
	if c.cfg.Failpoint != nil && c.cfg.Failpoint("checkpoint", "operation_started") != nil {
		return errInterrupted
	}
	created := false
	// The failed create's one cleanup; later calls reuse the first call's result.
	discard := sync.OnceValue(func() error { return c.discardOwnCheckpoint(checkpointID) })
	budget := checkpointRetryBudget{limit: checkpointRetryFloor}
	for _, entry := range plan.entries {
		if !entry.directory {
			budget.limit += checkpointRetryPerFile
		}
	}
	for _, step := range []struct {
		name string
		run  func() error
	}{
		{"copy", func() error { return c.copyCheckpoint(ctx, checkpointID, plan, &created, discard, ops, &budget) }},
		{"inventory", func() error { return c.inventoryCheckpoint(ctx, checkpointID, plan, discard, ops, &budget) }},
		{"witness", func() error { return c.witnessCheckpoint(ctx, checkpointID, plan, ops) }},
	} {
		// A crash leaves the operation open and discards nothing; Repair or the next
		// admitted Checkpoint or DeleteCheckpoint closes it.
		if err := c.step("checkpoint", step.name, step.run); errors.Is(err, errInterrupted) {
			return err
		} else if err != nil {
			discarded := !created || discard() == nil
			return c.closeCheckpointFailure("checkpoint", err, discarded, ops.beforeClose)
		}
	}
	return c.journal("checkpoint", "operation", "done")
}

// admitCheckpoint runs under the lock and writes nothing; the control and free
// boundaries run last, in that order.
func (c *Cell) admitCheckpoint(ctx context.Context, options CheckpointOptions, ops checkpointOperations) (*checkpointPlan, error) {
	if options.InstallationID == "" {
		return nil, errCheckpointIdentity
	}
	m, err := readManifest(c.cfg.Root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, errCheckpointState
	} else if err != nil {
		return nil, err
	}
	if m.InstallationID != options.InstallationID {
		return nil, errCheckpointIdentity
	}
	// A retained root is checkpointable: it is the moment before a purge.
	if !m.Installed || m.Cleanup != nil || !accountPattern.MatchString(m.AccountID) || m.LastVerifiedSchemaVersion < 1 {
		return nil, errCheckpointState
	}
	if progress, err := c.readJournal(); err != nil {
		return nil, err
	} else if (progress.pending != nil || progress.tornTail) && !checkpointLeftover(progress) {
		return nil, errCheckpointState
	}
	if err := c.checkpointCold(); err != nil {
		return nil, err
	}
	// A Faulted service is admitted: no server or postmaster is started, only
	// pg_controldata, at admission and again at the witness step.
	if d, err := c.ServiceDescriptor(); !errors.Is(err, os.ErrNotExist) {
		if _, transition := os.Lstat(c.path("service", "transition.json")); err != nil || d.DesiredState == "maintenance" || !errors.Is(transition, os.ErrNotExist) {
			return nil, errCheckpointState
		}
	}
	if _, err := c.password(); err != nil {
		return nil, err
	}
	if m.PrincipalLayoutVersion != 0 {
		if err := c.validatePrincipalPasswords(m); err != nil {
			return nil, err
		}
	}
	if !filepath.IsAbs(c.cfg.Binaries.PostgresDir) {
		return nil, errors.New("explicit PostgreSQL binary directory is required")
	}
	if _, err := os.Lstat(c.path("pg")); err != nil || c.validatePostgresDirectory() != nil {
		return nil, errCheckpointState
	}
	if _, err := os.Lstat(c.path("pg", "postmaster.pid")); !errors.Is(err, fs.ErrNotExist) {
		return nil, errCheckpointState
	}
	// As postmaster() does, the opened handle proves a small regular file before
	// the read; nothing waits on what stands at the name. Every failure refuses.
	if version, err := readRegular(context.Background(), openPostgresReader, func(info os.FileInfo) bool { return info.Size() <= 64 }, c.path("pg", "PG_VERSION"), errCheckpointSource, errCheckpointSource, errCheckpointSource); err != nil || strings.TrimSuffix(string(version), "\n") != "16" {
		return nil, errCheckpointSource
	}
	if err := c.validateRecovery(); err != nil {
		return nil, err
	}
	switch summaries, unknown, err := c.listCheckpoints(); {
	case err != nil:
		return nil, err
	case slices.ContainsFunc(summaries, func(summary CheckpointSummary) bool { return !summary.Complete }):
		return nil, errCheckpointIncomplete
	case unknown != 0:
		return nil, checkpointStateError{reason: "unknown_entries"}
	case len(summaries) >= checkpointLimit:
		return nil, errCheckpointLimit
	}
	plan, err := c.planCheckpoint(m)
	if err != nil {
		return nil, err
	}
	if plan.control, err = ops.control(ctx); err != nil {
		return nil, err
	}
	if plan.control.State != "shut down" {
		return nil, errCheckpointState
	}
	probe := c.recoveryDir()
	if plan.createRecovery {
		probe = c.cfg.Root
	}
	if free, _, err := ops.free(probe); err != nil {
		return nil, errCheckpointState
	} else if free < plan.need() {
		return nil, errCheckpointStorage
	}
	return plan, nil
}

// checkpointCold proves no recorded postgres, server or worker is alive.
func (c *Cell) checkpointCold() error {
	if err := c.verifyAll(); err != nil {
		return err
	}
	for _, component := range []string{"postgres", "server", "worker"} {
		if state, err := c.processState(component); err != nil {
			return err
		} else if state.Alive {
			return errCheckpointState
		}
	}
	return nil
}

// planCheckpoint lists the copy set: PGDATA in lexical walk order (directories
// and regular files, pg_tblspc empty), the manifest and the layout's custody.
func (c *Cell) planCheckpoint(m *Manifest) (*checkpointPlan, error) {
	plan := &checkpointPlan{manifest: m}
	err := filepath.WalkDir(c.path("pg"), func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(c.path("pg"), name)
		item := checkpointEntry{path: path.Join("pg", filepath.ToSlash(rel)), directory: entry.IsDir()}
		info, infoErr := entry.Info()
		if err != nil || infoErr != nil || strings.HasPrefix(item.path, "pg/pg_tblspc/") || (!item.directory && !entry.Type().IsRegular()) ||
			!checkpointNameSupported(item.path) {
			return errCheckpointSource
		}
		if !item.directory {
			item.size = info.Size()
		}
		plan.entries = append(plan.entries, item)
		return nil
	})
	info, statErr := os.Lstat(c.path("manifest.json"))
	if err != nil || statErr != nil || !info.Mode().IsRegular() {
		return nil, errCheckpointSource
	}
	plan.entries = append(plan.entries, checkpointEntry{path: "manifest.json", size: info.Size()}, checkpointEntry{path: "run", directory: true})
	purposes := []credentialPurpose{credentialBootstrap}
	if m.PrincipalLayoutVersion != 0 {
		purposes = append(purposes, principalCredentialPurposes[:]...)
	}
	for index, purpose := range purposes {
		if index == 1 {
			plan.entries = append(plan.entries, checkpointEntry{path: "run/credentials", directory: true})
		}
		source, pathErr := c.credentialPath(purpose)
		rel, err := filepath.Rel(c.cfg.Root, source)
		info, statErr := os.Lstat(source)
		if errors.Join(pathErr, err, statErr) != nil || !info.Mode().IsRegular() {
			return nil, errCheckpointSource
		}
		plan.entries = append(plan.entries, checkpointEntry{path: filepath.ToSlash(rel), size: info.Size(), custody: true, purpose: purpose})
	}
	_, err = os.Lstat(c.recoveryDir())
	plan.createRecovery = errors.Is(err, fs.ErrNotExist)
	if runtime.GOOS == "windows" {
		sample := filepath.Join(c.recoveryDir(), "ckpt_"+strings.Repeat("a", 16))
		fits := checkpointPathFits(sample, true)
		for _, entry := range plan.entries {
			fits = fits && checkpointPathFits(filepath.Join(sample, filepath.FromSlash(entry.path)), entry.directory)
		}
		if !fits {
			return nil, errCheckpointSource
		}
	}
	return plan, nil
}

// checkpointNameSupported admits a PGDATA path the inventory spells exactly
// (valid UTF-8) whose base name is no private temporary: format 1 refuses any
// .pending-* name, so a cell's own crash residue is refused, never copied.
func checkpointNameSupported(name string) bool {
	return utf8.ValidString(name) && !strings.HasPrefix(path.Base(name), ".pending-")
}

// checkpointPathFits is the Windows bound: below 260 UTF-16 units, with room
// for a 34-unit private temporary in a directory.
func checkpointPathFits(name string, directory bool) bool {
	units := len(utf16.Encode([]rune(name)))
	if directory {
		units += 34
	}
	return units < 260
}

// need is 2*S + 1 GiB (the second S is the drill's): copied files in whole 4
// KiB blocks, 4 KiB per created directory, 1 MiB and 256 bytes per entry.
func (p *checkpointPlan) need() uint64 {
	size, directories := uint64(1<<20), uint64(1)
	if p.createRecovery {
		directories++
	}
	for _, entry := range p.entries {
		size += 256
		if entry.directory {
			directories++
		} else {
			size += (uint64(entry.size) + checkpointBlock - 1) / checkpointBlock * checkpointBlock
		}
	}
	if size += directories * checkpointBlock; size > (math.MaxUint64-checkpointReserve)/2 {
		return math.MaxUint64
	}
	return 2*size + checkpointReserve
}

// syncCheckpointDirectory syncs a checkpoint directory or recovery/. Tests
// replace it; production always uses syncDirectory.
var syncCheckpointDirectory = syncDirectory

// openCheckpointEntry opens one planned file read-only: PGDATA through the
// confined source opener, the manifest and custody as private readers. None
// waits on what stands at the name; copyFile decides the handle before a read.
func (c *Cell) openCheckpointEntry(entry checkpointEntry, open func(string) (*os.File, error)) (*os.File, error) {
	if !entry.custody && entry.path != "manifest.json" {
		return open(strings.TrimPrefix(entry.path, "pg/"))
	}
	source, err := c.credentialPath(entry.purpose)
	if !entry.custody {
		return openPrivateReader(context.Background(), c.path("manifest.json"))
	} else if err != nil {
		return nil, err
	}
	return openPrivateReader(context.Background(), source)
}

// copyCheckpoint copies the plan in order into a new directory: each file of
// exactly its planned size through a SHA-256 tee into a private temporary
// published once. A failure removes the directory before the failed line.
func (c *Cell) copyCheckpoint(ctx context.Context, checkpointID string, plan *checkpointPlan, created *bool, discard func() error, ops checkpointOperations, budget *checkpointRetryBudget) error {
	dir, err := c.checkpointDir(checkpointID)
	if err != nil {
		return err
	}
	if err := privateDir(c.recoveryDir(), true); err != nil {
		return errCheckpointCopy
	}
	// The second refusal of an existing identifier: nothing is written into it.
	if _, err := os.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
		return errCheckpointCopy
	}
	*created = true
	fail := func(cause error) error { _ = discard(); return cause }
	if err := privateDir(dir, true); err != nil {
		return fail(errCheckpointCopy)
	}
	open, release, err := c.openCheckpointSource(context.Background())
	if err != nil {
		return fail(errCheckpointCopy)
	}
	defer release()
	copyFile := func(entry *checkpointEntry) error {
		source, err := c.openCheckpointEntry(*entry, open)
		if err != nil {
			return errCheckpointCopy
		}
		defer func() { _ = source.Close() }()
		if info, err := source.Stat(); err != nil || !info.Mode().IsRegular() || info.Size() != entry.size {
			return errCheckpointWitness
		}
		temporary, err := createPrivateTemp(filepath.Join(dir, filepath.FromSlash(path.Dir(entry.path))))
		if err != nil {
			return errCheckpointCopy
		}
		defer func() { _ = temporary.Close() }()
		hash := sha256.New()
		written, err := io.Copy(io.MultiWriter(temporary, hash), io.LimitReader(source, entry.size))
		if err != nil {
			return errCheckpointCopy
		}
		// Exactly the planned bytes: neither a short read nor growth after Stat.
		if extra, _ := source.Read(make([]byte, 1)); written != entry.size || extra != 0 {
			return errCheckpointWitness
		}
		if syncFile(temporary) != nil || temporary.Close() != nil {
			return errCheckpointCopy
		}
		target, attempted := filepath.Join(dir, filepath.FromSlash(entry.path)), false
		if retryCheckpointStep(ctx, budget, ops, func() error {
			// A post-move validation refusal can leave the complete target.
			// Only this call owns the directory; inventory re-hashes the file.
			if attempted {
				_, sourceErr := os.Lstat(temporary.Name())
				info, targetErr := os.Lstat(target)
				if errors.Is(sourceErr, fs.ErrNotExist) && targetErr == nil && info.Mode().IsRegular() {
					return nil
				}
			}
			attempted = true
			if ops.beforeStep != nil {
				ops.beforeStep("publish", temporary.Name())
			}
			return ops.publish(temporary.Name(), target)
		}) != nil {
			return errCheckpointCopy
		}
		copy(entry.digest[:], hash.Sum(nil))
		return nil
	}
	for index := range plan.entries {
		entry := &plan.entries[index]
		if ctx.Err() != nil {
			return fail(errCheckpointCopy)
		}
		if !entry.directory {
			err = copyFile(entry)
		} else if privateDir(filepath.Join(dir, filepath.FromSlash(entry.path)), true) != nil {
			err = errCheckpointCopy
		}
		if err != nil {
			return fail(err)
		}
	}
	// Plan order creates parents first; the reverse syncs deepest first.
	for index := len(plan.entries) - 1; index >= 0; index-- {
		if plan.entries[index].directory && syncCheckpointDirectory(filepath.Join(dir, filepath.FromSlash(plan.entries[index].path))) != nil {
			return fail(errCheckpointCopy)
		}
	}
	if err := syncCheckpointDirectory(dir); err != nil {
		return fail(errCheckpointCopy)
	}
	if ops.afterCopy != nil {
		ops.afterCopy(dir)
	}
	return nil
}

// checkpointInventoryEntry is one inventory.json entry. Format 1 defines the
// whole tree: exactly the entries plus inventory.json and witness.json.
type checkpointInventoryEntry struct {
	Path   string `json:"path"`
	Type   string `json:"type"`
	Size   *int64 `json:"size,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
}

type checkpointInventory struct {
	Format       int                        `json:"format"`
	CheckpointID string                     `json:"checkpoint_id"`
	Entries      []checkpointInventoryEntry `json:"entries"`
}

// inventoryCheckpoint proves the tree is exactly the plan and every copy has
// its copy-time size and digest, then publishes the sorted inventory.
func (c *Cell) inventoryCheckpoint(ctx context.Context, checkpointID string, plan *checkpointPlan, discard func() error, ops checkpointOperations, budget *checkpointRetryBudget) error {
	dir, err := c.checkpointDir(checkpointID)
	if err != nil {
		return err
	}
	fail := func(cause error) error { _ = discard(); return cause }
	planned := make(map[string]bool, len(plan.entries))
	for _, entry := range plan.entries {
		planned[entry.path] = entry.directory
	}
	seen := 0
	err = filepath.WalkDir(dir, func(name string, entry fs.DirEntry, err error) error {
		rel, relErr := filepath.Rel(dir, name)
		if err != nil || relErr != nil || rel == "." {
			return errors.Join(err, relErr)
		}
		directory, known := planned[filepath.ToSlash(rel)]
		if !known || directory != entry.IsDir() || (!directory && !entry.Type().IsRegular()) {
			return errCheckpointWitness
		}
		seen++
		return nil
	})
	if err != nil || seen != len(plan.entries) {
		return fail(errCheckpointWitness)
	}
	inventory := checkpointInventory{Format: 1, CheckpointID: checkpointID}
	for _, entry := range plan.entries {
		item := checkpointInventoryEntry{Path: entry.path, Type: "dir"}
		if !entry.directory {
			var copied *os.File
			err := retryCheckpointStep(ctx, budget, ops, func() error {
				if ops.beforeStep != nil {
					ops.beforeStep("open", filepath.Join(dir, filepath.FromSlash(entry.path)))
				}
				var err error
				copied, err = ops.open(filepath.Join(dir, filepath.FromSlash(entry.path)))
				return err
			})
			// The copy's handle is decided before a byte is read: a FIFO with a
			// writer held would park the hash.
			if err == nil {
				if info, statErr := copied.Stat(); statErr != nil || !info.Mode().IsRegular() || info.Size() != entry.size {
					_ = copied.Close()
					err = errCheckpointWitness
				}
			}
			if err != nil {
				if ctx.Err() != nil {
					return fail(errCheckpointCopy)
				}
				return fail(errCheckpointWitness)
			}
			digest, size, err := checkpointHash(copied)
			_ = copied.Close()
			if ctx.Err() != nil {
				return fail(errCheckpointCopy)
			}
			if err != nil || size != entry.size || digest != entry.digest {
				return fail(errCheckpointWitness)
			}
			item.Type, item.Size, item.SHA256 = "file", &size, hex.EncodeToString(digest[:])
		}
		inventory.Entries = append(inventory.Entries, item)
	}
	sort.Slice(inventory.Entries, func(i, j int) bool { return inventory.Entries[i].Path < inventory.Entries[j].Path })
	data, err := json.Marshal(inventory)
	if err != nil {
		return fail(errCheckpointCopy)
	}
	data = append(data, '\n')
	if err := writeAtomicBytes(filepath.Join(dir, checkpointInventoryName), data, nil); err != nil {
		return fail(errCheckpointCopy)
	}
	plan.inventory = sha256.Sum256(data)
	return nil
}

// checkpointWitness, published last, records digests that a later check can use
// to detect accidental corruption or truncation, not a same-user rewrite; it
// holds no path, secret or log text.
type checkpointWitness struct {
	Format                 int           `json:"format"`
	CheckpointID           string        `json:"checkpoint_id"`
	InstallationID         string        `json:"installation_id"`
	CreatedAt              time.Time     `json:"created_at"`
	LayoutVersion          int           `json:"layout_version"`
	PrincipalLayoutVersion int           `json:"principal_layout_version"`
	SchemaVersion          int           `json:"schema_version"`
	PostgresMajor          string        `json:"postgres_major"`
	Control                pgControlData `json:"control"`
	Files                  int           `json:"files"`
	Directories            int           `json:"directories"`
	Bytes                  int64         `json:"bytes"`
	InventorySHA256        string        `json:"inventory_sha256"`
}

// witnessCheckpoint re-proves the cold state (an equal control reading, no
// postmaster.pid, an unchanged pg_control), then publishes the witness.
func (c *Cell) witnessCheckpoint(ctx context.Context, checkpointID string, plan *checkpointPlan, ops checkpointOperations) error {
	dir, err := c.checkpointDir(checkpointID)
	if err != nil {
		return err
	}
	controlCtx, cancel := c.deadline(ctx)
	defer cancel()
	// A run that fails, or the caller's context ending, read nothing: a copy
	// failure. A reading that differs, a postmaster.pid or a changed pg_control
	// is a witness mismatch.
	second, err := ops.control(controlCtx)
	if ctx.Err() != nil || err != nil {
		return errCheckpointCopy
	}
	if _, pidErr := os.Lstat(c.path("pg", "postmaster.pid")); second != plan.control || !errors.Is(pidErr, fs.ErrNotExist) {
		return errCheckpointWitness
	}
	index := slices.IndexFunc(plan.entries, func(entry checkpointEntry) bool { return entry.path == "pg/global/pg_control" && !entry.directory })
	if index < 0 {
		return errCheckpointWitness
	}
	open, release, err := c.openCheckpointSource(context.Background())
	if err != nil {
		return errCheckpointWitness
	}
	defer release()
	live, err := open("global/pg_control")
	if err != nil {
		return errCheckpointWitness
	}
	// The opened handle is decided before a byte is read: a FIFO with a writer
	// held would park the hash.
	if info, err := live.Stat(); err != nil || !info.Mode().IsRegular() || info.Size() != plan.entries[index].size {
		_ = live.Close()
		return errCheckpointWitness
	}
	digest, size, err := checkpointHash(live)
	_ = live.Close()
	if err != nil || size != plan.entries[index].size || digest != plan.entries[index].digest {
		return errCheckpointWitness
	}
	m := plan.manifest
	witness := checkpointWitness{Format: 1, CheckpointID: checkpointID, InstallationID: m.InstallationID, CreatedAt: time.Now().UTC(),
		LayoutVersion: m.LayoutVersion, PrincipalLayoutVersion: m.PrincipalLayoutVersion, SchemaVersion: m.LastVerifiedSchemaVersion,
		PostgresMajor: "16", Control: plan.control, InventorySHA256: hex.EncodeToString(plan.inventory[:])}
	for _, entry := range plan.entries {
		if entry.directory {
			witness.Directories++
		} else {
			witness.Files, witness.Bytes = witness.Files+1, witness.Bytes+entry.size
		}
	}
	if err := writeAtomic(filepath.Join(dir, checkpointWitnessName), witness, nil); err != nil {
		return errCheckpointCopy
	}
	return nil
}

// closeCheckpointFailure closes a failed operation unless the journal ends in
// a torn record, which only Repair may truncate.
func (c *Cell) closeCheckpointFailure(operation string, stepErr error, discarded bool, beforeClose func()) error {
	if beforeClose != nil {
		beforeClose()
	}
	progress, err := c.readJournal()
	if err != nil || progress.tornTail || progress.pending == nil || progress.pending.Operation != operation {
		return stepErr
	}
	if err := c.journal(operation, "operation", "done"); err != nil {
		return err
	}
	var cause fixedCause
	if discarded {
		return stepErr
	} else if errors.As(stepErr, &cause) {
		return checkpointCleanupError{cause: cause}
	}
	return checkpointCleanupError{cause: stepErr}
}

// checkpointLeftover recognises the only open operation the verbs close: their
// own kind at a step they write, with no torn tail; all else refuses as before.
func checkpointLeftover(progress journalProgress) bool {
	pending := progress.pending
	if pending == nil || progress.tornTail {
		return false
	}
	switch pending.Operation {
	case "checkpoint":
		return slices.Contains([]string{"operation", "copy", "inventory", "witness"}, pending.Step)
	case "checkpoint_delete":
		return slices.Contains([]string{"operation", "unseal", "remove"}, pending.Step)
	}
	return false
}

// closeCheckpointLeftover, under the verb's lock after its admission, appends
// only operation/done to such a leftover; a recorded cleanup refuses unwritten.
func (c *Cell) closeCheckpointLeftover() error {
	progress, err := c.readJournal()
	if err != nil || !checkpointLeftover(progress) {
		return err
	}
	if cleanup, err := c.cleanupInterruption(); err != nil {
		return err
	} else if cleanup != nil {
		return errCheckpointState
	}
	return c.journal(progress.pending.Operation, "operation", "done")
}

// admitDeleteCheckpoint is DeleteCheckpoint's admission: it writes nothing and
// ends before the hold.
func (c *Cell) admitDeleteCheckpoint(options DeleteCheckpointOptions) error {
	// The identifier's shape is checked before any filesystem access.
	if !checkpointIDPattern.MatchString(options.CheckpointID) {
		return errCheckpointIdentity
	}
	if progress, err := c.readJournal(); err != nil {
		return err
	} else if (progress.pending != nil || progress.tornTail) && !checkpointLeftover(progress) {
		return errCheckpointState
	}
	if err := c.checkpointCold(); err != nil {
		return err
	}
	if err := c.validateRecovery(); err != nil {
		return err
	}
	dir, err := c.checkpointDir(options.CheckpointID)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		return errCheckpointIdentity
	} else if err != nil || privateDir(dir, false) != nil {
		return errCheckpointState
	}
	// A case-insensitive volume also resolves another spelling: only the exact
	// on-disk name is deleted.
	entries, err := os.ReadDir(c.recoveryDir())
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if err != nil || !recoveryEntryCanonical(names, options.CheckpointID) {
		return errCheckpointIdentity
	}
	return nil
}

func (c *Cell) deleteCheckpointLocked(_ context.Context, options DeleteCheckpointOptions) error {
	if err := c.admitDeleteCheckpoint(options); err != nil {
		return err
	}
	// recovery/ and the checkpoint are held as directories; every removal goes
	// through them once both names are bound (checkpoint_root.go).
	recovery, checkpoint, err := c.holdCheckpoint(options.CheckpointID)
	defer closeCheckpointRoots(recovery, checkpoint)
	if err != nil {
		return errCheckpointState
	}
	// A purge removed journal/; nothing else is recreated.
	if err := privateDir(c.path("journal"), true); err != nil {
		return err
	}
	if err := c.closeCheckpointLeftover(); err != nil {
		return err
	}
	if err := c.begin("checkpoint_delete"); err != nil {
		return err
	}
	for _, step := range []struct {
		name string
		run  func() error
	}{
		{"unseal", func() error { return c.unsealCheckpointHeld(recovery, checkpoint, options.CheckpointID) }},
		{"remove", func() error { return c.removeCheckpointHeld(recovery, checkpoint, options.CheckpointID) }},
	} {
		if err := c.step("checkpoint_delete", step.name, step.run); errors.Is(err, errInterrupted) {
			return err
		} else if err != nil {
			return c.closeCheckpointFailure("checkpoint_delete", err, true, nil)
		}
	}
	return c.journal("checkpoint_delete", "operation", "done")
}

// errCheckpointWitnessUnread: a witness that may exist was not read; it is never reported incomplete.
var errCheckpointWitnessUnread = errors.New("cannot read local cell checkpoint witness")

// errCheckpointWitnessIrregular: what stands at the witness name was examined and is not a regular file.
var errCheckpointWitnessIrregular = errors.New("local cell checkpoint witness is not a regular file")

// checkpointComplete reads a format-1 witness naming this identifier;
// verifyCheckpointDocuments also reads the witness for integrity verification.
// Incomplete is said only on proof: the name absent below a directory (the
// os.ErrNotExist of openRegular, which absentBelowDirectory backs: on Windows
// a file where the checkpoint directory was is a not-exist error too), what
// stands there examined and not a regular file (its Lstat after a failed open,
// or the opened handle's stat), or its whole bounded content read and not that
// witness. Every other open, inspect or read failure is returned, a failed
// stat of the opened handle too, and a lease wait that ctx ended.
func checkpointComplete(ctx context.Context, dir, checkpointID string) (checkpointWitness, bool, error) {
	f, err := openRegular(ctx, filepath.Join(dir, checkpointWitnessName), errCheckpointWitnessUnread, errCheckpointWitnessIrregular, errCheckpointWitnessUnread)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, errCheckpointWitnessIrregular) {
		return checkpointWitness{}, false, nil
	} else if err != nil {
		return checkpointWitness{}, false, err
	}
	defer func() { _ = f.Close() }()
	return checkpointWitnessRead(f, checkpointID)
}

// checkpointWitnessRead reads r to readJSON's bound, then decodes the bytes as
// readJSON does: a failed read is errCheckpointWitnessUnread, never
// incomplete; content that is not this format-1 witness is incomplete.
func checkpointWitnessRead(r io.Reader, checkpointID string) (checkpointWitness, bool, error) {
	data, err := io.ReadAll(io.LimitReader(r, 1<<20))
	if err != nil {
		return checkpointWitness{}, false, errCheckpointWitnessUnread
	}
	var witness checkpointWitness
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&witness) != nil || decoder.Decode(new(any)) != io.EOF {
		return checkpointWitness{}, false, nil
	}
	return witness, witness.Format == 1 && witness.CheckpointID == checkpointID, nil
}

// listCheckpoints classifies recovery/ after validateRecovery; entries that
// are not id-shaped directories are counted unknown.
func (c *Cell) listCheckpoints() ([]CheckpointSummary, int, error) {
	entries, err := os.ReadDir(c.recoveryDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, nil
	} else if err != nil {
		// Never report zero checkpoints for a recovery/ that could not be read.
		return nil, 0, checkpointStateError{reason: "boundary_unreadable"}
	}
	var summaries []CheckpointSummary
	unknown := 0
	for _, entry := range entries {
		dir, err := c.checkpointDir(entry.Name())
		if err != nil || !entry.IsDir() {
			unknown++
			continue
		}
		summary := CheckpointSummary{ID: entry.Name()}
		witness, complete, err := checkpointComplete(context.Background(), dir, entry.Name())
		if err != nil {
			// A witness that may exist and was not read is never listed.
			return nil, 0, err
		}
		if complete {
			summary.InstallationID, summary.CreatedAt, summary.Bytes, summary.Complete = witness.InstallationID, witness.CreatedAt, witness.Bytes, true
		}
		summaries = append(summaries, summary)
	}
	return summaries, unknown, nil
}

// checkpointSummaries is Inspect's read-only listing. Inspect retains its own
// acquired lock until this returns; without it we never descend into recovery.
func (c *Cell) checkpointSummaries(lock *operationLock, inspection *Inspection) error {
	switch {
	case inspection.Locked:
		inspection.CheckpointListing = "busy"
		return nil
	case lock == nil:
		inspection.CheckpointListing = "unowned"
		// An absent boundary is the only empty listing we can observe without
		// ownership. Never recheck or descend if it appears after this Lstat.
		if _, err := os.Lstat(c.recoveryDir()); errors.Is(err, os.ErrNotExist) {
			inspection.CheckpointListing = "listed"
			if c.inspectCheckpointListing != nil {
				c.inspectCheckpointListing("absent")
			}
		}
		return nil
	}
	inspection.CheckpointListing = "listed"
	if c.inspectCheckpointListing != nil {
		c.inspectCheckpointListing("owned")
	}
	if err := c.validateRecovery(); err != nil {
		return err
	}
	var err error
	inspection.Checkpoints, inspection.CheckpointUnknown, err = c.listCheckpoints()
	return err
}

// validateRecovery is the one validator of recovery/** (RV): every object at
// and below the canonically spelled directory must be private. It never
// follows a link or skips a directory.
func (c *Cell) validateRecovery() error {
	if _, err := os.Lstat(c.recoveryDir()); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return checkpointStateError{reason: "boundary_unreadable"}
	}
	entries, err := os.ReadDir(c.cfg.Root)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if err != nil {
		return checkpointStateError{reason: "boundary_unreadable"}
	}
	if !recoveryEntryCanonical(names, filepath.Base(c.recoveryDir())) {
		return checkpointStateError{reason: "boundary_noncanonical"}
	}
	if privateDir(c.recoveryDir(), false) != nil {
		return checkpointStateError{reason: "boundary_not_private"}
	}
	return recoveryTreeFailure(validatePrivateTreeSnapshot(c.recoveryDir(), filepath.WalkDir, validateRecoveryObject, os.Lstat))
}

// recoveryTreeFailure consumes only identities already established by the one
// snapshot walk. Object checks cannot distinguish unreadability from privacy
// refusal, so those use the coarser descendant reason. No OS error is retained.
func recoveryTreeFailure(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, errPrivateLayout) || errors.Is(err, fs.ErrNotExist) {
		return checkpointStateError{reason: "tree_unreadable"}
	}
	return checkpointStateError{reason: "descendant_refused"}
}

// recoveryEntryCanonical reports whether the listing holds want byte for byte.
func recoveryEntryCanonical(names []string, want string) bool {
	return slices.Contains(names, want)
}

func checkpointHash(r io.Reader) (digest [sha256.Size]byte, size int64, err error) {
	hash := sha256.New()
	size, err = io.Copy(hash, r)
	copy(digest[:], hash.Sum(nil))
	return digest, size, err
}

// pgControlData keeps five parsed values; the raw output holds a secret nonce.
type pgControlData struct {
	ControlVersion   string `json:"control_version"`
	CatalogVersion   string `json:"catalog_version"`
	SystemIdentifier string `json:"system_identifier"`
	State            string `json:"state"`
	CheckpointLSN    string `json:"checkpoint_lsn"`
}

var checkpointControlKeys = []string{"pg_control version number", "Catalog version number", "Database system identifier",
	"Database cluster state", "Latest checkpoint location"}

var checkpointClusterStates = []string{"starting up", "shut down", "shut down in recovery", "shutting down",
	"in crash recovery", "in archive recovery", "in production"}

var checkpointLSNPattern = regexp.MustCompile(`^[0-9A-F]{1,8}/[0-9A-F]{1,8}$`)

// checkpointControlOutput keeps one byte beyond the bound and drains the rest.
type checkpointControlOutput struct{ data []byte }

func (o *checkpointControlOutput) Write(p []byte) (int, error) {
	if room := checkpointControlLimit + 1 - len(o.data); room > 0 {
		o.data = append(o.data, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

// checkpointControl runs pg_controldata in the C locale with bounded stdout
// and discarded stderr; run failures reuse pgCommand's four fixed texts.
func (c *Cell) checkpointControl(ctx context.Context) (pgControlData, error) {
	ctx, cancel := c.deadline(ctx)
	defer cancel()
	cmd, err := c.command(ctx, filepath.Join(c.cfg.Binaries.PostgresDir, executable("pg_controldata")), "-D", c.path("pg"))
	if err != nil {
		return pgControlData{}, err
	}
	environment, err := c.postgresEnvironment()
	if err != nil {
		return pgControlData{}, err
	}
	cmd.Env = appendEnvironment(environment, "LC_ALL=C", "LC_MESSAGES=C", "LANG=C")
	output := &checkpointControlOutput{}
	cmd.Stdout = output
	release, err := configurePostgresCommand(cmd)
	if err != nil {
		return pgControlData{}, err
	}
	defer release()
	err = cmd.Run()
	data, parseErr := parseCheckpointControl(output.data)
	clear(output.data)
	var exit *exec.ExitError
	switch {
	case err == nil:
		return data, parseErr
	case ctx.Err() == context.Canceled:
		return pgControlData{}, fixedCause("PostgreSQL command cancelled")
	case ctx.Err() == context.DeadlineExceeded:
		return pgControlData{}, fixedCause("PostgreSQL command timed out")
	case errors.As(err, &exit):
		return pgControlData{}, fixedCause("PostgreSQL command exited with failure")
	}
	return pgControlData{}, fixedCause("PostgreSQL command could not start")
}

// parseCheckpointControl requires the version line first and refuses any
// WARNING (the CRC warning), a stray CR, a missing or repeated field and any
// value outside its shape. Other lines are ignored.
func parseCheckpointControl(raw []byte) (pgControlData, error) {
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	if len(raw) > checkpointControlLimit || strings.Contains(text, "\r") || !strings.HasPrefix(text, "pg_control version number:") {
		return pgControlData{}, errCheckpointState
	}
	fields := make(map[string]string, len(checkpointControlKeys))
	for _, line := range strings.Split(text, "\n") {
		key, value, found := strings.Cut(line, ":")
		key = strings.TrimSpace(key)
		// Only the five keys are recorded, so only they can repeat.
		if _, repeated := fields[key]; strings.HasPrefix(line, "WARNING") || (found && repeated) {
			return pgControlData{}, errCheckpointState
		}
		if found && slices.Contains(checkpointControlKeys, key) {
			fields[key] = strings.TrimSpace(value)
		}
	}
	data := pgControlData{ControlVersion: fields[checkpointControlKeys[0]], CatalogVersion: fields[checkpointControlKeys[1]],
		SystemIdentifier: fields[checkpointControlKeys[2]], State: fields[checkpointControlKeys[3]], CheckpointLSN: fields[checkpointControlKeys[4]]}
	if !checkpointDigits(data.ControlVersion, 10) || !checkpointDigits(data.CatalogVersion, 10) || !checkpointDigits(data.SystemIdentifier, 20) ||
		!slices.Contains(checkpointClusterStates, data.State) || !checkpointLSNPattern.MatchString(data.CheckpointLSN) {
		return pgControlData{}, errCheckpointState
	}
	return data, nil
}

func checkpointDigits(value string, limit int) bool {
	return len(value) >= 1 && len(value) <= limit && strings.Trim(value, "0123456789") == ""
}
