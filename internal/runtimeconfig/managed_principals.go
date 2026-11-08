package runtimeconfig

import (
	"errors"
	"net/url"
	"strings"
)

const managedPrincipalsVariable = "WITSELF_LOCAL_MANAGED_PRINCIPALS"
const migrationPhaseVariable = "WITSELF_LOCAL_MIGRATION_PHASE"

func managedPrincipalsFromEnv(role, runtime string, lookup func(string) (string, bool), environ []string) (Config, error) {
	managed, selected := lookup(managedPrincipalsVariable)
	phase, migration := lookup(migrationPhaseVariable)
	sync, _ := lookup("WITSELF_LOCAL_SYNC_SCHEMA")
	valid := selected && managed == "1" && runtime == "local" && sync == "1"
	for _, entry := range environ {
		name, _, _ := strings.Cut(entry, "=")
		for _, known := range []string{managedPrincipalsVariable, migrationPhaseVariable} {
			if strings.HasPrefix(strings.ToUpper(name), known) && (name != known || !valid) {
				return Config{}, errors.New("local managed principal selection is invalid")
			}
		}
	}
	if !selected && !migration {
		return Config{}, nil
	}
	if !valid || (migration && (phase != "1" || role != "server")) {
		return Config{}, errors.New("local managed principal selection is invalid")
	}
	if migration {
		for _, entry := range environ {
			name, _, _ := strings.Cut(entry, "=")
			upper := strings.ToUpper(name)
			if strings.HasPrefix(upper, "WITSELF_LOCAL_SHUTDOWN") || strings.HasPrefix(upper, captureProofPrefix) {
				return Config{}, errors.New("local migration phase configuration is invalid")
			}
		}
		for _, name := range []string{"WITSELF_LOCAL_SHUTDOWN_DESCRIPTOR", "WITSELF_LOCAL_SHUTDOWN_TRANSPORT", captureProofPrefix, captureProofPrefix + "_TOKEN", captureProofPrefix + "_STUB_EMAIL"} {
			if _, present := lookup(name); present {
				return Config{}, errors.New("local migration phase configuration is invalid")
			}
		}
		dsn, _ := lookup("WITSELF_DATABASE_URL")
		if !ManagedMigrationDSN(dsn) {
			return Config{}, errors.New("local migration phase configuration is invalid")
		}
	}
	return Config{ManagedPrincipals: true, MigrationPhase: migration}, nil
}

// ManagedMigrationDSN requires owner selection in the startup packet. Goose
// reopens the same DSN, so every physical connection retains migration custody.
func ManagedMigrationDSN(dsn string) bool {
	u, err := url.Parse(dsn)
	if err != nil || u.User == nil || u.User.Username() != "witself_sync_migrator" ||
		(u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Fragment != "" {
		return false
	}
	query, err := url.ParseQuery(u.RawQuery)
	return err == nil && len(query["options"]) == 1 && query.Get("options") == "-c role=witself_sync_owner" &&
		len(query["role"]) == 0 && len(query["user"]) == 0
}
