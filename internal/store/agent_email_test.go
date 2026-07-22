package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/agentemail"
)

func TestAgentEmailRetryCanaryDeliveryFingerprintV1Golden(t *testing.T) {
	t.Parallel()
	const (
		sender    = "canary-sender@example.com"
		recipient = "canary.abcdefghijkl2345@agent-mail.witwave.ai"
		want      = "0a9be96e2128380ffeaca4096c8154e8ad81306a3b912884a9d611df82a18d73"
	)
	raw := []byte(strings.Join([]string{
		"From: Canary Sender <canary-sender@example.com>",
		"To: Canary Agent <canary.abcdefghijkl2345@agent-mail.witwave.ai>",
		"Subject: retry canary golden",
		"Message-ID: <retry-canary-golden@example.com>",
		"Date: Tue, 21 Jul 2026 20:00:00 -0600",
		agentemail.RetryCanaryHeader + ": aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=utf-8",
		"Content-Transfer-Encoding: quoted-printable",
		"",
		"code=20123456",
	}, "\r\n"))

	got, _ := mustAgentEmailRetryCanaryFingerprint(t, raw, sender, recipient)
	if got != want {
		t.Fatalf("retry fingerprint v1 = %q, want %q", got, want)
	}
}

func TestAgentEmailRetryCanaryDeliveryFingerprintIgnoresTransportHeaders(t *testing.T) {
	t.Parallel()
	const (
		sender    = "canary-sender@example.com"
		recipient = "canary.abcdefghijkl2345@agent-mail.witwave.ai"
	)
	base := []byte(strings.Join([]string{
		"Received: from first.example by edge.example; Tue, 21 Jul 2026 20:00:01 -0600",
		"DKIM-Signature: v=1; a=rsa-sha256; b=first",
		"Authentication-Results: edge.example; dkim=pass",
		"From: Canary Sender <canary-sender@example.com>",
		"To: Canary Agent <canary.abcdefghijkl2345@agent-mail.witwave.ai>",
		"Subject: retry canary",
		"Message-ID: <retry-canary@example.com>",
		"Date: Tue, 21 Jul 2026 20:00:00 -0600",
		agentemail.RetryCanaryHeader + ": aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		"Content-Type: text/plain; charset=utf-8",
		"Content-Transfer-Encoding: quoted-printable",
		"",
		"code=20123456",
	}, "\r\n"))
	retry := []byte(strings.Join([]string{
		"Received: from retry.example by edge.example; Tue, 21 Jul 2026 20:05:01 -0600",
		"Received: by another-hop.example; Tue, 21 Jul 2026 20:05:00 -0600",
		"DKIM-Signature: v=1; a=rsa-sha256; b=second",
		"Authentication-Results: edge.example; dkim=pass; spf=pass",
		"From: Canary Sender <canary-sender@example.com>",
		"To: Canary Agent <canary.abcdefghijkl2345@agent-mail.witwave.ai>",
		"Subject: retry canary",
		"Message-ID: <retry-canary@example.com>",
		"Date: Tue, 21 Jul 2026 20:00:00 -0600",
		agentemail.RetryCanaryHeader + ": aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		"Content-Type: text/plain; charset=utf-8",
		"Content-Transfer-Encoding: quoted-printable",
		"",
		"code=20123456",
	}, "\r\n"))

	baseFingerprint, baseParsed := mustAgentEmailRetryCanaryFingerprint(
		t, base, sender, recipient,
	)
	retryFingerprint, retryParsed := mustAgentEmailRetryCanaryFingerprint(
		t, retry, sender, recipient,
	)
	if baseFingerprint != retryFingerprint {
		t.Fatalf("transport-header retry fingerprint = %q, want %q", retryFingerprint, baseFingerprint)
	}
	if baseParsed.Text != retryParsed.Text || baseParsed.Text != "code 123456" {
		t.Fatalf("decoded retry text = %q / %q", baseParsed.Text, retryParsed.Text)
	}

	baseRawDigest := sha256.Sum256(base)
	retryRawDigest := sha256.Sum256(retry)
	baseDuplicateGroup := agentEmailDuplicateGroup(
		hex.EncodeToString(baseRawDigest[:]), recipient, sender,
	)
	retryDuplicateGroup := agentEmailDuplicateGroup(
		hex.EncodeToString(retryRawDigest[:]), recipient, sender,
	)
	if baseDuplicateGroup == retryDuplicateGroup {
		t.Fatal("ordinary duplicate grouping ignored raw transport-header changes")
	}

	subjectChanged := []byte(strings.Replace(string(retry),
		"Subject: retry canary", "Subject: changed canary", 1))
	bodyChanged := []byte(strings.Replace(string(retry),
		"code=20123456", "code=20654321", 1))
	contentTypeChanged := []byte(strings.Replace(string(retry),
		"Content-Type: text/plain; charset=utf-8",
		"Content-Type: text/plain; charset=us-ascii", 1))
	// Both bodies decode to the same text, proving the exact MIME-body field is
	// independently covered rather than relying only on the text projection.
	equivalentDecodedBody := []byte(strings.Replace(string(retry),
		"code=20123456", "code 123456", 1))
	_, equivalentParsed := mustAgentEmailRetryCanaryFingerprint(
		t, equivalentDecodedBody, sender, recipient,
	)
	if equivalentParsed.Text != baseParsed.Text {
		t.Fatalf("equivalent decoded text = %q, want %q", equivalentParsed.Text, baseParsed.Text)
	}

	changes := []struct {
		name      string
		raw       []byte
		sender    string
		recipient string
	}{
		{name: "subject", raw: subjectChanged, sender: sender, recipient: recipient},
		{name: "body", raw: bodyChanged, sender: sender, recipient: recipient},
		{name: "exact body", raw: equivalentDecodedBody, sender: sender, recipient: recipient},
		{name: "content type", raw: contentTypeChanged, sender: sender, recipient: recipient},
		{name: "envelope sender", raw: retry, sender: "other@example.com", recipient: recipient},
		{name: "envelope recipient", raw: retry, sender: sender, recipient: "other.abcdefghijkl2345@agent-mail.witwave.ai"},
	}
	for _, change := range changes {
		t.Run(change.name, func(t *testing.T) {
			fingerprint, _ := mustAgentEmailRetryCanaryFingerprint(
				t, change.raw, change.sender, change.recipient,
			)
			if fingerprint == baseFingerprint {
				t.Fatalf("%s change preserved retry fingerprint", change.name)
			}
		})
	}
}

