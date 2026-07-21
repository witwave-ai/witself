package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/agentemail"
)

func TestAgentEmailSignedIngestHTTPContract(t *testing.T) {
	pilot, privateKey := testAgentEmailPilotConfig(t)
	raw := []byte("From: sender@example.com\r\nTo: pilot@example.com\r\nSubject: code\r\n\r\n123456\r\n")
	metadata := testAgentEmailRelayMetadata(raw, pilot, "pilot-key")

	var calls int
	var ingestErr error
	var gotMetadata agentemail.RelayMetadata
	var gotRaw []byte
	handler := apiMux(Config{
		AgentEmailPilot: pilot,
		IngestAgentEmailPilot: func(_ context.Context, got agentemail.RelayMetadata, body []byte) error {
			calls++
			gotMetadata = got
			gotRaw = append([]byte(nil), body...)
			return ingestErr
		},
	})

	request := testAgentEmailIngestRequest(t, raw, metadata, privateKey)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assertAgentEmailVerdict(t, response, http.StatusOK, "accepted")
	if calls != 1 || gotMetadata != metadata || !bytes.Equal(gotRaw, raw) {
		t.Fatalf("ingest callback = calls %d metadata %+v raw %q", calls, gotMetadata, gotRaw)
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("ingest Cache-Control = %q", got)
	}

	ingestErr = ErrAgentEmailUnknownRecipient
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, testAgentEmailIngestRequest(t, raw, metadata, privateKey))
	assertAgentEmailVerdict(t, response, http.StatusNotFound, "unknown_recipient")

	ingestErr = ErrAgentEmailReceiveDisabled
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, testAgentEmailIngestRequest(t, raw, metadata, privateKey))
	assertAgentEmailVerdict(t, response, http.StatusServiceUnavailable, "receive_disabled")

	ingestErr = ErrAgentEmailRetryCanaryTemporary
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, testAgentEmailIngestRequest(t, raw, metadata, privateKey))
	assertAgentEmailVerdict(t, response, http.StatusServiceUnavailable, "temporary")

	ingestErr = ErrAgentEmailRetryCanaryPermanent
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, testAgentEmailIngestRequest(t, raw, metadata, privateKey))
	assertAgentEmailVerdict(t, response, http.StatusGone, "retry_canary_rejected")

	ingestErr = context.DeadlineExceeded
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, testAgentEmailIngestRequest(t, raw, metadata, privateKey))
	assertAgentEmailVerdict(t, response, http.StatusServiceUnavailable, "temporary")

	ingestErr = nil
	validCalls := calls
	t.Run("body digest is bound", func(t *testing.T) {
		altered := append([]byte(nil), raw...)
		altered[len(altered)-2] ^= 1
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, testAgentEmailIngestRequestWithSignedMetadata(t, altered, metadata, privateKey))
		assertAgentEmailVerdict(t, response, http.StatusUnauthorized, "invalid_relay")
	})
	t.Run("audience is bound to cell", func(t *testing.T) {
		other := metadata
		other.Audience = "other-cell"
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, testAgentEmailIngestRequest(t, raw, other, privateKey))
		assertAgentEmailVerdict(t, response, http.StatusUnauthorized, "invalid_relay")
	})
	t.Run("timestamp replay window is enforced", func(t *testing.T) {
		stale := metadata
		stale.Timestamp = pilot.Now().Add(-pilot.RelayReplayWindow - time.Second).Unix()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, testAgentEmailIngestRequest(t, raw, stale, privateKey))
		assertAgentEmailVerdict(t, response, http.StatusUnauthorized, "invalid_relay")
	})
	t.Run("unknown key id fails closed", func(t *testing.T) {
		unknown := metadata
		unknown.KeyID = "unknown-key"
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, testAgentEmailIngestRequest(t, raw, unknown, privateKey))
		assertAgentEmailVerdict(t, response, http.StatusUnauthorized, "invalid_relay")
	})
	t.Run("duplicate signed header is rejected", func(t *testing.T) {
		request := testAgentEmailIngestRequest(t, raw, metadata, privateKey)
		request.Header.Add(AgentEmailRelayHeaderAudience, metadata.Audience)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		assertAgentEmailVerdict(t, response, http.StatusUnauthorized, "invalid_relay")
	})
	t.Run("oversized raw body is permanent", func(t *testing.T) {
		body := bytes.Repeat([]byte("x"), agentemail.PilotMaximumRawBytes+1)
		request := testAgentEmailIngestRequestWithSignedMetadata(t, body, metadata, privateKey)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		assertAgentEmailVerdict(t, response, http.StatusRequestEntityTooLarge, "permanent")
	})
	if calls != validCalls {
		t.Fatalf("invalid relay requests reached ingest: calls %d want %d", calls, validCalls)
	}
}

