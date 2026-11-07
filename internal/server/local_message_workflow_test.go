package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/client"
)

// This bounded script models callback outcomes, not the production store. Its
// policy is synthetic: messaging enabled, two candidates, one assignee, thirty
// second leases and at most sixteen remembered writes. It proposes no defaults.
// Neither its mutex nor its retained state proves database arbitration/durability.
const localMessageLease = 30

var localMessagePrincipals = map[string]DomainPrincipal{
	"local-a":        {Kind: PrincipalKindAgent, AccountStatus: "active", ID: "agent-a", AccountID: "local-account", RealmID: "local-realm"},
	"local-b":        {Kind: PrincipalKindAgent, AccountStatus: "active", ID: "agent-b", AccountID: "local-account", RealmID: "local-realm"},
	"local-c":        {Kind: PrincipalKindAgent, AccountStatus: "active", ID: "agent-c", AccountID: "local-account", RealmID: "local-realm"},
	"local-outsider": {Kind: PrincipalKindAgent, AccountStatus: "active", ID: "outsider", AccountID: "other-account", RealmID: "other-realm"},
	"local-operator": {Kind: PrincipalKindOperator, AccountStatus: "active", ID: "operator", AccountID: "local-account"},
}

type localMessageStep struct {
	op, token, id string
	in, out       any
	err           error
	remember      bool
}

type localMessageMemo struct{ in, out any }

type localMessageState struct {
	mu          sync.Mutex
	now         time.Time
	steps       []localMessageStep
	calls       []string
	transitions map[string]int
	memo        map[string]localMessageMemo
	message     Message
	graph       MessageRequestDetail
	result      Message
	processing  MessageProcessing
	claim       MessageRequestClaim
	// Only contention uses these channels, before acquiring the state mutex.
	entered     chan struct{}
	admit       chan struct{}
	contendedOp string
	attempts    map[string]bool
}

type localMessageWire struct {
	method, path, token, key string
	body                     []byte
	status                   int
	response                 []byte
}

type localMessageTransport struct {
	t         *testing.T
	mu        sync.Mutex
	base      *http.Transport
	allowed   map[string]bool
	wires     []localMessageWire
	forbidden int
	losePath  string
}

func (tr *localMessageTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	tr.mu.Lock()
	allowed := tr.allowed[r.URL.Scheme+"://"+r.URL.Host]
	if !allowed {
		tr.forbidden++
		tr.mu.Unlock()
		return nil, errors.New("fixture refused outbound destination")
	}
	tr.mu.Unlock()
	var body []byte
	if r.Body != nil {
		var err error
		body, err = io.ReadAll(r.Body)
		if err != nil {
			return nil, errors.New("fixture could not read request")
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	resp, err := tr.base.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, errors.New("fixture could not read response")
	}
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	if resp.Header.Get("Cache-Control") != "private, no-store" {
		tr.t.Error("message response lost private no-store")
	}
	tr.mu.Lock()
	tr.wires = append(tr.wires, localMessageWire{r.Method, r.URL.RequestURI(), r.Header.Get("Authorization"), r.Header.Get("Idempotency-Key"), body, resp.StatusCode, raw})
	lose := tr.losePath != "" && r.URL.Path == tr.losePath
	if lose {
		tr.losePath = ""
	}
	tr.mu.Unlock()
	if lose {
		_ = resp.Body.Close()
		return nil, errors.New("fixture interrupted committed response")
	}
	return resp, nil
}

func newLocalMessageTransport(t *testing.T) *localMessageTransport {
	t.Helper()
	old := http.DefaultTransport
	tr := &localMessageTransport{t: t, base: &http.Transport{Proxy: nil}, allowed: make(map[string]bool)}
	http.DefaultTransport = tr
	t.Cleanup(func() {
		http.DefaultTransport = old
		tr.base.CloseIdleConnections()
		tr.mu.Lock()
		defer tr.mu.Unlock()
		if tr.forbidden != 0 {
			t.Error("workflow attempted forbidden outbound traffic")
		}
	})
	return tr
}

type localMessageFixture struct {
	t         *testing.T
	state     *localMessageState
	transport *localMessageTransport
	server    *httptest.Server
}

func newLocalMessageFixture(t *testing.T) *localMessageFixture {
	t.Helper()
	s := &localMessageState{now: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC), transitions: make(map[string]int), memo: make(map[string]localMessageMemo), attempts: make(map[string]bool)}
	until := s.now.Add(localMessageLease * time.Second)
	s.processing = MessageProcessing{State: "claimed", ClaimID: "ordinary-fence", Generation: 7, LeaseExpiresAt: &until}
	a := MessageAgent{Kind: "agent", AgentID: localMessagePrincipals["local-a"].ID}
	b := MessageAgent{Kind: "agent", AgentID: localMessagePrincipals["local-b"].ID}
	s.message = Message{ID: "ordinary-message", AccountID: localMessagePrincipals["local-a"].AccountID, RealmID: localMessagePrincipals["local-a"].RealmID, From: a, To: MessageRecipient{Kind: "agent", AgentID: b.AgentID}, Kind: "request", Body: "bounded ordinary content", Payload: json.RawMessage(`{"step":1}`), ThreadID: "ordinary-thread", Processing: s.processing}
	opening := s.message
	opening.ID = "opening-message"
	opening.Kind = "open_request"
	opening.ThreadID = opening.ID
	request := MessageRequest{ID: "delegation", AccountID: s.message.AccountID, RealmID: s.message.RealmID, Coordinator: a, OpeningMessageID: opening.ID, SelectionPolicy: "client_ranked", MaxAssignees: 1, CandidateCount: 2, State: "open"}
	s.claim = MessageRequestClaim{RequestID: request.ID, ClaimID: "request-fence", SelectionID: "selection", Agent: b, Generation: 11, State: "claimed", LeaseExpiresAt: &until}
	s.graph = MessageRequestDetail{Request: request, OpeningMessage: opening, Candidates: []MessageRequestCandidate{{Agent: b}, {Agent: MessageAgent{Kind: "agent", AgentID: localMessagePrincipals["local-c"].ID}}}}
	s.result = s.message
	s.result.ID = "result-message"
	s.result.Kind = "result"
	s.result.From = b
	s.result.To.AgentID = a.AgentID
	s.result.ReplyToMessageID = opening.ID
	s.result.ThreadID = opening.ThreadID
	f := &localMessageFixture{t: t, state: s, transport: newLocalMessageTransport(t)}
	f.start()
	t.Cleanup(func() {
		f.server.Close()
		s.mu.Lock()
		defer s.mu.Unlock()
		if len(s.steps) != 0 {
			t.Error("script left required callbacks uncalled")
		}
	})
	return f
}

// clone prevents handler projection from mutating retained callback records.
func localMessageClone[T any](in T) T {
	raw, _ := json.Marshal(in)
	var out T
	_ = json.Unmarshal(raw, &out)
	return out
}

