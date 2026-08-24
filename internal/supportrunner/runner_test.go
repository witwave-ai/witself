package supportrunner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestRunnerReplyPathAndOptionalRetriage(t *testing.T) {
	thread := runnerThread("tkt_1", "acc_1", []ticketMessage{{
		ID: "tkm_1", AuthorKind: authorKindOwner, Body: "How do I configure the CLI?",
	}})
	api := newFakeTicketAPI(thread)
	model := &fakeLLM{result: decision{
		Action:    decisionActionReply,
		ReplyBody: "  Use `witself integrations --verify`.  ",
		Retriage: retriage{
			Category: ticketCategoryTechnical,
			Priority: ticketPriorityHigh,
		},
	}}
	var failures []string
	runner := newRunner(testRunnerConfig(), api, model, func(ticketID string) {
		failures = append(failures, ticketID)
	})
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	runner.now = func() time.Time { return now }

	if err := runner.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if model.calls != 1 || len(api.replies) != 1 || len(api.retriages) != 1 {
		t.Fatalf("calls: llm=%d replies=%d retriages=%d", model.calls, len(api.replies), len(api.retriages))
	}
	if api.replies[0].body != "Use `witself integrations --verify`." {
		t.Fatalf("reply body = %q", api.replies[0].body)
	}
	wantRetriage := retriage{Category: ticketCategoryTechnical, Priority: ticketPriorityHigh}
	if api.retriages[0].change != wantRetriage {
		t.Fatalf("retriage = %+v, want %+v", api.retriages[0].change, wantRetriage)
	}
	if len(api.listOptions) != 1 {
		t.Fatalf("list calls = %d", len(api.listOptions))
	}
	opts := api.listOptions[0]
	if fmt.Sprint(opts.States) != fmt.Sprint([]string{ticketStateOpen, ticketStateAwaitingAdmin}) ||
		opts.Limit != ticketListLimit ||
		!opts.Since.Equal(now.Add(-testRunnerConfig().Lookback)) {
		t.Fatalf("list options = %+v", opts)
	}
	if len(failures) != 0 || runner.FailureCount() != 0 {
		t.Fatalf("failures = %v / %d", failures, runner.FailureCount())
	}
}

func TestNewRejectsDisabledConfigBeforeDependencyConstruction(t *testing.T) {
	cfg, err := FromEnv(mapLookup(map[string]string{
		adminTokenFileEnv:      "/must/not/be-read",
		anthropicAPIKeyFileEnv: "/must/not-be-read-either",
	}))
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if _, err := New(cfg, nil); !errors.Is(err, ErrDisabled) {
		t.Fatalf("New error = %v, want ErrDisabled", err)
	}
}

