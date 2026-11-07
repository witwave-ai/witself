package testenv

import (
	"testing"
)

// No database is configured or contacted. Disabled guards must preserve even
// invalid strings exactly; validation belongs exclusively to the enabled path.
func TestPostgresDSNReturnContract(t *testing.T) {
	previous := postgresTransactionGuardsEnabled.Swap(false)
	t.Cleanup(func() { postgresTransactionGuardsEnabled.Store(previous) })
	t.Setenv(databaseURLEnv, "")
	t.Setenv(requireDatabaseEnv, "")
	helpers := []struct {
		name string
		call func(*testing.T, string) string
	}{
		{"require", func(t *testing.T, value string) string {
			t.Setenv(databaseURLEnv, value)
			return RequirePostgres(t)
		}},
		{"direct_disabled_guard", func(t *testing.T, value string) string {
			return GuardPostgresTransactions(t, value)
		}},
	}
	// Keep the direct helper as an independently selected row. A Require-only
	// table cannot protect callers that pass their own DSN to the guard.
	if len(helpers) != 2 || helpers[1].name != "direct_disabled_guard" {
		t.Fatal("postgres return contract direct guard row missing")
	}
	for _, helper := range helpers {
		t.Run(helper.name, func(t *testing.T) {
			for _, input := range []struct{ name, value string }{
				{"postgres_url", "postgres://test.invalid/example?application_name=literal%20bytes"},
				{"postgresql_url", "postgresql://test.invalid/example?application_name=literal+bytes"},
				{"keyword", "host='test.invalid' dbname='example' application_name='literal bytes'"},
				{"malformed_url", "postgres://test.invalid/%zz"},
				{"malformed_keyword", "host='unterminated"},
				{"plain", "not a database configuration"},
			} {
				t.Run(input.name, func(t *testing.T) {
					if helper.call(t, input.value) != input.value {
						t.Fatal("postgres return contract changed disabled bytes")
					}
				})
			}
		})
	}
	if GuardPostgresTransactions(t, "") != "" {
		t.Fatal("postgres return contract changed empty disabled bytes")
	}
}
