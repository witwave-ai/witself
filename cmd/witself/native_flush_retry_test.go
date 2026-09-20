package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

func writeNativeFlushRetryFixture(t *testing.T, runtimeName string, at time.Time, change func(*transcriptcapture.Event)) transcriptcapture.PendingEvent {
	t.Helper()
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", home)
	t.Setenv("DSH_HOME", t.TempDir())
	dataFields := map[string]any{"grok_native_retry_deadline": at}
	if runtimeName == transcriptcapture.RuntimeDSH {
		dataFields = map[string]any{"dsh_native_retry_deadline": at, "dsh_native_retry_at": at}
	}
	data, err := json.Marshal(dataFields)
	if err != nil {
		t.Fatal(err)
	}
	event := transcriptcapture.Event{
		ID: "native-retry-stop", Runtime: runtimeName, SessionID: "native-retry-session",
		HookEvent: "Stop", NativeHookEvent: "stop", Kind: "turn.completed", Role: "system",
		OccurredAt: time.Now().UTC(), Data: data,
	}
	if change != nil {
		change(&event)
	}
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, "capture", "outbox", runtimeName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "00000000000000000001-"+event.ID+".json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return transcriptcapture.PendingEvent{Path: path, Event: event}
}

func TestNativeFlushRetryReopensDueGrokAndDSHStops(t *testing.T) {
	for _, runtimeName := range []string{transcriptcapture.RuntimeGrokBuild, transcriptcapture.RuntimeDSH} {
		t.Run(runtimeName, func(t *testing.T) {
			item := writeNativeFlushRetryFixture(t, runtimeName, time.Now().Add(-time.Second), nil)
			transcriptID := item.Event.TranscriptExternalID()
			blocked := map[string]error{transcriptID: nil, "unrelated": nil}
			pending, retried, err := waitNativeFlushRetry(context.Background(), runtimeName, blocked, nil)
			if err != nil || !retried || len(pending) != 1 || pending[0].Path != item.Path {
				t.Fatalf("retry = %t, pending count = %d, error = %v", retried, len(pending), err)
			}
			if _, exists := blocked[transcriptID]; exists {
				t.Fatal("due native Stop stayed blocked without a new hook")
			}
			if _, exists := blocked["unrelated"]; !exists {
				t.Fatal("retry reopened an unrelated transcript")
			}
		})
	}
}

func TestGrokNativeFlushRetryPreservesExcludedAndHeldStops(t *testing.T) {
	tests := []struct {
		name   string
		change func(*transcriptcapture.Event)
		held   bool
		reason error
	}{
		{name: "real transcript", change: func(event *transcriptcapture.Event) { event.SourceTranscriptPath = "/synthetic/updates.jsonl" }},
		{name: "real turn", change: func(event *transcriptcapture.Event) { event.TurnID = "known-turn" }},
		{name: "prompt identity", change: func(event *transcriptcapture.Event) {
			var data map[string]any
			if err := json.Unmarshal(event.Data, &data); err != nil {
				t.Fatal(err)
			}
			data["prompt_id"] = "known-prompt"
			event.Data, _ = json.Marshal(data)
		}},
		{name: "already finalized", change: func(event *transcriptcapture.Event) { event.NativeTurnFinalized = true }},
		{name: "held path", held: true},
		{name: "rejected transcript", reason: errors.New("synthetic rejection")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			item := writeNativeFlushRetryFixture(t, transcriptcapture.RuntimeGrokBuild, time.Now().Add(-time.Second), test.change)
			transcriptID := item.Event.TranscriptExternalID()
			blocked := map[string]error{transcriptID: test.reason}
			held := map[string]error{}
			if test.held {
				held[item.Path] = errors.New("synthetic privacy hold")
			}
			_, retried, err := waitNativeFlushRetry(context.Background(), transcriptcapture.RuntimeGrokBuild, blocked, held)
			if err != nil || retried {
				t.Fatalf("excluded Stop retry = %t, error = %v", retried, err)
			}
			if _, exists := blocked[transcriptID]; !exists {
				t.Fatal("excluded Stop was reopened")
			}
		})
	}
}

