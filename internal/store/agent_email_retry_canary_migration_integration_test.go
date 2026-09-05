package store

import (
	"testing"

	"github.com/witwave-ai/witself/internal/testenv"
)

func TestMigration61AgentEmailRetryCanaryPostgres(t *testing.T) {
	dsn := testenv.RequirePostgres(t)
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