func (f *localMessageFixture) start() {
	cfg := Config{AuthenticatePrincipal: func(_ context.Context, token string) (DomainPrincipal, bool, error) {
		p, ok := localMessagePrincipals[token]
		return p, ok, nil
	}}
	cfg.SendMessage = func(_ context.Context, p DomainPrincipal, in SendMessageRequest) (Message, error) {
		return localMessageCall[Message](f, "send", p, "", in)
	}
	cfg.ListMessages = func(_ context.Context, p DomainPrincipal, in MessageListOptions) (MessagePage, error) {
		return localMessageCall[MessagePage](f, "list", p, "", in)
	}
	cfg.PeekMessage = func(_ context.Context, p DomainPrincipal, id string) (Message, error) {
		return localMessageCall[Message](f, "peek", p, id, nil)
	}
	cfg.ReadMessage = func(_ context.Context, p DomainPrincipal, id string) (Message, error) {
		return localMessageCall[Message](f, "read", p, id, nil)
	}
	cfg.AckMessage = func(_ context.Context, p DomainPrincipal, id string) (Message, error) {
		return localMessageCall[Message](f, "ack", p, id, nil)
	}
	cfg.ClaimMessage = func(_ context.Context, p DomainPrincipal, id string, in ClaimMessageRequest) (MessageProcessing, error) {
		return localMessageCall[MessageProcessing](f, "claim", p, id, in)
	}
	cfg.RenewMessageClaim = func(_ context.Context, p DomainPrincipal, id string, in RenewMessageClaimRequest) (MessageProcessing, error) {
		return localMessageCall[MessageProcessing](f, "renew", p, id, in)
	}
	cfg.ReleaseMessageClaim = func(_ context.Context, p DomainPrincipal, id string, in MessageClaimRequest) (MessageProcessing, error) {
		return localMessageCall[MessageProcessing](f, "release", p, id, in)
	}
	cfg.CompleteMessage = func(_ context.Context, p DomainPrincipal, id string, in CompleteMessageRequest) (CompleteMessageResult, error) {
		return localMessageCall[CompleteMessageResult](f, "complete", p, id, in)
	}
	cfg.CreateMessageRequest = func(_ context.Context, p DomainPrincipal, in CreateMessageRequestRequest) (CreateMessageRequestResult, error) {
		return localMessageCall[CreateMessageRequestResult](f, "open", p, "", in)
	}
	cfg.ListMessageRequests = func(_ context.Context, p DomainPrincipal, in MessageRequestListOptions) (MessageRequestPage, error) {
		return localMessageCall[MessageRequestPage](f, "request-list", p, "", in)
	}
	cfg.GetMessageRequest = func(_ context.Context, p DomainPrincipal, id string) (MessageRequestDetail, error) {
		return localMessageCall[MessageRequestDetail](f, "show", p, id, nil)
	}
	cfg.OfferMessageRequest = func(_ context.Context, p DomainPrincipal, id string, in OfferMessageRequestRequest) (OfferMessageRequestResult, error) {
		return localMessageCall[OfferMessageRequestResult](f, "offer", p, id, in)
	}
	cfg.SelectMessageRequest = func(_ context.Context, p DomainPrincipal, id string, in SelectMessageRequestRequest) (SelectMessageRequestResult, error) {
		return localMessageCall[SelectMessageRequestResult](f, "select", p, id, in)
	}
	cfg.ClaimMessageRequest = func(_ context.Context, p DomainPrincipal, id string, in ClaimMessageRequestRequest) (MessageRequestClaim, error) {
		return localMessageCall[MessageRequestClaim](f, "request-claim", p, id, in)
	}
	cfg.RenewMessageRequest = func(_ context.Context, p DomainPrincipal, id string, in RenewMessageRequestRequest) (MessageRequestClaim, error) {
		return localMessageCall[MessageRequestClaim](f, "request-renew", p, id, in)
	}
	cfg.ReleaseMessageRequest = func(_ context.Context, p DomainPrincipal, id string, in ReleaseMessageRequestRequest) (MessageRequestClaim, error) {
		return localMessageCall[MessageRequestClaim](f, "request-release", p, id, in)
	}
	cfg.CompleteMessageRequest = func(_ context.Context, p DomainPrincipal, id string, in CompleteMessageRequestRequest) (CompleteMessageRequestResult, error) {
		return localMessageCall[CompleteMessageRequestResult](f, "request-complete", p, id, in)
	}
	f.server = httptest.NewServer(apiMux(cfg))
	f.transport.mu.Lock()
	f.transport.allowed[f.server.URL] = true
	f.transport.mu.Unlock()
}

func localMessageCall[T any](f *localMessageFixture, op string, p DomainPrincipal, id string, in any) (T, error) {
	var zero T
	s := f.state
	s.mu.Lock()
	entered, admit, contended := s.entered, s.admit, s.contendedOp == op
	s.mu.Unlock()
	if contended {
		entered <- struct{}{}
		<-admit
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, op)
	if contended {
		return localMessageContended[T](f, op, p, id, in)
	}
	if len(s.steps) == 0 {
		f.t.Error("unexpected backend callback or automatic retry")
		return zero, ErrBadInput
	}
	step := s.steps[0]
	s.steps = s.steps[1:]
	if step.op != op || p != localMessagePrincipals[step.token] || id != step.id {
		f.t.Error("callback order or authenticated coordinates changed")
		return zero, ErrBadInput
	}
	expectedKey, actualKey := localMessageKey(step.in), localMessageKey(in)
	if expectedKey != actualKey {
		f.t.Error("callback lost original idempotency key")
		return zero, ErrBadInput
	}
	if !reflect.DeepEqual(step.in, in) {
		f.t.Error("callback body or exact live fence changed")
		return zero, ErrBadInput
	}
	if step.err != nil {
		return zero, step.err
	}
	out := step.out
	if step.remember {
		key := op + "/" + p.ID + "/" + actualKey
		if old, ok := s.memo[key]; ok {
			if !reflect.DeepEqual(old.in, in) {
				return zero, ErrConflict
			}
			out = old.out
		} else {
			if len(s.memo) >= 16 || actualKey == "" {
				f.t.Error("script exceeded declared write policy")
				return zero, ErrBadInput
			}
			s.memo[key] = localMessageMemo{in: in, out: out}
			s.transitions[op]++
			if completed, ok := out.(CompleteMessageRequestResult); ok {
				// Retain the scripted completed graph at the callback commit,
				// including when the transport subsequently loses its response.
				s.claim = localMessageClone(completed.Claim)
				s.graph.Request = localMessageClone(completed.Request)
				s.graph.Claims = []MessageRequestClaim{localMessageClone(completed.Claim)}
			}
		}
	}
	result, ok := out.(T)
	if !ok {
		f.t.Error("script result type mismatch")
		return zero, ErrBadInput
	}
	return localMessageClone(result), nil
}

func localMessageKey(in any) string {
	if in == nil {
		return ""
	}
	field := reflect.ValueOf(in).FieldByName("IdempotencyKey")
	if field.IsValid() {
		return field.String()
	}
	return ""
}

func (f *localMessageFixture) expect(op, token, id string, in, out any, remember bool) {
	f.state.mu.Lock()
	defer f.state.mu.Unlock()
	f.state.steps = append(f.state.steps, localMessageStep{op: op, token: token, id: id, in: in, out: out, remember: remember})
}
func (f *localMessageFixture) refuse(op, token, id string, in any, err error) {
	f.state.mu.Lock()
	defer f.state.mu.Unlock()
	f.state.steps = append(f.state.steps, localMessageStep{op: op, token: token, id: id, in: in, err: err})
}
func (f *localMessageFixture) status(want int) {
	f.t.Helper()
	f.transport.mu.Lock()
	defer f.transport.mu.Unlock()
	if len(f.transport.wires) == 0 || f.transport.wires[len(f.transport.wires)-1].status != want {
		f.t.Fatal("message API returned noncanonical status")
	}
}
func localMessageOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal("local message operation failed")
	}
}
func localMessageConflict(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, client.ErrConflict) {
		t.Fatal("claim refusal did not return typed client conflict")
	}
}
func localMessagePublic(t *testing.T, m client.Message) {
	t.Helper()
	if m.Processing.ClaimID != "" || m.Processing.LeaseExpiresAt != nil {
		t.Fatal("ordinary projection exposed processing fence")
	}
}
func localMessageMetadata(t *testing.T, messages []client.Message) {
	t.Helper()
	if len(messages) != 1 {
		t.Fatal("metadata page lost message")
	}
	if messages[0].Body != "" || len(messages[0].Payload) != 0 {
		t.Fatal("list must remain metadata-only despite callback content")
	}
	localMessagePublic(t, messages[0])
}
func (f *localMessageFixture) transitions(op string, want int) {
	f.t.Helper()
	f.state.mu.Lock()
	defer f.state.mu.Unlock()
	if f.state.transitions[op] != want {
		f.t.Fatal("replay created an extra fixture transition")
	}
}

func (f *localMessageFixture) send(body string) (client.Message, error) {
	in := SendMessageRequest{To: MessageRecipientRequest{Kind: "agent", ID: localMessagePrincipals["local-b"].ID}, Kind: "request", Body: body, Payload: f.state.message.Payload, IdempotencyKey: "send-key"}
	f.expect("send", "local-a", "", in, f.state.message, true)
	return client.SendMessage(f.t.Context(), f.server.URL, "local-a", client.SendMessageInput{To: in.To.ID, Kind: in.Kind, Body: in.Body, Payload: in.Payload, IdempotencyKey: in.IdempotencyKey})
}

