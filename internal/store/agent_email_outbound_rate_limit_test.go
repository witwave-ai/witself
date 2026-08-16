package store

import (
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/plans"
)

func TestAgentEmailOutboundRecipientScopeIDIsStableAndOpaque(t *testing.T) {
	const accountID = "acc_aaaaaaaaaaaaaaaa"
	const recipient = "recipient@example.com"
	first := agentEmailOutboundRecipientScopeID(accountID, recipient)
	second := agentEmailOutboundRecipientScopeID(accountID, recipient)
	otherRecipient := agentEmailOutboundRecipientScopeID(accountID, "other@example.com")
	otherAccount := agentEmailOutboundRecipientScopeID("acc_bbbbbbbbbbbbbbbb", recipient)
	if first != second || first == otherRecipient || first == otherAccount || len(first) != 64 {
		t.Fatalf("recipient scope ids are not stable, account-local SHA-256 values")
	}
	if strings.Contains(first, recipient) || strings.Contains(first, "@") {
		t.Fatalf("recipient scope id exposed address material: %q", first)
	}
}

func TestAgentEmailOutboundRateIntervalUsesRequestedWindow(t *testing.T) {
	minute := agentEmailOutboundRateIntervalMicroseconds(60, 60)
	day := agentEmailOutboundRateIntervalMicroseconds(60, 24*60*60)
	if minute != 1_000_000 || day != 1_440_000_000 {
		t.Fatalf("rate intervals minute=%d day=%d", minute, day)
	}
	if got := agentEmailOutboundRateIntervalMicroseconds(0, 60); got != 0 {
		t.Fatalf("zero-limit interval = %d, want 0", got)
	}
	if got := agentEmailOutboundRateIntervalMicroseconds(1, 0); got != 0 {
		t.Fatalf("zero-window interval = %d, want 0", got)
	}
}

func TestAgentEmailOutboundDailyRatesUseBoundedBurstCapacity(t *testing.T) {
	account := agentEmailOutboundRateDebit{
		burstLimit: plans.MaxAgentEmailSentPerAccountDayBurst,
	}
	if got := agentEmailOutboundRateBucketCapacity(
		account, plans.MaxAgentEmailSentPerAccountDay,
	); got != plans.MaxAgentEmailSentPerAccountDayBurst {
		t.Fatalf("daily account burst = %d", got)
	}
	recipient := agentEmailOutboundRateDebit{
		burstLimit: plans.MaxAgentEmailSentPerRecipientDayBurst,
	}
	if got := agentEmailOutboundRateBucketCapacity(
		recipient, plans.MaxAgentEmailSentPerRecipientDay,
	); got != plans.MaxAgentEmailSentPerRecipientDayBurst {
		t.Fatalf("daily recipient burst = %d", got)
	}
	if got := agentEmailOutboundRateBucketCapacity(
		agentEmailOutboundRateDebit{}, 30,
	); got != 30 {
		t.Fatalf("minute burst = %d, want effective limit", got)
	}
}
