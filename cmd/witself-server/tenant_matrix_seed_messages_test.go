package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/agentemail"
	"github.com/witwave-ai/witself/internal/server"
	"github.com/witwave-ai/witself/internal/store"
)

// tenantSeedMessages prepares both directions and every destructive action on
// a separate object before the fingerprint. HTTP shapes follow
// internal/server/message_test.go and message_request_test.go. Inbound email
// uses verified relay material at the store boundary, and disposable request
// candidates use their store lifecycle without minting extra credentials.
func tenantSeedMessages(t *testing.T, rt *tenantRuntime, side tenantSide) {
	t.Helper()
	v := rt.f.bySide[side]
	mainToken, peerToken := rt.creds[side][credAgent], v["token.peer"]
	incoming := tenantSeedOneMessage(t, rt, side, peerToken, v["agent"], "message")
	v["message"], v["message-peer-to-main"] = incoming.ID, incoming.ID
	outgoing := tenantSeedOneMessage(t, rt, side, mainToken, v["peer_agent"], "message-main-to-peer")
	v["message-main-to-peer"] = outgoing.ThreadID
	for _, row := range tenantRows() {
		if row.Pattern != "POST /v1/messages/{action}" {
			continue
		}
		for _, probe := range row.Probes {
			action, _, _ := strings.Cut(probe.Name, "-")
			key := "message_" + action
			msg := tenantSeedOneMessage(t, rt, side, peerToken, v["agent"], probe.Fresh)
			values := map[string]string{key: msg.ID}
			if action == "ack" {
				// MC2 must be able to commit its deliberately unscoped ack.
				// Migration 0020 requires read_at before acked_at; an unread
				// fixture would make that mutation silently fail its CHECK.
				var read struct {
					Message server.Message `json:"message"`
				}
				tenantSeedMessageHTTP(t, rt, mainToken, "POST", "/v1/messages/"+msg.ID+":read", map[string]any{}, &read)
			}
			if action == "renew" || action == "release" || action == "complete" {
				var claimed struct {
					Processing server.MessageProcessing `json:"processing"`
				}
				tenantSeedMessageHTTP(t, rt, mainToken, "POST", "/v1/messages/"+msg.ID+":claim", map[string]any{"lease_seconds": 900}, &claimed)
				values["message_claim_id"] = claimed.Processing.ClaimID
			}
			tenantMessageProbeValues(rt, side, row, probe, values)
		}
	}
	tenantSeedMessageRequests(t, rt, side)
	tenantSeedEmails(t, rt, side)
}

func tenantSeedOneMessage(t *testing.T, rt *tenantRuntime, side tenantSide, token, recipient, key string) server.Message {
	t.Helper()
	v := rt.f.bySide[side]
	var out struct {
		Message server.Message `json:"message"`
	}
	tenantSeedMessageHTTP(t, rt, token, "POST", "/v1/messages", map[string]any{
		"to":      map[string]any{"kind": "agent", "id": recipient},
		"subject": tenantDomainCanary(v, side, key+".subject"),
		"body":    tenantDomainCanary(v, side, key+".body"),
	}, &out)
	v["seed."+key+".id"], v["seed."+key+".thread"] = out.Message.ID, out.Message.ThreadID
	return out.Message
}

// Message requests need different callers for coordinator and candidate
// actions. Select/cancel belong to main; offer/decline and claim lifecycles
// belong to main as a candidate on peer's request. Each fence-body probe has
// its own request, so an earlier control cannot consume its prerequisite.
func tenantSeedMessageRequests(t *testing.T, rt *tenantRuntime, side tenantSide) {
	t.Helper()
	v := rt.f.bySide[side]
	mainToken, peerToken := rt.creds[side][credAgent], v["token.peer"]
	base := tenantSeedOpenMessageRequest(t, rt, side, mainToken, "message_request")
	v["message_request"] = base.ID
	for _, row := range tenantRows() {
		if row.Pattern != "POST /v1/message-requests/{action}" {
			continue
		}
		for _, probe := range row.Probes {
			action, _, _ := strings.Cut(probe.Name, "-")
			if probe.Name == "selected-agent-body" {
				action = "select"
			}
			coordinatorToken, candidateToken, candidateID := peerToken, mainToken, v["agent"]
			if action == "select" || action == "cancel" {
				coordinatorToken, candidateToken, candidateID = mainToken, peerToken, v["peer_agent"]
			}
			request := tenantSeedOpenMessageRequest(t, rt, side, coordinatorToken, probe.Fresh)
			values := map[string]string{"message_request_" + action: request.ID}
			if action == "select" || action == "claim" || action == "renew" || action == "release" || action == "complete" {
				var offered struct {
					Offer server.MessageRequestOffer `json:"offer"`
				}
				tenantSeedMessageHTTP(t, rt, candidateToken, "POST", "/v1/message-requests/"+request.ID+":offer", map[string]any{
					"body": tenantDomainCanary(v, side, probe.Fresh+".offer.body"),
				}, &offered)
				v["seed."+probe.Fresh+".offer.message"] = offered.Offer.Message.ID
				tenantSeedDeclineOtherCandidates(t, rt, side, coordinatorToken, request.ID)
				if action != "select" {
					var selected struct {
						Selection server.MessageRequestSelection `json:"selection"`
						Claims    []server.MessageRequestClaim   `json:"claims"`
					}
					tenantSeedMessageHTTP(t, rt, coordinatorToken, "POST", "/v1/message-requests/"+request.ID+":select", map[string]any{
						"selected_agent_ids": []string{candidateID}, "reservation_seconds": 900,
					}, &selected)
					if len(selected.Claims) != 1 {
						t.Fatal("tenant request seed did not reserve exactly one candidate")
					}
					v["seed."+probe.Fresh+".selection"] = selected.Selection.ID
					v["seed."+probe.Fresh+".claim"] = selected.Claims[0].ClaimID
					if action != "claim" {
						var claimed struct {
							Claim server.MessageRequestClaim `json:"claim"`
						}
						tenantSeedMessageHTTP(t, rt, candidateToken, "POST", "/v1/message-requests/"+request.ID+":claim", map[string]any{"lease_seconds": 900}, &claimed)
						values["message_request_claim_id"] = claimed.Claim.ClaimID
					}
				}
			}
			tenantMessageProbeValues(rt, side, row, probe, values)
		}
	}
}

