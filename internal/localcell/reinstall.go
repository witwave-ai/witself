package localcell

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/witwave-ai/witself/internal/store"
)

// ReinstallOptions names the retained installation to readmit. The caller reads
// InstallationID from Inspect; its exact value is the current call's consent.
type ReinstallOptions struct{ InstallationID string }

// reinstallOperations replaces only Reinstall's external boundaries in package
// tests. Nil per-Cell state uses the production operations.
type reinstallOperations struct {
	postgres func(context.Context, *Manifest) error
	query    func(context.Context, *Manifest, string) (string, error)
	stop     func(context.Context) error
}

func (c *Cell) reinstallOps() reinstallOperations {
	if c.reinstallTest != nil {
		return *c.reinstallTest
	}
	return reinstallOperations{postgres: c.startRetainedPostgres, query: c.psql, stop: c.stopAll}
}

// Reinstall readmits exactly the named installation that ordinary Remove left
// stopped with its PGDATA, custody and journal retained, for this build only.
// PostgreSQL runs only to prove that the retained custody the layout uses
// authenticates and that the cluster identity and schema match; it is stopped
// before the retained marker is cleared, last. A legacy proof authenticates the
// bootstrap password through the account query. A selected proof authenticates
// the six role credentials; its bootstrap password is format-checked only,
// because the selected layout never uses the bootstrap login after install.
// A service in maintenance, Faulted or mid-transition refuses. No credential is
// created, rotated or deleted, and nothing is migrated, converted, purged or
// deleted.
func (c *Cell) Reinstall(ctx context.Context, options ReinstallOptions) error {
	lock, err := c.own(context.Background(), false)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	if err := c.maintenanceFenceAllows(context.Background(), 0); err != nil {
		return err
	}
	return c.reinstallLocked(ctx, options)
}

func (c *Cell) reinstallLocked(ctx context.Context, options ReinstallOptions) error {
	m, err := c.admitReinstall(context.Background(), options)
	if err != nil {
		return err
	}
	if err := c.layout(); err != nil {
		return err
	}
	if err := c.begin(context.Background(), "reinstall"); err != nil {
		return err
	}
	if err := c.reinstallBoundary("operation_started"); err != nil {
		return err
	}
	ops := c.reinstallOps()
	// The marker is cleared only after the proof and the stop are durably done.
	steps := []struct {
		name string
		run  func(context.Context) error
	}{
		{"postgres", func(ctx context.Context) error {
			if err := c.prepareComponentPortsFor(context.Background(), "reinstall", m, "postgres", false, false); err != nil {
				return err
			}
			return ops.postgres(ctx, m)
		}},
		{"verified", func(ctx context.Context) error { return c.verifyRetainedIdentity(ctx, m, ops.query) }},
		{"stop", ops.stop},
		{"readmitted", func(context.Context) error {
			m.RemovalRetained = false
			return writeAtomic(c.path("manifest.json"), m, nil)
		}},
	}
	for _, step := range steps {
		// The step's own cut is told apart before the deferred cancel, after
		// which the step's context is always done.
		own := false
		err := c.step(context.Background(), "reinstall", step.name, func() error {
			stepCtx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
			defer cancel()
			stepCtx = withBindOperation(stepCtx, ctx)
			runErr := step.run(stepCtx)
			own = serviceContextLeaseCut(stepCtx, runErr)
			return runErr
		})
		if err == nil {
			continue
		}
		// A failed stop or publication stays open for Repair, as does a crash.
		if errors.Is(err, errInterrupted) || (step.name != "postgres" && step.name != "verified") {
			return err
		}
		rollbackCtx, rollbackCancel := c.deadline(context.Background())
		defer rollbackCancel()
		// As Start's, a failed rollback or finish keeps the step's own cut.
		var cut *leaseWaitCut
		if ops.stop(rollbackCtx) != nil {
			if own && errors.As(err, &cut) {
				return fmt.Errorf("local cell reinstall failed; rollback incomplete, repair required: %w", cut)
			}
			return errors.New("local cell reinstall failed; rollback incomplete, repair required")
		}
		if finishErr := c.finishReinstall(context.Background(), m); finishErr != nil {
			if own && errors.As(err, &cut) {
				return fmt.Errorf("local cell reinstall failed; rollback completion failed, repair required: %w", cut)
			}
			return finishErr
		}
		return err
	}
	return c.finishReinstall(context.Background(), m)
}

// admitReinstall runs under the operation lock and writes nothing; nothing is
// signalled, minted or repaired. A refusal leaves the tree as the pinned lock
// prefix left it: like Start's, that prefix creates run/ and run/operation.lock
// when a purge removed them, and nothing else.
func (c *Cell) admitReinstall(ctx context.Context, options ReinstallOptions) (*Manifest, error) {
	if options.InstallationID == "" {
		return nil, errReinstallIdentity
	}
	m, err := readManifest(ctx, c.cfg.Root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, errReinstallState
	}
	if err != nil {
		return nil, err
	}
	if m.InstallationID != options.InstallationID {
		return nil, errReinstallIdentity
	}
	progress, err := c.readJournal(ctx)
	if err != nil {
		return nil, err
	}
	if err := c.reinstallAvailable(ctx, m, progress); err != nil {
		return nil, err
	}
	if err := c.verifyAll(ctx); err != nil {
		return nil, err
	}
	for _, component := range []string{"postgres", "server", "worker"} {
		state, err := c.processState(ctx, component)
		if err != nil {
			return nil, err
		}
		if state.Alive {
			return nil, errReinstallState
		}
	}
	if err := c.reinstallServiceFence(); err != nil {
		return nil, err
	}
	if _, err := c.password(ctx); err != nil {
		return nil, err
	}
	if m.PrincipalLayoutVersion != 0 {
		if err := c.validatePrincipalPasswords(ctx, m); err != nil {
			return nil, err
		}
	}
	if !filepath.IsAbs(c.cfg.Binaries.PostgresDir) {
		return nil, errors.New("explicit PostgreSQL binary directory is required")
	}
	return m, nil
}