func TestAgentEmailOwnerHTTPContractAndAuthorization(t *testing.T) {
	pilot, _ := testAgentEmailPilotConfig(t)
	ownerID := "agent_aaaaaaaaaaaaaaaa"
	realmID := "realm_aaaaaaaaaaaaaaaa"
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	lease := now.Add(5 * time.Minute)
	message := AgentEmailMessage{
		ID: "emsg_aaaaaaaaaaaaaaaa", AccountID: "acc_1", RealmID: realmID,
		MailboxID: "emb_aaaaaaaaaaaaaaaa", OwnerAgentID: ownerID,
		AddressID: "eaddr_aaaaaaaaaaaaaaaa", Provider: "cloudflare_email_routing",
		EnvelopeSender: "sender@example.com", EnvelopeRecipient: "pilot@example.com",
		RawSizeBytes: 42, ParseState: "parsed", Subject: "untrusted subject",
		SPFResult: "unknown", DKIMResult: "unknown", DMARCResult: "unknown",
		SpamVerdict: "unknown", SenderVerificationState: "unverified",
		ReceivedAt: now, CreatedAt: now, Folder: "inbox", DeliveredAt: now,
		ReadState: AgentEmailReadState{State: "unread"},
		Processing: AgentEmailProcessing{
			State: "claimed", Generation: 3, FailureCount: 1,
			ClaimID: "ecl_aaaaaaaaaaaaaaaa", LeaseExpiresAt: &lease,
		},
		Text: "untrusted body with code 123456", TextKind: "plain",
	}
	principalForToken := func(token string) DomainPrincipal {
		p := DomainPrincipal{
			Kind: PrincipalKindAgent, ID: ownerID, AccountID: "acc_1", RealmID: realmID,
			AccountStatus: "active", AccessProfile: AccessProfileFull,
		}
		switch token {
		case "operator":
			p.Kind, p.ID, p.RealmID = PrincipalKindOperator, "opr_1", ""
		case "curator":
			p.AccessProfile = AccessProfileCuratorPreview
		case "unenrolled":
			p.ID = "agent_zzzzzzzzzzzzzzzz"
		}
		return p
	}
	auth := func(_ context.Context, token string) (DomainPrincipal, bool, error) {
		return principalForToken(token), true, nil
	}
	var listCalls, readCalls, ackCalls, codeCalls int
	var claimRequest ClaimAgentEmailRequest
	var renewRequest RenewAgentEmailClaimRequest
	var releaseRequest ReleaseAgentEmailClaimRequest
	var completeRequest CompleteAgentEmailRequest
	cfg := Config{
		AuthenticatePrincipal: auth, AgentEmailPilot: pilot,
		GetAgentEmailAddress: func(context.Context, DomainPrincipal) (AgentEmailAddress, error) {
			return AgentEmailAddress{
				ID: "eaddr_aaaaaaaaaaaaaaaa", MailboxID: message.MailboxID,
				AccountID: "acc_1", RealmID: realmID, OwnerAgentID: ownerID,
				Address: "pilot.realm@agent-mail.witwave.ai", ReceiveState: "disabled",
				AgentReceiveState: "enabled", RealmReceiveState: "disabled",
			}, nil
		},
		ListAgentEmails: func(_ context.Context, _ DomainPrincipal, opts AgentEmailListOptions) (AgentEmailPage, error) {
			listCalls++
			if opts.OldestFirst && opts.Unacked {
				if opts.Limit != 2 {
					t.Fatalf("listen options = %+v", opts)
				}
				return AgentEmailPage{}, nil
			}
			if !opts.Unread || opts.Unacked || opts.OldestFirst || opts.Limit != 7 || opts.Cursor != "cursor-1" {
				t.Fatalf("list options = %+v", opts)
			}
			return AgentEmailPage{Messages: []AgentEmailMessage{message}, NextCursor: "cursor-2"}, nil
		},
		ReadAgentEmail: func(_ context.Context, _ DomainPrincipal, id string) (AgentEmailMessage, error) {
			readCalls++
			if id != message.ID {
				t.Fatalf("read id = %q", id)
			}
			return message, nil
		},
		AckAgentEmail: func(context.Context, DomainPrincipal, string) (AgentEmailMessage, error) {
			ackCalls++
			result := message
			result.ReadState.State = "acked"
			return result, nil
		},
		MarkAgentEmailCodeConsumed: func(context.Context, DomainPrincipal, string) (AgentEmailMessage, error) {
			codeCalls++
			result := message
			result.ReadState.CodeConsumedAt = &now
			return result, nil
		},
		GetSelfAgentEmailCheckpoint: func(context.Context, DomainPrincipal) (AgentEmailCheckpoint, error) {
			return AgentEmailCheckpoint{Pending: true, MailboxPending: true}, nil
		},
		ClaimAgentEmail: func(_ context.Context, _ DomainPrincipal, _ string, in ClaimAgentEmailRequest) (AgentEmailProcessing, error) {
			claimRequest = in
			return message.Processing, nil
		},
		RenewAgentEmailClaim: func(_ context.Context, _ DomainPrincipal, _ string, in RenewAgentEmailClaimRequest) (AgentEmailProcessing, error) {
			renewRequest = in
			return message.Processing, nil
		},
		ReleaseAgentEmailClaim: func(_ context.Context, _ DomainPrincipal, _ string, in ReleaseAgentEmailClaimRequest) (AgentEmailProcessing, error) {
			releaseRequest = in
			return AgentEmailProcessing{State: "available", Generation: in.Generation}, nil
		},
		CompleteAgentEmail: func(_ context.Context, _ DomainPrincipal, _ string, in CompleteAgentEmailRequest) (AgentEmailProcessing, error) {
			completeRequest = in
			return AgentEmailProcessing{State: "completed", Generation: in.Generation, ClaimID: in.ClaimID}, nil
		},
	}
	handler := apiMux(cfg)

	response := performAgentEmailOwnerRequest(handler, http.MethodGet, "/v1/email/address", "full", "", nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "pilot.realm@agent-mail.witwave.ai") ||
		!strings.Contains(response.Body.String(), `"receive_state":"disabled"`) ||
		!strings.Contains(response.Body.String(), `"realm_receive_state":"disabled"`) {
		t.Fatalf("address response = %d %s", response.Code, response.Body.String())
	}

	response = performAgentEmailOwnerRequest(handler, http.MethodGet, "/v1/email?unread=true&limit=7&cursor=cursor-1", "full", "", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("list response = %d %s", response.Code, response.Body.String())
	}
	var listEnvelope struct {
		Messages   []AgentEmailMessage `json:"messages"`
		NextCursor string              `json:"next_cursor"`
	}
	decodeAgentEmailTestJSON(t, response, &listEnvelope)
	if len(listEnvelope.Messages) != 1 || listEnvelope.NextCursor != "cursor-2" ||
		listEnvelope.Messages[0].Text != "" || listEnvelope.Messages[0].TextKind != "" ||
		listEnvelope.Messages[0].Processing.ClaimID != "" || listEnvelope.Messages[0].Processing.LeaseExpiresAt != nil {
		t.Fatalf("list leaked content or fence = %+v", listEnvelope)
	}

	response = performAgentEmailOwnerRequest(handler, http.MethodPost, "/v1/email:listen", "full", `{"wait_seconds":0,"limit":2}`, nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"timed_out":true`) {
		t.Fatalf("listen response = %d %s", response.Code, response.Body.String())
	}

	response = performAgentEmailOwnerRequest(handler, http.MethodPost, "/v1/email/"+message.ID+":read", "full", "", nil)
	var readEnvelope struct {
		Message AgentEmailMessage `json:"message"`
	}
	decodeAgentEmailTestJSON(t, response, &readEnvelope)
	if response.Code != http.StatusOK || readEnvelope.Message.Text != message.Text ||
		readEnvelope.Message.Processing.ClaimID != "" || readEnvelope.Message.Processing.LeaseExpiresAt != nil {
		t.Fatalf("read response = %d %+v", response.Code, readEnvelope.Message)
	}

	for _, operation := range []string{"ack", "code-consumed"} {
		response = performAgentEmailOwnerRequest(handler, http.MethodPost, "/v1/email/"+message.ID+":"+operation, "full", "", nil)
		var envelope struct {
			Message AgentEmailMessage `json:"message"`
		}
		decodeAgentEmailTestJSON(t, response, &envelope)
		if response.Code != http.StatusOK || envelope.Message.Text != "" || envelope.Message.Processing.ClaimID != "" {
			t.Fatalf("%s response = %d %+v", operation, response.Code, envelope.Message)
		}
	}

	processingCases := []struct {
		operation string
		body      string
		key       string
	}{
		{"claim", `{"lease_seconds":90}`, "claim-key"},
		{"renew", `{"claim_id":"ecl_aaaaaaaaaaaaaaaa","generation":3,"lease_seconds":120}`, ""},
		{"release", `{"claim_id":"ecl_aaaaaaaaaaaaaaaa","generation":3,"deterministic_failure":true}`, ""},
		{"complete", `{"claim_id":"ecl_aaaaaaaaaaaaaaaa","generation":3}`, "complete-key"},
	}
	response = performAgentEmailOwnerRequest(handler, http.MethodPost, "/v1/email/"+message.ID+":claim", "full", `{"lease_seconds":90,"owner_agent_id":"agent_bbbbbbbbbbbbbbbb"}`, nil)
	if response.Code != http.StatusBadRequest || claimRequest.LeaseSeconds != 0 {
		t.Fatalf("spoofed claim response = %d %s / request %+v", response.Code, response.Body.String(), claimRequest)
	}
	for _, tc := range processingCases {
		headers := map[string]string{}
		if tc.key != "" {
			headers["Idempotency-Key"] = tc.key
		}
		response = performAgentEmailOwnerRequest(handler, http.MethodPost, "/v1/email/"+message.ID+":"+tc.operation, "full", tc.body, headers)
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"processing"`) {
			t.Fatalf("%s response = %d %s", tc.operation, response.Code, response.Body.String())
		}
	}
	if claimRequest.LeaseSeconds != 90 || claimRequest.IdempotencyKey != "claim-key" ||
		renewRequest.LeaseSeconds != 120 || renewRequest.ClaimID != "ecl_aaaaaaaaaaaaaaaa" ||
		!releaseRequest.DeterministicFailure || completeRequest.IdempotencyKey != "complete-key" {
		t.Fatalf("processing requests = claim %+v renew %+v release %+v complete %+v", claimRequest, renewRequest, releaseRequest, completeRequest)
	}

	response = performAgentEmailOwnerRequest(handler, http.MethodGet, "/v1/email/checkpoint", "full", "", nil)
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "123456") ||
		!strings.Contains(response.Body.String(), `"mailbox_pending":true`) {
		t.Fatalf("checkpoint response = %d %s", response.Code, response.Body.String())
	}

	priorListCalls := listCalls
	for _, token := range []string{"operator", "curator", "unenrolled"} {
		response = performAgentEmailOwnerRequest(handler, http.MethodGet, "/v1/email", token, "", nil)
		if response.Code != http.StatusForbidden {
			t.Fatalf("%s list status = %d, want 403", token, response.Code)
		}
		response = performAgentEmailOwnerRequest(handler, http.MethodPost, "/v1/email/"+message.ID+":claim", token, `{"lease_seconds":90}`, nil)
		if response.Code != http.StatusForbidden {
			t.Fatalf("%s processing status = %d, want 403", token, response.Code)
		}
	}
	if listCalls != priorListCalls || readCalls != 1 || ackCalls != 1 || codeCalls != 1 {
		t.Fatalf("authorization reached hooks: list %d/%d read %d ack %d code %d", listCalls, priorListCalls, readCalls, ackCalls, codeCalls)
	}
	response = performAgentEmailOwnerRequest(handler, http.MethodGet, "/v1/email", "", "", nil)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("missing token status = %d", response.Code)
	}
	if got := response.Header().Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("email route Cache-Control = %q", got)
	}
}

