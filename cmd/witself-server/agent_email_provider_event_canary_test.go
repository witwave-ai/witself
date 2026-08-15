package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/store"
)

func TestParseAgentEmailProviderEventCanaryCommandArgs(t *testing.T) {
	args := []string{
		"--send-id", "esnd_bbbbbbbbbbbbbbbb",
		"--json",
		"--expected-accepted-at", "2026-08-15T12:34:56.123456Z",
		"--account-id", "acc_aaaaaaaaaaaaaaaa",
	}
	input, ok := parseAgentEmailProviderEventCanaryCommandArgs(args)
	if !ok || input.AccountID != "acc_aaaaaaaaaaaaaaaa" ||
		input.SendID != "esnd_bbbbbbbbbbbbbbbb" ||
		input.ExpectedAcceptedAt.Format(time.RFC3339Nano) != "2026-08-15T12:34:56.123456Z" {
		t.Fatalf("parsed provider-event canary = %#v / %t", input, ok)
	}
	for _, invalid := range [][]string{
		args[:len(args)-2],
		append(append([]string{}, args...), "--json"),
		{"--account-id", "acc_aaaaaaaaaaaaaaaa", "--send-id", "esnd_bbbbbbbbbbbbbbbb", "--expected-accepted-at", "2026-08-15T12:34:56+00:00"},
		{"--account-id", "acct_aaaaaaaaaaaaaaaa", "--send-id", "esnd_bbbbbbbbbbbbbbbb", "--expected-accepted-at", "2026-08-15T12:34:56Z", "--json"},
		{"--account-id", "acc_aaaaaaaaaaaaaaaa", "--send-id", "esnd_bbbbbbbbbbbbbbbb", "--expected-accepted-at", "2026-08-15T12:34:56Z", "--extra"},
	} {
		if _, ok := parseAgentEmailProviderEventCanaryCommandArgs(invalid); ok {
			t.Fatalf("invalid arguments accepted: %#v", invalid)
		}
	}
}

func TestAgentEmailProviderEventCanaryExercisesLocalhostReplayAndRedacts(t *testing.T) {
	acceptedAt := time.Date(2026, 8, 15, 12, 30, 0, 0, time.UTC)
	const (
		token      = "provider-event-token-private-0123456789"
		providerID = "private-provider-content-marker"
	)
	fake := &fakeAgentEmailProviderEventCanaryStore{
		target: store.AgentEmailProviderEventCanaryTarget{
			AccountID: "acc_aaaaaaaaaaaaaaaa", SendID: "esnd_bbbbbbbbbbbbbbbb",
			ProviderMessageID: providerID, AcceptedAt: acceptedAt,
		},
	}
	input := agentEmailProviderEventCanaryInput{
		AccountID: fake.target.AccountID, SendID: fake.target.SendID,
		ExpectedAcceptedAt: acceptedAt,
	}
	eventID := agentEmailProviderEventCanaryEventID(input)
	var stdout, stderr bytes.Buffer
	listens := 0
	listen := func(network, address string) (net.Listener, error) {
		listens++
		if network != "tcp4" || address != "127.0.0.1:0" {
			t.Fatalf("listen = %q %q", network, address)
		}
		return net.Listen(network, address)
	}
	code := runAgentEmailProviderEventCanaryWithDependencies(
		context.Background(),
		input, token, fake, &stdout, &stderr, listen,
	)
	if code != 0 || stderr.Len() != 0 || listens != 1 {
		t.Fatalf("canary exit=%d stderr=%q listens=%d", code, stderr.String(), listens)
	}
	wantOutput := `{"send_id":"esnd_bbbbbbbbbbbbbbbb","status":"passed","count":1,"state":"delivered"}` + "\n"
	if stdout.String() != wantOutput {
		t.Fatalf("canary output = %q, want %q", stdout.String(), wantOutput)
	}
	if fake.prepareCalls != 1 || fake.verifyCalls != 1 || len(fake.applied) != 3 {
		t.Fatalf("store calls prepare=%d apply=%d verify=%d",
			fake.prepareCalls, len(fake.applied), fake.verifyCalls)
	}
	if !reflect.DeepEqual(fake.applied[0], fake.applied[1]) ||
		fake.applied[0].EventClass != store.AgentEmailOutboundProviderEventDelivered ||
		fake.applied[2].EventClass != store.AgentEmailOutboundProviderEventDeferred {
		t.Fatalf("provider event sequence = %#v", fake.applied)
	}
	for _, event := range fake.applied {
		if event.EventID != eventID || event.ProviderMessageID != providerID ||
			event.Provider != store.AgentEmailOutboundCloudflareProvider ||
			!event.OccurredAt.Equal(acceptedAt) {
			t.Fatalf("provider event correlation changed: %#v", event)
		}
	}
	for _, private := range []string{token, eventID, providerID, "content-marker"} {
		if strings.Contains(stdout.String()+stderr.String(), private) {
			t.Fatalf("command output exposed private value %q", private)
		}
	}
}

