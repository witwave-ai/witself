package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/agentemail"
	"github.com/witwave-ai/witself/internal/server"
	"github.com/witwave-ai/witself/internal/store"
	"github.com/witwave-ai/witself/internal/testenv"
)

var cohortStartupSchema atomic.Uint64

func startupCohortStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := testenv.RequirePostgres(t)
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("witself_email_startup_%d_%d", os.Getpid(), cohortStartupSchema.Add(1))
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		_ = admin.Close()
		t.Fatal(err)
	}
	var st *store.Store
	t.Cleanup(func() {
		if st != nil {
			st.Close()
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if _, err := admin.ExecContext(ctx, `DROP SCHEMA `+schema+` CASCADE`); err != nil {
			t.Error(err)
		}
		if err := admin.Close(); err != nil {
			t.Error(err)
		}
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	st, err = store.Open(context.Background(), u.String())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestAgentEmailCohortDeferralIngestPostgres(t *testing.T) {
	st := startupCohortStore(t)
	t.Setenv(agentEmailProviderEventTokenEnv, strings.Repeat("x", 32))
	t.Setenv("WITSELF_HOME", t.TempDir())
	ctx := context.Background()
	a, err := st.ProvisionAccount(ctx, "deferral-startup@example.test", "deferral fixture", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := st.ActivateAccount(ctx, a.AccountID); err != nil || !ok {
		t.Fatal("activate fixture", err)
	}
	r, err := st.CreateRealm(ctx, a.AccountID, "deferral realm")
	if err != nil {
		t.Fatal(err)
	}
	g, err := st.CreateAgent(ctx, a.AccountID, r.ID, "deferral agent")
	if err != nil {
		t.Fatal(err)
	}
	const absent = "acc_zzzzzzzzzzzzzzzz"
	receive := server.AgentEmailReceiveConfig{Enabled: true, Mode: server.AgentEmailReceiveModeProduction, Domain: "witmail.net", Audience: "test-cell", AccountIDs: map[string]bool{a.AccountID: true}}
	address, err := st.EnsureAgentEmailMailbox(ctx, toStoreAgentEmailReceiveScope(receive), a.AccountID, r.ID, g.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	receive.AccountIDs = map[string]bool{absent: true}
	var cfg server.Config
	var log bytes.Buffer
	if err := configureAgentEmailWithLog(ctx, &cfg, st, receive, &log); err != nil {
		t.Fatal(err)
	}
	want := agentEmailProductionCohortStartupLine(store.AgentEmailProductionCohortResidency{ConfiguredAccountCount: 1, UnknownAccountCount: 1, RetryCanary: store.AgentEmailRetryCanaryNone}) + "\n"
	if log.String() != want {
		t.Fatal("startup line did not classify the omitted cohort")
	}
	const sender = "sender@example.test"
	raw := []byte("From: " + sender + "\r\nTo: " + address.Address + "\r\nSubject: deferral\r\n\r\nhello\r\n")
	digest := sha256.Sum256(raw)
	relay := agentemail.RelayMetadata{Timestamp: time.Now().Unix(), KeyID: "test-relay", Audience: receive.Audience, EnvelopeSender: sender, EnvelopeRecipient: address.Address, RawSize: int64(len(raw)), RawSHA256: hex.EncodeToString(digest[:])}
	private := []string{a.AccountID, a.OperatorID, r.ID, g.ID, address.Address, sender, absent}
	for range 2 {
		err := cfg.IngestAgentEmailPilot(ctx, relay, raw)
		if !errors.Is(err, server.ErrAgentEmailCohortDeferred) {
			t.Fatal("omitted resident did not map to the cohort deferral")
		}
		for _, value := range private {
			if strings.Contains(err.Error(), value) || strings.Contains(log.String(), value) {
				t.Fatal("deferral exposed a fixture value")
			}
		}
	}
	want += agentEmailCohortDeferralLine(1) + "\n"
	if log.String() != want {
		t.Fatal("repeated ingest did not produce exactly startup and one deferral line")
	}
	receive.AccountIDs = map[string]bool{a.AccountID: true}
	if err := configureAgentEmailWithLog(ctx, &cfg, st, receive, &log); err != nil {
		t.Fatal(err)
	}
	want += agentEmailProductionCohortStartupLine(store.AgentEmailProductionCohortResidency{ConfiguredAccountCount: 1, ResidentAccountCount: 1, RetryCanary: store.AgentEmailRetryCanaryNone}) + "\n"
	if err := cfg.IngestAgentEmailPilot(ctx, relay, raw); err != nil {
		t.Fatal("corrected cohort did not accept ingest")
	}
	if log.String() != want {
		t.Fatal("corrected cohort wrote a deferral line")
	}
}

func TestAgentEmailStartupFleetCohortPostgres(t *testing.T) {
	st := startupCohortStore(t)
	// This is the sole environment read in configureAgentEmailWithLog and its
	// immediate configuration helpers. A synthetic valid value isolates the test.
	t.Setenv(agentEmailProviderEventTokenEnv, strings.Repeat("x", 32))
	t.Setenv("WITSELF_HOME", t.TempDir())
	ctx := context.Background()
	const absent = "acc_zzzzzzzzzzzzzzzz"
	receive := server.AgentEmailReceiveConfig{Enabled: true, Mode: server.AgentEmailReceiveModeProduction, Domain: "witmail.net", Audience: "test-cell", AccountIDs: map[string]bool{absent: true}}
	var cfg server.Config
	var log bytes.Buffer
	if err := configureAgentEmailWithLog(ctx, &cfg, st, receive, &log); err != nil {
		t.Fatal(err)
	}
	want := agentEmailProductionCohortStartupLine(store.AgentEmailProductionCohortResidency{ConfiguredAccountCount: 1, UnknownAccountCount: 1, RetryCanary: store.AgentEmailRetryCanaryNone}) + "\n"
	if cfg.IngestAgentEmailPilot == nil || log.String() != want {
		t.Fatal("startup did not wire ingest with exactly one value-free line")
	}
	a, err := st.ProvisionAccount(ctx, "startup@example.test", "startup fixture", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := st.ActivateAccount(ctx, a.AccountID); err != nil || !ok {
		t.Fatal("activate", err)
	}
	r, err := st.CreateRealm(ctx, a.AccountID, "startup realm")
	if err != nil {
		t.Fatal(err)
	}
	g, err := st.CreateAgent(ctx, a.AccountID, r.ID, "startup agent")
	if err != nil {
		t.Fatal(err)
	}
	receive.RetryCanaryAgentID = g.ID
	cfg = server.Config{}
	log.Reset()
	err = configureAgentEmailWithLog(ctx, &cfg, st, receive, &log)
	if err == nil || !errors.Is(err, store.ErrAgentEmailPilotNotEnrolled) || cfg.IngestAgentEmailPilot != nil || log.Len() != 0 {
		t.Fatal("failed startup did not fail closed")
	}
	if err.Error() != newAgentEmailLogSafeError("agent-email production startup preflight", "preflight_failed", store.ErrAgentEmailPilotNotEnrolled).Error() {
		t.Fatal("startup error is not log safe")
	}
	for _, id := range []string{absent, a.AccountID, a.OperatorID, r.ID, g.ID} {
		if strings.Contains(err.Error(), id) || strings.Contains(log.String(), id) {
			t.Fatal("startup leaked a fixture identifier")
		}
	}
}