func TestAgentEmailOperatorReceiveControlHTTPContract(t *testing.T) {
	pilot, _ := testAgentEmailPilotConfig(t)
	now := time.Date(2026, 7, 21, 18, 0, 0, 0, time.UTC)
	var agentTarget, agentState, realmTarget, realmState string
	auth := func(_ context.Context, token string) (string, string, string, bool, error) {
		status := "active"
		switch token {
		case "operator-token":
		case "suspended-token":
			status = "suspended"
		case "pending-token":
			status = "pending"
		default:
			return "", "", "", false, nil
		}
		return "opr_1", "acc_1", status, true, nil
	}
	cfg := Config{
		Authenticate: auth, AgentEmailPilot: pilot,
		GetAgentEmailReceiveControl: func(_ context.Context, accountID, operatorID, agentID string) (AgentEmailReceiveControl, error) {
			if accountID != "acc_1" || operatorID != "opr_1" {
				t.Fatalf("agent get principal = %q/%q", accountID, operatorID)
			}
			agentTarget = agentID
			return AgentEmailReceiveControl{
				AccountID: accountID, RealmID: "realm_aaaaaaaaaaaaaaaa", AgentID: agentID,
				ReceiveState: "disabled", AgentReceiveState: "enabled",
				RealmReceiveState: "disabled", RowVersion: 3, UpdatedAt: now,
			}, nil
		},
		SetAgentEmailReceiveControl: func(_ context.Context, accountID, _ string, agentID, state string) (AgentEmailReceiveControl, error) {
			agentTarget, agentState = agentID, state
			return AgentEmailReceiveControl{
				AccountID: accountID, RealmID: "realm_aaaaaaaaaaaaaaaa", AgentID: agentID,
				ReceiveState: state, AgentReceiveState: state,
				RealmReceiveState: "enabled", RowVersion: 4, UpdatedAt: now,
			}, nil
		},
		GetRealmEmailReceiveControl: func(_ context.Context, accountID, _ string, realmID string) (AgentEmailRealmReceiveControl, error) {
			realmTarget = realmID
			return AgentEmailRealmReceiveControl{
				AccountID: accountID, RealmID: realmID, ReceiveState: "enabled",
				MailboxCount: 5, RowVersion: 1, UpdatedAt: now,
			}, nil
		},
		SetRealmEmailReceiveControl: func(_ context.Context, accountID, _ string, realmID, state string) (AgentEmailRealmReceiveControl, error) {
			realmTarget, realmState = realmID, state
			return AgentEmailRealmReceiveControl{
				AccountID: accountID, RealmID: realmID, ReceiveState: state,
				MailboxCount: 5, RowVersion: 2, UpdatedAt: now,
			}, nil
		},
	}
	handler := apiMux(cfg)
	agentID := "agent_aaaaaaaaaaaaaaaa"
	realmID := "realm_aaaaaaaaaaaaaaaa"

	response := performAgentEmailOwnerRequest(handler, http.MethodGet,
		"/v1/agents/"+agentID+"/email-receive", "operator-token", "", nil)
	if response.Code != http.StatusOK || agentTarget != agentID ||
		strings.Contains(response.Body.String(), "@") ||
		!strings.Contains(response.Body.String(), `"realm_receive_state":"disabled"`) {
		t.Fatalf("agent control get = %d %s target=%q", response.Code, response.Body.String(), agentTarget)
	}
	response = performAgentEmailOwnerRequest(handler, http.MethodPatch,
		"/v1/agents/"+agentID+"/email-receive", "operator-token",
		`{"receive_state":"disabled"}`, nil)
	if response.Code != http.StatusOK || agentState != "disabled" {
		t.Fatalf("agent control set = %d %s state=%q", response.Code, response.Body.String(), agentState)
	}
	response = performAgentEmailOwnerRequest(handler, http.MethodPatch,
		"/v1/agents/"+agentID+"/email-receive", "operator-token",
		`{"receive_state":"enabled","agent_id":"agent_bbbbbbbbbbbbbbbb"}`, nil)
	if response.Code != http.StatusBadRequest || agentState != "disabled" {
		t.Fatalf("spoofed agent control = %d %s state=%q", response.Code, response.Body.String(), agentState)
	}

	response = performAgentEmailOwnerRequest(handler, http.MethodGet,
		"/v1/realms/"+realmID+"/email-receive", "operator-token", "", nil)
	if response.Code != http.StatusOK || realmTarget != realmID ||
		!strings.Contains(response.Body.String(), `"mailbox_count":5`) {
		t.Fatalf("realm control get = %d %s target=%q", response.Code, response.Body.String(), realmTarget)
	}
	response = performAgentEmailOwnerRequest(handler, http.MethodPatch,
		"/v1/realms/"+realmID+"/email-receive", "operator-token",
		`{"receive_state":"disabled"}`, nil)
	if response.Code != http.StatusOK || realmState != "disabled" {
		t.Fatalf("realm control set = %d %s state=%q", response.Code, response.Body.String(), realmState)
	}
	for _, target := range []string{
		"/v1/agents/" + agentID + "/email-receive",
		"/v1/realms/" + realmID + "/email-receive",
	} {
		response = performAgentEmailOwnerRequest(handler, http.MethodGet, target, "agent-token", "", nil)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("non-operator %s status = %d", target, response.Code)
		}
		if got := response.Header().Get("Cache-Control"); got != "private, no-store" {
			t.Fatalf("operator control Cache-Control = %q", got)
		}
		response = performAgentEmailOwnerRequest(handler, http.MethodGet, target, "suspended-token", "", nil)
		if response.Code != http.StatusOK {
			t.Fatalf("suspended control GET %s status = %d body=%s", target, response.Code, response.Body.String())
		}
		response = performAgentEmailOwnerRequest(handler, http.MethodPatch, target, "suspended-token",
			`{"receive_state":"disabled"}`, nil)
		if response.Code != http.StatusOK {
			t.Fatalf("suspended disable %s status = %d body=%s", target, response.Code, response.Body.String())
		}
		response = performAgentEmailOwnerRequest(handler, http.MethodPatch, target, "suspended-token",
			`{"receive_state":"enabled"}`, nil)
		if response.Code != http.StatusForbidden {
			t.Fatalf("suspended enable %s status = %d body=%s", target, response.Code, response.Body.String())
		}
		response = performAgentEmailOwnerRequest(handler, http.MethodGet, target, "pending-token", "", nil)
		if response.Code != http.StatusForbidden {
			t.Fatalf("pending control GET %s status = %d", target, response.Code)
		}
	}
}

