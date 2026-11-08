package localcell

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/witwave-ai/witself/internal/store"
)

// net/url.Query accepts form-encoded spaces, but PostgreSQL URI consumers
// preserve literal plus signs. Exercise the parser used by physical connections.
func TestInstallerPrincipalsMigrationStartupOption(t *testing.T) {
	c, m, _ := selectedCredentialFixture(t)
	environment, err := c.childEnvironment(m, "migration")
	if err != nil {
		t.Fatal("migration startup option fixture failed")
	}
	for _, entry := range environment {
		dsn, ok := strings.CutPrefix(entry, "WITSELF_DATABASE_URL=")
		if !ok {
			continue
		}
		config, err := pgx.ParseConfig(dsn)
		if err != nil {
			t.Fatal("migration startup option could not be parsed by PostgreSQL driver")
		}
		if config.User != installerMigratorRole || config.RuntimeParams["options"] != "-c role="+string(store.SyncPrincipalOwner) {
			t.Fatal("migration startup option lost owner selection in PostgreSQL driver")
		}
		return
	}
	t.Fatal("migration startup option connection missing")
}