func TestLocalMessageWorkflowOrdinary(t *testing.T) {
	f := newLocalMessageFixture(t)
	ctx, url, s := t.Context(), f.server.URL, f.state
	sent, err := f.send(s.message.Body)
	localMessageOK(t, err)
	f.status(http.StatusCreated)
	if sent.ID != s.message.ID {
		t.Fatal("send lost fixture message identity")
	}
	localMessagePublic(t, sent)
	again, err := f.send(s.message.Body)
	localMessageOK(t, err)
	if sent.ID != again.ID {
		t.Fatal("send replay changed identity")
	}
	_, err = f.send("changed ordinary content")
	localMessageConflict(t, err)
	f.status(http.StatusConflict)
	f.transitions("send", 1)
	f.expect("list", "local-b", "", MessageListOptions{Direction: "inbox", Limit: 1}, MessagePage{Messages: []Message{s.message}}, false)
	page, err := client.ListMessages(ctx, url, "local-b", client.MessageListOptions{Direction: "inbox", Limit: 1})
	localMessageOK(t, err)
	f.status(http.StatusOK)
	localMessageMetadata(t, page.Messages)
	zero := 0
	f.expect("list", "local-b", "", MessageListOptions{Direction: "inbox", Unacked: true, OldestFirst: true, Limit: 1}, MessagePage{Messages: []Message{s.message}}, false)
	listened, err := client.ListenMessages(ctx, url, "local-b", client.MessageListenOptions{WaitSeconds: &zero, Limit: 1})
	localMessageOK(t, err)
	f.status(http.StatusOK)
	localMessageMetadata(t, listened.Messages)
	if listened.TimedOut {
		t.Fatal("nonempty listen timed out")
	}
	if !reflect.DeepEqual(s.calls, []string{"send", "send", "send", "list", "list"}) {
		t.Fatal("metadata access invoked read or acknowledgment")
	}
	f.expect("peek", "local-b", sent.ID, nil, s.message, false)
	peeked, err := client.PeekMessage(ctx, url, "local-b", sent.ID)
	localMessageOK(t, err)
	f.status(http.StatusOK)
	if peeked.Body != s.message.Body || !bytes.Equal(peeked.Payload, s.message.Payload) {
		t.Fatal("explicit peek lost content")
	}
	localMessagePublic(t, peeked)
	if s.calls[len(s.calls)-1] != "peek" {
		t.Fatal("peek invoked read callback")
	}
	f.expect("read", "local-b", sent.ID, nil, s.message, false)
	read, err := client.ReadMessage(ctx, url, "local-b", sent.ID)
	localMessageOK(t, err)
	f.status(http.StatusOK)
	if read.Body != peeked.Body {
		t.Fatal("read lost explicit content")
	}
	localMessagePublic(t, read)
	f.refuse("claim", "local-outsider", sent.ID, ClaimMessageRequest{LeaseSeconds: localMessageLease, IdempotencyKey: "outsider-key"}, ErrForbidden)
	_, err = client.ClaimMessage(ctx, url, "local-outsider", sent.ID, client.ClaimMessageInput{LeaseSeconds: localMessageLease, IdempotencyKey: "outsider-key"})
	if !errors.Is(err, client.ErrNotFound) {
		t.Fatal("unauthorized processing revealed membership")
	}
	f.status(http.StatusNotFound)
	f.expect("claim", "local-b", sent.ID, ClaimMessageRequest{LeaseSeconds: localMessageLease, IdempotencyKey: "claim-one"}, s.processing, false)
	fence, err := client.ClaimMessage(ctx, url, "local-b", sent.ID, client.ClaimMessageInput{LeaseSeconds: localMessageLease, IdempotencyKey: "claim-one"})
	localMessageOK(t, err)
	f.status(http.StatusOK)
	if fence.ClaimID != s.processing.ClaimID || fence.Generation != s.processing.Generation || fence.LeaseExpiresAt == nil || !fence.LeaseExpiresAt.Equal(*s.processing.LeaseExpiresAt) {
		t.Fatal("claim lost exact authorized fence")
	}
	f.expect("renew", "local-b", sent.ID, RenewMessageClaimRequest{ClaimID: fence.ClaimID, Generation: fence.Generation, LeaseSeconds: localMessageLease}, s.processing, false)
	renewed, err := client.RenewMessageClaim(ctx, url, "local-b", sent.ID, client.RenewMessageClaimInput{ClaimID: fence.ClaimID, Generation: fence.Generation, LeaseSeconds: localMessageLease})
	localMessageOK(t, err)
	f.status(http.StatusOK)
	if !reflect.DeepEqual(renewed, fence) {
		t.Fatal("renew changed live fence")
	}
	released := s.processing
	released.State = "pending"
	f.expect("release", "local-b", sent.ID, MessageClaimRequest{ClaimID: fence.ClaimID, Generation: fence.Generation}, released, false)
	release, err := client.ReleaseMessageClaim(ctx, url, "local-b", sent.ID, client.MessageClaimInput{ClaimID: fence.ClaimID, Generation: fence.Generation})
	if err != nil || release.ClaimID != fence.ClaimID || release.Generation != fence.Generation {
		t.Fatal("positive release must carry exact nonzero live fence")
	}
	f.status(http.StatusOK)
	s.processing.Generation++
	f.expect("claim", "local-b", sent.ID, ClaimMessageRequest{LeaseSeconds: localMessageLease, IdempotencyKey: "claim-two"}, s.processing, false)
	reclaimed, err := client.ClaimMessage(ctx, url, "local-b", sent.ID, client.ClaimMessageInput{LeaseSeconds: localMessageLease, IdempotencyKey: "claim-two"})
	localMessageOK(t, err)
	if reclaimed.Generation != fence.Generation+1 {
		t.Fatal("reclaim lost successor generation")
	}
	result := s.result
	result.ThreadID = sent.ThreadID
	result.ReplyToMessageID = sent.ID
	completed := s.processing
	completed.State = "completed"
	completed.ResultMessageID = result.ID
	complete := CompleteMessageRequest{ClaimID: reclaimed.ClaimID, Generation: reclaimed.Generation, Body: "ordinary result", IdempotencyKey: "ordinary-complete"}
	var first client.CompleteMessageResult
	for i := 0; i < 2; i++ {
		f.expect("complete", "local-b", sent.ID, complete, CompleteMessageResult{Processing: completed, Message: result}, true)
		got, callErr := client.CompleteMessage(ctx, url, "local-b", sent.ID, client.CompleteMessageInput{ClaimID: complete.ClaimID, Generation: complete.Generation, Body: complete.Body, IdempotencyKey: complete.IdempotencyKey})
		localMessageOK(t, callErr)
		f.status(http.StatusCreated)
		localMessagePublic(t, got.Message)
		if got.Processing.ClaimID != reclaimed.ClaimID || got.Processing.Generation != reclaimed.Generation || got.Message.ReplyToMessageID != sent.ID || got.Message.ThreadID != sent.ThreadID || got.Processing.ResultMessageID != got.Message.ID {
			t.Fatal("ordinary completion lost fence or result linkage")
		}
		if i == 0 {
			first = got
		} else if !reflect.DeepEqual(first, got) {
			t.Fatal("ordinary completion replay changed original identity")
		}
	}
	changed := complete
	changed.Body = "changed result"
	f.expect("complete", "local-b", sent.ID, changed, CompleteMessageResult{}, true)
	_, err = client.CompleteMessage(ctx, url, "local-b", sent.ID, client.CompleteMessageInput{ClaimID: changed.ClaimID, Generation: changed.Generation, Body: changed.Body, IdempotencyKey: changed.IdempotencyKey})
	localMessageConflict(t, err)
	f.transitions("complete", 1)
	acked := s.message
	acked.ReadState.State = "acked"
	f.expect("ack", "local-b", sent.ID, nil, acked, false)
	ack, err := client.AckMessage(ctx, url, "local-b", sent.ID)
	localMessageOK(t, err)
	f.status(http.StatusOK)
	localMessagePublic(t, ack)
	if ack.ReadState.State != "acked" {
		t.Fatal("acknowledgment lost applicable message state")
	}
	wantOrder := []string{"send", "send", "send", "list", "list", "peek", "read", "claim", "claim", "renew", "release", "claim", "complete", "complete", "complete", "ack"}
	if !reflect.DeepEqual(s.calls, wantOrder) {
		t.Fatal("ordinary workflow changed explicit callback order")
	}
}