func TestAgentEmailRetryCanaryHTTPContract(t *testing.T) {
	pilot, _ := testAgentEmailPilotConfig(t)
	pilot.RetryCanaryAgentID = "agent_aaaaaaaaaaaaaaaa"
	const challenge = "11111111-2222-4333-8444-555555555555"
	auth := func(_ context.Context, token string) (DomainPrincipal, bool, error) {
		p := DomainPrincipal{
			Kind: PrincipalKindAgent, ID: pilot.RetryCanaryAgentID,
			AccountID: "acc_1", RealmID: "realm_aaaaaaaaaaaaaaaa",
			AccountStatus: "active", AccessProfile: AccessProfileFull,
		}
		if token == "other" {
			p.ID = "agent_bbbbbbbbbbbbbbbb"
		}
		return p, token == "canary" || token == "other", nil
	}
	var armedChallenge, statusChallenge string
	handler := apiMux(Config{
		AuthenticatePrincipal: auth, AgentEmailPilot: pilot,
		ArmAgentEmailRetryCanary: func(_ context.Context, _ DomainPrincipal, got string) (AgentEmailRetryCanaryCheckpoint, error) {
			armedChallenge = got
			return AgentEmailRetryCanaryCheckpoint{State: "armed", Armed: true}, nil
		},
		GetAgentEmailRetryCanary: func(_ context.Context, _ DomainPrincipal, got string) (AgentEmailRetryCanaryCheckpoint, error) {
			statusChallenge = got
			return AgentEmailRetryCanaryCheckpoint{
				State: "accepted", Armed: true, Tempfailed: true,
				Accepted: true, TempfailCount: 1,
			}, nil
		},
	})
	response := performAgentEmailOwnerRequest(handler, http.MethodPost,
		"/v1/email/retry-canary:arm", "canary", `{"challenge":"`+challenge+`"}`, nil)
	if response.Code != http.StatusOK || armedChallenge != challenge ||
		strings.Contains(response.Body.String(), challenge) {
		t.Fatalf("canary arm = %d %s challenge=%q", response.Code, response.Body.String(), armedChallenge)
	}
	response = performAgentEmailOwnerRequest(handler, http.MethodPost,
		"/v1/email/retry-canary:status", "canary", `{"challenge":"`+challenge+`"}`, nil)
	if response.Code != http.StatusOK || statusChallenge != challenge ||
		!strings.Contains(response.Body.String(), `"accepted":true`) ||
		strings.Contains(response.Body.String(), challenge) {
		t.Fatalf("canary status = %d %s challenge=%q", response.Code, response.Body.String(), statusChallenge)
	}
	response = performAgentEmailOwnerRequest(handler, http.MethodPost,
		"/v1/email/retry-canary:arm?challenge="+challenge, "canary", `{}`, nil)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("query challenge status = %d", response.Code)
	}
	response = performAgentEmailOwnerRequest(handler, http.MethodPost,
		"/v1/email/retry-canary:arm", "other", `{"challenge":"`+challenge+`"}`, nil)
	if response.Code != http.StatusForbidden {
		t.Fatalf("non-canary arm status = %d", response.Code)
	}
}

