package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/agenttui"
	"github.com/witwave-ai/witself/internal/dashboard"
)

func TestAccountConsoleBothContextReuse(t *testing.T) {
	for _, name := range []string{"agent-agent", "manager-manager", "agent-manager", "manager-agent", "different-operator", "forged-agent-registry", "forged-manager-registry", "revoked-session", "revoked-caller", "legacy-agent", "legacy-manager", "legacy-claimed-manager", "viewer-redirect", "viewer-oversized", "viewer-unknown", "replacement"} {
		t.Run(name, func(t *testing.T) {
			consoleTestHome(t)
			conn, manager, revoked := accountTestBackend(t)
			caller := manager
			binding := &dashboard.ViewerBinding{AccountID: manager.Identity.AccountID, OperatorID: manager.Identity.OperatorID, Role: manager.Identity.Role}
			liveBinding, registryBinding := binding, binding
			available, legacy := true, false
			want := false
			switch name {
			case "agent-agent":
				caller = nil
				liveBinding = nil
				registryBinding = nil
				available = false
				want = true
			case "manager-manager":
				want = true
			case "agent-manager":
				caller = nil
			case "manager-agent":
				liveBinding = nil
				registryBinding = nil
				available = false
			case "different-operator":
				other := *binding
				other.OperatorID = "op_other"
				liveBinding = &other
				registryBinding = &other
			case "forged-agent-registry":
				caller = nil
				registryBinding = nil
			case "forged-manager-registry":
				liveBinding = nil
				available = false
			case "revoked-session":
				caller = nil
				registryBinding = nil
				available = false
			case "revoked-caller":
				revoked.Store(true)
			case "legacy-agent":
				caller = nil
				liveBinding = nil
				registryBinding = nil
				legacy = true
				want = true
			case "legacy-manager":
				registryBinding = nil
				legacy = true
			case "legacy-claimed-manager":
				caller = nil
				legacy = true
			}
			identity := consoleTestIdentity()
			var entry dashboard.RegistryEntry
			entry = consoleTestEntry(t, conn, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(dashboard.MarkerHeader, dashboard.RegistrySchemaVersion)
				if r.URL.Path == "/" {
					http.SetCookie(w, &http.Cookie{Name: "session", Value: "synthetic"})
					w.Header().Set("Location", "/")
					w.WriteHeader(303)
					return
				}
				cookie, err := r.Cookie("session")
				if err != nil || cookie.Value != "synthetic" {
					t.Error("viewer was not authenticated")
					w.WriteHeader(401)
					return
				}
				if r.URL.Path == "/api/self" {
					_ = json.NewEncoder(w).Encode(map[string]any{"identity": identity})
					return
				}
				if r.URL.Path != "/api/console/viewer" {
					t.Error("arbitrary probe route")
					w.WriteHeader(404)
					return
				}
				switch {
				case legacy:
					w.WriteHeader(404)
					return
				case name == "viewer-redirect":
					w.Header().Set("Location", conn.Endpoint+"/v1/whoami")
					w.WriteHeader(302)
					return
				case name == "viewer-oversized":
					_, _ = w.Write([]byte(strings.Repeat("x", 4097)))
					return
				case name == "viewer-unknown":
					_, _ = w.Write([]byte(`{}`))
					return
				case name == "replacement":
					replacement := entry
					other := *binding
					other.OperatorID = "op_replacement"
					replacement.Manager = &other
					if dashboard.WriteRegistryEntry(replacement) != nil {
						t.Error("replace registry")
					}
				}
				_ = json.NewEncoder(w).Encode(dashboard.ViewerContext{SchemaVersion: dashboard.ViewerSchema, AccountID: identity.AccountID, RealmID: identity.RealmID, AgentID: identity.AgentID, Manager: liveBinding, Available: available})
			})
			entry.Manager = registryBinding
			if !legacy {
				entry.ViewerContract = dashboard.ViewerSchema
			}
			consoleTestWriteEntry(t, entry)
			controller := newTUIConsole(context.Background(), conn, identity, time.Second, accountConsoleOptions{Manager: caller, Roots: accountConsoleLocalRoots()})
			defer func() { _ = controller.Close() }()
			controller.opener = func(context.Context, string) error { t.Error("conflicting console opened"); return nil }
			status, err := controller.Status(context.Background())
			if want {
				if err != nil || status.State != agenttui.ConsoleRunning {
					t.Fatal("compatible context refused")
				}
			} else {
				if err != errConsoleConflict || status.State != agenttui.ConsoleConflict {
					t.Fatal("conflicting context reused")
				}
				if name != "replacement" {
					if _, err := controller.Open(context.Background()); err == nil {
						t.Fatal("conflicting context opened")
					}
				}
			}
			current, err := dashboard.ReadRegistryInstance(identity.AgentID)
			if err != nil || current.PID != entry.PID {
				t.Fatal("external process registry lost")
			}
		})
	}
}
