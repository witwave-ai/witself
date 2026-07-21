package store

import (
	"os"
	"testing"
)

func TestMigration61AgentEmailRetryCanaryPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	st, isolatedDSN := newMigrationTestStore(t, dsn)
	migrationTestUpTo(t, isolatedDSN, 60)
	assertMigrationTestVersion(t, isolatedDSN, 60)
	assertMigrationTestTable(t, st, "agent_email_retry_canary_arms", false)
	migrationTestUpTo(t, isolatedDSN, 61)
	assertMigrationTestVersion(t, isolatedDSN, 61)
	assertMigrationTestTable(t, st, "agent_email_retry_canary_arms", true)
	assertMigrationTestColumn(t, st, "agent_email_retry_canary_arms", "challenge_sha256", true)
	assertMigrationTestColumn(t, st, "agent_email_retry_canary_arms", "accepted_message_id", true)
	assertMigrationTestIndexUnique(t, st, "agent_email_retry_canary_arms",
		"agent_email_retry_canary_one_live_arm", true)
}
