package localcell

import (
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
// incomplete one reports only its identifier.
type CheckpointSummary struct {
	ID             string    `json:"id"`
	InstallationID string    `json:"installation_id,omitempty"`
	CreatedAt      time.Time `json:"created_at,omitzero"`
	Bytes          int64     `json:"bytes,omitempty"`
	Complete       bool      `json:"complete"`
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
		ops.open = func(name string) (*os.File, error) { return openPrivateFile(name, os.O_RDONLY, 0) }
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
	if err := c.begin("checkpoint"); err != nil {
		return err
	}
	if c.cfg.Failpoint != nil && c.cfg.Failpoint("checkpoint", "operation_started") != nil {
		return errInterrupted
	}
	created := false
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
		{"copy", func() error { return c.copyCheckpoint(ctx, checkpointID, plan, &created, ops, &budget) }},
		{"inventory", func() error { return c.inventoryCheckpoint(ctx, checkpointID, plan, ops, &budget) }},
		{"witness", func() error { return c.witnessCheckpoint(ctx, checkpointID, plan, ops) }},
	} {
		// A crash leaves the operation open for Repair and discards nothing.
		if err := c.step("checkpoint", step.name, step.run); errors.Is(err, errInterrupted) {
			return err
		} else if err != nil {
			discarded := !created || c.discardOwnCheckpoint(checkpointID) == nil
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
	} else if progress.pending != nil || progress.tornTail {
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
	// As postmaster() does, read only after Lstat proves a small regular file.
	if info, err := os.Lstat(c.path("pg", "PG_VERSION")); err != nil || !info.Mode().IsRegular() || info.Size() > 64 {
		return nil, errCheckpointSource
	}
	if version, err := readPostgresFile(c.path("pg", "PG_VERSION")); err != nil || strings.TrimSuffix(string(version), "\n") != "16" {
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
		return nil, errCheckpointState
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
// confined source opener, the manifest and custody as private files.
func (c *Cell) openCheckpointEntry(entry checkpointEntry, open func(string) (*os.File, error)) (*os.File, error) {
	if !entry.custody && entry.path != "manifest.json" {
		return open(strings.TrimPrefix(entry.path, "pg/"))
	}
	source, err := c.credentialPath(entry.purpose)
	if !entry.custody {
		return openPrivateFile(c.path("manifest.json"), os.O_RDONLY, 0)
	} else if err != nil {
		return nil, err
	}
	return openPrivateFile(source, os.O_RDONLY, 0)
}

// copyCheckpoint copies the plan in order into a new directory: each file of
// exactly its planned size through a SHA-256 tee into a private temporary
// published once. A failure removes the directory before the failed line.
func (c *Cell) copyCheckpoint(ctx context.Context, checkpointID string, plan *checkpointPlan, created *bool, ops checkpointOperations, budget *checkpointRetryBudget) error {
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
	fail := func(cause error) error { return c.failCheckpoint(checkpointID, cause) }
	if err := privateDir(dir, true); err != nil {
		return fail(errCheckpointCopy)
	}
	open, release, err := c.openCheckpointSource()
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
func (c *Cell) inventoryCheckpoint(ctx context.Context, checkpointID string, plan *checkpointPlan, ops checkpointOperations, budget *checkpointRetryBudget) error {
	dir, err := c.checkpointDir(checkpointID)
	if err != nil {
		return err
	}
	fail := func(cause error) error { return c.failCheckpoint(checkpointID, cause) }
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
	open, release, err := c.openCheckpointSource()
	if err != nil {
		return errCheckpointWitness
	}
	defer release()
	live, err := open("global/pg_control")
	if err != nil {
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

// discardOwnCheckpoint removes this call's directory only while it has no
// witness; a present witness is not a failed discard.
func (c *Cell) discardOwnCheckpoint(checkpointID string) error {
	dir, err := c.checkpointDir(checkpointID)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(filepath.Join(dir, checkpointWitnessName)); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return errCheckpointCopy
	}
	if err := os.RemoveAll(dir); err != nil {
		return errCheckpointCopy
	}
	return syncCheckpointDirectory(c.recoveryDir())
}

// failCheckpoint releases this call's directory before the step's failed line.
func (c *Cell) failCheckpoint(checkpointID string, cause error) error {
	_ = c.discardOwnCheckpoint(checkpointID)
	return cause
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

func (c *Cell) deleteCheckpointLocked(_ context.Context, options DeleteCheckpointOptions) error {
	// The identifier's shape is checked before any filesystem access.
	if !checkpointIDPattern.MatchString(options.CheckpointID) {
		return errCheckpointIdentity
	}
	if progress, err := c.readJournal(); err != nil {
		return err
	} else if progress.pending != nil || progress.tornTail {
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
	// A purge removed journal/; nothing else is recreated.
	if err := privateDir(c.path("journal"), true); err != nil {
		return err
	}
	if err := c.begin("checkpoint_delete"); err != nil {
		return err
	}
	for _, step := range []struct {
		name string
		run  func() error
	}{
		{"unseal", func() error {
			if _, err := os.Lstat(filepath.Join(dir, checkpointWitnessName)); err == nil {
				if err := removePrivateFile(filepath.Join(dir, checkpointWitnessName)); err != nil {
					return err
				}
			} else if !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			return syncCheckpointDirectory(dir)
		}},
		{"remove", func() error {
			if err := os.RemoveAll(dir); err != nil {
				return err
			}
			return syncCheckpointDirectory(c.recoveryDir())
		}},
	} {
		if err := c.step("checkpoint_delete", step.name, step.run); errors.Is(err, errInterrupted) {
			return err
		} else if err != nil {
			return c.closeCheckpointFailure("checkpoint_delete", err, true, nil)
		}
	}
	return c.journal("checkpoint_delete", "operation", "done")
}

// checkpointComplete is the one reader of witness.json: a format-1 witness
// naming this identifier.
func checkpointComplete(dir, checkpointID string) (checkpointWitness, bool) {
	var witness checkpointWitness
	if err := readJSON(filepath.Join(dir, checkpointWitnessName), &witness); err != nil {
		return checkpointWitness{}, false
	}
	return witness, witness.Format == 1 && witness.CheckpointID == checkpointID
}

// listCheckpoints classifies recovery/ after validateRecovery; entries that
// are not id-shaped directories are counted unknown.
func (c *Cell) listCheckpoints() ([]CheckpointSummary, int, error) {
	entries, err := os.ReadDir(c.recoveryDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, nil
	} else if err != nil {
		// Never report zero checkpoints for a recovery/ that could not be read.
		return nil, 0, errCheckpointState
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
		if witness, complete := checkpointComplete(dir, entry.Name()); complete {
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
	inspection.Checkpoints, _, err = c.listCheckpoints()
	return err
}

// validateRecovery is the one validator of recovery/** (RV): every object at
// and below the canonically spelled directory must be private. It never
// follows a link or skips a directory.
func (c *Cell) validateRecovery() error {
	if _, err := os.Lstat(c.recoveryDir()); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return errCheckpointState
	}
	entries, err := os.ReadDir(c.cfg.Root)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if err != nil || !recoveryEntryCanonical(names, filepath.Base(c.recoveryDir())) || privateDir(c.recoveryDir(), false) != nil ||
		validatePrivateTreeSnapshot(c.recoveryDir(), filepath.WalkDir, validateRecoveryObject, os.Lstat) != nil {
		return errCheckpointState
	}
	return nil
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
