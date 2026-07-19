package main

import (
	"errors"
	"testing"

	"github.com/witwave-ai/witself/internal/server"
	"github.com/witwave-ai/witself/internal/store"
)

func TestConfigureSecretsWiresCompleteVertical(t *testing.T) {
	var cfg server.Config
	configureSecrets(&cfg, &store.Store{})
	callbacks := map[string]bool{
		"get key": cfg.GetCurrentVaultKey != nil, "register key": cfg.RegisterVaultKey != nil,
		"create": cfg.CreateSecret != nil, "list": cfg.ListSecrets != nil, "get": cfg.GetSecret != nil,
		"archive": cfg.ArchiveSecret != nil, "restore": cfg.RestoreSecret != nil,
		"access": cfg.AccessSecretField != nil,
	}
	for name, wired := range callbacks {
		if !wired {
			t.Errorf("%s callback is nil", name)
		}
	}
}

func TestMapSecretError(t *testing.T) {
	tests := []struct {
		name string
		in   error
		want error
	}{
		{name: "input", in: store.ErrSecretInputInvalid, want: server.ErrBadInput},
		{name: "forbidden", in: store.ErrSecretForbidden, want: server.ErrForbidden},
		{name: "inactive", in: store.ErrAccountNotActive, want: server.ErrForbidden},
		{name: "agent missing", in: store.ErrAgentNotFound, want: server.ErrForbidden},
		{name: "account missing", in: store.ErrAccountNotFound, want: server.ErrNotFound},
		{name: "secret missing", in: store.ErrSecretNotFound, want: server.ErrNotFound},
		{name: "field missing", in: store.ErrSecretFieldNotFound, want: server.ErrNotFound},
		{name: "idempotency", in: store.ErrSecretIdempotencyConflict, want: server.ErrIdempotencyConflict},
		{name: "key unavailable", in: store.ErrVaultKeyUnavailable, want: server.ErrSecretVaultKeyUnavailable},
		{name: "key mismatch", in: store.ErrVaultKeyMismatch, want: server.ErrSecretVaultKeyMismatch},
		{name: "secret conflict", in: store.ErrSecretConflict, want: server.ErrConflict},
		{name: "key conflict", in: store.ErrVaultKeyConflict, want: server.ErrConflict},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := mapSecretError(test.in); !errors.Is(got, test.want) {
				t.Fatalf("mapSecretError(%v) = %v, want %v", test.in, got, test.want)
			}
		})
	}
}
