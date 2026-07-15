package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNormalizeAgentActivityInputAcceptsFutureRuntimeLabels(t *testing.T) {
	occurred := time.Date(2026, 7, 15, 18, 0, 0, 123, time.FixedZone("offset", -6*60*60))
	got, err := normalizeAgentActivityInput(AgentActivityInput{
		Runtime: " gemini-cli ", LocationID: " install-123 ", Location: " Scott's laptop ",
		Event: " TurnCompleted ", EventID: " evt_123 ", EventOccurredAt: occurred,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Runtime != "gemini-cli" || got.LocationID != "install-123" ||
		got.Location != "Scott's laptop" || got.Event != "TurnCompleted" ||
		got.EventID != "evt_123" || got.EventOccurredAt.Location() != time.UTC ||
		!got.EventOccurredAt.Equal(occurred) {
		t.Fatalf("normalized input = %#v", got)
	}
}

func TestNormalizeAgentActivityInputRejectsUnboundedOrUnsafeMetadata(t *testing.T) {
	valid := AgentActivityInput{
		Runtime: "codex", LocationID: "install-1", Event: "Stop",
		EventID: "evt_1", EventOccurredAt: time.Now(),
	}
	tests := []struct {
		name   string
		mutate func(*AgentActivityInput)
	}{
		{name: "missing runtime", mutate: func(in *AgentActivityInput) { in.Runtime = "" }},
		{name: "runtime control", mutate: func(in *AgentActivityInput) { in.Runtime = "codex\nsecret" }},
		{name: "location id too long", mutate: func(in *AgentActivityInput) { in.LocationID = strings.Repeat("x", maxAgentActivityLabelBytes+1) }},
		{name: "location control", mutate: func(in *AgentActivityInput) { in.Location = "home\tprivate" }},
		{name: "missing event", mutate: func(in *AgentActivityInput) { in.Event = "" }},
		{name: "missing event id", mutate: func(in *AgentActivityInput) { in.EventID = "" }},
		{name: "missing event time", mutate: func(in *AgentActivityInput) { in.EventOccurredAt = time.Time{} }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := valid
			tc.mutate(&in)
			if _, err := normalizeAgentActivityInput(in); !errors.Is(err, ErrAgentActivityInputInvalid) {
				t.Fatalf("error = %v, want ErrAgentActivityInputInvalid", err)
			}
		})
	}
}

func TestAgentActivityStoreRejectsNonAgentWithoutDatabaseAccess(t *testing.T) {
	st := &Store{}
	if _, err := st.TouchAgentActivity(context.Background(), Principal{Kind: PrincipalOperator}, AgentActivityInput{}); !errors.Is(err, ErrAgentActivityForbidden) {
		t.Fatalf("touch error = %v", err)
	}
	if _, err := st.ListAgentPeers(context.Background(), Principal{Kind: PrincipalOperator}); !errors.Is(err, ErrAgentActivityForbidden) {
		t.Fatalf("list error = %v", err)
	}
	curator := Principal{Kind: PrincipalAgent, AccessProfile: AccessProfileCuratorPreview}
	if _, err := st.TouchAgentActivity(context.Background(), curator, AgentActivityInput{}); !errors.Is(err, ErrAgentActivityForbidden) {
		t.Fatalf("curator touch error = %v", err)
	}
	if _, err := st.ListAgentPeers(context.Background(), curator); !errors.Is(err, ErrAgentActivityForbidden) {
		t.Fatalf("curator list error = %v", err)
	}
}

func TestImportedAgentActivityRejectsHostileProjectionRows(t *testing.T) {
	exportedAt := time.Date(2026, 7, 15, 20, 0, 0, 0, time.UTC)
	validRow := func() map[string]any {
		return map[string]any{
			"agent_id": "agent_1", "runtime": "codex", "location_id": "install-1",
			"location": "home", "last_event": "Stop", "last_event_id": "evt_1",
			"last_event_occurred_at": exportedAt.Add(-time.Minute).Format(time.RFC3339Nano),
			"last_activity_at":       exportedAt.Add(-time.Second).Format(time.RFC3339Nano),
		}
	}
	newCtx := func() *importCtx {
		ic := newImportCtx("acc_1")
		ic.exportedAt = exportedAt
		ic.agents["agent_1"] = true
		return ic
	}

	tests := []struct {
		name   string
		mutate func(map[string]any)
		want   string
	}{
		{name: "foreign agent", mutate: func(row map[string]any) { row["agent_id"] = "agent_other" }, want: "not present"},
		{name: "missing event timestamp", mutate: func(row map[string]any) { delete(row, "last_event_occurred_at") }, want: "last_event_occurred_at"},
		{name: "future event timestamp", mutate: func(row map[string]any) {
			row["last_event_occurred_at"] = exportedAt.Add(time.Second).Format(time.RFC3339Nano)
		}, want: "later than manifest"},
		{name: "future activity timestamp", mutate: func(row map[string]any) {
			row["last_activity_at"] = exportedAt.Add(time.Second).Format(time.RFC3339Nano)
		}, want: "later than manifest"},
		{name: "activity before event", mutate: func(row map[string]any) {
			row["last_activity_at"] = exportedAt.Add(-2 * time.Minute).Format(time.RFC3339Nano)
		}, want: "precedes last_event_occurred_at"},
		{name: "trimmed runtime", mutate: func(row map[string]any) { row["runtime"] = " codex " }, want: "canonical clean label"},
		{name: "unsafe location", mutate: func(row map[string]any) { row["location"] = "home\nsecret" }, want: "canonical clean label"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			row := validRow()
			tc.mutate(row)
			err := newCtx().validateAndRecord("agent_activity", row)
			if err == nil || !errors.Is(err, ErrArchiveContent) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want ErrArchiveContent containing %q", err, tc.want)
			}
		})
	}

	ic := newCtx()
	if err := ic.validateAndRecord("agent_activity", validRow()); err != nil {
		t.Fatalf("first valid projection = %v", err)
	}
	duplicate := validRow()
	duplicate["last_event_id"] = "evt_2"
	if err := ic.validateAndRecord("agent_activity", duplicate); err == nil ||
		!errors.Is(err, ErrArchiveContent) || !strings.Contains(err.Error(), "duplicates") {
		t.Fatalf("duplicate projection error = %v", err)
	}
}
