package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestArgoHealthLevel pins the worst-wins aggregation: all Synced +
// Healthy is good, a Progressing app warns/degrades, and a Degraded
// app is bad regardless of the others.
func TestArgoHealthLevel(t *testing.T) {
	mk := func(name, sync, health string) argoApplication {
		var a argoApplication
		a.Metadata.Name = name
		a.Status.Sync.Status = sync
		a.Status.Health.Status = health
		return a
	}
	cases := []struct {
		name string
		apps []argoApplication
		want healthLevel
	}{
		{"none", nil, healthUnknown},
		{"all healthy", []argoApplication{mk("a", "Synced", "Healthy"), mk("b", "Synced", "Healthy")}, healthGood},
		{"progressing", []argoApplication{mk("a", "Synced", "Healthy"), mk("b", "Synced", "Progressing")}, healthDegraded},
		{"degraded wins", []argoApplication{mk("a", "Synced", "Progressing"), mk("b", "OutOfSync", "Degraded")}, healthBad},
		{"missing is bad", []argoApplication{mk("a", "Synced", "Missing")}, healthBad},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := argoHealth(tc.apps)
			if got.Level != tc.want {
				t.Fatalf("level = %d, want %d (detail %q)", got.Level, tc.want, got.Detail)
			}
		})
	}
	// The gauge counts must be populated: 1 of 2 healthy in the
	// "progressing" case.
	if got := argoHealth([]argoApplication{
		func() argoApplication {
			var a argoApplication
			a.Status.Sync.Status = "Synced"
			a.Status.Health.Status = "Healthy"
			return a
		}(),
		func() argoApplication {
			var a argoApplication
			a.Status.Sync.Status = "Synced"
			a.Status.Health.Status = "Progressing"
			return a
		}(),
	}); got.Have != 1 || got.Total != 2 {
		t.Fatalf("argo gauge counts = %d/%d, want 1/2", got.Have, got.Total)
	}
}

// TestDBStatusLevels pins the per-cloud status→level maps across the
// healthy, transitional, and failed states of each provider.
func TestDBStatusLevels(t *testing.T) {
	cases := []struct {
		name   string
		mapper func(string) healthLevel
		status string
		want   healthLevel
	}{
		{"rds available", awsDBLevel, "available", healthGood},
		{"rds backing-up", awsDBLevel, "backing-up", healthWarn},
		{"rds modifying caps", awsDBLevel, "MODIFYING", healthWarn},
		{"rds storage-full", awsDBLevel, "storage-full", healthDegraded},
		{"rds failed", awsDBLevel, "failed", healthBad},
		{"rds stopped", awsDBLevel, "stopped", healthBad},
		{"rds unknown", awsDBLevel, "who-knows", healthUnknown},
		{"sql runnable", gcpDBLevel, "RUNNABLE", healthGood},
		{"sql maintenance", gcpDBLevel, "MAINTENANCE", healthWarn},
		{"sql suspended", gcpDBLevel, "SUSPENDED", healthBad},
		{"sql failed", gcpDBLevel, "FAILED", healthBad},
		{"pg ready", azureDBLevel, "Ready", healthGood},
		{"pg updating", azureDBLevel, "Updating", healthWarn},
		{"pg stopped", azureDBLevel, "Stopped", healthDegraded},
		{"pg dropping", azureDBLevel, "Dropping", healthBad},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.mapper(tc.status); got != tc.want {
				t.Fatalf("%s: level = %d, want %d", tc.status, got, tc.want)
			}
		})
	}
}

// TestDBLevelWrap pins the wrapper: the raw status becomes the detail,
// and an empty status is unknown.
func TestDBLevelWrap(t *testing.T) {
	got := dbLevel("available", awsDBLevel)
	if got.Level != healthGood || got.Detail != "available" {
		t.Fatalf("wrap = %+v, want good/available", got)
	}
	if got := dbLevel("", awsDBLevel); got.Level != healthUnknown {
		t.Fatalf("empty status must be unknown, got %d", got.Level)
	}
}

// TestHealthLevelJSONRoundTrip pins the wire form: levels marshal to
// their stable string names and back, so the CLI and dashboard agree.
func TestHealthLevelJSONRoundTrip(t *testing.T) {
	report := cellHealthReport{
		Kubernetes: subsystemHealth{Level: healthGood, Detail: "apiserver ready", Have: 3, Total: 3},
		Database:   subsystemHealth{Level: healthUnknown, Detail: "not wired"},
		Argo:       subsystemHealth{Level: healthDegraded, Detail: "app x Progressing", Have: 2, Total: 3},
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"level":"good"`) || !strings.Contains(string(raw), `"level":"degraded"`) {
		t.Fatalf("levels must marshal as names: %s", raw)
	}
	var back cellHealthReport
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.Kubernetes.Level != healthGood || back.Argo.Level != healthDegraded || back.Database.Level != healthUnknown {
		t.Fatalf("round trip changed levels: %+v", back)
	}
}
