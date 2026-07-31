package store

import "testing"

func TestMessageRateDebitRetryable(t *testing.T) {
	for _, test := range []struct {
		name             string
		limit, attempted int64
		want             bool
	}{
		{name: "exhausted but debit fits", limit: 60, attempted: 1, want: true},
		{name: "zero effective limit", limit: 0, attempted: 1, want: false},
		{name: "zero debit", limit: 60, attempted: 0, want: false},
		{name: "fanout exceeds capacity", limit: 1, attempted: 2, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := messageRateDebitRetryable(test.limit, test.attempted); got != test.want {
				t.Fatalf("messageRateDebitRetryable(%d, %d) = %t, want %t",
					test.limit, test.attempted, got, test.want)
			}
		})
	}
}

func TestMessageRateIntervalUsesCeilingForNonDivisorLimit(t *testing.T) {
	const limit int64 = 7
	interval := messageRateIntervalMicroseconds(limit)
	if interval != 8_571_429 {
		t.Fatalf("interval = %d, want 8571429", interval)
	}
	if interval*limit < messageRateWindowMicroseconds {
		t.Fatalf("ceiling interval permits more than %d debits per window", limit)
	}
	if (interval-1)*limit >= messageRateWindowMicroseconds {
		t.Fatalf("interval = %d is not the minimum strict interval", interval)
	}
}

func TestMessageDeliveredUsageQuantityCountsOnlyDeliveredTargets(t *testing.T) {
	targets := []messageDeliveryTarget{
		{state: MessageDeliveryDelivered},
		{state: MessageDeliveryFailed},
		{state: MessageDeliveryDelivered},
	}
	if got := messageDeliveredUsageQuantity(targets); got != 2 {
		t.Fatalf("delivered usage quantity = %d, want 2", got)
	}
	if got := messageDeliveredUsageQuantity([]messageDeliveryTarget{{state: MessageDeliveryFailed}}); got != 0 {
		t.Fatalf("failed-only delivered usage quantity = %d, want 0", got)
	}
}
