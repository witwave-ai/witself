package dashboard

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// The HEAD reader predates manager/viewer metadata and accepts unknown JSON
// fields. Keep its complete projection here to exercise old status serialization
// as well as the credential field used by old TUI discovery.
type legacyRegistryEntry struct {
	SchemaVersion string    `json:"schema_version"`
	AgentID       string    `json:"agent_id"`
	AgentName     string    `json:"agent_name"`
	Account       string    `json:"account"`
	AccountID     string    `json:"account_id,omitempty"`
	RealmID       string    `json:"realm_id,omitempty"`
	Endpoint      string    `json:"endpoint,omitempty"`
	Realm         string    `json:"realm"`
	Port          int       `json:"port"`
	PID           int       `json:"pid"`
	URL           string    `json:"url"`
	AccessURL     string    `json:"access_url"`
	StartedAt     time.Time `json:"started_at"`
}

func TestRegistryManagerLegacyReaderCompatibility(t *testing.T) {
	for _, manager := range []bool{false, true} {
		t.Run(map[bool]string{false: "agent", true: "manager"}[manager], func(t *testing.T) {
			t.Setenv("WITSELF_HOME", t.TempDir())
			entry := registryInstanceFixture()
			if manager {
				entry.Manager = &ViewerBinding{AccountID: entry.AccountID, OperatorID: "op_synthetic", Role: "account_owner"}
				entry.ViewerContract = ViewerSchema
			}
			if err := WriteRegistryEntry(entry); err != nil {
				t.Fatal(err)
			}
			path, _ := RegistryPath(entry.AgentID)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var legacy legacyRegistryEntry
			if err := json.Unmarshal(raw, &legacy); err != nil {
				t.Fatal(err)
			}
			status, err := json.Marshal(struct {
				legacyRegistryEntry
				Live bool `json:"live"`
			}{legacy, true})
			if err != nil {
				t.Fatal(err)
			}
			if manager {
				if legacy.AccessURL != "" || strings.Contains(string(status), "?token=") {
					t.Fatal("old status reader disclosed manager opening authority")
				}
				// HEAD TUI validEntry requires this exact token URL prefix before
				// attempting identity verification or launching any browser.
				if strings.HasPrefix(legacy.AccessURL, legacy.URL+"?token=") {
					t.Fatal("old TUI reader can reuse manager opening authority")
				}
			} else if legacy.AccessURL != entry.AccessURL {
				t.Fatal("agent-only legacy access changed")
			}
			for _, read := range []func(string) (RegistryEntry, error){ReadRegistryEntry, ReadRegistryInstance} {
				current, err := read(entry.AgentID)
				if err != nil || !SameRegistryInstance(current, entry) {
					t.Fatal("current reader lost private instance fence")
				}
			}
			entries, err := ListRegistryEntries()
			if err != nil || len(entries) != 1 || !SameRegistryInstance(entries[0], entry) {
				t.Fatal("current list reader lost private instance fence")
			}
			if removed, err := ReleaseRegistryInstance(context.Background(), entry); err != nil || !removed {
				t.Fatal("current owner cannot release manager record")
			}
		})
	}
}

func TestRegistryManagerCredentialBinding(t *testing.T) {
	for _, raw := range []string{
		`{"manager_access_url":"private"}`,
		`{"manager":{},"access_url":"legacy","manager_access_url":"private"}`,
	} {
		var entry RegistryEntry
		if decodeRegistryRecord([]byte(raw), &entry) == nil {
			t.Fatal("ambiguous or unbound manager opening authority accepted")
		}
	}
}
