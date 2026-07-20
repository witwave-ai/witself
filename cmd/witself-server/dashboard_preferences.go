package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/witwave-ai/witself/internal/server"
	"github.com/witwave-ai/witself/internal/store"
)

// configureDashboardPreferences binds the per-agent dashboard UI preference
// row to PostgreSQL. The row is namespaced UI state (theme choice), never
// agent content; strict validation and the size cap live in the store.
func configureDashboardPreferences(cfg *server.Config, st *store.Store) {
	cfg.GetDashboardPreferences = func(ctx context.Context, p server.DomainPrincipal) (*server.DashboardPreferences, error) {
		prefs, err := st.GetDashboardPreferences(ctx, toStorePrincipal(p))
		if err != nil {
			return nil, mapDashboardPreferencesError(err)
		}
		if prefs == nil {
			return nil, nil
		}
		out := toServerDashboardPreferences(*prefs)
		return &out, nil
	}
	cfg.PutDashboardPreferences = func(ctx context.Context, p server.DomainPrincipal, in server.PutDashboardPreferencesRequest) (server.DashboardPreferences, error) {
		prefs, err := st.PutDashboardPreferences(ctx, toStorePrincipal(p), json.RawMessage(in.Prefs))
		if err != nil {
			return server.DashboardPreferences{}, mapDashboardPreferencesError(err)
		}
		return toServerDashboardPreferences(prefs), nil
	}
}

func toServerDashboardPreferences(value store.DashboardPreferences) server.DashboardPreferences {
	return server.DashboardPreferences{
		AgentID: value.AgentID, Prefs: value.Prefs, UpdatedAt: value.UpdatedAt,
	}
}

func mapDashboardPreferencesError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrDashboardPreferencesInvalid):
		return fmt.Errorf("%w: %v", server.ErrBadInput, err)
	case errors.Is(err, store.ErrDashboardPreferencesForbidden):
		return server.ErrForbidden
	case errors.Is(err, store.ErrAgentNotFound):
		return server.ErrNotFound
	default:
		return err
	}
}