func TestAgentEmailPilotDefaultOffAndValidation(t *testing.T) {
	disabled := apiMux(Config{
		AuthenticatePrincipal: func(context.Context, string) (DomainPrincipal, bool, error) {
			return DomainPrincipal{}, true, nil
		},
		GetAgentEmailAddress: func(context.Context, DomainPrincipal) (AgentEmailAddress, error) {
			return AgentEmailAddress{}, nil
		},
		IngestAgentEmailPilot: func(context.Context, agentemail.RelayMetadata, []byte) error { return nil },
	})
	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/v1/email/address", nil),
		httptest.NewRequest(http.MethodPost, "/v1/internal/agent-email:ingest", nil),
	} {
		response := httptest.NewRecorder()
		disabled.ServeHTTP(response, request)
		if response.Code != http.StatusNotFound {
			t.Fatalf("default-off %s status = %d", request.URL.Path, response.Code)
		}
	}

	pilot, _ := testAgentEmailPilotConfig(t)
	if err := ValidateAgentEmailPilotConfig(pilot); err != nil {
		t.Fatalf("valid pilot config: %v", err)
	}
	tooFew := pilot
	tooFew.AgentIDs = map[string]bool{"agent_aaaaaaaaaaaaaaaa": true}
	if err := ValidateAgentEmailPilotConfig(tooFew); err == nil {
		t.Fatal("one-agent pilot config accepted")
	}
	twoRealms := pilot
	twoRealms.RealmIDs = map[string]bool{
		"realm_aaaaaaaaaaaaaaaa": true, "realm_bbbbbbbbbbbbbbbb": true,
	}
	if err := ValidateAgentEmailPilotConfig(twoRealms); err == nil {
		t.Fatal("two-realm pilot config accepted")
	}
	unboundedReplay := pilot
	unboundedReplay.RelayReplayWindow = 16 * time.Minute
	if err := ValidateAgentEmailPilotConfig(unboundedReplay); err == nil {
		t.Fatal("unbounded relay replay window accepted")
	}
	badKey := pilot
	badKey.RelayPublicKeys = map[string]ed25519.PublicKey{"pilot-key": {1}}
	if err := ValidateAgentEmailPilotConfig(badKey); err == nil {
		t.Fatal("short relay public key accepted")
	}
	badCanary := pilot
	badCanary.RetryCanaryAgentID = "agent_zzzzzzzzzzzzzzzz"
	if err := ValidateAgentEmailPilotConfig(badCanary); err == nil {
		t.Fatal("unenrolled retry canary accepted")
	}
}

