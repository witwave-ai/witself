//go:build !windows

package localcell

func (c *Cell) baseEnvironment() ([]string, error) {
	return []string{
		"PATH=" + c.cfg.Binaries.PostgresDir,
		"HOME=" + c.path("home"),
		"TMPDIR=" + c.path("tmp"),
		"TEMP=" + c.path("tmp"),
		"TMP=" + c.path("tmp"),
	}, nil
}

func (c *Cell) postgresEnvironment() ([]string, error) { return c.baseEnvironment() }

func appendEnvironment(environment []string, entries ...string) []string {
	return append(environment, entries...)
}