// reinstallAvailable is the side-effect-free offline predicate that Inspect and
// admission share, so Inspect offers reinstall exactly where these terms hold:
// the marker on an installed manifest without a cleanup checkpoint, a verified
// account at this build's schema, a closed journal without a torn tail, private
// PGDATA and PostgreSQL 16. Liveness, the caller's identity, the service fence
// and custody are admission's alone.
func (c *Cell) reinstallAvailable(ctx context.Context, m *Manifest, progress journalProgress) error {
	if !m.RemovalRetained || !m.Installed || m.Cleanup != nil {
		return errReinstallState
	}
	// A never-verified cell holds nothing to readmit; purge it instead.
	if !accountPattern.MatchString(m.AccountID) || m.LastVerifiedSchemaVersion < 1 {
		return errReinstallState
	}
	if m.LastVerifiedSchemaVersion != store.SchemaVersion() {
		return errReinstallVersion
	}
	if progress.pending != nil || progress.tornTail {
		return errReinstallState
	}
	if _, err := os.Lstat(c.path("pg")); err != nil || c.validatePostgresDirectory() != nil {
		return errReinstallState
	}
	// As postmaster() does, read only after the opened handle proves a small
	// regular file: the open never waits, so a FIFO at the name, even one renamed
	// in after any earlier check, cannot block the lock holder, and a symlink at
	// the name is refused, never followed.
	version, err := readRegular(ctx, openPostgresReader, func(info os.FileInfo) bool { return info.Size() <= 64 }, c.path("pg", "PG_VERSION"),
		errReinstallState, errReinstallState, errReinstallState)
	if cut := (*leaseWaitCut)(nil); errors.As(err, &cut) {
		return cut
	} else if err != nil {
		return errReinstallState
	}
	if strings.TrimSuffix(string(version), "\n") != "16" {
		return errReinstallVersion
	}
	return nil
}

// reinstallServiceFence keeps the service start fence for a described cell:
// maintenance, Faulted or a pending transition refuses every launch, so it
// refuses the proof too. Remove always publishes stopped, so stopped and
// running are admitted. No descriptor means no service fence; an unreadable
// or invalid descriptor refuses.
func (c *Cell) reinstallServiceFence() error {
	d, err := c.ServiceDescriptor()
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || d.DesiredState == "maintenance" || d.Faulted {
		return errReinstallState
	}
	if _, err := os.Lstat(c.path("service", "transition.json")); !errors.Is(err, os.ErrNotExist) {
		return errReinstallState
	}
	return nil
}

// startRetainedPostgres is startServicePostgres without its retained-removal
// refusal. Reinstall's proof is its only caller; an unmarked manifest refuses.
func (c *Cell) startRetainedPostgres(ctx context.Context, m *Manifest) error {
	if !m.RemovalRetained {
		return errReinstallState
	}
	if err := c.startPostgres(ctx, m); err != nil {
		return err
	}
	return c.bindServiceProcessBoot(bindOperation(ctx), "postgres")
}

// verifyRetainedIdentity writes nothing, not even the manifest. Selected layouts
// reuse the install-time proof; its fixed causes are returned unchanged.
func (c *Cell) verifyRetainedIdentity(ctx context.Context, m *Manifest, query func(context.Context, *Manifest, string) (string, error)) error {
	if m.PrincipalLayoutVersion != 0 {
		return c.principalOperations().verify(ctx, m)
	}
	account, err := query(ctx, m, "SELECT id FROM accounts WHERE is_default AND status = 'active'")
	if err != nil || account != m.AccountID {
		return errReinstallIdentity
	}
	version, err := query(ctx, m, "SELECT version_id FROM goose_db_version WHERE is_applied ORDER BY id DESC LIMIT 1")
	if err != nil {
		return errReinstallVersion
	}
	if schema, err := strconv.Atoi(version); err != nil || schema != m.LastVerifiedSchemaVersion {
		return errReinstallVersion
	}
	return nil
}

// finishReinstall closes the operation. After a failed proof the marker is still
// set: the cell stays removed and a new Reinstall call may retry.
func (c *Cell) finishReinstall(ctx context.Context, m *Manifest) error {
	m.LastOperation = "reinstall"
	if err := writeAtomic(c.path("manifest.json"), m, nil); err != nil {
		return err
	}
	if err := c.reinstallBoundary("operation_manifest"); err != nil {
		return err
	}
	return c.journal(ctx, "reinstall", "operation", "done")
}

func (c *Cell) reinstallBoundary(name string) error {
	if c.cfg.Failpoint != nil && c.cfg.Failpoint("reinstall", name) != nil {
		return errInterrupted
	}
	return nil
}
