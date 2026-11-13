package localcell

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"
)

func (c *Cell) own(create bool) (*operationLock, error) {
	if err := c.validateRoot(create); err != nil {
		return nil, classifyOwnershipFailure(c.cfg.Root, err)
	}
	lock, err := acquireLock(c.cfg.Root, true)
	if err != nil {
		return nil, classifyOwnershipFailure(c.cfg.Root, err)
	}
	// Validate again after taking the stable lock, before following layout paths.
	if err := c.validateRoot(false); err != nil {
		_ = lock.Close()
		return nil, err
	}
	return lock, nil
}

func (c *Cell) ownExisting() (*operationLock, error) {
	if err := c.validateRoot(false); err != nil {
		return nil, classifyOwnershipFailure(c.cfg.Root, err)
	}
	lock, err := acquireLock(c.cfg.Root, false)
	if err != nil {
		return nil, classifyOwnershipFailure(c.cfg.Root, err)
	}
	if lock == nil {
		return c.own(false)
	}
	if err := c.validateRoot(false); err != nil {
		_ = lock.Close()
		return nil, err
	}
	return lock, nil
}

func (c *Cell) begin(operation string) error {
	progress, err := c.readJournal()
	if err != nil {
		return err
	}
	if progress.pending != nil || progress.tornTail {
		return errors.New("local cell operation interrupted; run repair first")
	}
	return c.journal(operation, "operation", "started")
}

func (c *Cell) finish(m *Manifest, operation string) error {
	m.LastOperation = operation
	if err := writeAtomic(c.path("manifest.json"), m, nil); err != nil {
		return err
	}
	return c.journal(operation, "operation", "done")
}

