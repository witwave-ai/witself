package runtimeconfig

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type readinessStore struct {
	pingErr, schemaErr, accountErr error
	version                        int
	account                        string
}

func (s readinessStore) Ping(context.Context) error { return s.pingErr }
func (s readinessStore) AppliedSchemaVersion(context.Context) (int, error) {
	return s.version, s.schemaErr
}
func (s readinessStore) LocalBootstrapAccount(context.Context) (string, error) {
	return s.account, s.accountErr
}

func TestLocalReadinessDependencies(t *testing.T) {
	private := errors.New("private-database-error")
	for _, tc := range []struct {
		name     string
		store    BootstrapStore
		expected string
		ready    bool
	}{
		{name: "missing store"},
		{name: "ping failure", store: readinessStore{pingErr: private}},
		{name: "schema query failure", store: readinessStore{schemaErr: private}},
		{name: "older schema", store: readinessStore{version: 6, account: "private-account"}},
		{name: "newer schema", store: readinessStore{version: 8, account: "private-account"}},
		{name: "missing account", store: readinessStore{version: 7}},
		{name: "account query failure", store: readinessStore{version: 7, accountErr: private}},
		{name: "account mismatch", store: readinessStore{version: 7, account: "private-account"}, expected: "private-other-account"},
		{name: "ready unbound", store: readinessStore{version: 7, account: "private-account"}, ready: true},
		{name: "ready bound", store: readinessStore{version: 7, account: "private-account"}, expected: "private-account", ready: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckReady(context.Background(), tc.store, 7, tc.expected)
			if (err == nil) != tc.ready {
				t.Fatalf("ready=%t, want %t", err == nil, tc.ready)
			}
			if err != nil && strings.Contains(err.Error(), "private-") {
				t.Fatal("readiness disclosed a database value")
			}
		})
	}
}
