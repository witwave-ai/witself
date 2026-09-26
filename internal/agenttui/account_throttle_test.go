package agenttui

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/dashboard"
)

func TestAccountAuthorityPollingRate(t *testing.T) {
	for _, active := range []bool{false, true} {
		name := "idle"
		if active {
			name = "account"
		}
		t.Run(name, func(t *testing.T) {
			f := newFake()
			m := testModel(t, f)
			now := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
			m.account.now = func() time.Time { return now }
			accountSettle(t, m, m.accountPollAuthority())
			m.account.mode = active
			for i := 1; i < 30; i++ {
				now = now.Add(2 * time.Second)
				accountSettle(t, m, m.accountPollAuthority())
			}
			want := 2
			if active {
				want = 30
			}
			if got := f.count(dashboard.ResourceAccountContext); got != want {
				t.Fatalf("checks per minute = %d, want %d", got, want)
			}
		})
	}
}

func TestAccountAuthorityBackoffAndRecovery(t *testing.T) {
	f := newFake()
	fail := false
	f.read = func(ctx context.Context, r dashboard.ReadRequest) (json.RawMessage, error) {
		if r.Resource == dashboard.ResourceAccountContext && fail {
			return nil, errors.New("synthetic unavailable")
		}
		return f.Source.Read(ctx, r)
	}
	m := testModel(t, f)
	now := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	m.account.now = func() time.Time { return now }
	authorizeAccount(t, m)
	m.account.mode = true
	fail = true
	now = now.Add(2 * time.Second)
	accountSettle(t, m, m.accountPollAuthority())
	if m.account.context.available() || m.account.mode {
		t.Fatal("failure retained account access")
	}
	for _, delay := range []time.Duration{2, 4, 8, 16, 32, 60, 60} {
		delay *= time.Second
		if m.account.authorityBackoff != delay {
			t.Fatalf("backoff = %s, want %s", m.account.authorityBackoff, delay)
		}
		before := f.count(dashboard.ResourceAccountContext)
		now = now.Add(delay - time.Nanosecond)
		accountSettle(t, m, m.accountPollAuthority())
		if f.count(dashboard.ResourceAccountContext) != before {
			t.Fatal("request before retry deadline")
		}
		now = now.Add(time.Nanosecond)
		accountSettle(t, m, m.accountPollAuthority())
		if f.count(dashboard.ResourceAccountContext) != before+1 {
			t.Fatal("retry deadline not honored")
		}
	}
	fail = false
	now = now.Add(60 * time.Second)
	accountSettle(t, m, m.accountPollAuthority())
	if !m.account.context.available() || m.account.authorityBackoff != 0 {
		t.Fatal("success did not reset backoff")
	}
	m.account.mode = true
	now = now.Add(2 * time.Second)
	before := f.count(dashboard.ResourceAccountContext)
	accountSettle(t, m, m.accountPollAuthority())
	if f.count(dashboard.ResourceAccountContext) != before+1 {
		t.Fatal("active polling did not resume")
	}
}