// Install creates a new disposable cluster. Existing data is never adopted or
// overwritten. A failed or interrupted installation is retained for Repair.
func (c *Cell) Install(ctx context.Context, options InstallOptions) error {
	timeout := c.cfg.Timeout
	if options.ManagedPrincipals {
		timeout = c.readinessTimeout("server", &Manifest{})
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// Selection never adopts a manifest or old PGDATA, even before lock/layout
	// creation. Repeating the check under the lock closes concurrent adoption.
	if options.ManagedPrincipals {
		if _, err := os.Lstat(c.path("manifest.json")); !errors.Is(err, os.ErrNotExist) {
			return errPrincipalLayout
		}
		if entries, err := os.ReadDir(c.path("pg")); (err == nil && len(entries) != 0) || (err != nil && !errors.Is(err, os.ErrNotExist)) {
			return errPrincipalLayout
		}
	}
	lock, err := c.own(true)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	if _, err := readManifest(c.cfg.Root); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return err
		}
		return errors.New("local cell is already installed")
	}
	if entries, err := os.ReadDir(c.path("pg")); err == nil && len(entries) > 0 {
		return errors.New("refusing existing PostgreSQL data; repair required")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("cannot inspect PostgreSQL data")
	}
	if err := c.maintenanceFenceAllows(0); err != nil {
		return err
	}
	if err := c.layout(); err != nil {
		return err
	}
	ports, err := c.allocateAvailablePorts(Ports{}, "")
	if err != nil {
		return err
	}
	installationID, err := newLocalCellInstallationID(runtime.GOOS)
	if err != nil {
		return errors.New("cannot allocate local cell identity")
	}
	pgVersion, err := c.reportedVersion(ctx, filepath.Join(c.cfg.Binaries.PostgresDir, executable("postgres")))
	if err != nil || !postgresVersionPattern.MatchString(pgVersion) {
		return errors.New("cannot read PostgreSQL major/minor version")
	}
	serverVersion, err := c.reportedVersion(ctx, c.cfg.Binaries.Server)
	if err != nil {
		return err
	}
	workerVersion, err := c.reportedVersion(ctx, c.cfg.Binaries.Worker)
	if err != nil {
		return err
	}
	m := &Manifest{LayoutVersion: LayoutVersion, InstallationID: installationID, CreatedAt: time.Now().UTC(),
		PostgreSQLVersion: pgVersion, ServerVersion: serverVersion, WorkerVersion: workerVersion,
		Ports: ports, Database: localDatabaseName, Role: "localcell", LastOperation: "install"}
	m.MigrationWait, m.JobRestartPolicy = c.runtimeSettings(m)
	if options.ManagedPrincipals {
		m.PrincipalLayoutVersion = PrincipalLayoutVersion
		m.PrincipalRoles = installerRoleNames()
	}
	if err := c.begin("install"); err != nil {
		return err
	}
	steps := []struct {
		name string
		run  func() error
	}{
		{"manifest", func() error { return writeAtomic(c.path("manifest.json"), m, nil) }},
		{"password", c.makePassword},
		{"initdb", func() error {
			if err := c.preparePostgresDirectory(); err != nil {
				return err
			}
			return c.pgCommand(ctx, "initdb", "-D", c.path("pg"), "-U", m.Role, "--auth=scram-sha-256", "--pwfile="+c.path("run", "postgres.password"), "--encoding=UTF8", "--locale=C")
		}},
		{"configure", func() error { return c.configurePostgres(m) }},
		{"postgres", func() error { return c.startServicePostgres(ctx, m) }},
		{"database", func() error {
			// initdb creates the bootstrap role. createdb must connect to a
			// template database in THIS new cluster to create the application DB.
			_, err := c.databaseCommand(ctx, m, "createdb", "--no-password", "-h", "127.0.0.1", "-p", strconv.Itoa(m.Ports.Postgres), "-U", m.Role, "--maintenance-db=template1", "--template=template0", localDatabaseName)
			return err
		}},
	}
	if options.ManagedPrincipals {
		steps = append(steps, struct {
			name string
			run  func() error
		}{"principal_credentials", func() error { return c.principalOperations().credentials(ctx, m) }})
		ops := c.principalOperations()
		for i, fn := range []func(context.Context, *Manifest) error{ops.provision, ops.migrate, ops.grants, ops.verify} {
			steps = append(steps, struct {
				name string
				run  func() error
			}{selectedInstallSteps[i+1], func() error { return fn(ctx, m) }})
		}
	}
	if !options.LeaveRunning {
		steps = append(steps, struct {
			name string
			run  func() error
		}{"stop", func() error { return c.stopAll(ctx) }})
	}
	for _, step := range steps {
		if err := c.step("install", step.name, step.run); err != nil {
			if !errors.Is(err, errInterrupted) {
				rollbackCtx, rollbackCancel := c.deadline(context.Background())
				defer rollbackCancel()
				_ = c.stopAll(rollbackCtx)
			}
			return err
		}
	}
	m.Installed = true
	return c.finish(m, "install")
}

// installerStartOperations replaces only external startup boundaries in package
// tests. Nil per-Cell state uses the same production operations for every phase.
type installerStartOperations struct {
	postgres func(context.Context, *Manifest) error
	verify   func(context.Context, *Manifest) error
	launch   func(context.Context, *Manifest, string, func() error) error
	ready    func(context.Context, *Manifest, string, bool) error
	account  func(context.Context, *Manifest) error
}

func (c *Cell) principalStartOperations() installerStartOperations {
	if c.installerStart != nil {
		return *c.installerStart
	}
	return installerStartOperations{
		postgres: c.startServicePostgres, verify: c.verifyRuntimePrincipalStart,
		launch: c.startChildPrepared, ready: c.waitReadyWithPortRecovery,
		account: c.verifyStartAccount,
	}
}

func (c *Cell) verifyStartAccount(ctx context.Context, m *Manifest) error {
	account, err := c.psql(ctx, m, "SELECT id FROM accounts WHERE is_default AND status = 'active'")
	if err != nil || !accountPattern.MatchString(account) || (m.AccountID != "" && account != m.AccountID) {
		return errors.New("local cell bootstrap account mismatch")
	}
	version, err := c.psql(ctx, m, "SELECT version_id FROM goose_db_version WHERE is_applied ORDER BY id DESC LIMIT 1")
	if err != nil {
		return err
	}
	schema, err := strconv.Atoi(version)
	if err != nil || schema < 1 {
		return errors.New("local cell schema verification failed")
	}
	m.AccountID, m.LastVerifiedSchemaVersion = account, schema
	if m.PrincipalLayoutVersion != 0 {
		if err := c.prepareSelectedHealthCredential(ctx, m); err != nil {
			return err
		}
	}
	return writeAtomic(c.path("manifest.json"), m, nil)
}