func TestAgentEmailProcessingLeaseBounds(t *testing.T) {
	for _, seconds := range []int{0, 30, 300, 900} {
		if !agentEmailLeaseSecondsWithinBounds(seconds) {
			t.Errorf("lease %d should be accepted", seconds)
		}
	}
	for _, seconds := range []int{-1, 1, 29, 901} {
		if agentEmailLeaseSecondsWithinBounds(seconds) {
			t.Errorf("lease %d should be rejected", seconds)
		}
	}
}

func testAgentEmailPilotConfig(t *testing.T) (AgentEmailPilotConfig, ed25519.PrivateKey) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	return AgentEmailPilotConfig{
		Enabled: true, Domain: "agent-mail.witwave.ai", Audience: "cell-one",
		RealmIDs: map[string]bool{"realm_aaaaaaaaaaaaaaaa": true},
		AgentIDs: map[string]bool{
			"agent_aaaaaaaaaaaaaaaa": true,
			"agent_bbbbbbbbbbbbbbbb": true,
			"agent_cccccccccccccccc": true,
			"agent_dddddddddddddddd": true,
			"agent_eeeeeeeeeeeeeeee": true,
		},
		RelayPublicKeys:   map[string]ed25519.PublicKey{"pilot-key": publicKey},
		RelayReplayWindow: 5 * time.Minute,
		Now:               func() time.Time { return now },
	}, privateKey
}