func (f *localMessageFixture) open(body string) (client.CreateMessageRequestResult, error) {
	in := CreateMessageRequestRequest{Body: body, SelectionPolicy: "client_ranked", MaxAssignees: 1, OfferWindowSeconds: 20, ExpiresInSeconds: 120, IdempotencyKey: "open-key"}
	f.expect("open", "local-a", "", in, CreateMessageRequestResult{Request: f.state.graph.Request, OpeningMessage: f.state.graph.OpeningMessage}, true)
	return client.CreateMessageRequest(f.t.Context(), f.server.URL, "local-a", client.CreateMessageRequestInput{Body: in.Body, SelectionPolicy: in.SelectionPolicy, MaxAssignees: in.MaxAssignees, OfferWindowSeconds: in.OfferWindowSeconds, ExpiresInSeconds: in.ExpiresInSeconds, IdempotencyKey: in.IdempotencyKey})
}

func (f *localMessageFixture) completeRequest(in CompleteMessageRequestRequest) (client.CompleteMessageRequestResult, error) {
	claim := f.state.claim
	claim.State = "completed"
	claim.ResultMessageID = f.state.result.ID
	completedRequest := f.state.graph.Request
	completedRequest.State = "completed"
	f.expect("request-complete", "local-b", f.state.graph.Request.ID, in, CompleteMessageRequestResult{Request: completedRequest, Claim: claim, Message: f.state.result}, true)
	return client.CompleteMessageRequest(f.t.Context(), f.server.URL, "local-b", f.state.graph.Request.ID, client.CompleteMessageRequestInput{ClaimID: in.ClaimID, Generation: in.Generation, Body: in.Body, IdempotencyKey: in.IdempotencyKey})
}

func TestLocalMessageWorkflowRequest(t *testing.T) {
	f := newLocalMessageFixture(t)
	s, ctx, url := f.state, t.Context(), f.server.URL
	opened, err := f.open(s.graph.OpeningMessage.Body)
	localMessageOK(t, err)
	f.status(http.StatusCreated)
	localMessagePublic(t, opened.OpeningMessage)
	if opened.Request.ID != s.graph.Request.ID || opened.Request.OpeningMessageID != opened.OpeningMessage.ID || opened.Request.Coordinator.AgentID != localMessagePrincipals["local-a"].ID {
		t.Fatal("request opening lost authenticated linkage")
	}
	replay, err := f.open(s.graph.OpeningMessage.Body)
	localMessageOK(t, err)
	if !reflect.DeepEqual(opened, replay) {
		t.Fatal("opening replay changed identities")
	}
	_, err = f.open("changed opening")
	localMessageConflict(t, err)
	f.transitions("open", 1)
	for _, token := range []string{"local-b", "local-c"} {
		offerMessage := s.graph.OpeningMessage
		offerMessage.ID = "offer-" + token
		offerMessage.Kind = "offer"
		offerMessage.From.AgentID = localMessagePrincipals[token].ID
		offerMessage.ReplyToMessageID = opened.OpeningMessage.ID
		offer := MessageRequestOffer{Agent: offerMessage.From, Message: offerMessage}
		input := OfferMessageRequestRequest{Body: "candidate offer", IdempotencyKey: "offer-" + token}
		var first client.OfferMessageRequestResult
		for i := 0; i < 2; i++ {
			f.expect("offer", token, opened.Request.ID, input, OfferMessageRequestResult{Request: s.graph.Request, Offer: offer}, true)
			got, offerErr := client.OfferMessageRequest(ctx, url, token, opened.Request.ID, client.OfferMessageRequestInput{Body: input.Body, IdempotencyKey: input.IdempotencyKey})
			localMessageOK(t, offerErr)
			f.status(http.StatusCreated)
			localMessagePublic(t, got.Offer.Message)
			if got.Offer.Agent.AgentID != localMessagePrincipals[token].ID || got.Offer.Message.ReplyToMessageID != opened.OpeningMessage.ID || got.Offer.Message.ThreadID != opened.OpeningMessage.ThreadID {
				t.Fatal("candidate offer lost graph linkage")
			}
			if i == 0 {
				first = got
			} else if !reflect.DeepEqual(first, got) {
				t.Fatal("offer replay changed identity")
			}
		}
		changed := input
		changed.Body = "changed offer"
		f.expect("offer", token, opened.Request.ID, changed, OfferMessageRequestResult{}, true)
		_, err = client.OfferMessageRequest(ctx, url, token, opened.Request.ID, client.OfferMessageRequestInput{Body: changed.Body, IdempotencyKey: changed.IdempotencyKey})
		localMessageConflict(t, err)
		s.graph.Offers = append(s.graph.Offers, offer)
	}
	f.transitions("offer", 2)
	selected := []string{localMessagePrincipals["local-b"].ID}
	selection := MessageRequestSelection{ID: s.claim.SelectionID, Generation: 1, Coordinator: s.graph.Request.Coordinator, SelectedAgentIDs: selected}
	s.graph.Selections = []MessageRequestSelection{selection}
	s.graph.Request.SelectedAgentIDs = selected
	selectInput := SelectMessageRequestRequest{SelectedAgentIDs: selected, ReservationSeconds: localMessageLease, IdempotencyKey: "select-key"}
	var firstSelection client.SelectMessageRequestResult
	for i := 0; i < 2; i++ {
		f.expect("select", "local-a", opened.Request.ID, selectInput, SelectMessageRequestResult{Request: s.graph.Request, Selection: selection, Claims: []MessageRequestClaim{s.claim}}, true)
		got, selectErr := client.SelectMessageRequest(ctx, url, "local-a", opened.Request.ID, client.SelectMessageRequestInput{SelectedAgentIDs: selected, ReservationSeconds: localMessageLease, IdempotencyKey: selectInput.IdempotencyKey})
		localMessageOK(t, selectErr)
		f.status(http.StatusCreated)
		if !reflect.DeepEqual(got.Selection.SelectedAgentIDs, selected) || len(got.Claims) != 1 || got.Claims[0].SelectionID != got.Selection.ID || got.Claims[0].RequestID != opened.Request.ID {
			t.Fatal("client selected list or reserved claim linkage changed")
		}
		if i == 0 {
			firstSelection = got
		} else if !reflect.DeepEqual(firstSelection, got) {
			t.Fatal("selection replay changed identity")
		}
	}
	changedSelection := selectInput
	changedSelection.SelectedAgentIDs = []string{localMessagePrincipals["local-c"].ID}
	f.expect("select", "local-a", opened.Request.ID, changedSelection, SelectMessageRequestResult{}, true)
	_, err = client.SelectMessageRequest(ctx, url, "local-a", opened.Request.ID, client.SelectMessageRequestInput{SelectedAgentIDs: changedSelection.SelectedAgentIDs, ReservationSeconds: localMessageLease, IdempotencyKey: changedSelection.IdempotencyKey})
	localMessageConflict(t, err)
	f.transitions("select", 1)
	f.expect("request-claim", "local-b", opened.Request.ID, ClaimMessageRequestRequest{LeaseSeconds: localMessageLease, IdempotencyKey: "request-claim-one"}, s.claim, false)
	claim, err := client.ClaimMessageRequest(ctx, url, "local-b", opened.Request.ID, client.ClaimMessageRequestInput{LeaseSeconds: localMessageLease, IdempotencyKey: "request-claim-one"})
	localMessageOK(t, err)
	f.status(http.StatusOK)
	if claim.ClaimID != s.claim.ClaimID || claim.Generation != s.claim.Generation || claim.LeaseExpiresAt == nil || !claim.LeaseExpiresAt.Equal(*s.claim.LeaseExpiresAt) {
		t.Fatal("request claim lost positive fence")
	}
	f.expect("request-renew", "local-b", opened.Request.ID, RenewMessageRequestRequest{ClaimID: claim.ClaimID, Generation: claim.Generation, LeaseSeconds: localMessageLease}, s.claim, false)
	renewed, err := client.RenewMessageRequest(ctx, url, "local-b", opened.Request.ID, client.RenewMessageRequestInput{ClaimID: claim.ClaimID, Generation: claim.Generation, LeaseSeconds: localMessageLease})
	localMessageOK(t, err)
	f.status(http.StatusOK)
	if !reflect.DeepEqual(renewed, claim) {
		t.Fatal("request renewal changed exact fence")
	}
	f.expect("request-release", "local-b", opened.Request.ID, ReleaseMessageRequestRequest{ClaimID: claim.ClaimID, Generation: claim.Generation}, s.claim, false)
	released, err := client.ReleaseMessageRequest(ctx, url, "local-b", opened.Request.ID, client.ReleaseMessageRequestInput{ClaimID: claim.ClaimID, Generation: claim.Generation})
	localMessageOK(t, err)
	f.status(http.StatusOK)
	if released.ClaimID != claim.ClaimID || released.Generation != claim.Generation {
		t.Fatal("request release lost exact fence")
	}
	s.claim.Generation++
	f.expect("request-claim", "local-b", opened.Request.ID, ClaimMessageRequestRequest{LeaseSeconds: localMessageLease, IdempotencyKey: "request-claim-two"}, s.claim, false)
	reclaimed, err := client.ClaimMessageRequest(ctx, url, "local-b", opened.Request.ID, client.ClaimMessageRequestInput{LeaseSeconds: localMessageLease, IdempotencyKey: "request-claim-two"})
	localMessageOK(t, err)
	if reclaimed.Generation != claim.Generation+1 {
		t.Fatal("request reclaim lost successor fence")
	}
	complete := CompleteMessageRequestRequest{ClaimID: reclaimed.ClaimID, Generation: reclaimed.Generation, Body: "selected result", IdempotencyKey: "request-complete"}
	result, err := f.completeRequest(complete)
	if err != nil || result.Claim.Generation != reclaimed.Generation || result.Claim.ClaimID != reclaimed.ClaimID || result.Claim.ResultMessageID != result.Message.ID || result.Message.ReplyToMessageID != opened.OpeningMessage.ID || result.Message.ThreadID != opened.OpeningMessage.ThreadID {
		t.Fatal("positive request completion lost exact nonzero fence or linked result")
	}
	f.status(http.StatusCreated)
	localMessagePublic(t, result.Message)
	repeated, err := f.completeRequest(complete)
	localMessageOK(t, err)
	if !reflect.DeepEqual(result, repeated) {
		t.Fatal("request completion replay changed identity")
	}
	changedComplete := complete
	changedComplete.Body = "changed selected result"
	_, err = f.completeRequest(changedComplete)
	localMessageConflict(t, err)
	f.transitions("request-complete", 1)
	s.graph.Claims = []MessageRequestClaim{localMessageClone(s.claim)}
	f.showAndList()
	// Protocol-linked records remain the store callback's classification duty.
	for _, msg := range []Message{s.graph.OpeningMessage, s.graph.Offers[0].Message, s.result} {
		in := ClaimMessageRequest{LeaseSeconds: localMessageLease, IdempotencyKey: "ordinary-protocol-claim"}
		f.refuse("claim", "local-b", msg.ID, in, ErrConflict)
		_, err = client.ClaimMessage(ctx, url, "local-b", msg.ID, client.ClaimMessageInput{LeaseSeconds: in.LeaseSeconds, IdempotencyKey: in.IdempotencyKey})
		localMessageConflict(t, err)
		f.status(http.StatusConflict)
	}
	f.rejectSpoofing()
}