// Start orders PostgreSQL, server readiness, then worker readiness. It never
// inherits the caller's environment or assumes HTTP readiness proves ownership.
func (c *Cell) Start(ctx context.Context) error {
	lock, err := c.own(false)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	m, err := readManifest(c.cfg.Root)
	if err != nil {
		return err
	}
	if !m.Installed {
		return errors.New("local cell installation is incomplete")
	}
	if err := c.serviceStartFence(); err != nil {
		return err
	}
	if err := c.maintenanceFenceAllows(0); err != nil {
		return err
	}
	m.MigrationWait, m.JobRestartPolicy = c.runtimeSettings(m)
	if m.RemovalRetained {
		return errors.New("local cell installation is incomplete")
	}
	if err := c.layout(); err != nil {
		return err
	}
	if m.PrincipalLayoutVersion == 0 {
		if _, err := c.password(); err != nil {
			return err
		}
	} else if err := c.validatePrincipalPasswords(m); err != nil {
		return err
	}
	if err := c.verifyAll(); err != nil {
		return err
	}
	before := map[string]bool{}
	for _, component := range []string{"postgres", "server", "worker"} {
		state, err := c.processState(component)
		if err != nil {
			return err
		}
		before[component] = state.Alive
	}
	if err := c.begin("start"); err != nil {
		return err
	}
	launched := make(map[string]bool)
	ops := c.principalStartOperations()
	prepareChild := func(component string) func() error {
		return func() error {
			// A replacement belongs to this Start even if the entry snapshot
			// contained its now-exited predecessor. Roll back the replacement.
			before[component] = false
			if err := c.prepareComponentPorts(m, component, false, false); err != nil {
				return err
			}
			launched[component] = true
			return nil
		}
	}
	steps := []struct {
		name string
		run  func(context.Context) error
	}{
		{"postgres", func(ctx context.Context) error {
			if err := c.verifyAll(); err != nil {
				return err
			}
			state, err := c.processState("postgres")
			if err != nil {
				return err
			}
			if state.Alive {
				if !state.Matches {
					return errors.New("local cell process identity mismatch")
				}
				return c.bindServiceProcessBoot("postgres")
			}
			before["postgres"] = false
			if err := c.prepareComponentPorts(m, "postgres", false, false); err != nil {
				return err
			}
			return ops.postgres(ctx, m)
		}},
		{"server", func(ctx context.Context) error {
			if m.PrincipalLayoutVersion != 0 {
				return runPrincipalStartup(func() error { return ops.verify(ctx, m) },
					func() error { return ops.launch(ctx, m, "server", prepareChild("server")) })
			}
			return ops.launch(ctx, m, "server", prepareChild("server"))
		}},
		{"server_ready", func(ctx context.Context) error {
			return ops.ready(ctx, m, "server", launched["server"])
		}},
		{"account", func(ctx context.Context) error { return ops.account(ctx, m) }},
		{"worker", func(ctx context.Context) error {
			return ops.launch(ctx, m, "worker", prepareChild("worker"))
		}},
		{"worker_ready", func(ctx context.Context) error {
			return ops.ready(ctx, m, "worker", launched["worker"])
		}},
	}
	for _, step := range steps {
		err := c.step("start", step.name, func() error {
			timeout := c.cfg.Timeout
			if step.name == "server_ready" {
				timeout = c.readinessTimeout("server", m)
			}
			stepCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			// The readiness steps run even when the step context is already done:
			// waitReady turns the cancellation into the fixed readiness cause with
			// the pre-rollback process observation (slice LC-S2-PW3), which a bare
			// context error would discard (review LC-S2-R1R4, R4-1).
			if err := stepCtx.Err(); err != nil && step.name != "server_ready" && step.name != "worker_ready" {
				return err
			}
			return step.run(stepCtx)
		})
		if err != nil {
			if errors.Is(err, errInterrupted) {
				return err
			}
			rollbackCtx, rollbackCancel := c.deadline(context.Background())
			defer rollbackCancel()
			if rollbackErr := c.stopSelected(rollbackCtx, before); rollbackErr != nil {
				return errors.New("local cell startup failed; rollback incomplete, repair required")
			}
			if finishErr := c.finish(m, "start"); finishErr != nil {
				return finishErr
			}
			return err
		}
	}
	return c.finish(m, "start")
}

