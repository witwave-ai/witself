package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var planT0 = time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)

// TestPlanStateRoundTrip pins persistence: what save writes, load
// returns — a dashboard restart keeps armed plans armed.
func TestPlanStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "infra-previews.json")
	plans := map[string]time.Time{
		"aws-sandbox-usw2-dev": planT0.Add(-10 * time.Minute),
		"gcp-sandbox-use1-dev": planT0.Add(-2 * time.Minute),
	}
	savePlanState(path, plans, planT0)
	got := loadPlanState(path, planT0)
	if len(got) != 2 {
		t.Fatalf("round trip lost entries: %v", got)
	}
	if !got["aws-sandbox-usw2-dev"].Equal(plans["aws-sandbox-usw2-dev"]) {
		t.Fatalf("timestamp changed in round trip: %v", got["aws-sandbox-usw2-dev"])
	}
}

// TestPlanStateExpiryOnLoad pins the staleness window across a
// restart: a preview older than previewTTL must NOT re-arm `u` when
// the dashboard comes back up.
func TestPlanStateExpiryOnLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "infra-previews.json")
	savePlanState(path, map[string]time.Time{
		"fresh-cell": planT0.Add(-previewTTL + time.Minute),
		"stale-cell": planT0.Add(-previewTTL - time.Minute),
	}, planT0.Add(-previewTTL+time.Minute)) // save while both are young enough to persist
	got := loadPlanState(path, planT0)
	if _, ok := got["fresh-cell"]; !ok {
		t.Fatal("fresh entry must survive the load")
	}
	if _, ok := got["stale-cell"]; ok {
		t.Fatal("expired entry must be dropped on load")
	}
}

// TestPlanStateTolerant pins the failure posture: missing or corrupt
// state files yield an empty map, never an error or a crash — the
// worst outcome of losing plan state is re-running a preview.
func TestPlanStateTolerant(t *testing.T) {
	if got := loadPlanState(filepath.Join(t.TempDir(), "nope.json"), planT0); len(got) != 0 {
		t.Fatalf("missing file must yield empty map, got %v", got)
	}
	bad := filepath.Join(t.TempDir(), "corrupt.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := loadPlanState(bad, planT0); len(got) != 0 {
		t.Fatalf("corrupt file must yield empty map, got %v", got)
	}
}

// TestPlanArmedTTL pins the in-session expiry: a dashboard left open
// past previewTTL disarms `u` exactly like a restart would — the
// safety rationale (the operator saw a RECENT diff) doesn't care
// whether the process restarted.
func TestPlanArmedTTL(t *testing.T) {
	m := dashboardModel{
		now:         func() time.Time { return planT0 },
		previewSeen: map[string]time.Time{"cell-young": planT0.Add(-59 * time.Minute), "cell-old": planT0.Add(-61 * time.Minute)},
	}
	if !m.planArmed("cell-young") {
		t.Fatal("59-minute-old preview must still arm u")
	}
	if m.planArmed("cell-old") {
		t.Fatal("61-minute-old preview must be expired")
	}
	if m.planArmed("cell-never") {
		t.Fatal("never-previewed cell must not be armed")
	}
}

// TestOpDonePersistsPlanState pins the write trigger: a successful
// preview lands on disk immediately, so a crash or restart right
// after still remembers the armed plan.
func TestOpDonePersistsPlanState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "infra-previews.json")
	m := dashboardModel{
		ctx: context.Background(), cli: fakeSource{},
		now:      func() time.Time { return planT0 },
		planPath: path,
		op:       &opRun{kind: opPreview, cell: "aws-sandbox-usw2-dev"},
	}
	next, _ := m.Update(opDoneMsg{cell: "aws-sandbox-usw2-dev", err: nil})
	_ = next
	got := loadPlanState(path, planT0)
	if _, ok := got["aws-sandbox-usw2-dev"]; !ok {
		t.Fatal("successful preview must persist to the plan state file")
	}
	// And consumption clears it from disk too.
	m2 := next.(dashboardModel)
	m2.op = &opRun{kind: opUp, cell: "aws-sandbox-usw2-dev"}
	next2, _ := m2.Update(opDoneMsg{cell: "aws-sandbox-usw2-dev", err: nil})
	_ = next2
	got = loadPlanState(path, planT0)
	if _, ok := got["aws-sandbox-usw2-dev"]; ok {
		t.Fatal("successful up must clear the persisted plan — it was consumed")
	}
}

// TestExpiredPlanMessaging pins the operator-facing wording: pressing
// u on an expired plan explains WHY it refused (expired, not absent),
// and the context pane distinguishes the two states.
func TestExpiredPlanMessaging(t *testing.T) {
	states := []cellState{{name: "aws-sandbox-usw2-dev", entry: cellEntry{Cloud: strPtr("aws")}}}
	m := seedModel(states, 120, 30)
	m.previewSeen = map[string]time.Time{"aws-sandbox-usw2-dev": m.now().Add(-previewTTL - time.Minute)}

	next, _ := m.startOpKey("u")
	m2 := next.(dashboardModel)
	if m2.pending != nil {
		t.Fatal("expired plan must not open the up confirm")
	}
	if !strings.Contains(m2.status, "expired") {
		t.Fatalf("status must say the preview expired: %q", m2.status)
	}
	if !strings.Contains(m2.View(), "preview expired — press p again") {
		t.Fatal("context pane must show the expired-plan line")
	}
	// And the ◆ mark must be gone.
	if strings.Contains(m2.View(), "◆ aws-sandbox") {
		t.Fatal("expired plan must not show the armed mark")
	}
}
