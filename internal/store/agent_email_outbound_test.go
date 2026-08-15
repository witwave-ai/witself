package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/plans"
)

func TestNormalizeSendAgentEmailInputIsSingleRecipientPlainText(t *testing.T) {
	draft, err := normalizeSendAgentEmailInput(SendAgentEmailInput{
		To: " Recipient@Example.COM ", Subject: " status ",
		Text: "plain text", IdempotencyKey: " send-1 ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if draft.to != "recipient@example.com" || draft.subject != "status" ||
		draft.text != "plain text" || draft.idempotencyKey != "send-1" ||
		draft.requestKind != AgentEmailOutboundRequestDirect {
		t.Fatalf("normalized draft = %#v", draft)
	}
	if draft.requestHash == "" || len(draft.requestHash) != 64 {
		t.Fatalf("request hash = %q", draft.requestHash)
	}

	for _, test := range []struct {
		name string
		in   SendAgentEmailInput
	}{
		{name: "display name", in: SendAgentEmailInput{
			To: "Person <person@example.com>", Subject: "x", Text: "x", IdempotencyKey: "k"}},
		{name: "multiple", in: SendAgentEmailInput{
			To: "one@example.com,two@example.com", Subject: "x", Text: "x", IdempotencyKey: "k"}},
		{name: "subject newline", in: SendAgentEmailInput{
			To: "one@example.com", Subject: "hello\r\nBcc: x@example.com",
			Text: "x", IdempotencyKey: "k"}},
		{name: "subject tab", in: SendAgentEmailInput{
			To: "one@example.com", Subject: "hello\tworld",
			Text: "x", IdempotencyKey: "k"}},
		{name: "empty text", in: SendAgentEmailInput{
			To: "one@example.com", Subject: "x", Text: " \n\t", IdempotencyKey: "k"}},
		{name: "nul text", in: SendAgentEmailInput{
			To: "one@example.com", Subject: "x", Text: "a\x00b", IdempotencyKey: "k"}},
		{name: "missing key", in: SendAgentEmailInput{
			To: "one@example.com", Subject: "x", Text: "x"}},
		{name: "missing subject", in: SendAgentEmailInput{
			To: "one@example.com", Text: "x", IdempotencyKey: "k"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := normalizeSendAgentEmailInput(test.in); !errors.Is(err, ErrAgentEmailOutboundInputInvalid) {
				t.Fatalf("error = %v, want invalid outbound input", err)
			}
		})
	}
}

func TestResolveAgentEmailOutboundReplyTargetUsesReplyToThenFrom(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		headerFrom string
		want       string
		wantErr    bool
	}{
		{
			name: "reply to wins",
			raw: "From: Sender <sender@example.com>\r\n" +
				"Reply-To: Reply Desk <reply@example.com>\r\n\r\nbody",
			headerFrom: "Sender <sender@example.com>", want: "reply@example.com",
		},
		{
			name:       "from fallback",
			raw:        "From: Sender <sender@example.com>\r\n\r\nbody",
			headerFrom: "Sender <sender@example.com>", want: "sender@example.com",
		},
		{
			name: "multiple reply targets fail closed",
			raw: "From: sender@example.com\r\n" +
				"Reply-To: one@example.com, two@example.com\r\n\r\nbody",
			headerFrom: "sender@example.com", wantErr: true,
		},
		{
			name:       "missing retained headers fail closed",
			headerFrom: "sender@example.com", wantErr: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := resolveAgentEmailOutboundReplyTarget([]byte(test.raw), test.headerFrom)
			if test.wantErr {
				if !errors.Is(err, ErrAgentEmailReplyUnavailable) {
					t.Fatalf("error = %v", err)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("target = %q / %v, want %q", got, err, test.want)
			}
		})
	}
}

func TestAgentEmailOutboundRequestHashPinsExactNormalizedIntent(t *testing.T) {
	base := agentEmailOutboundRequestHash("direct", "to@example.com", "subject", "text")
	if repeat := agentEmailOutboundRequestHash("direct", "to@example.com", "subject", "text"); repeat != base {
		t.Fatalf("repeat hash = %q, want %q", repeat, base)
	}
	for _, changed := range []string{
		agentEmailOutboundRequestHash("reply", "to@example.com", "subject", "text"),
		agentEmailOutboundRequestHash("direct", "other@example.com", "subject", "text"),
		agentEmailOutboundRequestHash("direct", "to@example.com", "other", "text"),
		agentEmailOutboundRequestHash("direct", "to@example.com", "subject", "other"),
	} {
		if changed == base {
			t.Fatal("changed request reused the same request hash")
		}
	}
}

func TestSafeAgentEmailReplySubjectIsBoundedAndHeaderSafe(t *testing.T) {
	if got := safeAgentEmailReplySubject("status\r\nBcc: target@example.com"); got != "Re: status Bcc: target@example.com" {
		t.Fatalf("safe reply subject = %q", got)
	}
	if got := safeAgentEmailReplySubject("status\u0085Bcc: target@example.com"); got != "Re: status Bcc: target@example.com" {
		t.Fatalf("safe Unicode-control reply subject = %q", got)
	}
	if got := safeAgentEmailReplySubject("RE: existing"); got != "RE: existing" {
		t.Fatalf("existing reply subject = %q", got)
	}
	if got := safeAgentEmailReplySubject(""); got != "Re: (no subject)" {
		t.Fatalf("empty reply subject = %q", got)
	}
	got := safeAgentEmailReplySubject(strings.Repeat("é", maximumAgentEmailOutboundSubjectBytes))
	if len(got) > maximumAgentEmailOutboundSubjectBytes || !strings.HasPrefix(got, "Re: ") {
		t.Fatalf("bounded reply subject length/prefix = %d/%q", len(got), got[:4])
	}
}

func TestAgentEmailOutboundProviderEventTransitions(t *testing.T) {
	tests := []struct {
		current, event        string
		state, code, suppress string
		change                bool
	}{
		{AgentEmailOutboundAccepted, AgentEmailOutboundProviderEventDelivered,
			AgentEmailOutboundDelivered, "", "", true},
		{AgentEmailOutboundDelivered, AgentEmailOutboundProviderEventDeferred,
			"", "", "", false},
		{AgentEmailOutboundDelivered, AgentEmailOutboundProviderEventBounced,
			AgentEmailOutboundBounced, AgentEmailOutboundErrorRecipientHardBounce,
			"hard_bounce", true},
		{AgentEmailOutboundDelivered, AgentEmailOutboundProviderEventComplained,
			AgentEmailOutboundFailed, AgentEmailOutboundErrorRecipientComplained,
			"complained", true},
		{AgentEmailOutboundRejected, AgentEmailOutboundProviderEventComplained,
			"", "", "complained", false},
	}
	for _, test := range tests {
		state, code, change, suppress := agentEmailOutboundProviderEventTransition(
			test.current, test.event,
		)
		if state != test.state || code != test.code || change != test.change ||
			suppress != test.suppress {
			t.Errorf("transition %s/%s = %q/%q/%t/%q, want %q/%q/%t/%q",
				test.current, test.event, state, code, change, suppress,
				test.state, test.code, test.change, test.suppress)
		}
	}
}

func TestAgentEmailOutboundFinalizeInputUsesClosedVocabulary(t *testing.T) {
	accepted := FinalizeAgentEmailOutboundInput{
		State: AgentEmailOutboundAccepted, Provider: "cloudflare_email_sending",
		ProviderMessageID: "provider-1",
	}
	if err := normalizeAgentEmailOutboundFinalizeInput(&accepted); err != nil {
		t.Fatal(err)
	}
	bad := accepted
	bad.State = "sent"
	if err := normalizeAgentEmailOutboundFinalizeInput(&bad); !errors.Is(err, ErrAgentEmailOutboundInputInvalid) {
		t.Fatalf("unknown state error = %v", err)
	}
	bounced := FinalizeAgentEmailOutboundInput{
		State: AgentEmailOutboundBounced, Provider: "cloudflare_email_sending",
		ErrorCode: AgentEmailOutboundErrorProviderFailed,
	}
	if err := normalizeAgentEmailOutboundFinalizeInput(&bounced); !errors.Is(err, ErrAgentEmailOutboundInputInvalid) {
		t.Fatalf("soft/unknown bounce error = %v", err)
	}
	failed := FinalizeAgentEmailOutboundInput{
		State: AgentEmailOutboundFailed, Provider: "cloudflare_email_sending",
		ErrorCode: AgentEmailOutboundErrorProviderTimeout,
	}
	if err := normalizeAgentEmailOutboundFinalizeInput(&failed); !errors.Is(err, ErrAgentEmailOutboundInputInvalid) {
		t.Fatalf("ambiguous result disguised as failed error = %v", err)
	}
	badProviderID := accepted
	badProviderID.ProviderMessageID = "provider\x00id"
	if err := normalizeAgentEmailOutboundFinalizeInput(&badProviderID); !errors.Is(err, ErrAgentEmailOutboundInputInvalid) {
		t.Fatalf("unsafe provider message id error = %v", err)
	}
}

func TestEffectiveAgentEmailOutboundRateLimitKeepsPlatformBreaker(t *testing.T) {
	const platform int64 = 30
	for _, test := range []struct {
		name       string
		limits     map[string]int64
		want       int64
		wantSource string
	}{
		{name: "unset", want: platform, wantSource: AgentEmailOutboundRateSourcePlatform},
		{name: "lower plan", limits: map[string]int64{
			plans.AgentEmailSentPerAgentMinuteLimit: 5,
		}, want: 5, wantSource: AgentEmailOutboundRateSourcePlan},
		{name: "zero plan", limits: map[string]int64{
			plans.AgentEmailSentPerAgentMinuteLimit: 0,
		}, want: 0, wantSource: AgentEmailOutboundRateSourcePlan},
		{name: "malformed higher", limits: map[string]int64{
			plans.AgentEmailSentPerAgentMinuteLimit: 31,
		}, want: platform, wantSource: AgentEmailOutboundRateSourcePlatform},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, source := effectiveAgentEmailOutboundRateLimit(
				test.limits, plans.AgentEmailSentPerAgentMinuteLimit, platform,
			)
			if got != test.want || source != test.wantSource {
				t.Fatalf("effective limit = %d/%s, want %d/%s",
					got, source, test.want, test.wantSource)
			}
		})
	}
}

func TestAgentEmailOutboundCursorRoundTrip(t *testing.T) {
	wantTime := time.Unix(1_700_000_000, 123).UTC()
	wantID := "esnd_abcdefghijklmnop"
	cursor := encodeAgentEmailOutboundCursor(wantTime, wantID)
	gotTime, gotID, err := decodeAgentEmailOutboundCursor(cursor)
	if err != nil || !gotTime.Equal(wantTime) || gotID != wantID {
		t.Fatalf("decode = %s/%s/%v, want %s/%s", gotTime, gotID, err, wantTime, wantID)
	}
	if _, _, err := decodeAgentEmailOutboundCursor("not base64 !"); !errors.Is(err, ErrAgentEmailOutboundCursorInvalid) {
		t.Fatalf("bad cursor error = %v", err)
	}
}
