package localcell

import "context"

// ReinstallQueryForTest replaces only Reinstall's verification query boundary.
// The production PostgreSQL launch and stop boundaries remain in place.
func ReinstallQueryForTest(c *Cell, query func(context.Context, *Manifest, string) (string, error)) {
	ops := c.reinstallOps()
	ops.query = query
	c.reinstallTest = &ops
}

// SeedVerifiedSchemaForTest publishes a verified schema while holding the
// operation lock. The synthetic lifecycle peer reports schema 1 to Start.
func SeedVerifiedSchemaForTest(c *Cell, schema int) error {
	lock, err := c.own(false)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	m, err := readManifest(c.cfg.Root)
	if err != nil {
		return err
	}
	m.LastVerifiedSchemaVersion = schema
	return writeAtomic(c.path("manifest.json"), m, nil)
}

// The three reinstall fixed causes, for errors.Is in external tests.
var (
	ErrReinstallStateUnsupportedForTest = errReinstallState
	ErrReinstallIdentityMismatchForTest = errReinstallIdentity
	ErrReinstallVersionMismatchForTest  = errReinstallVersion
)