func TestAgentEmailRetryCanaryDeliveryFingerprintCoversParsedProjection(t *testing.T) {
	t.Parallel()
	raw := []byte("Subject: base\r\n\r\nbody")
	parsed, err := agentemail.ParseMessage(raw, true)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := func(
		state, errorCode string,
		projection agentemail.ParsedMessage,
	) string {
		t.Helper()
		value, err := agentEmailRetryCanaryDeliveryFingerprint(
			raw, "sender@example.com", "agent.abcdefghijkl2345@agent-mail.witwave.ai",
			state, errorCode, projection,
		)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	base := fingerprint(AgentEmailParseParsed, "", parsed)
	assertChanged := func(name string, state, errorCode string, projection agentemail.ParsedMessage) {
		t.Helper()
		if got := fingerprint(state, errorCode, projection); got == base {
			t.Fatalf("%s did not change retry fingerprint", name)
		}
	}

	assertChanged("parse state", AgentEmailParseError, "", parsed)
	assertChanged("parse error", AgentEmailParseParsed, "malformed_message", parsed)
	mutated := parsed
	mutated.HeaderFrom = "sender@example.com"
	assertChanged("from", AgentEmailParseParsed, "", mutated)
	mutated = parsed
	mutated.HeaderTo = "agent@example.com"
	assertChanged("to", AgentEmailParseParsed, "", mutated)
	mutated = parsed
	mutated.HeaderSubject = "changed"
	assertChanged("subject", AgentEmailParseParsed, "", mutated)
	mutated = parsed
	mutated.MIMEMessageID = "<message@example.com>"
	assertChanged("message id", AgentEmailParseParsed, "", mutated)
	mutated = parsed
	mutated.MIMEContentType = "text/html"
	assertChanged("content type", AgentEmailParseParsed, "", mutated)
	mutated = parsed
	mutated.MIMETransferEncoding = "base64"
	assertChanged("transfer encoding", AgentEmailParseParsed, "", mutated)
	mutated = parsed
	mutated.MIMEVersion = "1.0"
	assertChanged("MIME version", AgentEmailParseParsed, "", mutated)
	mutated = parsed
	messageDate := time.Unix(1784692800, 0).UTC()
	mutated.MessageDate = &messageDate
	assertChanged("message date", AgentEmailParseParsed, "", mutated)
	mutated = parsed
	mutated.TextKind = "text/html-rendered"
	assertChanged("text kind", AgentEmailParseParsed, "", mutated)
	mutated = parsed
	mutated.Text = "changed body"
	assertChanged("text", AgentEmailParseParsed, "", mutated)
	mutated = parsed
	mutated.AttachmentCount = 1
	assertChanged("attachment count", AgentEmailParseParsed, "", mutated)

	// Length-prefix framing prevents adjacent field ambiguity.
	first := parsed
	first.HeaderFrom, first.HeaderTo = "ab", "c"
	second := parsed
	second.HeaderFrom, second.HeaderTo = "a", "bc"
	if fingerprint(AgentEmailParseParsed, "", first) == fingerprint(AgentEmailParseParsed, "", second) {
		t.Fatal("length-prefix framing allowed adjacent-field collision")
	}
}

func TestAgentEmailRetryCanaryDeliveryFingerprintParseErrorsRequireExactRaw(t *testing.T) {
	t.Parallel()
	const (
		sender    = "sender@example.com"
		recipient = "agent.abcdefghijkl2345@agent-mail.witwave.ai"
	)
	base := []byte("Received: first\r\nContent-Transfer-Encoding: invalid\r\n\r\nbody")
	retry := []byte("Received: second\r\nContent-Transfer-Encoding: invalid\r\n\r\nbody")
	if _, err := agentemail.ParseMessage(base, true); err == nil {
		t.Fatal("invalid transfer encoding unexpectedly parsed")
	}
	baseFingerprint, _ := mustAgentEmailRetryCanaryFingerprint(t, base, sender, recipient)
	retryFingerprint, _ := mustAgentEmailRetryCanaryFingerprint(t, retry, sender, recipient)
	if baseFingerprint == retryFingerprint {
		t.Fatal("parse-error retry ignored raw header changes")
	}
	digest := sha256.Sum256(base)
	want := agentEmailDuplicateGroup(hex.EncodeToString(digest[:]), recipient, sender)
	if baseFingerprint != want {
		t.Fatalf("parse-error fingerprint = %q, want legacy exact-raw %q", baseFingerprint, want)
	}
}

func mustAgentEmailRetryCanaryFingerprint(
	t *testing.T,
	raw []byte,
	envelopeSender, envelopeRecipient string,
) (string, agentemail.ParsedMessage) {
	t.Helper()
	parsed, parseErr := agentemail.ParseMessage(raw, true)
	parseState := AgentEmailParseParsed
	parseErrorCode := ""
	if parseErr != nil {
		parseState = AgentEmailParseError
		parseErrorCode = agentemail.ParseErrorCode(parseErr)
	}
	fingerprint, err := agentEmailRetryCanaryDeliveryFingerprint(
		raw, envelopeSender, envelopeRecipient, parseState, parseErrorCode, parsed,
	)
	if err != nil {
		t.Fatal(err)
	}
	return fingerprint, parsed
}

func TestNormalizeAgentEmailProcessingInputs(t *testing.T) {
	if got, err := normalizeAgentEmailLease(0); err != nil || got != defaultAgentEmailLease {
		t.Fatalf("default lease = %v / %v", got, err)
	}
	for _, invalid := range []time.Duration{time.Second, 29 * time.Second, 16 * time.Minute} {
		if _, err := normalizeAgentEmailLease(invalid); !errors.Is(err, ErrAgentEmailInputInvalid) {
			t.Fatalf("lease %v error = %v", invalid, err)
		}
	}
	first, err := normalizeAgentEmailKey(" retry-key ", "claim")
	if err != nil || len(first) != 64 || strings.Contains(first, "retry-key") {
		t.Fatalf("processing key hash = %q / %v", first, err)
	}
	second, err := normalizeAgentEmailKey("retry-key", "completion")
	if err != nil || second != first {
		t.Fatalf("stable processing key hash = %q / %v, want %q", second, err, first)
	}
	if _, err := normalizeAgentEmailKey("", "claim"); !errors.Is(err, ErrAgentEmailInputInvalid) {
		t.Fatalf("empty key error = %v", err)
	}
	claimID, generation, err := normalizeAgentEmailFence(" ecl_aaaaaaaaaaaaaaaa ", 4)
	if err != nil || claimID != "ecl_aaaaaaaaaaaaaaaa" || generation != 4 {
		t.Fatalf("normalized fence = %q/%d / %v", claimID, generation, err)
	}
	for _, invalid := range []struct {
		claimID    string
		generation int64
	}{
		{"ecl_bad", 1}, {"mcl_aaaaaaaaaaaaaaaa", 1},
		{"ecl_aaaaaaaaaaaaaaaa", 0}, {"ecl_aaaaaaaaaaaaaaaa", maximumAgentEmailGeneration + 1},
	} {
		if _, _, err := normalizeAgentEmailFence(invalid.claimID, invalid.generation); !errors.Is(err, ErrAgentEmailInputInvalid) {
			t.Fatalf("fence %#v error = %v", invalid, err)
		}
	}
}

func TestNormalizeAgentEmailPilotScope(t *testing.T) {
	realmID := "realm_aaaaaaaaaaaaaaaa"
	agents := map[string]bool{
		"agent_aaaaaaaaaaaaaaaa": true,
		"agent_bbbbbbbbbbbbbbbb": true,
		"agent_cccccccccccccccc": true,
		"agent_dddddddddddddddd": true,
		"agent_eeeeeeeeeeeeeeee": true,
	}
	scope := AgentEmailPilotScope{
		Enabled: true, Domain: "Agent-Mail.Witwave.AI.", Audience: "cell-pilot-1",
		RealmIDs: map[string]bool{realmID: true}, AgentIDs: agents,
	}
	if got, err := normalizeAgentEmailPilotScope(scope); err != nil || got != "agent-mail.witwave.ai" {
		t.Fatalf("normalized scope = %q / %v", got, err)
	}
	tooFew := scope
	tooFew.AgentIDs = map[string]bool{"agent_aaaaaaaaaaaaaaaa": true}
	if _, err := normalizeAgentEmailPilotScope(tooFew); !errors.Is(err, ErrAgentEmailInputInvalid) {
		t.Fatalf("too-few scope error = %v", err)
	}
	twoRealms := scope
	twoRealms.RealmIDs = map[string]bool{
		realmID: true, "realm_bbbbbbbbbbbbbbbb": true,
	}
	if _, err := normalizeAgentEmailPilotScope(twoRealms); !errors.Is(err, ErrAgentEmailInputInvalid) {
		t.Fatalf("two-realm scope error = %v", err)
	}
	if _, err := requireAgentEmailPilotEnrollment(AgentEmailPilotScope{}, realmID, "agent_aaaaaaaaaaaaaaaa"); !errors.Is(err, ErrAgentEmailPilotDisabled) {
		t.Fatalf("disabled enrollment error = %v", err)
	}
	if _, err := requireAgentEmailPilotEnrollment(scope, realmID, "agent_zzzzzzzzzzzzzzzz"); !errors.Is(err, ErrAgentEmailPilotNotEnrolled) {
		t.Fatalf("unenrolled agent error = %v", err)
	}
	restricted := Principal{
		Kind: PrincipalAgent, ID: "agent_aaaaaaaaaaaaaaaa", RealmID: realmID,
		AccessProfile: AccessProfileCuratorPreview,
	}
	if err := requireAgentEmailPilotPrincipal(scope, restricted); !errors.Is(err, ErrAgentEmailForbidden) {
		t.Fatalf("restricted principal error = %v", err)
	}
}

func TestAgentEmailCursorRoundTrip(t *testing.T) {
	wantTime := time.Unix(0, 1721570400123456789).UTC()
	wantID := "emsg_aaaaaaaaaaaaaaaa"
	cursor := encodeAgentEmailCursor(wantTime, wantID)
	gotTime, gotID, err := decodeAgentEmailCursor(cursor)
	if err != nil || !gotTime.Equal(wantTime) || gotID != wantID {
		t.Fatalf("cursor round trip = %v/%q / %v", gotTime, gotID, err)
	}
	for _, invalid := range []string{"", "no-colon", "0:" + wantID, "1:emsg_bad"} {
		if _, _, err := decodeAgentEmailCursor(invalid); !errors.Is(err, ErrAgentEmailCursorInvalid) {
			t.Fatalf("cursor %q error = %v", invalid, err)
		}
	}
}
