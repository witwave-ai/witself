package main

import (
	"errors"
	"testing"

	"github.com/witwave-ai/witself/internal/server"
	"github.com/witwave-ai/witself/internal/store"
)

func TestConfigureDashboardPreferencesWiresBothCallbacks(t *testing.T) {
	var cfg server.Config
	configureDashboardPreferences(&cfg, &store.Store{})
	if cfg.GetDashboardPreferences == nil {
		t.Error("get callback is nil")
	}
	if cfg.PutDashboardPreferences == nil {
		t.Error("put callback is nil")
	}
}

func TestMapDashboardPreferencesError(t *testing.T) {
	tests := []struct {
		name string
		in   error
		want error
	}{
		{name: "invalid", in: store.ErrDashboardPreferencesInvalid, want: server.ErrBadInput},
		{name: "forbidden", in: store.ErrDashboardPreferencesForbidden, want: server.ErrForbidden},
		{name: "agent missing", in: store.ErrAgentNotFound, want: server.ErrNotFound},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := mapDashboardPreferencesError(test.in); !errors.Is(got, test.want) {
				t.Fatalf("mapDashboardPreferencesError(%v) = %v, want %v", test.in, got, test.want)
			}
		})
	}
	if mapDashboardPreferencesError(nil) != nil {
		t.Fatal("nil must map to nil")
	}
}