func testAgentEmailRelayMetadata(raw []byte, pilot AgentEmailPilotConfig, keyID string) agentemail.RelayMetadata {
	digest := sha256.Sum256(raw)
	return agentemail.RelayMetadata{
		Timestamp: pilot.Now().Unix(), KeyID: keyID, Audience: pilot.Audience,
		EnvelopeSender:    "sender@example.com",
		EnvelopeRecipient: "pilot.realm@agent-mail.witwave.ai",
		RawSize:           int64(len(raw)), RawSHA256: hex.EncodeToString(digest[:]),
	}
}

func testAgentEmailIngestRequest(t *testing.T, raw []byte, metadata agentemail.RelayMetadata, privateKey ed25519.PrivateKey) *http.Request {
	t.Helper()
	return testAgentEmailIngestRequestWithSignedMetadata(t, raw, metadata, privateKey)
}

func testAgentEmailIngestRequestWithSignedMetadata(t *testing.T, body []byte, metadata agentemail.RelayMetadata, privateKey ed25519.PrivateKey) *http.Request {
	t.Helper()
	canonical, err := agentemail.CanonicalSignatureInput(metadata)
	if err != nil {
		t.Fatal(err)
	}
	signature := ed25519.Sign(privateKey, canonical)
	request := httptest.NewRequest(http.MethodPost, "/v1/internal/agent-email:ingest", bytes.NewReader(body))
	request.Header.Set("Content-Type", "message/rfc822")
	request.Header.Set(AgentEmailRelayHeaderVersion, agentemail.RelaySignatureVersion)
	request.Header.Set(AgentEmailRelayHeaderTimestamp, strconv.FormatInt(metadata.Timestamp, 10))
	request.Header.Set(AgentEmailRelayHeaderKeyID, metadata.KeyID)
	request.Header.Set(AgentEmailRelayHeaderAudience, metadata.Audience)
	request.Header.Set(AgentEmailRelayHeaderEnvelopeFrom, base64.RawURLEncoding.EncodeToString([]byte(metadata.EnvelopeSender)))
	request.Header.Set(AgentEmailRelayHeaderEnvelopeTo, base64.RawURLEncoding.EncodeToString([]byte(metadata.EnvelopeRecipient)))
	request.Header.Set(AgentEmailRelayHeaderRawSize, strconv.FormatInt(metadata.RawSize, 10))
	request.Header.Set(AgentEmailRelayHeaderRawSHA256, "sha256:"+metadata.RawSHA256)
	request.Header.Set(AgentEmailRelayHeaderSignature, base64.StdEncoding.EncodeToString(signature))
	return request
}

func assertAgentEmailVerdict(t *testing.T, response *httptest.ResponseRecorder, status int, verdict string) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("verdict status = %d body %s, want %d", response.Code, response.Body.String(), status)
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body) != 1 || body["verdict"] != verdict {
		t.Fatalf("verdict body = %#v, want only %q", body, verdict)
	}
}

func performAgentEmailOwnerRequest(handler http.Handler, method, target, token, body string, headers map[string]string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func decodeAgentEmailTestJSON(t *testing.T, response *httptest.ResponseRecorder, destination any) {
	t.Helper()
	if err := json.Unmarshal(response.Body.Bytes(), destination); err != nil {
		t.Fatalf("decode %s: %v", response.Body.String(), err)
	}
}
