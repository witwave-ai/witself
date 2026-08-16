package store

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/plans"
)

func TestAgentEmailRateIntervalUsesCeilingForNonDivisorLimit(t *testing.T) {
	const limit int64 = 7
	interval := agentEmailRateIntervalNanoseconds(limit)
	if interval != 8_571_428_572 {
		t.Fatalf("interval = %d, want 8571428572", interval)
	}
	if interval*limit < agentEmailRateWindowNanoseconds {
		t.Fatalf("ceiling interval permits more than %d debits per window", limit)
	}
	if (interval-1)*limit >= agentEmailRateWindowNanoseconds {
		t.Fatalf("interval = %d is not the minimum strict interval", interval)
	}
	for _, limit := range []int64{0, -1} {
		if got := agentEmailRateIntervalNanoseconds(limit); got != 0 {
			t.Fatalf("interval for limit %d = %d, want 0", limit, got)
		}
	}
}

func TestAgentEmailAccountRateUsesBoundedBurstCapacity(t *testing.T) {
	countDebit := agentEmailRateDebit{
		burstLimit: plans.MaxAgentEmailReceivedPerAccountMinuteBurst,
	}
	if got := agentEmailRateBucketCapacity(
		countDebit, plans.MaxAgentEmailReceivedPerAccountMinute,
	); got != plans.MaxAgentEmailReceivedPerAccountMinuteBurst {
		t.Fatalf("account count burst = %d", got)
	}
	byteDebit := agentEmailRateDebit{
		burstLimit: plans.MaxAgentEmailReceivedBytesPerAccountMinuteBurst,
	}
	if got := agentEmailRateBucketCapacity(
		byteDebit, plans.MaxAgentEmailReceivedBytesPerAccountMinute,
	); got != plans.MaxAgentEmailReceivedBytesPerAccountMinuteBurst {
		t.Fatalf("account byte burst = %d", got)
	}
	if got := agentEmailRateBucketCapacity(agentEmailRateDebit{}, 7); got != 7 {
		t.Fatalf("ordinary rate burst = %d, want effective limit", got)
	}
	if got := agentEmailRateBucketCapacity(agentEmailRateDebit{burstLimit: 10}, 4); got != 4 {
		t.Fatalf("lower effective limit burst = %d, want 4", got)
	}
}

func TestAgentEmailSenderScopeIDIsStableAndValueFree(t *testing.T) {
	const (
		sender    = "sender@example.com"
		recipient = "owner.realm@agent-mail.witwave.ai"
		want      = "477d7bde0f648883db69375a0df54e1dcc7e220f11d38ccf2fd0240134e2aa17"
	)

	got := agentEmailSenderScopeID(sender, recipient)
	if got != want {
		t.Fatalf("sender scope id = %q, want stable vector %q", got, want)
	}
	if repeat := agentEmailSenderScopeID(sender, recipient); repeat != got {
		t.Fatalf("sender scope id changed across calls: %q != %q", repeat, got)
	}
	if len(got) != 64 {
		t.Fatalf("sender scope id length = %d, want 64", len(got))
	}
	if _, err := hex.DecodeString(got); err != nil {
		t.Fatalf("sender scope id is not lowercase hex: %v", err)
	}
	if strings.Contains(got, sender) || strings.Contains(got, recipient) ||
		strings.Contains(got, "example") || strings.Contains(got, "agent-mail") {
		t.Fatalf("sender scope id contains plaintext input: %q", got)
	}
	if changed := agentEmailSenderScopeID("other@example.com", recipient); changed == got {
		t.Fatal("changing sender did not change sender scope id")
	}
	if changed := agentEmailSenderScopeID(sender, "other.realm@agent-mail.witwave.ai"); changed == got {
		t.Fatal("changing recipient did not change sender scope id")
	}
}

func TestEffectiveAgentEmailRateLimitKeepsPlatformBound(t *testing.T) {
	const platformLimit int64 = 300
	for _, test := range []struct {
		name       string
		limits     map[string]int64
		wantLimit  int64
		wantSource string
	}{
		{
			name:      "missing key uses platform",
			limits:    map[string]int64{},
			wantLimit: platformLimit, wantSource: AgentEmailRateSourcePlatform,
		},
		{
			name:      "nil limits use platform",
			wantLimit: platformLimit, wantSource: AgentEmailRateSourcePlatform,
		},
		{
			name:      "lower plan cap wins",
			limits:    map[string]int64{plans.AgentEmailReceivedPerRecipientMinuteLimit: 60},
			wantLimit: 60, wantSource: AgentEmailRateSourcePlan,
		},
		{
			name:      "zero plan cap wins",
			limits:    map[string]int64{plans.AgentEmailReceivedPerRecipientMinuteLimit: 0},
			wantLimit: 0, wantSource: AgentEmailRateSourcePlan,
		},
		{
			name:      "equal plan cap remains attributable to plan",
			limits:    map[string]int64{plans.AgentEmailReceivedPerRecipientMinuteLimit: platformLimit},
			wantLimit: platformLimit, wantSource: AgentEmailRateSourcePlan,
		},
		{
			name:      "defensive platform bound wins over malformed larger plan cap",
			limits:    map[string]int64{plans.AgentEmailReceivedPerRecipientMinuteLimit: platformLimit + 1},
			wantLimit: platformLimit, wantSource: AgentEmailRateSourcePlatform,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			gotLimit, gotSource := effectiveAgentEmailRateLimit(
				test.limits,
				plans.AgentEmailReceivedPerRecipientMinuteLimit,
				platformLimit,
			)
			if gotLimit != test.wantLimit || gotSource != test.wantSource {
				t.Fatalf("effective limit = %d/%q, want %d/%q",
					gotLimit, gotSource, test.wantLimit, test.wantSource)
			}
		})
	}
}

func TestAgentEmailRateDebitRetryable(t *testing.T) {
	for _, test := range []struct {
		name             string
		limit, attempted int64
		want             bool
	}{
		{name: "exhausted but debit fits", limit: 60, attempted: 1, want: true},
		{name: "weighted debit fits", limit: 1024, attempted: 1024, want: true},
		{name: "zero effective limit", limit: 0, attempted: 1, want: false},
		{name: "zero debit", limit: 60, attempted: 0, want: false},
		{name: "weighted debit exceeds capacity", limit: 1024, attempted: 1025, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := agentEmailRateDebitRetryable(test.limit, test.attempted); got != test.want {
				t.Fatalf("agentEmailRateDebitRetryable(%d, %d) = %t, want %t",
					test.limit, test.attempted, got, test.want)
			}
		})
	}
}