func (f *localMessageFixture) showAndList() {
	f.t.Helper()
	s := f.state
	before := len(s.calls)
	f.expect("show", "local-a", s.graph.Request.ID, nil, s.graph, false)
	detail, err := client.GetMessageRequest(f.t.Context(), f.server.URL, "local-a", s.graph.Request.ID)
	localMessageOK(f.t, err)
	f.status(http.StatusOK)
	localMessagePublic(f.t, detail.OpeningMessage)
	if detail.Request.ID != s.graph.Request.ID || detail.Request.State != s.graph.Request.State || detail.Request.OpeningMessageID != detail.OpeningMessage.ID || detail.OpeningMessage.ID != s.graph.OpeningMessage.ID || len(detail.Offers) != len(s.graph.Offers) {
		f.t.Fatal("show lost retained request graph")
	}
	for i, offer := range detail.Offers {
		localMessagePublic(f.t, offer.Message)
		if offer.Message.ID != s.graph.Offers[i].Message.ID || offer.Message.ReplyToMessageID != detail.OpeningMessage.ID || offer.Message.ThreadID != detail.OpeningMessage.ThreadID {
			f.t.Fatal("show changed offer identity")
		}
	}
	if len(detail.Selections) != len(s.graph.Selections) || len(detail.Claims) != len(s.graph.Claims) {
		f.t.Fatal("request graph lost selection or claim records")
	}
	for i, selected := range detail.Selections {
		expected := s.graph.Selections[i]
		if selected.ID != expected.ID || selected.Generation != expected.Generation || selected.Coordinator.AgentID != expected.Coordinator.AgentID || !reflect.DeepEqual(selected.SelectedAgentIDs, expected.SelectedAgentIDs) {
			f.t.Fatal("request graph changed client selection coordinates")
		}
	}
	for i, claim := range detail.Claims {
		expected := s.graph.Claims[i]
		if claim.ClaimID != expected.ClaimID || claim.RequestID != detail.Request.ID || claim.SelectionID != expected.SelectionID || claim.Generation != expected.Generation || claim.Agent.AgentID != expected.Agent.AgentID || claim.State != expected.State || claim.ResultMessageID != expected.ResultMessageID {
			f.t.Fatal("request graph lost completed claim or result linkage")
		}
	}
	f.expect("request-list", "local-a", "", MessageRequestListOptions{Limit: 1}, MessageRequestPage{Requests: []MessageRequest{s.graph.Request}}, false)
	page, err := client.ListMessageRequests(f.t.Context(), f.server.URL, "local-a", client.MessageRequestListOptions{Limit: 1})
	localMessageOK(f.t, err)
	f.status(http.StatusOK)
	if len(page.Requests) != 1 || page.Requests[0].ID != detail.Request.ID {
		f.t.Fatal("request listing lost graph identity")
	}
	if !reflect.DeepEqual(s.calls[before:], []string{"show", "request-list"}) {
		f.t.Fatal("read-only request access selected or executed work")
	}
}

func (f *localMessageFixture) raw(token, path string, body []byte) (int, []byte) {
	f.t.Helper()
	req, err := http.NewRequestWithContext(f.t.Context(), http.MethodPost, f.server.URL+path, bytes.NewReader(body))
	localMessageOK(f.t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Transport: f.transport, Timeout: 5 * time.Second}).Do(req)
	localMessageOK(f.t, err)
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(response.Body)
	localMessageOK(f.t, err)
	return response.StatusCode, raw
}

func (f *localMessageFixture) rejectSpoofing() {
	f.t.Helper()
	before := len(f.state.calls)
	paths := []string{"/v1/message-requests", "/v1/message-requests/" + f.state.graph.Request.ID + ":offer", "/v1/message-requests/" + f.state.graph.Request.ID + ":select", "/v1/message-requests/" + f.state.graph.Request.ID + ":complete"}
	for _, path := range paths {
		for _, field := range []string{"account_id", "realm_id", "coordinator_agent_id"} {
			body, err := json.Marshal(map[string]any{field: "spoofed", "body": "untrusted input"})
			localMessageOK(f.t, err)
			status, _ := f.raw("local-a", path, body)
			if status != http.StatusBadRequest {
				f.t.Fatal("derived identity spoof reached callback boundary")
			}
		}
		status, _ := f.raw("local-operator", path, []byte(`{}`))
		if status != http.StatusForbidden {
			f.t.Fatal("non-agent principal reached request callback")
		}
	}
	if len(f.state.calls) != before {
		f.t.Fatal("authorization rejection invoked backend callback")
	}
}

