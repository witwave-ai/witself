package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/agentemailoutbound"
	"github.com/witwave-ai/witself/internal/store"
)

func TestProcessAgentEmailOutboundBatchStartsProviderFenceBeforeSend(t *testing.T) {
	st := &fakeAgentEmailOutboundWorkerStore{
		claims:     []store.AgentEmailOutboundDispatch{validAgentEmailOutboundClaim()},
		reconciled: 2,
	}
	client := &fakeAgentEmailOutboundDispatchClient{
		actions: &st.actions,
		response: agentemailoutbound.Response{
			SchemaVersion:     agentemailoutbound.ResponseSchemaVersion,
			SendID:            "esnd_test",
			State:             agentemailoutbound.StateAccepted,
			Provider:          agentEmailOutboundProvider,
			ProviderMessageID: "provider-message-1",
		},
	}

	result, err := processAgentEmailOutboundBatch(
		context.Background(), st, client, defaultAgentEmailOutboundWorkerConfig(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Claimed != 1 || result.Accepted != 1 || result.ExpiredReconciled != 2 {
		t.Fatalf("result = %#v", result)
	}
	wantActions := []string{"reconcile", "claim", "start", "send", "finalize", "claim"}
	if !equalStrings(st.actions, wantActions) {
		t.Fatalf("actions = %#v, want %#v", st.actions, wantActions)
	}
	if st.finalized.State != store.AgentEmailOutboundAccepted ||
		st.finalized.Provider != agentEmailOutboundProvider ||
		st.finalized.ProviderMessageID != "provider-message-1" {
		t.Fatalf("finalization = %#v", st.finalized)
	}
	if client.dispatch.Text != "hello from the agent" ||
		client.dispatch.From != "scott.default@send.witmail.net" ||
		client.dispatch.ReplyTo != "scott.default@witmail.net" {
		t.Fatalf("provider dispatch = %#v", client.dispatch)
	}
}

func TestDispatchClaimedAgentEmailSchedulesExactReplayOnTransportUncertainty(t *testing.T) {
	st := &fakeAgentEmailOutboundWorkerStore{}
	claim := validAgentEmailOutboundClaim()
	client := &fakeAgentEmailOutboundDispatchClient{
		actions: &st.actions,
		response: agentemailoutbound.Response{
			SchemaVersion: agentemailoutbound.ResponseSchemaVersion,
			SendID:        claim.Message.ID,
			State:         agentemailoutbound.StateAmbiguous,
			Provider:      "managed",
		},
		err: errors.New("connection reset after request write"),
	}

	outcome, err := dispatchClaimedAgentEmail(
		context.Background(), st, client, time.Second, claim,
	)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != store.AgentEmailOutboundQueued {
		t.Fatalf("outcome = %q", outcome)
	}
	if st.retry.ErrorCode != store.AgentEmailOutboundErrorProviderConnectionReset ||
		!st.retry.PreserveProviderBoundary ||
		st.retry.Provider != agentEmailOutboundProvider ||
		st.ambiguous.Claim.ClaimID != "" || st.finalized.Claim.ClaimID != "" {
		t.Fatalf("ambiguous=%#v retry=%#v finalized=%#v", st.ambiguous, st.retry, st.finalized)
	}
	if !equalStrings(st.actions, []string{"start", "send", "retry"}) {
		t.Fatalf("actions = %#v", st.actions)
	}
}

func TestDispatchClaimedAgentEmailRetriesOnlyKnownNonAcceptance(t *testing.T) {
	st := &fakeAgentEmailOutboundWorkerStore{}
	claim := validAgentEmailOutboundClaim()
	client := &fakeAgentEmailOutboundDispatchClient{
		actions: &st.actions,
		response: agentemailoutbound.Response{
			SchemaVersion:     agentemailoutbound.ResponseSchemaVersion,
			SendID:            claim.Message.ID,
			State:             agentemailoutbound.StateRetryable,
			Provider:          agentEmailOutboundProvider,
			ErrorCode:         "provider_daily_limit",
			RetryAfterSeconds: 60,
		},
		err: errors.New("adapter returned HTTP 429"),
	}
	before := time.Now().UTC()
	outcome, err := dispatchClaimedAgentEmail(
		context.Background(), st, client, time.Second, claim,
	)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != store.AgentEmailOutboundQueued ||
		st.retry.ErrorCode != store.AgentEmailOutboundErrorProviderRateLimited ||
		st.retry.PreserveProviderBoundary ||
		st.retry.Provider != agentEmailOutboundProvider ||
		st.retry.RetryAt.Before(before.Add(59*time.Second)) ||
		st.retry.RetryAt.After(time.Now().UTC().Add(61*time.Second)) {
		t.Fatalf("outcome=%q retry=%#v", outcome, st.retry)
	}
	if !equalStrings(st.actions, []string{"start", "send", "retry"}) {
		t.Fatalf("actions = %#v", st.actions)
	}
}

func TestDispatchClaimedAgentEmailReplaysDurableReceiptAfterLostSettlement(t *testing.T) {
	claim := validAgentEmailOutboundClaim()
	claim.Message.State = store.AgentEmailOutboundProviderStarted
	st := &fakeAgentEmailOutboundWorkerStore{
		claims:   []store.AgentEmailOutboundDispatch{claim},
		startErr: store.ErrAgentEmailOutboundProviderAlreadyStarted,
	}
	client := &fakeAgentEmailOutboundDispatchClient{
		actions: &st.actions,
		response: agentemailoutbound.Response{
			SchemaVersion: agentemailoutbound.ResponseSchemaVersion,
			SendID:        claim.Message.ID, State: agentemailoutbound.StateAccepted,
			Provider: agentEmailOutboundProvider, ProviderMessageID: "provider-recovered",
		},
	}
	outcome, err := dispatchClaimedAgentEmail(
		context.Background(), st, client, time.Second, claim,
	)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != store.AgentEmailOutboundAccepted || client.calls != 1 ||
		st.finalized.ProviderMessageID != "provider-recovered" {
		t.Fatalf("outcome=%q calls=%d finalized=%#v", outcome, client.calls, st.finalized)
	}
	if !equalStrings(st.actions, []string{"start", "send", "finalize"}) {
		t.Fatalf("actions = %#v", st.actions)
	}
}

func TestDispatchClaimedAgentEmailNeverCallsProviderWhenGateCloses(t *testing.T) {
	st := &fakeAgentEmailOutboundWorkerStore{startErr: store.ErrAgentEmailSendDisabled}
	client := &fakeAgentEmailOutboundDispatchClient{actions: &st.actions}
	outcome, err := dispatchClaimedAgentEmail(
		context.Background(), st, client, time.Second, validAgentEmailOutboundClaim(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != store.AgentEmailOutboundCanceled || client.calls != 0 {
		t.Fatalf("outcome=%q provider calls=%d", outcome, client.calls)
	}
	if !equalStrings(st.actions, []string{"start"}) {
		t.Fatalf("actions = %#v", st.actions)
	}
}

func TestAgentEmailOutboundWorkerConfigBounds(t *testing.T) {
	valid := defaultAgentEmailOutboundWorkerConfig()
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*agentEmailOutboundWorkerConfig){
		"batch":            func(c *agentEmailOutboundWorkerConfig) { c.BatchSize = 101 },
		"interval":         func(c *agentEmailOutboundWorkerConfig) { c.Interval = 99 * time.Millisecond },
		"batch timeout":    func(c *agentEmailOutboundWorkerConfig) { c.BatchTimeout = 500 * time.Millisecond },
		"provider timeout": func(c *agentEmailOutboundWorkerConfig) { c.ProviderTimeout = 61 * time.Second },
		"settlement room":  func(c *agentEmailOutboundWorkerConfig) { c.ProviderTimeout = c.BatchTimeout },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := valid
			mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatalf("accepted invalid config %#v", cfg)
			}
		})
	}
}

