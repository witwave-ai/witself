package localcell

import (
	"context"
	"errors"
	"os"
	"strings"
)

type serviceBootRecord struct {
	InstallationID string `json:"installation_id"`
	BootID         string `json:"boot_id"`
}

// reconcileServiceBoot never obtains signaling authority from an old PID. All
// records are validated before removing any stale identity, including PG's PID
// file, which otherwise can mistake a reused PID for a live postmaster.
func (c *Cell) reconcileServiceBoot(ctx context.Context) error {
	lock, err := c.own(ctx, false)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	boot, err := serviceBootID()
	if err != nil {
		return err
	}
	d, err := c.serviceDescriptor(ctx)
	if err != nil {
		return err
	}
	var previous serviceBootRecord
	err = readJSON(ctx, c.path("service", "boot.json"), &previous)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil && (previous.InstallationID != d.InstallationID || previous.BootID == "") {
		return ErrServiceOwnershipConflict
	}
	if err := c.maintenanceFenceAllows(ctx, 0); err != nil {
		return err
	}
	if previous.BootID != "" && previous.BootID != boot {
		if err := c.reconcileOldBoot(ctx, previous.BootID, boot); err != nil {
			return err
		}
	}
	// writeAtomic carries the one bounded retry of a transient Windows private
	// refusal; a retry here would nest inside it.
	return writeAtomic(c.path("service", "boot.json"), serviceBootRecord{d.InstallationID, boot}, nil)
}

func (c *Cell) reconcileOldBoot(ctx context.Context, oldBoot, currentBoot string) error {
	if oldBoot == "" || currentBoot == "" || oldBoot == currentBoot {
		return ErrServiceOwnershipConflict
	}
	var records []string
	var pg ProcessRecord
	for _, component := range []string{"postgres", "server", "worker"} {
		var record ProcessRecord
		err := readJSON(ctx, c.path("run", component+".json"), &record)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || record.Root != c.cfg.Root || record.Component != component || record.PID <= 1 || record.StartTime == "" || record.LaunchPending {
			return serviceReadFailure(err, ErrServiceOwnershipConflict)
		}
		// Linux's boot-bound process identity is independently checked. On
		// Darwin the trusted boot epoch fences the entire prior run set.
		if strings.HasPrefix(currentBoot, "linux:") && !strings.HasPrefix(record.StartTime, oldBoot+":") {
			return ErrServiceOwnershipConflict
		}
		if strings.HasPrefix(currentBoot, "windows:") {
			// A creation FILETIME is not a boot identity. Unknown legacy records
			// and records from the new boot cannot be deleted. Even an old-boot
			// claim must not erase an independently observed live incarnation.
			if record.BootID != oldBoot {
				return ErrServiceOwnershipConflict
			}
			identity, alive, err := processIdentity(record.PID)
			if err != nil || (alive && identity == record.StartTime) {
				return ErrServiceOwnershipConflict
			}
		}
		records = append(records, c.path("run", component+".json"))
		if component == "postgres" {
			pg = record
		}
	}
	pid, err := c.postmaster(ctx)
	if err != nil {
		return err
	}
	if pid != 0 && (pg.PID != pid || pg.StartTime == "") {
		return ErrServiceOwnershipConflict
	}
	// Remove PG's file first so a crash during reconciliation can be retried
	// with the still-durable PostgreSQL identity record.
	if pid != 0 {
		if err := removePrivateFile(c.path("pg", "postmaster.pid")); err != nil {
			return err
		}
		if err := syncDirectory(c.path("pg")); err != nil {
			return err
		}
	}
	for _, path := range records {
		if err := removePrivateFile(path); err != nil {
			return err
		}
	}
	return syncDirectory(c.path("run"))
}

// bindServiceProcessBoot extends only explicit local service launches. The
// platform launcher remains responsible for its pre-execution PID fence. A
// crash before this second publication leaves a record without boot authority;
// Windows old-boot reconciliation refuses that record instead of guessing.
func (c *Cell) bindServiceProcessBoot(ctx context.Context, component string) error {
	return c.bindServiceProcessBootWithReader(ctx, component, serviceBootID)
}

// bindOperationKey carries an operation's own context inside each of its step
// contexts, so that a binding after a child started ends with the operation,
// never at the step's own deadline: the next step has a fresh one, so a binding
// that outlasted its step's budget and then bound lets the operation complete.
type bindOperationKey struct{}

// Nothing nests: a step loop run inside another's step would carry its own
// operation, the outer step's context, over the outer one, so its bindings
// would end at the outer step's deadline (LC-LEASE-2E7, n3).
func withBindOperation(step, operation context.Context) context.Context {
	return context.WithValue(step, bindOperationKey{}, operation)
}

// bindOperation is the operation's context that ctx carries, else ctx itself.
func bindOperation(ctx context.Context) context.Context {
	if operation, ok := ctx.Value(bindOperationKey{}).(context.Context); ok {
		return operation
	}
	return ctx
}

func (c *Cell) bindServiceProcessBootWithReader(ctx context.Context, component string, readBoot func() (string, error)) (result error) {
	defer func() { result = serviceFailureFallback(result, "boot_binding") }()
	if os.Getenv("WITSELF_RUNTIME") != "local" {
		return nil
	}
	if _, err := c.serviceDescriptor(ctx); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	boot, err := readBoot()
	if err != nil {
		return err
	}
	var record ProcessRecord
	if err := readJSON(ctx, c.path("run", component+".json"), &record); err != nil {
		return err
	}
	if record.Root != c.cfg.Root || record.Component != component || record.LaunchPending || (record.BootID != "" && record.BootID != boot) {
		return ErrServiceOwnershipConflict
	}
	identity, alive, err := processIdentity(record.PID)
	if err != nil || !alive || identity != record.StartTime {
		return ErrServiceOwnershipConflict
	}
	record.BootID = boot
	return writeAtomic(c.path("run", component+".json"), record, nil)
}

func (c *Cell) startServicePostgres(ctx context.Context, m *Manifest) error {
	if m.RemovalRetained {
		return errors.New("local cell installation is incomplete")
	}
	if err := c.startPostgres(ctx, m); err != nil {
		return err
	}
	return c.bindServiceProcessBoot(bindOperation(ctx), "postgres")
}