// Stop refuses all signals if any recorded process has conflicting identity.
func (c *Cell) Stop(ctx context.Context) error {
	return c.stopWithServiceFence(ctx, true)
}

// A host restart or child failure shuts down the owned tree while preserving
// running intent. Only an explicit Stop changes the durable desired state.
func (c *Cell) stopServiceChildren(ctx context.Context) error {
	return c.stopWithServiceFence(ctx, false)
}

func (c *Cell) stopWithServiceFence(ctx context.Context, fence bool) error {
	ctx, cancel := c.serviceShutdownDeadline(ctx)
	defer cancel()
	lock, err := c.own(false)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	if fence {
		if err := c.setServiceDesiredStateLocked("stopped"); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	m, err := readManifest(c.cfg.Root)
	if err != nil {
		return err
	}
	if err := c.verifyAll(); err != nil {
		return err
	}
	if err := c.layout(); err != nil {
		return err
	}
	if err := c.begin("stop"); err != nil {
		return err
	}
	if err := c.step("stop", "stop", func() error { return c.stopAll(ctx) }); err != nil {
		return err
	}
	return c.finish(m, "stop")
}

// Repair reverts an interrupted operation to stopped state using its journal
// and PID fences. It preserves PGDATA, including incomplete initdb output.
// An install is completed only when all required steps are durably done. Other
// incomplete installations stay uninstalled; they are never guessed/adopted.
func (c *Cell) Repair(ctx context.Context) error {
	parent := ctx
	ctx, cancel := c.deadline(ctx)
	defer cancel()
	lock, err := c.ownExisting()
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	m, manifestErr := readManifest(c.cfg.Root)
	if manifestErr != nil && !errors.Is(manifestErr, os.ErrNotExist) {
		return manifestErr
	}
	if m != nil && m.PrincipalLayoutVersion != 0 {
		selected, selectedCancel := context.WithTimeout(parent, c.readinessTimeout("server", m))
		defer selectedCancel()
		ctx = selected
	}
	if err := c.maintenanceFenceAllows(0); err != nil {
		return err
	}
	progress, err := c.readJournal()
	if err != nil {
		return err
	}
	if m != nil && m.Cleanup != nil && progress.pending != nil && progress.pending.Operation != "remove" {
		return errors.New("mismatched local cell journal operation")
	}
	if err := c.discardJournalTail(progress); err != nil {
		return err
	}
	pending := progress.pending
	if pending == nil {
		return nil
	}
	if (pending.Operation == "start" || pending.Operation == "install") && pending.Step == "postgres" {
		if err := c.recoverPostgresRecord(); err != nil {
			return err
		}
	}
	if err := c.verifyAll(); err != nil {
		return err
	}
	if m != nil && pending.Operation == "remove" {
		if err := c.stopAll(ctx); err != nil {
			return err
		}
		if err := c.retainRemoval(m); err != nil {
			return err
		}
		return c.finishRemoval(m, false)
	}
	if m != nil && m.PrincipalLayoutVersion != 0 && pending.Operation == "install" {
		// Only this fresh installation's durable database phase authorizes
		// resumption. A partial initdb or partial credential publication stays
		// visible and non-startable, never converted or guessed.
		if !progress.done["database"] {
			_ = c.stopAll(ctx)
			return errPrincipalRepair
		}
		if err := c.principalOperations().resumePostgres(ctx, m); err != nil {
			return err
		}
		if m.Installed && !progress.installComplete(m) {
			_ = c.stopAll(ctx)
			return errPrincipalRepair
		}
		if progress.done["principals"] && !m.Installed {
			if err := c.principalOperations().provision(ctx, m); err != nil {
				_ = c.stopAll(ctx)
				return err
			}
		}
		if err := c.installPrincipals(ctx, m, progress); err != nil {
			_ = c.stopAll(ctx)
			return err
		}
		// A persisted verification record alone cannot bless changed custody.
		if err := c.principalOperations().verify(ctx, m); err != nil {
			_ = c.stopAll(ctx)
			return err
		}
		progress, err = c.readJournal()
		if err != nil {
			return err
		}
	}
	if err := c.step(pending.Operation, "revert_stop", func() error { return c.stopAll(ctx) }); err != nil {
		return err
	}
	if m != nil {
		if progress.installComplete(m) {
			m.Installed = true
		}
		m.LastOperation = "repair"
		if err := writeAtomic(c.path("manifest.json"), m, nil); err != nil {
			return err
		}
	}
	return c.journal(pending.Operation, "operation", "done")
}

// Remove stops the cell and retains its custody and recovery history. Only a
// current Purge call deletes owned data/runtime paths. The root lock remains.
func (c *Cell) Remove(ctx context.Context, options RemoveOptions) error {
	ctx, cancel := c.serviceShutdownDeadline(ctx)
	defer cancel()
	if err := c.exitServiceHostForRemoval(ctx); err != nil {
		return err
	}
	lock, err := c.ownExisting()
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	if err := c.maintenanceFenceAllows(0); err != nil {
		return err
	}
	if err := c.setServiceDesiredStateLocked("stopped"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	m, err := readManifest(c.cfg.Root)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if m == nil {
		if _, err := os.Stat(c.path("pg")); !errors.Is(err, os.ErrNotExist) {
			return errors.New("refusing removal of unrecorded PostgreSQL data")
		}
		return nil
	}
	if err := c.verifyAll(); err != nil {
		return err
	}
	if err := c.layout(); err != nil {
		return err
	}
	if err := c.begin("remove"); err != nil {
		return err
	}
	if err := c.step("remove", "stop", func() error { return c.stopAll(ctx) }); err != nil {
		return err
	}
	if !options.Purge {
		if err := c.retainRemoval(m); err != nil {
			return err
		}
		return c.finishRemoval(m, false)
	}
	if options.Purge {
		for _, name := range []string{"spool", "tmp", "logs"} {
			if err := c.step("remove", name, func() error { return os.RemoveAll(c.path(name)) }); err != nil {
				return err
			}
		}
		// Disable startup durably before the only destructive data step. Repair
		// retains any remaining data after an interrupted purge.
		m.Installed = false
		if err := writeAtomic(c.path("manifest.json"), m, nil); err != nil {
			return err
		}
		if err := c.step("remove", "purge", func() error { return os.RemoveAll(c.path("pg")) }); err != nil {
			return err
		}
	}
	m.LastOperation = "remove"
	// Checkpoint the final journal intent in the manifest before deleting the
	// journal itself. Repair can replay this bounded cleanup without guessing.
	m.Cleanup = &journalEntry{Operation: "remove", Step: "runtime_cleanup", State: "started", At: time.Now().UTC()}
	if err := writeAtomic(c.path("manifest.json"), m, nil); err != nil {
		return err
	}
	if err := c.removalBoundary("cleanup_checkpoint"); err != nil {
		return err
	}
	if err := c.journal("remove", "runtime_cleanup", "started"); err != nil {
		return err
	}
	return c.finishRemoval(m, options.Purge)
}

func (c *Cell) finishRemoval(m *Manifest, purge bool) error {
	if !purge {
		return c.finishRetainedRemoval(m)
	}
	// The stable root lock remains held while the subordinate lock inode and
	// journal are removed. Runtime cleanup is repeatable after interruption.
	for _, name := range []string{"run", "journal"} {
		if err := os.RemoveAll(c.path(name)); err != nil {
			return errors.New("cannot remove local cell runtime directory")
		}
		if c.cfg.Failpoint != nil && c.cfg.Failpoint("remove", "cleanup_"+name) != nil {
			return errInterrupted
		}
	}
	if err := os.Remove(c.path("manifest.json")); err != nil {
		return errors.New("cannot remove local cell manifest")
	}
	return syncDirectory(c.cfg.Root)
}

// Inspect reads state and probes the advisory lock without creating records.
func (c *Cell) Inspect(_ context.Context) (Inspection, error) {
	result := Inspection{Processes: map[string]ProcessState{}}
	if err := c.validateRoot(false); err != nil {
		return result, err
	}
	lock, err := acquireLock(c.cfg.Root, false)
	if errors.Is(err, ErrLocked) {
		result.Locked = true
	} else if err != nil {
		return result, err
	} else if lock != nil {
		defer func() { _ = lock.Close() }()
	}
	result.Manifest, err = readManifest(c.cfg.Root)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return result, err
	}
	if info, err := os.Stat(c.path("pg")); err == nil {
		result.PGDataPresent = info.IsDir()
	} else if !errors.Is(err, os.ErrNotExist) {
		return result, errors.New("cannot inspect PostgreSQL data")
	}
	progress, err := c.readJournal()
	if err != nil {
		return result, err
	}
	result.Interrupted = progress.pending
	result.JournalTailIncomplete = progress.tornTail
	if result.Manifest != nil && !result.Manifest.Installed {
		result.InstallState = "install incomplete; purge to start over"
		if progress.installComplete(result.Manifest) {
			result.InstallState = "install completion pending; run repair"
		}
	}
	if result.Manifest != nil && result.Manifest.PrincipalLayoutVersion != 0 {
		result.PrincipalTopology = "unverified"
		if result.Interrupted != nil && !result.Manifest.Installed {
			result.InstallState = "selected install incomplete; run repair"
		}
	}
	if result.Manifest != nil && result.Manifest.RemovalRetained {
		// The marker refuses startup either way; claim retained data only while
		// PGDATA exists (an interrupted purge may already have deleted it).
		result.InstallState = "removed; data retained; reinstall unsupported"
		if !result.PGDataPresent {
			result.InstallState = "removed; data purged or missing; reinstall unsupported"
		}
	}
	for _, component := range []string{"postgres", "server", "worker"} {
		state, err := c.processState(component)
		if err != nil {
			return result, err
		}
		if component == "postgres" {
			pid, err := c.postmaster()
			if err != nil {
				state.Matches = false
			} else if pid == 0 {
				state.Matches = false
			} else if state.PID == 0 {
				_, alive, identityErr := processIdentity(pid)
				if identityErr != nil {
					return result, errors.New("cannot inspect PostgreSQL process")
				}
				state = ProcessState{PID: pid, Alive: alive}
			} else if state.PID != pid {
				state.Matches = false
			}
		}
		result.Processes[component] = state
	}
	return result, nil
}

// Status observes readiness only for live processes whose identity matches.
func (c *Cell) Status(ctx context.Context) (Status, error) {
	inspection, err := c.Inspect(ctx)
	result := Status{Inspection: inspection}
	if err != nil || inspection.Manifest == nil {
		return result, err
	}
	ctx, cancel := c.deadline(ctx)
	defer cancel()
	client := localHTTPClient()
	defer client.CloseIdleConnections()
	m := inspection.Manifest
	if m.PrincipalLayoutVersion != 0 && !inspection.Locked && inspection.Interrupted == nil {
		if err := c.verifyMaintenanceTopology(ctx, m); err == nil {
			result.PrincipalTopology = "verified"
		}
	}
	server, worker := inspection.Processes["server"], inspection.Processes["worker"]
	result.ServerReady = server.Alive && server.Matches && ready(ctx, client, m.Ports.ServerHealth)
	result.WorkerReady = worker.Alive && worker.Matches && ready(ctx, client, m.Ports.WorkerHealth)
	// Readiness cannot survive a process exit or identity change during HTTP.
	for _, component := range []struct {
		name  string
		ready *bool
	}{{"server", &result.ServerReady}, {"worker", &result.WorkerReady}} {
		if *component.ready {
			current, err := c.processState(component.name)
			previous := inspection.Processes[component.name]
			*component.ready = err == nil && current.Alive && current.Matches && current.PID == previous.PID
			if err == nil {
				result.Processes[component.name] = current
			}
		}
	}
	return result, nil
}

func (c *Cell) processState(component string) (ProcessState, error) {
	state, _, err := c.processStateWithRecord(component)
	return state, err
}

// Return the exact record used by the observation. Readiness retains this
// fence even if rollback or another actor later changes the durable record.
func (c *Cell) processStateWithRecord(component string) (ProcessState, ProcessRecord, error) {
	var record ProcessRecord
	if err := readJSON(c.path("run", component+".json"), &record); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ProcessState{}, record, nil
		}
		return ProcessState{}, record, err
	}
	if record.PID <= 1 || record.StartTime == "" {
		return ProcessState{}, record, errors.New("invalid local cell process record")
	}
	identity, alive, err := processIdentity(record.PID)
	if err != nil {
		return ProcessState{}, record, errors.New("cannot verify local cell process identity")
	}
	return ProcessState{PID: record.PID, Alive: alive, Matches: alive && identity == record.StartTime && record.Root == c.cfg.Root && record.Component == component}, record, nil
}