// SelectMessageRequest waits until every snapshot candidate has responded or
// the offer deadline has elapsed (message_request.go:1962-1975). Existing F1
// fixture agents are also in this realm snapshot. Decline their pending offers
// through the store lifecycle, as
// TestMessageRequestClientRankedLifecyclePostgres does,
// rather than changing timestamps or sleeping. These disposable agents need
// no additional HTTP credentials merely to seed their terminal responses.
func tenantSeedDeclineOtherCandidates(t *testing.T, rt *tenantRuntime, side tenantSide, coordinatorToken, requestID string) {
	t.Helper()
	var detail struct {
		Candidates []server.MessageRequestCandidate `json:"candidates"`
	}
	tenantSeedMessageHTTP(t, rt, coordinatorToken, "GET", "/v1/message-requests/"+requestID, nil, &detail)
	for _, candidate := range detail.Candidates {
		if candidate.ResponseState != store.MessageRequestCandidatePending {
			continue
		}
		principal := tenantSeedPrincipal(rt, side, credAgent, "")
		principal.ID, principal.AgentName = candidate.Agent.AgentID, candidate.Agent.AgentName
		_, err := rt.st.DeclineMessageRequest(context.Background(), principal, requestID, store.DeclineMessageRequestInput{})
		tenantSeedObjectError(t, "DeclineMessageRequest pending candidate", err)
	}
}

func tenantSeedOpenMessageRequest(t *testing.T, rt *tenantRuntime, side tenantSide, token, key string) server.MessageRequest {
	t.Helper()
	v := rt.f.bySide[side]
	var out struct {
		Request        server.MessageRequest `json:"request"`
		OpeningMessage server.Message        `json:"opening_message"`
	}
	tenantSeedMessageHTTP(t, rt, token, "POST", "/v1/message-requests", map[string]any{
		"subject":          tenantDomainCanary(v, side, key+".subject"),
		"body":             tenantDomainCanary(v, side, key+".body"),
		"selection_policy": "client_ranked", "max_assignees": 1,
		"offer_window_seconds": 900, "expires_in_seconds": 3600,
	}, &out)
	v["seed."+key+".id"] = out.Request.ID
	v["seed."+key+".opening"] = out.OpeningMessage.ID
	v["seed."+key+".thread"] = out.OpeningMessage.ThreadID
	return out.Request
}

