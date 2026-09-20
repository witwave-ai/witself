package plans

import "encoding/json"

//go:generate go run ./cmd/export-contract -output ../../infra/cloudflare/control-plane/src/plan-contract.json

// IntegerBounds describes the inclusive range accepted for one policy key.
type IntegerBounds struct {
	Minimum int64 `json:"minimum"`
	Maximum int64 `json:"maximum"`
}

// ValidationContract is the Worker-facing projection of the Go snapshot
// vocabulary and integer bounds. It contains no account or deployment state.
type ValidationContract struct {
	SchemaVersion string                   `json:"schema_version"`
	FeatureKeys   []string                 `json:"feature_keys"`
	LimitMaximums map[string]int64         `json:"limit_maximums"`
	PolicyBounds  map[string]IntegerBounds `json:"policy_bounds"`
}

// WorkerValidationContract returns a fresh contract for the Worker validators.
// The boundary-parity test checks these exported bounds against ValidateLimits
// and ValidatePolicies, so adding a Go key or changing a bound cannot silently
// leave the generated Worker vocabulary behind.
func WorkerValidationContract() ValidationContract {
	maximums := map[string]int64{
		AgentEmailMaxRawBytesLimit:                     MaxAgentEmailRawBytes,
		AgentEmailSentPerAgentMinuteLimit:              MaxAgentEmailSentPerAgentMinute,
		AgentEmailSentPerRealmMinuteLimit:              MaxAgentEmailSentPerRealmMinute,
		MessageSentPerAgentMinuteLimit:                 MaxMessageSentPerAgentMinute,
		MessageDeliveredPerRealmMinuteLimit:            MaxMessageDeliveredPerRealmMinute,
		MessageDeliveredPerRecipientMinuteLimit:        MaxMessageDeliveredPerRecipientMinute,
		AgentEmailReceivedPerSenderMinuteLimit:         MaxAgentEmailReceivedPerSenderMinute,
		AgentEmailReceivedPerRecipientMinuteLimit:      MaxAgentEmailReceivedPerRecipientMinute,
		AgentEmailReceivedPerRealmMinuteLimit:          MaxAgentEmailReceivedPerRealmMinute,
		AgentEmailReceivedBytesPerSenderMinuteLimit:    MaxAgentEmailReceivedBytesPerSenderMinute,
		AgentEmailReceivedBytesPerRecipientMinuteLimit: MaxAgentEmailReceivedBytesPerRecipientMinute,
		AgentEmailReceivedBytesPerRealmMinuteLimit:     MaxAgentEmailReceivedBytesPerRealmMinute,
	}
	for _, key := range SupportedLimitKeys() {
		if _, ok := maximums[key]; !ok {
			maximums[key] = MaxPlanLimit
		}
	}
	return ValidationContract{
		SchemaVersion: "witself.plan-contract.v1",
		FeatureKeys:   SupportedFeatureKeys(),
		LimitMaximums: maximums,
		PolicyBounds: map[string]IntegerBounds{
			AgentEmailEntitlementVersionPolicy:    {AgentEmailEntitlementVersion, AgentEmailEntitlementVersion},
			AgentEmailRetentionDaysPolicy:         {1, MaxAgentEmailRetentionDays},
			CollaborationEntitlementVersionPolicy: {CollaborationEntitlementVersion, CollaborationEntitlementVersion},
			MessageRetentionDaysPolicy:            {1, MaxMessageRetentionDays},
			MessagingEntitlementVersionPolicy:     {MessagingEntitlementVersion, MessagingEntitlementVersion},
			TranscriptRetentionDaysPolicy:         {1, MaxTranscriptRetentionDays},
		},
	}
}

// WorkerValidationContractJSON returns the deterministic checked-in contract.
func WorkerValidationContractJSON() ([]byte, error) {
	data, err := json.MarshalIndent(WorkerValidationContract(), "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}