// Only this callback model arbitrates the two arrivals. The test below checks
// real client/API propagation of its decision, not production unique ownership.
// The caller holds state.mu; both arrivals pass a channel barrier first.
func localMessageContended[T any](f *localMessageFixture, op string, p DomainPrincipal, id string, in any) (T, error) {
	var zero T
	s := f.state
	key := localMessageKey(in)
	lease := 0
	switch input := in.(type) {
	case ClaimMessageRequest:
		lease = input.LeaseSeconds
	case ClaimMessageRequestRequest:
		lease = input.LeaseSeconds
	}
	expectedID := s.message.ID
	if op == "request-claim" {
		expectedID = s.graph.Request.ID
	}
	if p != localMessagePrincipals["local-b"] || id != expectedID || lease != localMessageLease || (key != "attempt-one" && key != "attempt-two" && key != "after-expiry") {
		f.t.Error("contending callback coordinates changed")
		return zero, ErrBadInput
	}
	if s.attempts[key] {
		f.t.Error("client automatically retried contended claim")
		return zero, ErrBadInput
	}
	s.attempts[key] = true
	until := s.now.Add(localMessageLease * time.Second)
	var out any
	if op == "request-claim" {
		if s.claim.LeaseExpiresAt != nil && s.now.Before(*s.claim.LeaseExpiresAt) {
			return zero, ErrBusy
		}
		s.claim.Generation++
		s.claim.LeaseExpiresAt = &until
		out = s.claim
	} else {
		if s.processing.LeaseExpiresAt != nil && s.now.Before(*s.processing.LeaseExpiresAt) {
			return zero, ErrBusy
		}
		s.processing.Generation++
		s.processing.LeaseExpiresAt = &until
		out = s.processing
	}
	result, ok := out.(T)
	if !ok {
		f.t.Error("contending callback result type changed")
		return zero, ErrBadInput
	}
	return localMessageClone(result), nil
}

func TestLocalMessageWorkflowContentionAndExpiry(t *testing.T) {
	for _, request := range []bool{false, true} {
		name := "ordinary"
		if request {
			name = "request"
		}
		t.Run(name, func(t *testing.T) {
			f := newLocalMessageFixture(t)
			s := f.state
			ctx := t.Context()
			s.contendedOp = "claim"
			if request {
				s.contendedOp = "request-claim"
			}
			s.processing.LeaseExpiresAt = nil
			s.claim.LeaseExpiresAt = nil
			s.entered = make(chan struct{}, 2)
			s.admit = make(chan struct{})
			type outcome struct {
				fence client.MessageProcessing
				claim client.MessageRequestClaim
				err   error
			}
			acquire := func(key string) outcome {
				if request {
					claim, err := client.ClaimMessageRequest(ctx, f.server.URL, "local-b", s.graph.Request.ID, client.ClaimMessageRequestInput{LeaseSeconds: localMessageLease, IdempotencyKey: key})
					return outcome{claim: claim, err: err}
				}
				fence, err := client.ClaimMessage(ctx, f.server.URL, "local-b", s.message.ID, client.ClaimMessageInput{LeaseSeconds: localMessageLease, IdempotencyKey: key})
				return outcome{fence: fence, err: err}
			}
			responses := make(chan outcome, 2)
			go func() { responses <- acquire("attempt-one") }()
			go func() { responses <- acquire("attempt-two") }()
			// Both callbacks have arrived; the scheduler is free to choose the winner.
			admitted := false
			defer func() {
				if !admitted {
					close(s.admit)
				}
			}()
			for i := 0; i < 2; i++ {
				select {
				case <-s.entered:
				case <-time.After(5 * time.Second):
					t.Fatal("both claim callbacks did not arrive")
				}
			}
			close(s.admit)
			admitted = true
			successes, conflicts := 0, 0
			var winner outcome
			for i := 0; i < 2; i++ {
				var got outcome
				select {
				case got = <-responses:
				case <-time.After(5 * time.Second):
					t.Fatal("claim callback did not finish")
				}
				if got.err == nil {
					successes++
					winner = got
				} else {
					localMessageConflict(t, got.err)
					conflicts++
				}
			}
			if successes != 1 || conflicts != 1 {
				t.Fatal("losing claimant must receive typed conflict never success")
			}
			s.mu.Lock()
			if len(s.calls) != 2 || len(s.attempts) != 2 {
				t.Error("contention changed exact callback count")
			}
			s.now = s.now.Add((localMessageLease + 1) * time.Second)
			s.entered = make(chan struct{}, 1)
			s.mu.Unlock()
			successor := acquire("after-expiry")
			localMessageOK(t, successor.err)
			s.mu.Lock()
			s.contendedOp = ""
			s.mu.Unlock()
			oldID, oldGeneration := winner.fence.ClaimID, winner.fence.Generation
			newID, newGeneration := successor.fence.ClaimID, successor.fence.Generation
			if request {
				oldID, oldGeneration = winner.claim.ClaimID, winner.claim.Generation
				newID, newGeneration = successor.claim.ClaimID, successor.claim.Generation
			}
			if oldID == "" || newID != oldID || oldGeneration == 0 || newGeneration != oldGeneration+1 {
				t.Fatal("expiry lost exact successor generation")
			}
			if request {
				if successor.claim.LeaseExpiresAt == nil || !successor.claim.LeaseExpiresAt.Equal(s.now.Add(localMessageLease*time.Second)) {
					t.Fatal("successor lease ignored controlled clock")
				}
				f.refuse("request-renew", "local-b", s.graph.Request.ID, RenewMessageRequestRequest{ClaimID: oldID, Generation: oldGeneration, LeaseSeconds: localMessageLease}, ErrConflict)
				_, err := client.RenewMessageRequest(ctx, f.server.URL, "local-b", s.graph.Request.ID, client.RenewMessageRequestInput{ClaimID: oldID, Generation: oldGeneration, LeaseSeconds: localMessageLease})
				localMessageConflict(t, err)
				f.status(http.StatusConflict)
				f.refuse("request-release", "local-b", s.graph.Request.ID, ReleaseMessageRequestRequest{ClaimID: oldID, Generation: oldGeneration}, ErrConflict)
				_, err = client.ReleaseMessageRequest(ctx, f.server.URL, "local-b", s.graph.Request.ID, client.ReleaseMessageRequestInput{ClaimID: oldID, Generation: oldGeneration})
				localMessageConflict(t, err)
				stale := CompleteMessageRequestRequest{ClaimID: oldID, Generation: oldGeneration, Body: "stale result", IdempotencyKey: "stale-complete"}
				f.refuse("request-complete", "local-b", s.graph.Request.ID, stale, ErrConflict)
				_, err = client.CompleteMessageRequest(ctx, f.server.URL, "local-b", s.graph.Request.ID, client.CompleteMessageRequestInput{ClaimID: stale.ClaimID, Generation: stale.Generation, Body: stale.Body, IdempotencyKey: stale.IdempotencyKey})
				localMessageConflict(t, err)
				f.transitions("request-complete", 0)
				result, err := f.completeRequest(CompleteMessageRequestRequest{ClaimID: newID, Generation: newGeneration, Body: "current result", IdempotencyKey: "current-complete"})
				localMessageOK(t, err)
				f.status(http.StatusCreated)
				if result.Claim.ClaimID != newID || result.Claim.Generation != newGeneration || result.Claim.ResultMessageID != result.Message.ID {
					t.Fatal("current request completion lost original coordinates")
				}
				f.transitions("request-complete", 1)
			} else {
				if successor.fence.LeaseExpiresAt == nil || !successor.fence.LeaseExpiresAt.Equal(s.now.Add(localMessageLease*time.Second)) {
					t.Fatal("successor lease ignored controlled clock")
				}
				f.refuse("renew", "local-b", s.message.ID, RenewMessageClaimRequest{ClaimID: oldID, Generation: oldGeneration, LeaseSeconds: localMessageLease}, ErrConflict)
				_, err := client.RenewMessageClaim(ctx, f.server.URL, "local-b", s.message.ID, client.RenewMessageClaimInput{ClaimID: oldID, Generation: oldGeneration, LeaseSeconds: localMessageLease})
				localMessageConflict(t, err)
				f.refuse("release", "local-b", s.message.ID, MessageClaimRequest{ClaimID: oldID, Generation: oldGeneration}, ErrConflict)
				_, err = client.ReleaseMessageClaim(ctx, f.server.URL, "local-b", s.message.ID, client.MessageClaimInput{ClaimID: oldID, Generation: oldGeneration})
				localMessageConflict(t, err)
				stale := CompleteMessageRequest{ClaimID: oldID, Generation: oldGeneration, Body: "stale result", IdempotencyKey: "stale-complete"}
				f.refuse("complete", "local-b", s.message.ID, stale, ErrConflict)
				_, err = client.CompleteMessage(ctx, f.server.URL, "local-b", s.message.ID, client.CompleteMessageInput{ClaimID: stale.ClaimID, Generation: stale.Generation, Body: stale.Body, IdempotencyKey: stale.IdempotencyKey})
				localMessageConflict(t, err)
				f.transitions("complete", 0)
				input := CompleteMessageRequest{ClaimID: newID, Generation: newGeneration, Body: "current result", IdempotencyKey: "current-complete"}
				processing := s.processing
				processing.ResultMessageID = s.result.ID
				f.expect("complete", "local-b", s.message.ID, input, CompleteMessageResult{Processing: processing, Message: s.result}, true)
				result, err := client.CompleteMessage(ctx, f.server.URL, "local-b", s.message.ID, client.CompleteMessageInput{ClaimID: input.ClaimID, Generation: input.Generation, Body: input.Body, IdempotencyKey: input.IdempotencyKey})
				localMessageOK(t, err)
				f.status(http.StatusCreated)
				if result.Processing.ClaimID != newID || result.Processing.Generation != newGeneration || result.Processing.ResultMessageID != result.Message.ID {
					t.Fatal("current ordinary completion lost original coordinates")
				}
				f.transitions("complete", 1)
			}
			f.hiddenMembership(request)
		})
	}
}