func TestAgentEmailProviderEventCanaryFailsBeforeHTTPOnPreflightMismatch(t *testing.T) {
	const privateCause = "private-provider-id private-token private-content"
	fake := &fakeAgentEmailProviderEventCanaryStore{
		prepareErr: fmt.Errorf("%s: %w", privateCause, store.ErrAgentEmailProviderEventCanaryFence),
	}
	var stdout, stderr bytes.Buffer
	listens := 0
	code := runAgentEmailProviderEventCanaryWithDependencies(
		context.Background(),
		agentEmailProviderEventCanaryInput{
			AccountID: "acc_aaaaaaaaaaaaaaaa", SendID: "esnd_bbbbbbbbbbbbbbbb",
			ExpectedAcceptedAt: time.Date(2026, 8, 15, 12, 30, 0, 0, time.UTC),
		},
		"provider-event-token-private-0123456789", fake, &stdout, &stderr,
		func(string, string) (net.Listener, error) {
			listens++
			return nil, errors.New("must not listen")
		},
	)
	if code != 1 || stdout.Len() != 0 || listens != 0 ||
		fake.prepareCalls != 1 || len(fake.applied) != 0 || fake.verifyCalls != 0 {
		t.Fatalf("mismatch exit=%d stdout=%q stderr=%q listens=%d calls=%d/%d/%d",
			code, stdout.String(), stderr.String(), listens,
			fake.prepareCalls, len(fake.applied), fake.verifyCalls)
	}
	if stderr.String() != "witself-server: agent-email provider-event canary failed (reason=preflight_failed)\n" ||
		strings.Contains(stderr.String(), privateCause) {
		t.Fatalf("mismatch stderr = %q", stderr.String())
	}
}