func (c *Cell) postmaster() (int, error) {
	path := c.path("pg", "postmaster.pid")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > 8192 {
		return 0, errors.New("invalid PostgreSQL process record")
	}
	data, err := readPostgresFile(path)
	if err != nil {
		return 0, errors.New("cannot read PostgreSQL process record")
	}
	if pid, handled, err := c.pendingPostmasterPID(data); handled || err != nil {
		return pid, err
	}
	lines := postmasterLines(data)
	if len(lines) < 3 || filepath.Clean(lines[1]) != c.path("pg") {
		return 0, errors.New("PostgreSQL data directory identity mismatch")
	}
	pid, err := strconv.Atoi(lines[0])
	if err != nil || pid <= 1 {
		return 0, errors.New("invalid PostgreSQL process identity")
	}
	return pid, nil
}

func (c *Cell) verifyAll() error {
	pgPID, err := c.postmaster()
	if err != nil {
		return err
	}
	for _, component := range []string{"postgres", "server", "worker"} {
		state, err := c.processState(component)
		if err != nil {
			return err
		}
		if state.Alive && !state.Matches {
			return errors.New("local cell process identity mismatch; refusing to signal")
		}
		if component == "postgres" {
			if state.Alive && pgPID != state.PID && !c.pendingPostgresWithoutPID(state, pgPID) {
				return errors.New("PostgreSQL process ownership is unproven; refusing to signal")
			}
			if pgPID != 0 {
				_, pgAlive, identityErr := processIdentity(pgPID)
				if identityErr != nil || (pgAlive && (!state.Alive || pgPID != state.PID)) {
					return errors.New("PostgreSQL process ownership is unproven; refusing to signal")
				}
			}
		}
	}
	return nil
}