func TestRunnerRunCancelsWithoutTimerDrainRace(t *testing.T) {
	config := testRunnerConfig()
	config.Interval = time.Microsecond
	api := newFakeTicketAPI()
	api.listCalled = make(chan struct{})
	runner := newRunner(config, api, &fakeLLM{forbidden: true, t: t}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()

	select {
	case <-api.listCalled:
	case <-time.After(time.Second):
		t.Fatal("Run did not perform its immediate tick")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run hung while canceling near a timer tick")
	}
}

func TestRunnerRunCountsIncompleteListWithoutMutating(t *testing.T) {
	config := testRunnerConfig()
	config.Interval = time.Hour
	api := newFakeTicketAPI()
	api.listErr = errors.New("incomplete fleet snapshot")
	model := &fakeLLM{forbidden: true, t: t}
	failure := make(chan string, 1)
	runner := newRunner(config, api, model, func(ticketID string) {
		failure <- ticketID
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()

	select {
	case ticketID := <-failure:
		if ticketID != "" {
			t.Fatalf("list failure log key = %q, want value-free empty ticket id", ticketID)
		}
	case <-time.After(time.Second):
		t.Fatal("list failure was not counted")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not stop after list failure")
	}
	if runner.FailureCount() != 1 {
		t.Fatalf("failure count = %d, want 1", runner.FailureCount())
	}
	assertNoMutations(t, api)
}

func TestRunnerPermanentlySkipsFleetAdminThread(t *testing.T) {
	thread := runnerThread("tkt_1", "acc_1", []ticketMessage{
		{ID: "tkm_1", AuthorKind: authorKindFleetAdmin, Body: "A human owns this."},
		{ID: "tkm_2", AuthorKind: authorKindOwner, Body: "More detail."},
	})
	api := newFakeTicketAPI(thread)
	model := &fakeLLM{forbidden: true, t: t}
	runner := newRunner(testRunnerConfig(), api, model, nil)
	if err := runner.tick(context.Background()); err != nil {
		t.Fatalf("first tick: %v", err)
	}

	updated := runnerThread("tkt_1", "acc_1", []ticketMessage{
		{ID: "tkm_1", AuthorKind: authorKindFleetAdmin, Body: "A human owns this."},
		{ID: "tkm_2", AuthorKind: authorKindOwner, Body: "More detail."},
		{ID: "tkm_3", AuthorKind: authorKindOwner, Body: "Still waiting."},
	})
	api.tickets = []ticket{updated.Ticket}
	api.threads["tkt_1"] = []ticketThread{updated}
	if err := runner.tick(context.Background()); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if api.getCalls["tkt_1"] != 1 {
		t.Fatalf("Get calls = %d, permanent skip should avoid the second read", api.getCalls["tkt_1"])
	}
	if len(api.replies) != 0 || len(api.retriages) != 0 {
		t.Fatal("fleet-admin thread was mutated")
	}
}

func TestRunnerMaxAssistantRepliesGuard(t *testing.T) {
	thread := runnerThread("tkt_1", "acc_1", []ticketMessage{
		{ID: "tkm_1", AuthorKind: authorKindOwner, Body: "Question"},
		{ID: "tkm_2", AuthorKind: authorKindAssistant, Body: "Answer one"},
		{ID: "tkm_3", AuthorKind: authorKindAssistant, Body: "Answer two"},
		{ID: "tkm_4", AuthorKind: authorKindAssistant, Body: "Answer three"},
		{ID: "tkm_5", AuthorKind: authorKindOperator, Body: "Still stuck"},
	})
	api := newFakeTicketAPI(thread)
	model := &fakeLLM{forbidden: true, t: t}
	runner := newRunner(testRunnerConfig(), api, model, nil)
	if err := runner.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	assertNoMutations(t, api)
}

func TestRunnerFreshnessDrop(t *testing.T) {
	original := runnerThread("tkt_1", "acc_1", []ticketMessage{{
		ID: "tkm_1", AuthorKind: authorKindOwner, Body: "Original question",
	}})
	fresh := runnerThread("tkt_1", "acc_1", []ticketMessage{
		{ID: "tkm_1", AuthorKind: authorKindOwner, Body: "Original question"},
		{ID: "tkm_2", AuthorKind: authorKindOwner, Body: "Concurrent update"},
	})
	api := newFakeTicketAPI(original)
	api.threads["tkt_1"] = []ticketThread{original, fresh}
	model := &fakeLLM{result: decision{Action: decisionActionReply, ReplyBody: "Stale answer"}}
	runner := newRunner(testRunnerConfig(), api, model, nil)
	if err := runner.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if model.calls != 1 || api.getCalls["tkt_1"] != 2 {
		t.Fatalf("calls: llm=%d get=%d", model.calls, api.getCalls["tkt_1"])
	}
	assertNoMutations(t, api)
}

func TestRunnerFailuresAreFailSafeAndValueFree(t *testing.T) {
	valid := decision{Action: decisionActionReply, ReplyBody: "A bounded answer"}
	tests := []struct {
		name      string
		result    decision
		modelErr  error
		firstRead error
	}{
		{name: "API read", result: valid, firstRead: errors.New("read failed with customer content")},
		{name: "LLM API", result: valid, modelErr: errors.New("provider error with model output")},
		{name: "action", result: decision{Action: "close", ReplyBody: "bad"}},
		{name: "empty body", result: decision{Action: decisionActionReply, ReplyBody: " \n"}},
		{name: "oversized body", result: decision{Action: decisionActionReply, ReplyBody: strings.Repeat("x", maxReplyBodyBytes+1)}},
		{name: "category enum", result: decision{Action: decisionActionReply, ReplyBody: "ok", Retriage: retriage{Category: "payments"}}},
		{name: "priority enum", result: decision{Action: decisionActionReply, ReplyBody: "ok", Retriage: retriage{Priority: "critical"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			thread := runnerThread("tkt_safe", "acc_private", []ticketMessage{{
				ID: "tkm_secret", AuthorKind: authorKindOwner, Body: "private customer content",
			}})
			api := newFakeTicketAPI(thread)
			api.getErrors["tkt_safe"] = test.firstRead
			model := &fakeLLM{result: test.result, err: test.modelErr}
			var logs []string
			runner := newRunner(testRunnerConfig(), api, model, func(ticketID string) {
				logs = append(logs, ticketID)
			})
			if err := runner.tick(context.Background()); err != nil {
				t.Fatalf("tick: %v", err)
			}
			assertNoMutations(t, api)
			if runner.FailureCount() != 1 || fmt.Sprint(logs) != "[tkt_safe]" {
				t.Fatalf("failure count/log = %d/%v, want ticket id only", runner.FailureCount(), logs)
			}
		})
	}
}

func TestRunnerSecurityGateRetriageOnlyBeforeLLM(t *testing.T) {
	thread := runnerThread("tkt_security", "acc_1", []ticketMessage{{
		ID: "tkm_1", AuthorKind: authorKindOwner, Body: "Unusual activity",
	}})
	thread.Ticket.Category = ticketCategorySecurity
	api := newFakeTicketAPI(thread)
	model := &fakeLLM{forbidden: true, t: t}
	runner := newRunner(testRunnerConfig(), api, model, nil)
	if err := runner.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(api.replies) != 0 || len(api.retriages) != 1 {
		t.Fatalf("mutations: replies=%d retriages=%d", len(api.replies), len(api.retriages))
	}
	if api.retriages[0].change != (retriage{Priority: ticketPriorityUrgent}) {
		t.Fatalf("security re-triage = %+v", api.retriages[0].change)
	}
}

func TestRunnerGatedTicketNeverCallsLLMWhenRetriageFails(t *testing.T) {
	thread := runnerThread("tkt_security", "acc_1", []ticketMessage{{
		ID: "tkm_1", AuthorKind: authorKindOwner, Body: "I found a vulnerability",
	}})
	api := newFakeTicketAPI(thread)
	api.retriageErr = errors.New("retriage unavailable")
	model := &fakeLLM{forbidden: true, t: t}
	runner := newRunner(testRunnerConfig(), api, model, nil)
	if err := runner.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if runner.FailureCount() != 1 || len(api.replies) != 0 || len(api.retriages) != 1 {
		t.Fatalf("failure/mutations = %d replies=%d retriages=%d", runner.FailureCount(), len(api.replies), len(api.retriages))
	}
	api.retriageErr = nil
	if err := runner.tick(context.Background()); err != nil {
		t.Fatalf("retry tick: %v", err)
	}
	if len(api.retriages) != 2 {
		t.Fatalf("retriage calls = %d, failed gate suggestion was suppressed", len(api.retriages))
	}
}

func TestRunnerRetriesUnchangedTicketAfterFailSafeFailure(t *testing.T) {
	t.Run("LLM API", func(t *testing.T) {
		thread := runnerThread("tkt_1", "acc_1", []ticketMessage{{
			ID: "tkm_1", AuthorKind: authorKindOwner, Body: "ordinary question",
		}})
		api := newFakeTicketAPI(thread)
		model := &fakeLLM{
			result: decision{Action: decisionActionEscalate},
			errors: []error{errors.New("temporary model failure"), nil},
		}
		runner := newRunner(testRunnerConfig(), api, model, nil)
		for i := 0; i < 2; i++ {
			if err := runner.tick(context.Background()); err != nil {
				t.Fatalf("tick %d: %v", i+1, err)
			}
		}
		if model.calls != 2 || api.getCalls["tkt_1"] != 2 {
			t.Fatalf("retry calls: llm=%d get=%d", model.calls, api.getCalls["tkt_1"])
		}
	})

	t.Run("validation", func(t *testing.T) {
		thread := runnerThread("tkt_1", "acc_1", []ticketMessage{{
			ID: "tkm_1", AuthorKind: authorKindOwner, Body: "ordinary question",
		}})
		api := newFakeTicketAPI(thread)
		model := &fakeLLM{results: []decision{
			{Action: decisionActionReply, ReplyBody: ""},
			{Action: decisionActionEscalate},
		}}
		runner := newRunner(testRunnerConfig(), api, model, nil)
		for i := 0; i < 2; i++ {
			if err := runner.tick(context.Background()); err != nil {
				t.Fatalf("tick %d: %v", i+1, err)
			}
		}
		if model.calls != 2 {
			t.Fatalf("LLM calls = %d, validation failure suppressed retry", model.calls)
		}
	})

	t.Run("freshness API", func(t *testing.T) {
		thread := runnerThread("tkt_1", "acc_1", []ticketMessage{{
			ID: "tkm_1", AuthorKind: authorKindOwner, Body: "ordinary question",
		}})
		api := newFakeTicketAPI(thread)
		api.getErrorQueue["tkt_1"] = []error{
			nil, errors.New("temporary freshness read failure"), nil, nil,
		}
		model := &fakeLLM{result: decision{Action: decisionActionReply, ReplyBody: "answer"}}
		runner := newRunner(testRunnerConfig(), api, model, nil)
		for i := 0; i < 2; i++ {
			if err := runner.tick(context.Background()); err != nil {
				t.Fatalf("tick %d: %v", i+1, err)
			}
		}
		if model.calls != 2 || api.getCalls["tkt_1"] != 4 || len(api.replies) != 1 {
			t.Fatalf("retry calls: llm=%d get=%d replies=%d", model.calls, api.getCalls["tkt_1"], len(api.replies))
		}
	})
}

func TestRunnerTickCap(t *testing.T) {
	api := &fakeTicketAPI{
		threads:   make(map[string][]ticketThread),
		getCalls:  make(map[string]int),
		getErrors: make(map[string]error),
	}
	for i := 0; i < 7; i++ {
		id := fmt.Sprintf("tkt_%d", i)
		thread := runnerThread(id, "acc_1", []ticketMessage{{
			ID: fmt.Sprintf("tkm_%d", i), AuthorKind: authorKindOwner, Body: "ordinary question",
		}})
		api.tickets = append(api.tickets, thread.Ticket)
		api.threads[id] = []ticketThread{thread}
	}
	config := testRunnerConfig()
	config.MaxTicketsPerTick = 5
	model := &fakeLLM{result: decision{Action: decisionActionEscalate}}
	runner := newRunner(config, api, model, nil)
	if err := runner.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if model.calls != 5 || len(runner.lastSeen) != 5 {
		t.Fatalf("processed llm/seen = %d/%d, want cap 5", model.calls, len(runner.lastSeen))
	}
	assertNoMutations(t, api)
}

func TestRunnerScansPastUnchangedNewestTicketsWithinBoundedList(t *testing.T) {
	api := &fakeTicketAPI{
		threads:       make(map[string][]ticketThread),
		getCalls:      make(map[string]int),
		getErrors:     make(map[string]error),
		getErrorQueue: make(map[string][]error),
	}
	for i := 0; i < 7; i++ {
		id := fmt.Sprintf("tkt_%d", i)
		thread := runnerThread(id, "acc_1", []ticketMessage{{
			ID: fmt.Sprintf("tkm_%d", i), AuthorKind: authorKindOwner, Body: "ordinary question",
		}})
		api.tickets = append(api.tickets, thread.Ticket)
		api.threads[id] = []ticketThread{thread}
	}
	config := testRunnerConfig()
	config.MaxTicketsPerTick = 2
	model := &fakeLLM{result: decision{Action: decisionActionEscalate}}
	runner := newRunner(config, api, model, nil)
	for i := 0; i < 5; i++ {
		runner.lastSeen[api.tickets[i].ID] = api.tickets[i].LastMessageID
	}

	if err := runner.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(api.listOptions) != 1 || api.listOptions[0].Limit != ticketListLimit {
		t.Fatalf("list limit = %+v, want bounded CP maximum %d", api.listOptions, ticketListLimit)
	}
	if model.calls != 2 || api.getCalls["tkt_5"] != 1 || api.getCalls["tkt_6"] != 1 {
		t.Fatalf("older candidates starved: llm=%d gets=%v", model.calls, api.getCalls)
	}
}

func TestRunnerSuppressesUnchangedTicketLLM(t *testing.T) {
	thread := runnerThread("tkt_1", "acc_1", []ticketMessage{{
		ID: "tkm_1", AuthorKind: authorKindOwner, Body: "ordinary question",
	}})
	api := newFakeTicketAPI(thread)
	model := &fakeLLM{result: decision{Action: decisionActionEscalate}}
	runner := newRunner(testRunnerConfig(), api, model, nil)
	for i := 0; i < 2; i++ {
		if err := runner.tick(context.Background()); err != nil {
			t.Fatalf("tick %d: %v", i+1, err)
		}
	}
	if model.calls != 1 || api.getCalls["tkt_1"] != 1 {
		t.Fatalf("unchanged calls: llm=%d get=%d", model.calls, api.getCalls["tkt_1"])
	}
	assertNoMutations(t, api)
}

func TestRunnerModelEscalationIsSilent(t *testing.T) {
	thread := runnerThread("tkt_1", "acc_1", []ticketMessage{{
		ID: "tkm_1", AuthorKind: authorKindOwner, Body: "unclear but ordinary question",
	}})
	api := newFakeTicketAPI(thread)
	model := &fakeLLM{result: decision{
		Action:         decisionActionEscalate,
		ReplyBody:      "must not post",
		Retriage:       retriage{Category: ticketCategoryTechnical, Priority: ticketPriorityHigh},
		EscalateReason: "needs human judgment",
	}}
	runner := newRunner(testRunnerConfig(), api, model, nil)
	if err := runner.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	assertNoMutations(t, api)
}

type fakeTicketAPI struct {
	tickets       []ticket
	threads       map[string][]ticketThread
	getErrors     map[string]error
	getErrorQueue map[string][]error
	listErr       error
	replyErr      error
	retriageErr   error

	listOptions []ticketListOptions
	listCalled  chan struct{}
	getCalls    map[string]int
	replies     []fakeReply
	retriages   []fakeRetriage
}

type fakeReply struct {
	accountID string
	ticketID  string
	body      string
}

type fakeRetriage struct {
	accountID string
	ticketID  string
	change    retriage
}

func newFakeTicketAPI(threads ...ticketThread) *fakeTicketAPI {
	api := &fakeTicketAPI{
		threads:       make(map[string][]ticketThread),
		getCalls:      make(map[string]int),
		getErrors:     make(map[string]error),
		getErrorQueue: make(map[string][]error),
	}
	for _, thread := range threads {
		api.tickets = append(api.tickets, thread.Ticket)
		api.threads[thread.Ticket.ID] = []ticketThread{thread}
	}
	return api
}

func (a *fakeTicketAPI) List(_ context.Context, opts ticketListOptions) ([]ticket, error) {
	a.listOptions = append(a.listOptions, opts)
	if a.listCalled != nil {
		select {
		case <-a.listCalled:
		default:
			close(a.listCalled)
		}
	}
	return append([]ticket(nil), a.tickets...), a.listErr
}

func (a *fakeTicketAPI) Get(_ context.Context, _, ticketID string) (ticketThread, error) {
	a.getCalls[ticketID]++
	if queue := a.getErrorQueue[ticketID]; len(queue) > 0 {
		err := queue[0]
		a.getErrorQueue[ticketID] = queue[1:]
		if err != nil {
			return ticketThread{}, err
		}
	}
	if err := a.getErrors[ticketID]; err != nil {
		return ticketThread{}, err
	}
	queue := a.threads[ticketID]
	if len(queue) == 0 {
		return ticketThread{}, errors.New("missing fake ticket")
	}
	result := queue[0]
	if len(queue) > 1 {
		a.threads[ticketID] = queue[1:]
	}
	return result, nil
}

func (a *fakeTicketAPI) ReplyAsAssistant(_ context.Context, accountID, ticketID, body string) error {
	a.replies = append(a.replies, fakeReply{accountID: accountID, ticketID: ticketID, body: body})
	return a.replyErr
}

func (a *fakeTicketAPI) Retriage(_ context.Context, accountID, ticketID string, change retriage) error {
	a.retriages = append(a.retriages, fakeRetriage{accountID: accountID, ticketID: ticketID, change: change})
	return a.retriageErr
}

type fakeLLM struct {
	t         *testing.T
	forbidden bool
	result    decision
	err       error
	results   []decision
	errors    []error
	calls     int
}

func (m *fakeLLM) Decide(_ context.Context, _ ticketThread) (decision, error) {
	m.calls++
	if m.forbidden {
		m.t.Helper()
		m.t.Fatal("LLM called for a ticket that must be mechanically skipped")
	}
	result := m.result
	if len(m.results) > 0 {
		result = m.results[0]
		m.results = m.results[1:]
	}
	err := m.err
	if len(m.errors) > 0 {
		err = m.errors[0]
		m.errors = m.errors[1:]
	}
	return result, err
}

func testRunnerConfig() Config {
	return Config{
		Enabled:             true,
		ControlPlane:        defaultControlPlane,
		Model:               defaultModel,
		Interval:            defaultInterval,
		MaxTicketsPerTick:   defaultMaxTicketsPerTick,
		LLMTimeout:          defaultLLMTimeout,
		MaxAssistantReplies: defaultMaxAssistantReplies,
		Lookback:            defaultLookback,
		adminToken:          "admin",
		anthropicAPIKey:     "api-key",
	}
}

func runnerThread(ticketID, accountID string, messages []ticketMessage) ticketThread {
	lastID := ""
	if len(messages) > 0 {
		lastID = messages[len(messages)-1].ID
	}
	return ticketThread{
		Ticket: ticket{
			ID:            ticketID,
			AccountID:     accountID,
			Subject:       "Technical question",
			Category:      ticketCategoryTechnical,
			State:         ticketStateAwaitingAdmin,
			Priority:      ticketPriorityNormal,
			LastMessageID: lastID,
		},
		Messages: messages,
	}
}

func assertNoMutations(t *testing.T, api *fakeTicketAPI) {
	t.Helper()
	if len(api.replies) != 0 || len(api.retriages) != 0 {
		t.Fatalf("mutations: replies=%+v retriages=%+v", api.replies, api.retriages)
	}
}

// A fleet admin can resolve a ticket between the runner's context read and
// its freshness re-read WITHOUT changing last_message_id (state changes do
// not post messages). Only the state clause of the freshness fence catches
// that; this pins it in isolation so the clause cannot be simplified away.
func TestRunnerFreshnessDropOnStateOnlyChange(t *testing.T) {
	original := runnerThread("tkt_1", "acc_1", []ticketMessage{{
		ID: "tkm_1", AuthorKind: authorKindOwner, Body: "Original question",
	}})
	fresh := original
	fresh.Ticket.State = "resolved"
	api := newFakeTicketAPI(original)
	api.threads["tkt_1"] = []ticketThread{original, fresh}
	model := &fakeLLM{result: decision{Action: decisionActionReply, ReplyBody: "Stale answer"}}
	runner := newRunner(testRunnerConfig(), api, model, nil)
	if err := runner.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if model.calls != 1 || api.getCalls["tkt_1"] != 2 {
		t.Fatalf("calls: llm=%d get=%d", model.calls, api.getCalls["tkt_1"])
	}
	assertNoMutations(t, api)
}

// The same race can land before the FIRST read: the listing said actionable,
// but the thread arrives already resolved (state moved, no new message).
// threadIsConsistent's state clause must stop it before the LLM is consulted.
func TestRunnerSkipsThreadAlreadyIneligibleOnFirstRead(t *testing.T) {
	thread := runnerThread("tkt_1", "acc_1", []ticketMessage{{
		ID: "tkm_1", AuthorKind: authorKindOwner, Body: "Original question",
	}})
	thread.Ticket.State = "resolved"
	api := newFakeTicketAPI(thread)
	model := &fakeLLM{forbidden: true, t: t}
	runner := newRunner(testRunnerConfig(), api, model, nil)
	if err := runner.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	assertNoMutations(t, api)
}
