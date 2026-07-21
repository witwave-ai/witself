package store

import "testing"

func TestAgentEmailUsageDimensionContract(t *testing.T) {
	dimensions := map[string]string{
		"received": UsageDimensionEmailReceived,
		"sent":     UsageDimensionEmailSent,
		"address":  UsageDimensionEmailAddress,
		"storage":  UsageDimensionEmailStorage,
	}
	want := map[string]string{
		"received": "email_received",
		"sent":     "email_sent",
		"address":  "email_address",
		"storage":  "email_storage_byte",
	}
	for name, dimension := range dimensions {
		if dimension != want[name] {
			t.Errorf("%s dimension = %q, want %q", name, dimension, want[name])
		}
		if !usageDimensionPattern.MatchString(dimension) {
			t.Errorf("%s dimension %q is not accepted by the usage ledger", name, dimension)
		}
	}
	for name, unit := range map[string]string{
		"email": UsageUnitEmail, "address": UsageUnitEmailAddress, "byte": UsageUnitByte,
	} {
		if !usageShortNamePattern.MatchString(unit) {
			t.Errorf("%s unit %q is not accepted by the usage ledger", name, unit)
		}
	}
}