// recoverPostgresRecord is permitted only by a recorded, interrupted postgres
// launch step. postmaster.pid supplies the data directory and start epoch;
// native process creation time must independently agree before recording a fence.
func (c *Cell) recoverPostgresRecord() error {
	pid, err := c.postmaster()
	if err != nil || pid == 0 {
		return err
	}
	state, err := c.processState("postgres")
	if err != nil {
		return err
	}
	if state.Alive {
		if !state.Matches || state.PID != pid {
			return fixedCause("PostgreSQL recovery identity mismatch")
		}
		return nil
	}
	data, err := readPostgresFile(c.path("pg", "postmaster.pid"))
	if err != nil {
		return fixedCause("cannot read PostgreSQL recovery identity")
	}
	lines := postmasterLines(data)
	if len(lines) < 3 || filepath.Clean(lines[1]) != c.path("pg") || lines[0] != strconv.Itoa(pid) {
		return fixedCause("PostgreSQL recovery record changed")
	}
	epoch, err := strconv.ParseInt(lines[2], 10, 64)
	if err != nil || epoch <= 0 {
		return fixedCause("invalid PostgreSQL recovery start time")
	}
	observeIdentity, observeStartedAt := processIdentity, processStartedAt
	if c.recoveryProcessIdentity != nil {
		observeIdentity = c.recoveryProcessIdentity
	}
	if c.recoveryProcessStartedAt != nil {
		observeStartedAt = c.recoveryProcessStartedAt
	}
	identityBefore, alive, err := observeIdentity(pid)
	if err != nil {
		return fixedCause("cannot verify PostgreSQL recovery process")
	}
	if !alive {
		// A proven-dead postmaster needs no recovered signaling authority.
		// Keep its stale PID file for PostgreSQL to reconcile on the next start.
		return nil
	}
	started, err := observeStartedAt(pid)
	if err != nil || !postgresRecoveryStartMatches(started, epoch) {
		return fixedCause("PostgreSQL recovery start time mismatch")
	}
	identity, alive, err := observeIdentity(pid)
	if err != nil || !alive || identity != identityBefore {
		return fixedCause("cannot verify PostgreSQL recovery process")
	}
	return writeAtomic(c.path("run", "postgres.json"), ProcessRecord{PID: pid, StartTime: identity, Root: c.cfg.Root, Component: "postgres"}, nil)
}