type fakeAgentEmailOutboundWorkerStore struct {
	claims     []store.AgentEmailOutboundDispatch
	claimIndex int
	reconciled int64
	startErr   error
	actions    []string
	finalized  store.FinalizeAgentEmailOutboundInput
	retry      store.RetryAgentEmailOutboundInput
	ambiguous  store.AmbiguousAgentEmailOutboundInput
}

func (s *fakeAgentEmailOutboundWorkerStore) ClaimAgentEmailOutbound(
	context.Context,
	store.AgentEmailOutboundClaimInput,
) (store.AgentEmailOutboundDispatch, error) {
	s.actions = append(s.actions, "claim")
	if s.claimIndex >= len(s.claims) {
		return store.AgentEmailOutboundDispatch{}, store.ErrAgentEmailOutboundEmpty
	}
	claim := s.claims[s.claimIndex]
	s.claimIndex++
	return claim, nil
}

func (s *fakeAgentEmailOutboundWorkerStore) StartAgentEmailOutboundProviderCall(
	_ context.Context,
	_ string,
	_ store.AgentEmailOutboundClaimFence,
) (store.AgentEmailOutboundDispatch, error) {
	s.actions = append(s.actions, "start")
	if s.startErr != nil {
		if len(s.claims) > 0 {
			return s.claims[max(0, s.claimIndex-1)], s.startErr
		}
		return validAgentEmailOutboundClaim(), s.startErr
	}
	if len(s.claims) > 0 {
		return s.claims[max(0, s.claimIndex-1)], nil
	}
	return validAgentEmailOutboundClaim(), nil
}

