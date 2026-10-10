package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/store"
	"github.com/witwave-ai/witself/internal/testenv"
)

func TestServerStoreOptionsImportTelemetry(t *testing.T) {
	dsn := testenv.RequirePostgres(t)
	var log bytes.Buffer
	ctx := context.Background()
	st, err := store.Open(ctx, dsn, serverStoreOptions(false, store.DefaultSupportTicketRateLimitConfig(), &log)...)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ImportAccount(ctx, "acc_zzzzzzzzzzzzzzzz", strings.NewReader("not an archive")); err == nil {
		t.Fatal("malformed archive accepted")
	}
	output := log.String()
	if strings.Count(output, "\n") != 1 || !strings.HasPrefix(output, `witself-server: account import memory purpose="import" account_id="acc_zzzzzzzzzzzzzzzz" `) ||
		!strings.Contains(output, `outcome="error" entries=0 `) {
		t.Fatal("production store options did not emit exactly one failed-import summary")
	}
}
