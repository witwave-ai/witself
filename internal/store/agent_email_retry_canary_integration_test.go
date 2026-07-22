package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/agentemail"
)

func TestAgentEmailRetryCanaryPostgresStableRetry(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, dsn)
	destination, _ := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := destination.Migrate(); err != nil {
		t.Fatal(err)
	}
	provisioned, err := st.ProvisionAccount(ctx,
		"agent-email-retry-canary@witwave.ai", "agent email retry canary", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %v / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "retry canary")
	if err != nil {
		t.Fatal(err)
	}
	agents := make([]Agent, 0, 5)
	enrolled := make(map[string]bool, 5)
	for _, name := range []string{"canary", "pilot two", "pilot three", "pilot four", "pilot five"} {
		agent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		agents = append(agents, agent)
		enrolled[agent.ID] = true
	}
	scope := AgentEmailPilotScope{
		Enabled: true, Domain: "agent-mail.witwave.ai", Audience: "retry-canary-1",
		RealmIDs: map[string]bool{realm.ID: true}, AgentIDs: enrolled,
		RetryCanaryAgentID: agents[0].ID,
	}
	addresses, err := st.ReconcileAgentEmailPilot(ctx, scope)
	if err != nil || len(addresses) != 5 {
		t.Fatalf("reconcile = %#v / %v", addresses, err)
	}
	principal := func(agent Agent) Principal {
		return Principal{
			Kind: PrincipalAgent, ID: agent.ID, AccountID: provisioned.AccountID,
			RealmID: realm.ID, AgentName: agent.Name, AccountStatus: "active",
		}
	}
	canary := principal(agents[0])
	address, err := st.GetAgentEmailAddress(ctx, scope, canary)
	if err != nil {
		t.Fatal(err)
	}
	const challenge = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	if _, err := st.ArmAgentEmailRetryCanary(ctx, scope, principal(agents[1]), challenge); !errors.Is(err, ErrAgentEmailForbidden) {
		t.Fatalf("non-canary arm error = %v", err)
	}
	armed, err := st.ArmAgentEmailRetryCanary(ctx, scope, canary, challenge)
	if err != nil || armed.State != agentEmailRetryCanaryArmed || !armed.Armed || armed.Tempfailed || armed.Accepted {
		t.Fatalf("armed checkpoint = %#v / %v", armed, err)
	}

	ingest := func(raw []byte) (AgentEmailMessage, error) {
		digest := sha256.Sum256(raw)
		return st.IngestAgentEmailPilot(ctx, scope, AgentEmailIngestInput{
			Relay: agentemail.RelayMetadata{
				Timestamp: time.Now().Unix(), KeyID: "pilot-key", Audience: scope.Audience,
				EnvelopeSender: "canary-sender@example.com", EnvelopeRecipient: address.Address,
				RawSize: int64(len(raw)), RawSHA256: hex.EncodeToString(digest[:]),
			},
			Raw: raw,
		})
	}
	missing := []byte("From: canary-sender@example.com\r\nSubject: missing\r\n\r\nbody")
	if _, err := ingest(missing); !errors.Is(err, ErrAgentEmailRetryCanaryTemporary) {
		t.Fatalf("missing header while armed = %v", err)
	}
	mismatch := []byte("From: canary-sender@example.com\r\nX-Witself-Canary-Retry: bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb\r\nSubject: mismatch\r\n\r\nbody")
	if _, err := ingest(mismatch); !errors.Is(err, ErrAgentEmailRetryCanaryTemporary) {
		t.Fatalf("mismatched header while armed = %v", err)
	}
	raw := []byte(strings.Join([]string{
		"Received: from first.example by edge.example; Tue, 21 Jul 2026 20:00:01 -0600",
		"DKIM-Signature: v=1; a=rsa-sha256; b=first",
		"Authentication-Results: edge.example; dkim=pass",
		"From: canary-sender@example.com",
		agentemail.RetryCanaryHeader + ": " + challenge,
		"Subject: retry canary",
		"Content-Type: text/plain; charset=utf-8",
		"",
		"code 123456",
	}, "\r\n"))
	transportRetry := []byte(strings.Join([]string{
		"Received: from retry.example by edge.example; Tue, 21 Jul 2026 20:05:01 -0600",
		"Received: by another-hop.example; Tue, 21 Jul 2026 20:05:00 -0600",
		"DKIM-Signature: v=1; a=rsa-sha256; b=second",
		"Authentication-Results: edge.example; dkim=pass; spf=pass",
		"From: canary-sender@example.com",
		agentemail.RetryCanaryHeader + ": " + challenge,
		"Subject: retry canary",
		"Content-Type: text/plain; charset=utf-8",
		"",
		"code 123456",
	}, "\r\n"))
	if _, err := ingest(raw); !errors.Is(err, ErrAgentEmailRetryCanaryTemporary) {
		t.Fatalf("first matching delivery = %v", err)
	}
	tempfailed, err := st.GetAgentEmailRetryCanaryStatus(ctx, scope, canary, challenge)
	if err != nil || tempfailed.State != agentEmailRetryCanaryTempfailed ||
		!tempfailed.Armed || !tempfailed.Tempfailed || tempfailed.Accepted || tempfailed.TempfailCount != 1 {
		t.Fatalf("tempfailed checkpoint = %#v / %v", tempfailed, err)
	}
	// The unused-arm TTL must not erase a committed first-attempt proof. A
	// provider retry after 15 minutes still has the separate 24-hour grace.
	if _, err := st.pool.Exec(ctx, `
		UPDATE agent_email_retry_canary_arms
		SET armed_at=armed_at-interval '16 minutes',
		    expires_at=expires_at-interval '16 minutes'
		WHERE account_id=$1 AND mailbox_id=$2 AND state='tempfailed'`,
		provisioned.AccountID, address.MailboxID); err != nil {
		t.Fatal(err)
	}
	// A runner crash after the committed tempfail loses the opaque challenge.
	// The retained proof must not wedge future runs for its 24-hour grace.
	const replacementChallenge = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	replacementArmed, err := st.ArmAgentEmailRetryCanary(ctx, scope, canary, replacementChallenge)
	if err != nil || replacementArmed.State != agentEmailRetryCanaryArmed ||
		replacementArmed.Tempfailed || replacementArmed.Accepted {
		t.Fatalf("replacement arm beside tempfailed proof = %#v / %v", replacementArmed, err)
	}
	changed := append(append([]byte(nil), transportRetry...), []byte("changed")...)
	if _, err := ingest(changed); !errors.Is(err, ErrAgentEmailRetryCanaryTemporary) {
		t.Fatalf("changed retry body = %v", err)
	}
	acceptedMessage, err := ingest(transportRetry)
	if err != nil {
		t.Fatalf("transport-header-only provider retry = %v", err)
	}
	accepted, err := st.GetAgentEmailRetryCanaryStatus(ctx, scope, canary, challenge)
	if err != nil || accepted.State != agentEmailRetryCanaryAccepted ||
		!accepted.Armed || !accepted.Tempfailed || !accepted.Accepted || accepted.TempfailCount != 1 {
		t.Fatalf("accepted checkpoint = %#v / %v", accepted, err)
	}
	replayedMessage, err := ingest(raw)
	if err != nil || replayedMessage.ID != acceptedMessage.ID {
		t.Fatalf("accepted replay = %#v / %v, want id %s", replayedMessage, err, acceptedMessage.ID)
	}
	replacementStillArmed, err := st.GetAgentEmailRetryCanaryStatus(
		ctx, scope, canary, replacementChallenge,
	)
	if err != nil || replacementStillArmed.State != agentEmailRetryCanaryArmed ||
		replacementStillArmed.Tempfailed || replacementStillArmed.Accepted {
		t.Fatalf("replacement arm after old retry = %#v / %v", replacementStillArmed, err)
	}
	var messages, arms int
	var storedChallenge, fingerprint, state string
	challengeDigest := sha256.Sum256([]byte(challenge))
	if err := st.pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM agent_email_messages WHERE account_id=$1),
		  count(*),min(challenge_sha256),min(delivery_fingerprint_sha256),min(state)
		FROM agent_email_retry_canary_arms
		WHERE account_id=$1 AND challenge_sha256=$2`, provisioned.AccountID,
		hex.EncodeToString(challengeDigest[:])).
		Scan(&messages, &arms, &storedChallenge, &fingerprint, &state); err != nil {
		t.Fatal(err)
	}
	if messages != 1 || arms != 1 || state != agentEmailRetryCanaryAccepted ||
		storedChallenge == challenge || !isSHA256Hex(storedChallenge) || !isSHA256Hex(fingerprint) {
		t.Fatalf("durable retry proof messages=%d arms=%d state=%q challenge=%q fingerprint=%q",
			messages, arms, state, storedChallenge, fingerprint)
	}

	replacementRaw := []byte(agentemail.RetryCanaryHeader + ": " + replacementChallenge +
		"\r\nSubject: replacement retry\r\n\r\nbody")
	if _, err := ingest(replacementRaw); !errors.Is(err, ErrAgentEmailRetryCanaryTemporary) {
		t.Fatalf("replacement first delivery = %v", err)
	}
	replacementMessage, err := ingest(replacementRaw)
	if err != nil {
		t.Fatalf("replacement provider retry = %v", err)
	}
	replacementReplay, err := ingest(replacementRaw)
	if err != nil || replacementReplay.ID != replacementMessage.ID {
		t.Fatalf("replacement replay = %#v / %v, want id %s",
			replacementReplay, err, replacementMessage.ID)
	}

	const concurrentChallenge = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	if _, err := st.ArmAgentEmailRetryCanary(ctx, scope, canary, concurrentChallenge); err != nil {
		t.Fatal(err)
	}
	concurrentRaw := []byte(agentemail.RetryCanaryHeader + ": " + concurrentChallenge +
		"\r\nSubject: concurrent retry\r\n\r\nbody")
	start := make(chan struct{})
	type ingestResult struct {
		message AgentEmailMessage
		err     error
	}
	results := make(chan ingestResult, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for range 2 {
		go func() {
			ready.Done()
			<-start
			message, err := ingest(concurrentRaw)
			results <- ingestResult{message: message, err: err}
		}()
	}
	ready.Wait()
	close(start)
	var temporaryCount, acceptedCount int
	var concurrentMessageID string
	for range 2 {
		result := <-results
		switch {
		case errors.Is(result.err, ErrAgentEmailRetryCanaryTemporary):
			temporaryCount++
		case result.err == nil:
			acceptedCount++
			concurrentMessageID = result.message.ID
		default:
			t.Fatalf("concurrent ingest error = %v", result.err)
		}
	}
	if temporaryCount != 1 || acceptedCount != 1 || concurrentMessageID == "" {
		t.Fatalf("concurrent outcomes temporary=%d accepted=%d message=%q",
			temporaryCount, acceptedCount, concurrentMessageID)
	}
	concurrentReplay, err := ingest(concurrentRaw)
	if err != nil || concurrentReplay.ID != concurrentMessageID {
		t.Fatalf("concurrent third replay = %#v / %v", concurrentReplay, err)
	}
	var deliveries, acceptedProofs int
	if err := st.pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM agent_email_messages WHERE account_id=$1),
		  (SELECT count(*) FROM agent_email_deliveries WHERE account_id=$1),
		  (SELECT count(*) FROM agent_email_retry_canary_arms
		   WHERE account_id=$1 AND state='accepted')`, provisioned.AccountID).
		Scan(&messages, &deliveries, &acceptedProofs); err != nil {
		t.Fatal(err)
	}
	if messages != 3 || deliveries != 3 || acceptedProofs != 3 {
		t.Fatalf("concurrent durable rows messages=%d deliveries=%d proofs=%d",
			messages, deliveries, acceptedProofs)
	}

	// A proof created by a pre-stable-fingerprint server stores the ordinary
	// raw-SHA duplicate group. Its exact retry must remain acceptable across an
	// upgrade, and later exact replays must still return the one accepted row.
	const legacyChallenge = "99999999-9999-4999-8999-999999999999"
	legacyRaw := []byte(agentemail.RetryCanaryHeader + ": " + legacyChallenge +
		"\r\nSubject: legacy retry\r\n\r\nbody")
	legacyRawDigest := sha256.Sum256(legacyRaw)
	legacyFingerprint := agentEmailDuplicateGroup(hex.EncodeToString(legacyRawDigest[:]),
		address.Address, "canary-sender@example.com")
	legacyChallengeDigest := sha256.Sum256([]byte(legacyChallenge))
	if _, err := st.pool.Exec(ctx, `
		WITH anchor AS (SELECT clock_timestamp()-interval '1 minute' AS at)
		INSERT INTO agent_email_retry_canary_arms
		  (account_id,realm_id,mailbox_id,owner_agent_id,challenge_sha256,state,
		   delivery_fingerprint_sha256,tempfail_count,row_version,armed_at,
		   expires_at,tempfailed_at,retry_expires_at)
		SELECT $1,$2,$3,$4,$5,'tempfailed',$6,1,2,at,
		       at+interval '15 minutes',at+interval '1 second',
		       at+interval '24 hours 1 second'
		FROM anchor`, provisioned.AccountID, realm.ID, address.MailboxID,
		agents[0].ID, hex.EncodeToString(legacyChallengeDigest[:]), legacyFingerprint); err != nil {
		t.Fatal(err)
	}
	legacyMessage, err := ingest(legacyRaw)
	if err != nil {
		t.Fatalf("legacy tempfailed proof retry = %v", err)
	}
	legacyReplay, err := ingest(legacyRaw)
	if err != nil || legacyReplay.ID != legacyMessage.ID {
		t.Fatalf("legacy accepted proof replay = %#v / %v, want id %s",
			legacyReplay, err, legacyMessage.ID)
	}
	if err := st.pool.QueryRow(ctx, `
		SELECT delivery_fingerprint_sha256
		FROM agent_email_retry_canary_arms
		WHERE account_id=$1 AND challenge_sha256=$2`, provisioned.AccountID,
		hex.EncodeToString(legacyChallengeDigest[:])).Scan(&fingerprint); err != nil ||
		fingerprint != legacyFingerprint {
		t.Fatalf("legacy accepted fingerprint = %q / %v, want %q",
			fingerprint, err, legacyFingerprint)
	}

	// A late exact retry after its grace is terminally rejected. It must not
	// become ordinary mail or trigger an unbounded provider retry loop.
	const expiredChallenge = "01234567-89ab-4def-8abc-0123456789ab"
	expiredRaw := []byte(agentemail.RetryCanaryHeader + ": " + expiredChallenge +
		"\r\nSubject: expired retry\r\n\r\nbody")
	expiredFingerprint, _ := mustAgentEmailRetryCanaryFingerprint(t, expiredRaw,
		"canary-sender@example.com", address.Address)
	expiredChallengeDigest := sha256.Sum256([]byte(expiredChallenge))
	if _, err := st.pool.Exec(ctx, `
		WITH anchor AS (SELECT clock_timestamp()-interval '25 hours 1 minute' AS at)
		INSERT INTO agent_email_retry_canary_arms
		  (account_id,realm_id,mailbox_id,owner_agent_id,challenge_sha256,state,
		   delivery_fingerprint_sha256,tempfail_count,row_version,armed_at,
		   expires_at,tempfailed_at,retry_expires_at)
		SELECT $1,$2,$3,$4,$5,'tempfailed',$6,1,2,at,at+interval '15 minutes',
		       at+interval '1 minute',at+interval '24 hours 1 minute'
		FROM anchor`, provisioned.AccountID, realm.ID, address.MailboxID,
		agents[0].ID, hex.EncodeToString(expiredChallengeDigest[:]), expiredFingerprint); err != nil {
		t.Fatal(err)
	}
	if _, err := ingest(expiredRaw); !errors.Is(err, ErrAgentEmailRetryCanaryPermanent) {
		t.Fatalf("late retry after grace = %v", err)
	}
	if err := st.pool.QueryRow(ctx, `
		SELECT state FROM agent_email_retry_canary_arms
		WHERE account_id=$1 AND challenge_sha256=$2`, provisioned.AccountID,
		hex.EncodeToString(expiredChallengeDigest[:])).Scan(&state); err != nil ||
		state != agentEmailRetryCanaryExpired {
		t.Fatalf("late retry tombstone state = %q / %v", state, err)
	}

	// Expired proof tombstones are compacted, but the dedicated mailbox still
	// terminally rejects the now-unknown synthetic marker instead of ordinary-
	// accepting it or asking the provider to retry forever.
	const staleChallenge = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	staleRaw := []byte(agentemail.RetryCanaryHeader + ": " + staleChallenge +
		"\r\nSubject: stale retry\r\n\r\nbody")
	staleFingerprint, _ := mustAgentEmailRetryCanaryFingerprint(t, staleRaw,
		"canary-sender@example.com", address.Address)
	staleChallengeDigest := sha256.Sum256([]byte(staleChallenge))
	if _, err := st.pool.Exec(ctx, `
		WITH anchor AS (SELECT clock_timestamp()-interval '9 days' AS at)
		INSERT INTO agent_email_retry_canary_arms
		  (account_id,realm_id,mailbox_id,owner_agent_id,challenge_sha256,state,
		   delivery_fingerprint_sha256,tempfail_count,row_version,armed_at,
		   expires_at,tempfailed_at,retry_expires_at)
		SELECT $1,$2,$3,$4,$5,'expired',$6,1,3,at,at+interval '15 minutes',
		       at+interval '1 minute',at+interval '24 hours 1 minute'
		FROM anchor`, provisioned.AccountID, realm.ID, address.MailboxID,
		agents[0].ID, hex.EncodeToString(staleChallengeDigest[:]), staleFingerprint); err != nil {
		t.Fatal(err)
	}
	if _, err := ingest(staleRaw); !errors.Is(err, ErrAgentEmailRetryCanaryPermanent) {
		t.Fatalf("stale retry after tombstone cleanup = %v", err)
	}
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM agent_email_retry_canary_arms
		WHERE account_id=$1 AND state='expired'`, provisioned.AccountID).Scan(&arms); err != nil {
		t.Fatal(err)
	}
	if arms != 1 {
		t.Fatalf("expired retry tombstones after bounded cleanup = %d, want retained recent proof", arms)
	}

	// mail.ReadMessage cannot prove physical marker absence after a malformed
	// header block. The dedicated mailbox rejects this terminally when no arm
	// is live instead of falling through to ordinary storage.
	malformedRaw := []byte("malformed-header\r\n" + agentemail.RetryCanaryHeader +
		": ffffffff-ffff-4fff-8fff-ffffffffffff\r\n\r\nbody")
	if _, err := ingest(malformedRaw); !errors.Is(err, ErrAgentEmailRetryCanaryPermanent) {
		t.Fatalf("malformed retry marker without live arm = %v", err)
	}
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM agent_email_messages WHERE account_id=$1`,
		provisioned.AccountID).Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if messages != 4 {
		t.Fatalf("terminal retry markers created messages = %d, want 4", messages)
	}

	// Exercise the real arm -> tempfail -> accepted row through account export
	// and restore, proving its timestamp/fingerprint shape is portable.
	if err := st.SuspendAccountSystem(ctx, provisioned.AccountID,
		"evacuation", "retry canary archive roundtrip"); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := st.ExportAccount(ctx, provisioned.AccountID, "retry-canary-source", "test", &archive); err != nil {
		t.Fatal(err)
	}
	if _, err := destination.ImportAccount(ctx, provisioned.AccountID, bytes.NewReader(archive.Bytes())); err != nil {
		t.Fatal(err)
	}
	var restoredState, restoredMessageID string
	if err := destination.pool.QueryRow(ctx, `
		SELECT state,accepted_message_id FROM agent_email_retry_canary_arms
		WHERE account_id=$1 AND challenge_sha256=$2`, provisioned.AccountID,
		storedChallenge).Scan(&restoredState, &restoredMessageID); err != nil {
		t.Fatal(err)
	}
	if restoredState != agentEmailRetryCanaryAccepted || restoredMessageID != acceptedMessage.ID {
		t.Fatalf("restored retry proof = %q/%q", restoredState, restoredMessageID)
	}
}
