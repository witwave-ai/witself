package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/server"
	"github.com/witwave-ai/witself/internal/store"
)

// TestMemoryCurationInputsMaxBytesAdapterPostgres pins the adapter's
// pass-through of max_bytes from the HTTP options into the store (issue
// #649): a bound of 8192 splits a run that fits one page under the server
// budget, and 8191 maps to server.ErrBadInput before any page is built.
func TestMemoryCurationInputsMaxBytesAdapterPostgres(t *testing.T) {
	st := startupCohortStore(t)
	ctx := context.Background()
	account, err := st.ProvisionAccount(ctx, "max-bytes-adapter@example.test", "max-bytes adapter fixture", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := st.ActivateAccount(ctx, account.AccountID); err != nil || !ok {
		t.Fatal("activate fixture", err)
	}
	realm, err := st.CreateRealm(ctx, account.AccountID, "max-bytes realm")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.CreateAgent(ctx, account.AccountID, realm.ID, "max-bytes agent")
	if err != nil {
		t.Fatal(err)
	}
	p := server.DomainPrincipal{
		Kind: server.PrincipalKindAgent, ID: agent.ID, AccountID: account.AccountID,
		RealmID: realm.ID, AccountStatus: "active",
	}
	sp := toStorePrincipal(p)

	// Three short conversations of three entries each: about 18 KB of
	// hydrated inputs, one page under the server budget and at least three
	// under an 8 KiB bound (each transcript input carries 4,200 bytes of
	// body alone, so two never share an 8 KiB page).
	for i := range 3 {
		transcript, err := st.CreateTranscript(ctx, sp.AccountID, sp.RealmID, sp.ID,
			store.CreateTranscriptInput{ExternalID: fmt.Sprintf("max-bytes-adapter-%d", i+1)})
		if err != nil {
			t.Fatal(err)
		}
		for j := range 3 {
			role, kind := store.TranscriptRoleUser, "message.user"
			if j%2 == 1 {
				role, kind = store.TranscriptRoleAssistant, "message.assistant"
			}
			if _, err := st.AppendTranscriptEntry(ctx, sp.AccountID, sp.RealmID, sp.ID,
				transcript.ID, store.AppendTranscriptEntryInput{
					ExternalID: fmt.Sprintf("max-bytes-adapter-%d-%d", i+1, j+1), Role: role,
					Body:    strings.Repeat(string(rune('a'+i)), 1400),
					Payload: json.RawMessage(fmt.Sprintf(`{"kind":%q}`, kind)),
				}); err != nil {
				t.Fatalf("append conversation %d entry %d: %v", i+1, j+1, err)
			}
		}
	}
	requested, err := st.RequestCuration(ctx, sp, store.RequestMemoryCurationInput{
		Scope:         store.MemoryCurationScope{Sources: []string{store.MemoryCurationSourceTranscript}},
		CoalescingKey: "max_bytes_adapter", TriggerReason: "manual_refine",
		IdempotencyKey: "max-bytes-adapter-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	started, err := st.StartCuration(ctx, sp, store.StartMemoryCurationInput{
		RequestID: requested.Request.ID, LeaseDuration: 5 * time.Minute,
		IdempotencyKey: "max-bytes-adapter-start",
	})
	if err != nil {
		t.Fatal(err)
	}
	if started.Run.TranscriptInputCount != 3 {
		t.Fatalf("transcript inputs = %d, want 3, one per conversation", started.Run.TranscriptInputCount)
	}

	var cfg server.Config
	configureMemoryCuration(&cfg, st)
	read := func(maxBytes int) (store.MemoryCurationRunInputPage, error) {
		t.Helper()
		result, err := cfg.GetMemoryCurationRunInputs(ctx, p, started.Run.ID, server.MemoryCurationRunInputOptions{
			FencingGeneration: started.Run.FencingGeneration, Limit: 200, MaxBytes: maxBytes,
		})
		if err != nil {
			return store.MemoryCurationRunInputPage{}, err
		}
		page, ok := result.(store.MemoryCurationRunInputPage)
		if !ok {
			t.Fatalf("max_bytes %d: the adapter returned %T, want a store page", maxBytes, result)
		}
		return page, nil
	}
	unbounded, err := read(0)
	if err != nil {
		t.Fatal(err)
	}
	if unbounded.NextCursor != "" || len(unbounded.Inputs) != started.Run.InputCount {
		t.Fatalf("fixture premise: the unbounded read returned %d of %d inputs with a next cursor=%t, want one page",
			len(unbounded.Inputs), started.Run.InputCount, unbounded.NextCursor != "")
	}
	bounded, err := read(8192)
	if err != nil {
		t.Fatal(err)
	}
	if bounded.NextCursor == "" || len(bounded.Inputs) >= started.Run.InputCount {
		t.Fatalf("max_bytes 8192 reached the store as the server budget: %d of %d inputs on the first page, want a next cursor",
			len(bounded.Inputs), started.Run.InputCount)
	}
	if _, err := read(8191); !errors.Is(err, server.ErrBadInput) {
		t.Fatalf("max_bytes 8191: error = %v, want bad input", err)
	}
}
