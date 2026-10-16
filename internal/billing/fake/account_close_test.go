package fake

import (
	"context"
	"testing"
)

func TestAccountClosureStopsRenewalAndCheckoutWithoutRefund(t *testing.T) {
	for _, interactive := range []bool{false, true} {
		f := New(Config{Prices: map[string]int64{"standard": 2000}, Interactive: interactive})
		ctx := context.Background()
		id, err := f.EnsureCustomer(ctx, "acct_owned", "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Subscribe(ctx, id, "standard"); err != nil {
			t.Fatal(err)
		}
		before, err := f.ListPayments(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if f.CancelForAccountClosure(ctx, id, "acct_other", "op_close") == nil {
			t.Fatal("accepted wrong account")
		}
		for range 2 {
			if err := f.CancelForAccountClosure(ctx, id, "acct_owned", "op_close"); err != nil {
				t.Fatal(err)
			}
		}
		if next, err := f.NextCharge(ctx, id); err != nil || next != nil {
			t.Fatalf("renewal survived closure: %v %v", next, err)
		}
		if _, err := f.Complete(id); err == nil {
			t.Fatal("old checkout survived closure")
		}
		after, err := f.ListPayments(ctx, id)
		if err != nil || len(before) != len(after) {
			t.Fatalf("closure modified payment history: %v", err)
		}
	}
}