func TestGrokNativeFlushRetryCanceledWaitStaysBlocked(t *testing.T) {
	for _, offset := range []time.Duration{-time.Second, time.Hour} {
		t.Run(offset.String(), func(t *testing.T) {
			item := writeNativeFlushRetryFixture(t, transcriptcapture.RuntimeGrokBuild, time.Now().Add(offset), nil)
			transcriptID := item.Event.TranscriptExternalID()
			blocked := map[string]error{transcriptID: nil}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_, retried, err := waitNativeFlushRetry(ctx, transcriptcapture.RuntimeGrokBuild, blocked, nil)
			if !errors.Is(err, context.Canceled) || retried {
				t.Fatalf("canceled retry = %t, error = %v", retried, err)
			}
			if _, exists := blocked[transcriptID]; !exists {
				t.Fatal("canceled wait reopened native Stop")
			}
		})
	}
}

func TestGrokFlushPreparesSixExpiredOrphanStopsInOnePass(t *testing.T) {
	cfg := transcriptcapture.Config{
		Runtime: transcriptcapture.RuntimeGrokBuild,
		Account: "synthetic-account", AccountID: "acc_fixture",
		Realm: "synthetic-realm", RealmID: "rlm_fixture",
		Agent: "synthetic-agent", AgentID: "agent_fixture", AgentName: "synthetic-agent",
		Location: transcriptcapture.Location{ID: "loc_fixture", Name: "synthetic-location"},
	}
	first := writeNativeFlushRetryFixture(t, cfg.Runtime, time.Time{}, func(event *transcriptcapture.Event) {
		event.Account, event.AccountID = cfg.Account, cfg.AccountID
		event.Realm, event.RealmID = cfg.Realm, cfg.RealmID
		event.Agent, event.AgentID, event.AgentName = cfg.Agent, cfg.AgentID, cfg.AgentName
		event.Location = cfg.Location
		event.OccurredAt = time.Now().UTC().Add(-time.Minute)
		event.Data = nil
	})
	// All six belong to one transcript: deferring the first Stop blocks every
	// later Stop in this preparation pass. Old events must not each receive a
	// fresh retry window when that transcript is reopened.
	for i := 1; i < 6; i++ {
		event := first.Event
		event.ID = fmt.Sprintf("native-retry-stop-%d", i)
		event.OccurredAt = event.OccurredAt.Add(time.Duration(i) * time.Millisecond)
		raw, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(filepath.Dir(first.Path), fmt.Sprintf("%020d-%s.json", i+1, event.ID))
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := transcriptcapture.Pending(cfg.Runtime)
	if err != nil || len(pending) != 6 {
		t.Fatalf("fixture pending = %d, error = %v", len(pending), err)
	}
	blocked, held := map[string]error{}, map[string]error{}
	ready, err := prepareTranscriptFlushEvents(pending, cfg, blocked, held)
	if err != nil || len(ready) != 6 || len(blocked) != 0 || len(held) != 0 {
		t.Fatalf("preparation ready = %d, blocked = %d, held = %d, error = %v", len(ready), len(blocked), len(held), err)
	}
	persisted, err := transcriptcapture.Pending(cfg.Runtime)
	if err != nil || len(persisted) != 6 {
		t.Fatalf("persisted pending = %d, error = %v", len(persisted), err)
	}
	for i, item := range persisted {
		var data map[string]string
		if !item.Event.NativeTurnFinalized || json.Unmarshal(item.Event.Data, &data) != nil ||
			len(data) != 1 || data["grok_native_finalization"] != "unresolved_prompt_without_transcript" {
			t.Fatalf("Stop %d did not persist its terminal outcome", i)
		}
	}
}
