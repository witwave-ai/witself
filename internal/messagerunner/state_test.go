package messagerunner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/client"
)

func TestConfigStoreNotificationLedgerIsPrivateContentFreeAndClearable(t *testing.T) {
	store := testConfigStore(t, "grok-build")
	ctx := context.Background()
	created := time.Date(2026, 7, 14, 12, 30, 0, 0, time.UTC)
	message := client.Message{
		ID: "msg_result", ThreadID: "thr_work", Kind: "result", CreatedAt: created,
		From:    client.MessageAgent{AgentID: "agent_bob", AgentName: "Bob"},
		Subject: "private subject", Body: "private body",
		Payload: []byte(`{"private":"payload"}`),
	}
	if err := store.RecordNotification(ctx, message); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordNotification(ctx, message); err != nil {
		t.Fatal(err)
	}
	notifications, err := store.Notifications(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(notifications) != 1 || notifications[0].MessageID != message.ID ||
		notifications[0].FromAgentName != "Bob" || notifications[0].CreatedAt != created {
		t.Fatalf("notifications = %#v", notifications)
	}
	raw, err := os.ReadFile(store.statePath())
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private subject", "private body", "private", "payload"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("state contains message content %q: %s", secret, raw)
		}
	}
	info, err := os.Stat(store.statePath())
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode = %v / %v", info, err)
	}
	removed, err := store.ClearNotifications(ctx, []string{"msg_unknown"})
	if err != nil || removed != 0 {
		t.Fatalf("clear unknown = %d / %v", removed, err)
	}
	removed, err = store.ClearNotifications(ctx, []string{message.ID})
	if err != nil || removed != 1 {
		t.Fatalf("clear exact = %d / %v", removed, err)
	}
	if remaining, err := store.Notifications(ctx); err != nil || len(remaining) != 0 {
		t.Fatalf("remaining = %#v / %v", remaining, err)
	}
}

func TestNotificationMatchesMessageUsesBoundedUTF8AgentName(t *testing.T) {
	store := testConfigStore(t, "claude-code")
	ctx := context.Background()
	longName := strings.Repeat("界", 100)
	message := client.Message{
		ID: "msg_result", ThreadID: "thr_work", Kind: " RESULT ",
		CreatedAt: time.Date(2026, 7, 14, 12, 30, 0, 0, time.UTC),
		From:      client.MessageAgent{AgentID: "agent_bob", AgentName: longName},
	}
	if err := store.RecordNotification(ctx, message); err != nil {
		t.Fatal(err)
	}
	items, err := store.Notifications(ctx)
	if err != nil || len(items) != 1 {
		t.Fatalf("notifications = %#v / %v", items, err)
	}
	if len(items[0].FromAgentName) > maximumOperationalNameBytes ||
		!strings.HasPrefix(longName, items[0].FromAgentName) ||
		!NotificationMatchesMessage(items[0], message) {
		t.Fatalf("bounded notification does not match canonical message: %#v", items[0])
	}

	mismatch := message
	mismatch.From.AgentID = "agent_other"
	if NotificationMatchesMessage(items[0], mismatch) {
		t.Fatal("notification matched a different canonical sender ID")
	}
}

func TestConfigStoreConsumeNotificationRequiresExactPointerAndIsAtomic(t *testing.T) {
	store := testConfigStore(t, "claude-code")
	ctx := context.Background()
	message := client.Message{
		ID: "msg_result", ThreadID: "thr_work", Kind: "result",
		CreatedAt: time.Date(2026, 7, 14, 12, 30, 0, 0, time.UTC),
		From:      client.MessageAgent{AgentID: "agent_bob", AgentName: "Bob"},
	}
	if err := store.RecordNotification(ctx, message); err != nil {
		t.Fatal(err)
	}
	items, err := store.Notifications(ctx)
	if err != nil || len(items) != 1 {
		t.Fatalf("notifications = %#v / %v", items, err)
	}
	changed := items[0]
	changed.ThreadID = "thr_rebound"
	if consumed, err := store.ConsumeNotification(ctx, changed); err == nil || consumed {
		t.Fatalf("changed pointer consumed = %t / %v", consumed, err)
	}
	if remaining, err := store.Notifications(ctx); err != nil || len(remaining) != 1 {
		t.Fatalf("changed-pointer failure removed state: %#v / %v", remaining, err)
	}

	start := make(chan struct{})
	type outcome struct {
		consumed bool
		err      error
	}
	results := make(chan outcome, 2)
	for range 2 {
		go func() {
			<-start
			consumed, err := store.ConsumeNotification(ctx, items[0])
			results <- outcome{consumed: consumed, err: err}
		}()
	}
	close(start)
	successes := 0
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.consumed {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent consume successes = %d, want 1", successes)
	}
	if remaining, err := store.Notifications(ctx); err != nil || len(remaining) != 0 {
		t.Fatalf("remaining = %#v / %v", remaining, err)
	}
}