func (f *localMessageFixture) hiddenMembership(request bool) {
	f.t.Helper()
	op, prefix, id := "claim", "/v1/messages/", f.state.message.ID
	if request {
		op, prefix, id = "request-claim", "/v1/message-requests/", f.state.graph.Request.ID
	}
	var previous []byte
	for i, token := range []string{"local-outsider", "local-b"} {
		callbackError := ErrForbidden
		if i == 1 {
			id = "missing-record"
			callbackError = ErrNotFound
		}
		var in any = ClaimMessageRequest{LeaseSeconds: localMessageLease}
		if request {
			in = ClaimMessageRequestRequest{LeaseSeconds: localMessageLease}
		}
		f.refuse(op, token, id, in, callbackError)
		body, err := json.Marshal(map[string]int{"lease_seconds": localMessageLease})
		localMessageOK(f.t, err)
		status, raw := f.raw(token, prefix+id+":claim", body)
		if status != http.StatusNotFound {
			f.t.Fatal("missing and nonmember outcomes must retain public 404")
		}
		if i == 0 {
			previous = raw
		} else if !bytes.Equal(previous, raw) {
			f.t.Fatal("missing and nonmember responses disclosed membership")
		}
	}
}

func (f *localMessageFixture) reconstruct() {
	f.t.Helper()
	f.state.mu.Lock()
	if len(f.state.steps) != 0 {
		f.state.mu.Unlock()
		f.t.Fatal("reconstruction retained pending callback script")
	}
	f.state.mu.Unlock()
	oldURL := f.server.URL
	f.server.Close()
	f.transport.mu.Lock()
	delete(f.transport.allowed, oldURL)
	f.transport.mu.Unlock()
	f.start()
	if oldURL == f.server.URL {
		f.t.Fatal("reconstruction did not create a new origin")
	}
}

func TestLocalMessageWorkflowReopenAndReplay(t *testing.T) {
	f := newLocalMessageFixture(t)
	s := f.state
	opened, err := f.open(s.graph.OpeningMessage.Body)
	localMessageOK(t, err)
	f.showAndList()
	originalRead := f.readOrdinary()
	complete := CompleteMessageRequestRequest{ClaimID: s.claim.ClaimID, Generation: s.claim.Generation, Body: "retained result", IdempotencyKey: "retained-complete"}
	// Commit the callback, then lose exactly its response. There is no implicit
	// retry policy: the next call below is made explicitly by this test.
	f.transport.mu.Lock()
	f.transport.losePath = "/v1/message-requests/" + s.graph.Request.ID + ":complete"
	before := len(f.transport.wires)
	f.transport.mu.Unlock()
	_, err = f.completeRequest(complete)
	if err == nil {
		t.Fatal("lost committed response did not interrupt caller")
	}
	f.transitions("request-complete", 1)
	f.transport.mu.Lock()
	attempts := len(f.transport.wires) - before
	f.transport.mu.Unlock()
	if attempts != 1 {
		t.Fatal("lost response caused automatic client replay")
	}
	f.reconstruct()
	repeatedOpen, err := f.open(s.graph.OpeningMessage.Body)
	localMessageOK(t, err)
	if !reflect.DeepEqual(opened, repeatedOpen) {
		t.Fatal("reconstruction changed retained opening identity")
	}
	f.transitions("open", 1)
	repeated, err := f.completeRequest(complete)
	localMessageOK(t, err)
	f.transitions("request-complete", 1)
	localMessagePublic(t, repeated.Message)
	if repeated.Claim.ResultMessageID != s.result.ID || repeated.Message.ID != s.result.ID || repeated.Message.ReplyToMessageID != opened.OpeningMessage.ID {
		t.Fatal("reconstruction changed retained result linkage")
	}
	f.transport.mu.Lock()
	first := f.transport.wires[before]
	last := f.transport.wires[len(f.transport.wires)-1]
	f.transport.mu.Unlock()
	if first.method != last.method || first.path != last.path || first.token != last.token || first.key == "" || first.key != last.key || !bytes.Equal(first.body, last.body) {
		t.Fatal("explicit lost-response replay changed bytes key or identity")
	}
	// A new key is not a replay in this explicit completed-state scenario.
	changed := complete
	changed.IdempotencyKey = "different-completion-key"
	f.refuse("request-complete", "local-b", s.graph.Request.ID, changed, ErrConflict)
	_, err = client.CompleteMessageRequest(t.Context(), f.server.URL, "local-b", s.graph.Request.ID, client.CompleteMessageRequestInput{ClaimID: changed.ClaimID, Generation: changed.Generation, Body: changed.Body, IdempotencyKey: changed.IdempotencyKey})
	localMessageConflict(t, err)
	f.transitions("request-complete", 1)
	f.showAndList()
	read := f.readOrdinary()
	if !reflect.DeepEqual(originalRead, read) {
		t.Fatal("reconstruction changed explicit read identity or content")
	}
	// Advance a retained ordinary lease in fixture time and carry its successor
	// through the reconstructed handlers; no crash/restart claim is made here.
	old := s.processing
	s.now = s.now.Add((localMessageLease + 1) * time.Second)
	s.processing.Generation++
	expiry := s.now.Add(localMessageLease * time.Second)
	s.processing.LeaseExpiresAt = &expiry
	f.expect("claim", "local-b", s.message.ID, ClaimMessageRequest{LeaseSeconds: localMessageLease, IdempotencyKey: "reopen-claim"}, s.processing, false)
	fence, err := client.ClaimMessage(t.Context(), f.server.URL, "local-b", s.message.ID, client.ClaimMessageInput{LeaseSeconds: localMessageLease, IdempotencyKey: "reopen-claim"})
	localMessageOK(t, err)
	if fence.Generation != old.Generation+1 || fence.ClaimID != old.ClaimID {
		t.Fatal("reconstructed handler lost successor fence")
	}
	f.refuse("renew", "local-b", s.message.ID, RenewMessageClaimRequest{ClaimID: old.ClaimID, Generation: old.Generation, LeaseSeconds: localMessageLease}, ErrConflict)
	_, err = client.RenewMessageClaim(t.Context(), f.server.URL, "local-b", s.message.ID, client.RenewMessageClaimInput{ClaimID: old.ClaimID, Generation: old.Generation, LeaseSeconds: localMessageLease})
	localMessageConflict(t, err)
	f.expect("renew", "local-b", s.message.ID, RenewMessageClaimRequest{ClaimID: fence.ClaimID, Generation: fence.Generation, LeaseSeconds: localMessageLease}, s.processing, false)
	_, err = client.RenewMessageClaim(t.Context(), f.server.URL, "local-b", s.message.ID, client.RenewMessageClaimInput{ClaimID: fence.ClaimID, Generation: fence.Generation, LeaseSeconds: localMessageLease})
	localMessageOK(t, err)
	// Idle fixture advancement is synchronous, with no runner or scheduled work.
	// This only observes the bounded fixture, not arbitrary production workers.
	s.mu.Lock()
	calls, transitions := len(s.calls), s.transitions["request-complete"]
	s.now = s.now.Add(time.Hour)
	s.mu.Unlock()
	f.reconstruct()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.calls) != calls || s.transitions["request-complete"] != transitions {
		t.Fatal("idle reconstruction performed background callback work")
	}
}