func tenantSeedEmails(t *testing.T, rt *tenantRuntime, side tenantSide) {
	t.Helper()
	v := rt.f.bySide[side]
	if side == sideB {
		// The status control must stand alone when -run excludes the arm
		// control. Repeating this exact challenge is idempotent in
		// store.ArmAgentEmailRetryCanary (agent_email.go:1045-1058).
		var armed map[string]any
		tenantSeedMessageHTTP(t, rt, rt.creds[side][credAgent], "POST", "/v1/email/retry-canary:arm", map[string]any{
			"challenge": v["canary_challenge"],
		}, &armed)
	}
	scope := store.AgentEmailReceiveScope{
		Enabled: true, Mode: store.AgentEmailReceiveModeProduction,
		Domain: "witmail.net", Audience: "tenant-matrix",
		AccountIDs: map[string]bool{v["account"]: true},
	}
	base := tenantSeedInboundEmail(t, rt, side, scope, "inbound_email")
	v["inbound_email"] = base.ID
	for _, row := range tenantRows() {
		if row.Pattern != "POST /v1/email/{action}" {
			continue
		}
		for _, probe := range row.Probes {
			action, _, _ := strings.Cut(probe.Name, "-")
			if probe.Name == "code-consumed" {
				action = "code_consumed"
			}
			msg := tenantSeedInboundEmail(t, rt, side, scope, probe.Fresh)
			values := map[string]string{"inbound_email_" + action: msg.ID}
			if action == "renew" || action == "release" || action == "complete" {
				var claimed struct {
					Processing store.AgentEmailProcessing `json:"processing"`
				}
				tenantSeedMessageHTTP(t, rt, rt.creds[side][credAgent], "POST", "/v1/email/"+msg.ID+":claim", map[string]any{"lease_seconds": 900}, &claimed)
				values["email_claim"] = claimed.Processing.ClaimID
			}
			tenantMessageProbeValues(rt, side, row, probe, values)
		}
	}
	var sent struct {
		Message server.AgentEmailOutboundMessage `json:"message"`
	}
	tenantSeedMessageHTTP(t, rt, rt.creds[side][credAgent], "POST", "/v1/email:send", map[string]any{
		"to": v["email_to"], "subject": tenantDomainCanary(v, side, "outbound_email.subject"),
		"text": tenantDomainCanary(v, side, "outbound_email.text"),
	}, &sent)
	v["outbound_email"] = sent.Message.ID
	if sent.Message.ID == "" {
		t.Fatal("tenant outbound seed did not return an object ID")
	}
}

// IngestAgentEmailPilot is the verified-envelope store boundary used by
// TestAgentEmailOutboundLifecyclePostgres; the seed cannot fabricate a signed
// HTTP ingress request without borrowing the child's isolated relay key.
func tenantSeedInboundEmail(t *testing.T, rt *tenantRuntime, side tenantSide, scope store.AgentEmailReceiveScope, key string) store.AgentEmailMessage {
	t.Helper()
	v := rt.f.bySide[side]
	from := tenantDomainCanary(v, side, key+".from") + "@example.invalid"
	sender := tenantDomainCanary(v, side, key+".sender") + "@example.invalid"
	messageID := "<" + tenantDomainCanary(v, side, key+".message-id") + "@example.invalid>"
	recipient := v["seed.agent.email"]
	raw := []byte("From: " + from + "\r\nTo: " + recipient + "\r\n" +
		"Subject: " + tenantDomainCanary(v, side, key+".subject") + "\r\n" +
		"Message-ID: " + messageID + "\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" +
		tenantDomainCanary(v, side, key+".body"))
	digest := sha256.Sum256(raw)
	msg, err := rt.st.IngestAgentEmailPilot(context.Background(), scope, store.AgentEmailIngestInput{
		Relay: agentemail.RelayMetadata{
			Timestamp: time.Now().Unix(), KeyID: "tenant-matrix-seed", Audience: scope.Audience,
			EnvelopeSender: sender, EnvelopeRecipient: recipient,
			RawSize: int64(len(raw)), RawSHA256: hex.EncodeToString(digest[:]),
		}, Raw: raw,
	})
	tenantSeedObjectError(t, "IngestAgentEmailPilot", err)
	v["seed."+key+".id"], v["seed."+key+".mailbox"] = msg.ID, msg.MailboxID
	v["seed."+key+".address-id"] = msg.AddressID
	v["seed."+key+".from"], v["seed."+key+".sender"] = from, sender
	v["seed."+key+".message-id"] = messageID
	return msg
}

// The overlay selects each probe's actual prerequisite. The union remains in
// bySide under distinct seed keys, so a response leaking a sibling fixture's
// ID or claim still fails the leak check.
func tenantMessageProbeValues(rt *tenantRuntime, side tenantSide, row tenantRow, probe tenantProbe, values map[string]string) {
	rt.probeValues[side][tenantProbeID(row, probe)] = values
	for key, value := range values {
		rt.f.bySide[side]["seed."+probe.Fresh+"."+key] = value
		if probe.BodyRef == "" {
			rt.f.bySide[side][key] = value
		}
	}
}

func tenantSeedMessageHTTP(t *testing.T, rt *tenantRuntime, token, method, path string, body, out any) {
	t.Helper()
	raw, err := json.Marshal(body)
	tenantSeedObjectError(t, "marshal message HTTP seed", err)
	response := rt.send(t, tenantRow{}, tenantProbe{}, tenantRequest{
		Method: method, Path: path, Body: raw,
		Headers: http.Header{"Content-Type": {"application/json"}},
	}, token)
	if response.Status < 200 || response.Status >= 300 {
		// Print only the route action, never a payload, email address or token.
		parsed, _ := url.Parse(path)
		segments := strings.Split(parsed.Path, "/")
		action := "create"
		if len(segments) > 0 {
			if _, suffix, ok := strings.Cut(segments[len(segments)-1], ":"); ok {
				action = suffix
			}
		}
		t.Fatalf("tenant message seed %s %s failed with status %d", method, action, response.Status)
	}
	tenantSeedObjectError(t, "decode message HTTP seed", json.Unmarshal(response.Body, out))
}