func TestAgentEmailProviderEventCanaryConcurrentSameFenceConverges(t *testing.T) {
	acceptedAt := time.Date(2026, 8, 15, 12, 30, 0, 123456000, time.UTC)
	input := agentEmailProviderEventCanaryInput{
		AccountID: "acc_aaaaaaaaaaaaaaaa", SendID: "esnd_bbbbbbbbbbbbbbbb",
		ExpectedAcceptedAt: acceptedAt,
	}
	target := store.AgentEmailProviderEventCanaryTarget{
		AccountID: input.AccountID, SendID: input.SendID,
		ProviderMessageID: "private-provider-id", AcceptedAt: acceptedAt,
	}
	eventIDA := agentEmailProviderEventCanaryEventID(input)
	exactA, changedA, err := agentEmailProviderEventCanaryRequestBodies(input, target, eventIDA)
	if err != nil {
		t.Fatal(err)
	}
	eventIDB := agentEmailProviderEventCanaryEventID(input)
	exactB, changedB, err := agentEmailProviderEventCanaryRequestBodies(input, target, eventIDB)
	if err != nil {
		t.Fatal(err)
	}
	if eventIDA != eventIDB || !bytes.Equal(exactA, exactB) ||
		!bytes.Equal(changedA, changedB) {
		t.Fatal("same canary fence did not produce byte-identical requests")
	}
	const wantEventID = "witself-canary-c447c462beaa83c855de52a4194ce86f447c2c4ba4a6afe856a02c493d4764c8"
	if eventIDA != wantEventID {
		t.Fatalf("domain-separated event id = %q, want %q", eventIDA, wantEventID)
	}
	changedFence := input
	changedFence.ExpectedAcceptedAt = acceptedAt.Add(time.Microsecond)
	if agentEmailProviderEventCanaryEventID(changedFence) == eventIDA {
		t.Fatal("changed accepted-at fence reused the synthetic event id")
	}

	prepareEntered := make(chan struct{}, 2)
	prepareRelease := make(chan struct{})
	fake := &fakeAgentEmailProviderEventCanaryStore{
		target: target, prepareEntered: prepareEntered,
		prepareRelease: prepareRelease,
	}
	type outcome struct {
		code           int
		stdout, stderr string
	}
	outcomes := make(chan outcome, 2)
	const token = "provider-event-token-private-0123456789"
	for index := 0; index < 2; index++ {
		go func() {
			var stdout, stderr bytes.Buffer
			code := runAgentEmailProviderEventCanaryWithDependencies(
				context.Background(), input, token, fake, &stdout, &stderr, nil,
			)
			outcomes <- outcome{code: code, stdout: stdout.String(), stderr: stderr.String()}
		}()
	}
	for index := 0; index < 2; index++ {
		select {
		case <-prepareEntered:
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent canary did not reach preflight fence")
		}
	}
	close(prepareRelease)
	wantOutput := `{"send_id":"esnd_bbbbbbbbbbbbbbbb","status":"passed","count":1,"state":"delivered"}` + "\n"
	for index := 0; index < 2; index++ {
		select {
		case result := <-outcomes:
			if result.code != 0 || result.stdout != wantOutput || result.stderr != "" {
				t.Fatalf("concurrent canary = %#v", result)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("concurrent canary did not complete")
		}
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.prepareCalls != 2 || fake.verifyCalls != 2 ||
		fake.receiptCount != 1 || len(fake.applied) != 6 {
		t.Fatalf("concurrent store calls prepare=%d apply=%d verify=%d receipts=%d",
			fake.prepareCalls, len(fake.applied), fake.verifyCalls, fake.receiptCount)
	}
	for _, event := range fake.applied {
		if event.EventID != eventIDA || !event.OccurredAt.Equal(acceptedAt) {
			t.Fatalf("concurrent event fence changed: %#v", event)
		}
	}
}

func TestAgentEmailProviderEventCanaryRestartAfterCompletedRun(t *testing.T) {
	acceptedAt := time.Date(2026, 8, 15, 12, 30, 0, 123456000, time.UTC)
	input := agentEmailProviderEventCanaryInput{
		AccountID: "acc_aaaaaaaaaaaaaaaa", SendID: "esnd_bbbbbbbbbbbbbbbb",
		ExpectedAcceptedAt: acceptedAt,
	}
	fake := &fakeAgentEmailProviderEventCanaryStore{
		target: store.AgentEmailProviderEventCanaryTarget{
			AccountID: input.AccountID, SendID: input.SendID,
			ProviderMessageID: "private-provider-id", AcceptedAt: acceptedAt,
		},
	}
	const token = "provider-event-token-private-0123456789"
	wantOutput := `{"send_id":"esnd_bbbbbbbbbbbbbbbb","status":"passed","count":1,"state":"delivered"}` + "\n"
	for run := 1; run <= 2; run++ {
		var stdout, stderr bytes.Buffer
		code := runAgentEmailProviderEventCanaryWithDependencies(
			context.Background(), input, token, fake, &stdout, &stderr, nil,
		)
		if code != 0 || stdout.String() != wantOutput || stderr.Len() != 0 {
			t.Fatalf("canary run %d exit=%d stdout=%q stderr=%q",
				run, code, stdout.String(), stderr.String())
		}
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.prepareCalls != 2 || fake.verifyCalls != 2 ||
		fake.receiptCount != 1 || len(fake.applied) != 6 {
		t.Fatalf("restart store calls prepare=%d apply=%d verify=%d receipts=%d",
			fake.prepareCalls, len(fake.applied), fake.verifyCalls, fake.receiptCount)
	}
	eventID := agentEmailProviderEventCanaryEventID(input)
	for _, preparedEventID := range fake.preparedEventIDs {
		if preparedEventID != eventID {
			t.Fatalf("restart preflight event id = %q", preparedEventID)
		}
	}
}

func TestAgentEmailProviderEventCanarySecondPreflightAfterFirstPOST(t *testing.T) {
	acceptedAt := time.Date(2026, 8, 15, 12, 30, 0, 123456000, time.UTC)
	input := agentEmailProviderEventCanaryInput{
		AccountID: "acc_aaaaaaaaaaaaaaaa", SendID: "esnd_bbbbbbbbbbbbbbbb",
		ExpectedAcceptedAt: acceptedAt,
	}
	prepareEntered := make(chan struct{})
	firstApplyPersisted := make(chan struct{})
	firstApplyRelease := make(chan struct{})
	fake := &fakeAgentEmailProviderEventCanaryStore{
		target: store.AgentEmailProviderEventCanaryTarget{
			AccountID: input.AccountID, SendID: input.SendID,
			ProviderMessageID: "private-provider-id", AcceptedAt: acceptedAt,
		},
		prepareEntered:      prepareEntered,
		firstApplyPersisted: firstApplyPersisted,
		firstApplyRelease:   firstApplyRelease,
	}
	type outcome struct {
		code           int
		stdout, stderr string
	}
	outcomes := make(chan outcome, 2)
	run := func() {
		var stdout, stderr bytes.Buffer
		code := runAgentEmailProviderEventCanaryWithDependencies(
			context.Background(), input,
			"provider-event-token-private-0123456789",
			fake, &stdout, &stderr, nil,
		)
		outcomes <- outcome{code: code, stdout: stdout.String(), stderr: stderr.String()}
	}
	go run()
	select {
	case <-prepareEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("first canary did not complete preflight")
	}
	select {
	case <-firstApplyPersisted:
	case <-time.After(5 * time.Second):
		t.Fatal("first canary did not persist its exact POST")
	}
	go run()
	select {
	case <-prepareEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("second canary did not preflight the completed receipt")
	}
	close(firstApplyRelease)
	wantOutput := `{"send_id":"esnd_bbbbbbbbbbbbbbbb","status":"passed","count":1,"state":"delivered"}` + "\n"
	for index := 0; index < 2; index++ {
		select {
		case result := <-outcomes:
			if result.code != 0 || result.stdout != wantOutput || result.stderr != "" {
				t.Fatalf("staggered canary = %#v", result)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("staggered canary did not complete")
		}
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.prepareCalls != 2 || fake.verifyCalls != 2 ||
		fake.receiptCount != 1 || len(fake.applied) != 6 {
		t.Fatalf("staggered store calls prepare=%d apply=%d verify=%d receipts=%d",
			fake.prepareCalls, len(fake.applied), fake.verifyCalls, fake.receiptCount)
	}
}

func TestAgentEmailProviderEventCanaryHTTPClientRefusesRedirects(t *testing.T) {
	redirectTargetCalls := 0
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirectTargetCalls++
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, &http.Request{}, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	client := newAgentEmailProviderEventCanaryHTTPClient()
	defer client.CloseIdleConnections()
	if _, err := postAgentEmailProviderEventCanary(
		context.Background(), client, redirect.URL,
		"provider-event-token-private-0123456789", []byte(`{}`),
	); err == nil {
		t.Fatal("redirect was followed or accepted")
	}
	if redirectTargetCalls != 0 {
		t.Fatalf("redirect target calls = %d", redirectTargetCalls)
	}
}

type fakeAgentEmailProviderEventCanaryStore struct {
	mu                  sync.Mutex
	target              store.AgentEmailProviderEventCanaryTarget
	prepareErr          error
	prepareCalls        int
	verifyCalls         int
	receiptCount        int
	preparedEventIDs    []string
	applied             []store.AgentEmailOutboundProviderEventInput
	first               *store.AgentEmailOutboundProviderEventInput
	prepareEntered      chan<- struct{}
	prepareRelease      <-chan struct{}
	firstApplyPersisted chan<- struct{}
	firstApplyRelease   <-chan struct{}
}

func (f *fakeAgentEmailProviderEventCanaryStore) PrepareAgentEmailProviderEventCanary(
	_ context.Context, accountID, sendID string, acceptedAt time.Time, eventID string,
) (store.AgentEmailProviderEventCanaryTarget, error) {
	f.mu.Lock()
	f.prepareCalls++
	f.preparedEventIDs = append(f.preparedEventIDs, eventID)
	prepareErr := f.prepareErr
	target := f.target
	prepareEntered := f.prepareEntered
	prepareRelease := f.prepareRelease
	if prepareErr == nil && (accountID != target.AccountID || sendID != target.SendID ||
		!acceptedAt.Equal(target.AcceptedAt) ||
		eventID != agentEmailProviderEventCanaryEventID(agentEmailProviderEventCanaryInput{
			AccountID: accountID, SendID: sendID, ExpectedAcceptedAt: acceptedAt,
		})) {
		prepareErr = store.ErrAgentEmailProviderEventCanaryFence
	}
	if prepareErr == nil && f.first != nil &&
		(f.receiptCount != 1 || f.first.Provider != store.AgentEmailOutboundCloudflareProvider ||
			f.first.EventID != eventID || f.first.ProviderMessageID != target.ProviderMessageID ||
			f.first.EventClass != store.AgentEmailOutboundProviderEventDelivered ||
			!f.first.OccurredAt.Equal(acceptedAt)) {
		prepareErr = store.ErrAgentEmailProviderEventCanaryFence
	}
	f.mu.Unlock()
	if prepareEntered != nil {
		prepareEntered <- struct{}{}
	}
	if prepareRelease != nil {
		<-prepareRelease
	}
	if prepareErr != nil {
		return store.AgentEmailProviderEventCanaryTarget{}, prepareErr
	}
	return target, nil
}

func (f *fakeAgentEmailProviderEventCanaryStore) ApplyAgentEmailOutboundProviderEvent(
	_ context.Context, event store.AgentEmailOutboundProviderEventInput,
) (store.AgentEmailOutboundProviderEventResult, error) {
	f.mu.Lock()
	f.applied = append(f.applied, event)
	if f.first == nil {
		copy := event
		f.first = &copy
		f.receiptCount = 1
		persisted := f.firstApplyPersisted
		release := f.firstApplyRelease
		f.mu.Unlock()
		if persisted != nil {
			persisted <- struct{}{}
		}
		if release != nil {
			<-release
		}
		return store.AgentEmailOutboundProviderEventResult{Applied: true}, nil
	}
	if reflect.DeepEqual(*f.first, event) {
		f.mu.Unlock()
		return store.AgentEmailOutboundProviderEventResult{Applied: false}, nil
	}
	if f.first.EventID == event.EventID {
		f.mu.Unlock()
		return store.AgentEmailOutboundProviderEventResult{}, store.ErrAgentEmailOutboundConflict
	}
	f.mu.Unlock()
	return store.AgentEmailOutboundProviderEventResult{}, errors.New("unexpected event")
}

func (f *fakeAgentEmailProviderEventCanaryStore) VerifyAgentEmailProviderEventCanary(
	_ context.Context, target store.AgentEmailProviderEventCanaryTarget, eventID string,
) (store.AgentEmailProviderEventCanaryVerification, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.verifyCalls++
	if target != f.target || f.first == nil || eventID != f.first.EventID {
		return store.AgentEmailProviderEventCanaryVerification{}, store.ErrAgentEmailProviderEventCanaryFence
	}
	return store.AgentEmailProviderEventCanaryVerification{
		SendID: target.SendID, State: store.AgentEmailOutboundDelivered,
		ProviderEventReceiptCount: 1, EmailSentUsageEventCount: 1,
	}, nil
}