// These errors are fixture policy outcomes. The production boundary under test
// is their HTTP/client translation, including the absence of implicit replay.
func TestLocalMessageWorkflowPolicyRefusals(t *testing.T) {
	for _, kind := range []string{"ordinary", "request"} {
		for _, refusal := range []string{"disabled", "rate", "hard"} {
			t.Run(kind+"/"+refusal, func(t *testing.T) {
				f := newLocalMessageFixture(t)
				ctx := context.Background()
				const token = "local-a"
				var supplied error
				wantStatus := http.StatusForbidden
				switch refusal {
				case "disabled":
					supplied = &FeatureNotEnabledError{Feature: "messaging"}
				case "rate":
					supplied = &MessageRateLimitError{Dimension: "message_sent", Scope: "agent", Limit: 4, Used: 4, Attempted: 1, WindowSeconds: 17, RetryAfter: 1500 * time.Millisecond, ResetAt: f.state.now, Source: f.state.message.Body, Retryable: true}
					wantStatus = http.StatusTooManyRequests
				case "hard":
					supplied = &MessageRateLimitError{Dimension: "message_delivered", Scope: "realm", Limit: 1, Used: 0, Attempted: 2, WindowSeconds: 17, RetryAfter: time.Hour, ResetAt: f.state.now, Source: f.state.message.Body, Retryable: false}
				}
				var err error
				operation := "send"
				if kind == "ordinary" {
					in := client.SendMessageInput{To: f.state.message.To.AgentID, Body: f.state.message.Body, Payload: f.state.message.Payload, IdempotencyKey: f.state.message.ID}
					expected := SendMessageRequest{To: MessageRecipientRequest{Kind: "agent", ID: in.To}, Body: in.Body, Payload: in.Payload, IdempotencyKey: in.IdempotencyKey}
					f.refuse(operation, token, "", expected, supplied)
					_, err = client.SendMessage(ctx, f.server.URL, token, in)
				} else {
					operation = "open"
					in := client.CreateMessageRequestInput{Body: f.state.graph.OpeningMessage.Body, Payload: f.state.graph.OpeningMessage.Payload, SelectionPolicy: f.state.graph.Request.SelectionPolicy, MaxAssignees: 1, OfferWindowSeconds: localMessageLease, ExpiresInSeconds: 60, IdempotencyKey: f.state.graph.Request.ID}
					expected := CreateMessageRequestRequest{Body: in.Body, Payload: in.Payload, SelectionPolicy: in.SelectionPolicy, MaxAssignees: in.MaxAssignees, OfferWindowSeconds: in.OfferWindowSeconds, ExpiresInSeconds: in.ExpiresInSeconds, IdempotencyKey: in.IdempotencyKey}
					f.refuse(operation, token, "", expected, supplied)
					_, err = client.CreateMessageRequest(ctx, f.server.URL, token, in)
				}
				f.status(wantStatus)
				var envelope struct {
					Code      string `json:"code"`
					Retryable bool   `json:"retryable"`
				}
				f.transport.mu.Lock()
				attempts := len(f.transport.wires)
				raw := append([]byte(nil), f.transport.wires[len(f.transport.wires)-1].response...)
				f.transport.mu.Unlock()
				if attempts != 1 {
					t.Fatal("policy refusal was automatically replayed")
				}
				if json.Unmarshal(raw, &envelope) != nil {
					t.Fatal("policy response was not JSON")
				}
				if refusal == "disabled" {
					var detail *client.FeatureNotEnabledError
					if !errors.Is(err, client.ErrFeatureNotEnabled) || !errors.As(err, &detail) || detail.Feature != "messaging" || detail.Retryable || envelope.Code != "feature_not_enabled" || envelope.Retryable {
						t.Fatal("feature refusal lost nonretryable typed details")
					}
				} else {
					var detail *client.MessageRateLimitError
					if !errors.Is(err, client.ErrMessageRateLimited) || !errors.As(err, &detail) {
						t.Fatal("message refusal lost its domain error type")
					}
					retryable := refusal == "rate"
					if detail.Retryable != retryable || envelope.Retryable != retryable || detail.WindowSeconds != 60 || detail.Source != "" || !detail.ResetAt.IsZero() {
						t.Fatal("message refusal did not retain bounded public policy details")
					}
					if retryable {
						if envelope.Code != "rate_limited" || detail.LimitDimension != "message_sent" || detail.Scope != "agent" || detail.LimitKey != "message_sent_per_agent_minute" || detail.Limit != 4 || detail.Used != 4 || detail.Attempted != 1 || detail.RetryAfter != 2*time.Second {
							t.Fatal("retryable message refusal details changed")
						}
					} else if envelope.Code != "limit_exceeded" || detail.LimitDimension != "message_delivered" || detail.Scope != "realm" || detail.LimitKey != "message_delivered_per_realm_minute" || detail.Limit != 1 || detail.Used != 0 || detail.Attempted != 2 || detail.RetryAfter != 0 {
						t.Fatal("hard message limit became retryable or lost its details")
					}
				}
				// Read-only calls remain explicit and cannot select or execute work after
				// a policy error. Any extra callback fails the fixture's ordered script.
				f.expect("list", token, "", MessageListOptions{}, MessagePage{Messages: []Message{f.state.message}}, false)
				page, readErr := client.ListMessages(ctx, f.server.URL, token, client.MessageListOptions{})
				localMessageOK(t, readErr)
				localMessageMetadata(t, page.Messages)
				f.status(http.StatusOK)
				f.expect("request-list", token, "", MessageRequestListOptions{}, MessageRequestPage{Requests: []MessageRequest{f.state.graph.Request}}, false)
				requests, readErr := client.ListMessageRequests(ctx, f.server.URL, token, client.MessageRequestListOptions{})
				localMessageOK(t, readErr)
				f.status(http.StatusOK)
				if len(requests.Requests) != 1 || requests.Requests[0].ID != f.state.graph.Request.ID {
					t.Fatal("read-only request listing changed linkage")
				}
				f.state.mu.Lock()
				defer f.state.mu.Unlock()
				if !reflect.DeepEqual(f.state.calls, []string{operation, "list", "request-list"}) || len(f.state.memo) != 0 || len(f.state.transitions) != 0 {
					t.Fatal("policy or read-only operation selected or executed work")
				}
			})
		}
	}
}

func (f *localMessageFixture) readOrdinary() client.Message {
	f.t.Helper()
	s := f.state
	f.expect("list", "local-b", "", MessageListOptions{Limit: 1}, MessagePage{Messages: []Message{s.message}}, false)
	listed, err := client.ListMessages(f.t.Context(), f.server.URL, "local-b", client.MessageListOptions{Limit: 1})
	localMessageOK(f.t, err)
	localMessageMetadata(f.t, listed.Messages)
	if listed.Messages[0].ID != s.message.ID {
		f.t.Fatal("reconstruction lost listed message identity")
	}
	f.expect("read", "local-b", s.message.ID, nil, s.message, false)
	read, err := client.ReadMessage(f.t.Context(), f.server.URL, "local-b", s.message.ID)
	localMessageOK(f.t, err)
	localMessagePublic(f.t, read)
	if read.ID != s.message.ID || read.Body != s.message.Body {
		f.t.Fatal("reconstruction lost explicit read content")
	}
	return read
}