func postgresRecoveryStartMatches(started time.Time, epoch int64) bool {
	// The postmaster records its epoch after process creation, which may lag
	// under load. Allow creation up to 60 seconds before that record and one
	// second after it for clock granularity. The unchanged before/after process
	// identity check above, not this window, prevents PID reuse. Compare the
	// positive difference only after ordering to avoid overflowing epoch+1.
	created := started.Unix()
	return epoch > 0 && created >= epoch-60 && (created <= epoch || created-epoch <= 1)
}

func (c *Cell) stopAll(ctx context.Context) error { return c.stopSelected(ctx, nil) }

func (c *Cell) stopSelected(ctx context.Context, keep map[string]bool) error {
	if runtime.GOOS != "windows" {
		return c.stopSelectedLegacy(ctx, keep)
	}
	if err := c.verifyAll(); err != nil {
		return err
	}
	return drainServiceTree(ctx, keep, func(ctx context.Context, component string) serviceStopOutcome {
		state, err := c.processState(component)
		if err != nil || (state.Alive && !state.Matches) {
			return serviceStopOutcome{Kind: serviceStopRefused, Component: component, Detail: "shutdown:incarnation"}
		}
		if state.PID == 0 {
			return serviceStopOutcome{Kind: serviceStopAlreadyExited, Component: component}
		}
		return c.stopChildOutcome(ctx, component, state)
	}, func(ctx context.Context) error {
		if err := c.verifyAll(); err != nil {
			return err
		}
		state, err := c.processState("postgres")
		if err != nil || !state.Alive {
			return err
		}
		if handled, err := c.terminatePendingPostgres(ctx, state); handled || err != nil {
			return err
		}
		return c.stopPostgres(ctx, state)
	})
}

func (c *Cell) stopSelectedLegacy(ctx context.Context, keep map[string]bool) error {
	if err := c.verifyAll(); err != nil {
		return err
	}
	for _, component := range []string{"worker", "server"} {
		if keep[component] {
			continue
		}
		state, err := c.processState(component)
		if err != nil {
			return err
		}
		if !state.Alive {
			continue
		}
		if !state.Matches {
			return errors.New("local cell process identity mismatch; refusing to signal")
		}
		if err := c.terminateChild(ctx, component, state); err != nil {
			return err
		}
		for {
			current, err := c.processState(component)
			if err != nil {
				return err
			}
			if !current.Alive || !current.Matches {
				break
			}
			select {
			case <-ctx.Done():
				return errors.New("local cell component stop deadline exceeded")
			case <-time.After(25 * time.Millisecond):
			}
		}
	}
	if keep["postgres"] {
		return nil
	}
	if err := c.verifyAll(); err != nil {
		return err
	}
	state, err := c.processState("postgres")
	if err != nil || !state.Alive {
		return err
	}
	if handled, err := c.terminatePendingPostgres(ctx, state); handled || err != nil {
		return err
	}
	return c.stopPostgres(ctx, state)
}