func TestConfigStoreNotificationLedgerFailsClosedAtCapacity(t *testing.T) {
	store := testConfigStore(t, "claude-code")
	state := persistedRunnerState{
		Schema: RunnerStateSchemaV1, Revision: 1, UpdatedAt: store.operationalNow(),
		Notifications: make([]Notification, maximumRunnerNotifications),
	}
	for index := range state.Notifications {
		state.Notifications[index] = Notification{
			MessageID: fmt.Sprintf("msg_%04d", index), ThreadID: "thr_full", Kind: "result",
			FromAgentID: "agent_bob", RecordedAt: store.operationalNow(),
		}
	}
	if err := writePrivateJSONAtomic(store.statePath(), state); err != nil {
		t.Fatal(err)
	}
	err := store.RecordNotification(context.Background(), client.Message{
		ID: "msg_overflow", ThreadID: "thr_full", Kind: "result",
		From: client.MessageAgent{AgentID: "agent_bob"},
	})
	if err == nil || !strings.Contains(err.Error(), "ledger is full") {
		t.Fatalf("overflow error = %v", err)
	}
	removed, err := store.ClearNotifications(context.Background(), nil)
	if err != nil || removed != maximumRunnerNotifications {
		t.Fatalf("clear all = %d / %v", removed, err)
	}
}

func TestConfigStoreRunnerHealthIsContentFreeAndClassified(t *testing.T) {
	store := testConfigStore(t, "claude-code")
	ctx := context.Background()
	privateError := errors.New("provider echoed a private message body")
	if err := store.RecordCycle(ctx, RunResult{MessageID: "msg_private"}, privateError); err != nil {
		t.Fatal(err)
	}
	health, err := store.RunnerHealth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if health.LastStatus != "error" || health.LastErrorClass != "cycle" || health.ConsecutiveFailures != 1 {
		t.Fatalf("failure health = %#v", health)
	}
	if err := store.RecordCycle(ctx, RunResult{Status: RunStatusCompleted, MessageID: "msg_private"}, nil); err != nil {
		t.Fatal(err)
	}
	health, err = store.RunnerHealth(ctx)
	if err != nil || health.LastStatus != RunStatusCompleted || health.LastErrorClass != "" || health.ConsecutiveFailures != 0 || health.LastSuccessAt.IsZero() {
		t.Fatalf("success health = %#v / %v", health, err)
	}
	raw, err := os.ReadFile(store.statePath())
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"private message body", "msg_private", privateError.Error()} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("health state leaked %q: %s", forbidden, raw)
		}
	}
}

func TestConfigStoreRequestScanCheckpointRoundTripIsContentFree(t *testing.T) {
	store := testConfigStore(t, "claude-code")
	ctx := context.Background()
	want := RequestScanCheckpoint{
		NextPhase: 2,
		Cursors: map[string]string{
			requestScanSelected: "1721064000000000000:mrq_aaaaaaaaaaaaaaaa",
			requestScanPending:  "1721063900000000000:mrq_bbbbbbbbbbbbbbbb",
		},
	}
	if err := store.SaveRequestScanCheckpoint(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err := store.LoadRequestScanCheckpoint(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.NextPhase != want.NextPhase ||
		got.Cursors[requestScanSelected] != want.Cursors[requestScanSelected] ||
		got.Cursors[requestScanPending] != want.Cursors[requestScanPending] {
		t.Fatalf("request scan checkpoint = %#v, want %#v", got, want)
	}
	got.Cursors[requestScanSelected] = "mutated"
	again, err := store.LoadRequestScanCheckpoint(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if again.Cursors[requestScanSelected] != want.Cursors[requestScanSelected] {
		t.Fatalf("stored request scan checkpoint aliased caller map: %#v", again)
	}
	if err := store.SaveRequestScanCheckpoint(ctx, RequestScanCheckpoint{
		Cursors: map[string]string{"unknown": "cursor"},
	}); err == nil {
		t.Fatal("invalid request scan lane was accepted")
	}
}
