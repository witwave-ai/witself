package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/testenv"
)

func TestUpdateAccountEmailCASPostgres(t *testing.T) {
	dsn := testenv.RequirePostgres(t)
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}

	const originalEmail = "email-change-cas-original@witwave.ai"
	provisioned, err := st.ProvisionAccount(ctx, originalEmail, "email change CAS", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %t / %v", activated, err)
	}

	const matchingEmail = "email-change-cas-matching@witwave.ai"
	if err := st.UpdateAccountEmail(
		ctx, provisioned.AccountID, provisioned.OperatorID, originalEmail, matchingEmail,
	); err != nil {
		t.Fatalf("matching expected_current: %v", err)
	}
	account, err := st.GetAccount(ctx, provisioned.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if account.Email != matchingEmail {
		t.Fatalf("email after matching expected_current = %q, want %q", account.Email, matchingEmail)
	}

	const refusedEmail = "email-change-cas-refused@witwave.ai"
	err = st.UpdateAccountEmail(
		ctx, provisioned.AccountID, provisioned.OperatorID, originalEmail, refusedEmail,
	)
	if !errors.Is(err, ErrEmailChangedSinceRequest) {
		t.Fatalf("mismatching expected_current error = %v, want ErrEmailChangedSinceRequest", err)
	}
	account, err = st.GetAccount(ctx, provisioned.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if account.Email != matchingEmail {
		t.Fatalf("email after mismatching expected_current = %q, want unchanged %q", account.Email, matchingEmail)
	}

	const unguardedEmail = "email-change-cas-unguarded@witwave.ai"
	if err := st.UpdateAccountEmail(
		ctx, provisioned.AccountID, provisioned.OperatorID, "", unguardedEmail,
	); err != nil {
		t.Fatalf("empty expected_current: %v", err)
	}
	account, err = st.GetAccount(ctx, provisioned.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if account.Email != unguardedEmail {
		t.Fatalf("email after empty expected_current = %q, want %q", account.Email, unguardedEmail)
	}
}