func (s *fakeAgentEmailOutboundWorkerStore) FinalizeAgentEmailOutbound(
	_ context.Context,
	_ string,
	in store.FinalizeAgentEmailOutboundInput,
) (store.AgentEmailOutboundMessage, error) {
	s.actions = append(s.actions, "finalize")
	s.finalized = in
	return store.AgentEmailOutboundMessage{State: in.State}, nil
}

func (s *fakeAgentEmailOutboundWorkerStore) RetryAgentEmailOutbound(
	_ context.Context,
	_ string,
	in store.RetryAgentEmailOutboundInput,
) (store.AgentEmailOutboundMessage, error) {
	s.actions = append(s.actions, "retry")
	s.retry = in
	state := store.AgentEmailOutboundQueued
	if in.PreserveProviderBoundary {
		state = store.AgentEmailOutboundProviderStarted
	}
	return store.AgentEmailOutboundMessage{State: state}, nil
}

func (s *fakeAgentEmailOutboundWorkerStore) MarkAgentEmailOutboundAmbiguous(
	_ context.Context,
	_ string,
	in store.AmbiguousAgentEmailOutboundInput,
) (store.AgentEmailOutboundMessage, error) {
	s.actions = append(s.actions, "ambiguous")
	s.ambiguous = in
	return store.AgentEmailOutboundMessage{State: store.AgentEmailOutboundAmbiguous}, nil
}

func (s *fakeAgentEmailOutboundWorkerStore) ReconcileExhaustedAgentEmailOutbound(
	context.Context,
	int,
) (int64, error) {
	s.actions = append(s.actions, "reconcile")
	return s.reconciled, nil
}

type fakeAgentEmailOutboundDispatchClient struct {
	actions  *[]string
	response agentemailoutbound.Response
	err      error
	dispatch agentemailoutbound.Dispatch
	calls    int
}

func (c *fakeAgentEmailOutboundDispatchClient) Send(
	_ context.Context,
	dispatch agentemailoutbound.Dispatch,
) (agentemailoutbound.Response, error) {
	*c.actions = append(*c.actions, "send")
	c.calls++
	c.dispatch = dispatch
	return c.response, c.err
}

func validAgentEmailOutboundClaim() store.AgentEmailOutboundDispatch {
	return store.AgentEmailOutboundDispatch{
		Message: store.AgentEmailOutboundMessage{
			ID:             "esnd_test",
			AccountID:      "acc_test",
			RealmID:        "realm_test",
			OwnerAgentID:   "agent_test",
			FromAddress:    "scott.default@send.witmail.net",
			ReplyToAddress: "scott.default@witmail.net",
			ToAddress:      "recipient@example.com",
			Subject:        "Hello",
		},
		Text: "hello from the agent",
		Claim: store.AgentEmailOutboundClaimFence{
			ClaimID: "escl_test", Generation: 1,
		},
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
