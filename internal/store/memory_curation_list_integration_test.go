package store

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestMemoryCurationDueQueuePriorityAndAgeOrderPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	provisioned, err := st.ProvisionAccount(ctx,
		"memory-curation-order@witwave.ai", "memory curation order", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %v / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "default")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "primary")
	if err != nil {
		t.Fatal(err)
	}
	p := Principal{Kind: PrincipalAgent, ID: agent.ID, AccountID: provisioned.AccountID,
		RealmID: realm.ID, AccountStatus: "active", AccessProfile: AccessProfileFull}

	now := time.Now().UTC()
	type requestSpec struct {
		key      string
		priority int
		dueAt    time.Time
	}
	specs := []requestSpec{
		{key: "ordinary-old", priority: 0, dueAt: now.Add(-20 * time.Minute)},
		{key: "priority-newer", priority: 10, dueAt: now.Add(-10 * time.Minute)},
		{key: "priority-oldest", priority: 10, dueAt: now.Add(-30 * time.Minute)},
	}
	ids := make(map[string]string, len(specs))
	for index := range specs {
		spec := specs[index]
		created, err := st.RequestCuration(ctx, p, RequestMemoryCurationInput{
			Scope:         MemoryCurationScope{Sources: []string{MemoryCurationSourceMemory}},
			CoalescingKey: spec.key, TriggerReason: "manual_refine",
			Priority: spec.priority, DueAt: &spec.dueAt,
			IdempotencyKey: "order-" + spec.key,
		})
		if err != nil {
			t.Fatal(err)
		}
		ids[spec.key] = created.Request.ID
	}

	want := []string{ids["priority-oldest"], ids["priority-newer"], ids["ordinary-old"]}
	var got []string
	var databaseBefore time.Time
	if err := st.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&databaseBefore); err != nil {
		t.Fatal(err)
	}
	cursor := ""
	firstCursor := ""
	for {
		page, err := st.ListCurationRequests(ctx, p, MemoryCurationRequestListOptions{Limit: 1, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Requests) != 1 {
			t.Fatalf("page requests = %#v", page.Requests)
		}
		got = append(got, page.Requests[0].ID)
		if page.NextCursor == "" {
			break
		}
		if firstCursor == "" {
			firstCursor = page.NextCursor
		}
		cursor = page.NextCursor
	}
	var databaseAfter time.Time
	if err := st.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&databaseAfter); err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeMemoryCurationRequestCursor(firstCursor, "", false, false)
	if err != nil || decoded.AsOf.Before(databaseBefore) || decoded.AsOf.After(databaseAfter) {
		t.Fatalf("queue cursor database clock = %#v / %v, want between %s and %s",
			decoded, err, databaseBefore, databaseAfter)
	}
	if len(got) != len(want) {
		t.Fatalf("queue length = %d, want %d: %v", len(got), len(want), got)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("queue order = %v, want %v", got, want)
		}
	}
}

func TestMemoryCurationDueQueueExcludesSensitiveWithoutHidingFullTokenTranscriptWorkPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	provisioned, err := st.ProvisionAccount(ctx,
		"memory-curation-eligibility@witwave.ai", "memory curation eligibility", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %v / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "default")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "primary")
	if err != nil {
		t.Fatal(err)
	}
	full := Principal{Kind: PrincipalAgent, ID: agent.ID, AccountID: provisioned.AccountID,
		RealmID: realm.ID, AccountStatus: "active", AccessProfile: AccessProfileFull}

	now := time.Now().UTC().Add(-time.Minute)
	type requestSpec struct {
		key              string
		priority         int
		source           string
		includeSensitive bool
	}
	specs := []requestSpec{
		{key: "sensitive-head", priority: 30, source: MemoryCurationSourceMemory, includeSensitive: true},
		{key: "transcript", priority: 20, source: MemoryCurationSourceTranscript},
		{key: "ordinary", priority: 10, source: MemoryCurationSourceMemory},
	}
	ids := make(map[string]string, len(specs))
	for _, spec := range specs {
		created, err := st.RequestCuration(ctx, full, RequestMemoryCurationInput{
			Scope: MemoryCurationScope{
				Sources: []string{spec.source}, IncludeSensitive: spec.includeSensitive,
			},
			CoalescingKey: spec.key, TriggerReason: "manual_refine", Priority: spec.priority,
			DueAt: &now, IdempotencyKey: "eligibility-" + spec.key,
		})
		if err != nil {
			t.Fatal(err)
		}
		ids[spec.key] = created.Request.ID
	}

	unfiltered, err := st.ListCurationRequests(ctx, full, MemoryCurationRequestListOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(unfiltered.Requests) != 3 || unfiltered.Requests[0].ID != ids["sensitive-head"] {
		t.Fatalf("unfiltered full-token queue = %#v", unfiltered.Requests)
	}

	var eligible []string
	cursor := ""
	for {
		page, err := st.ListCurationRequests(ctx, full, MemoryCurationRequestListOptions{
			Limit: 1, Cursor: cursor, ExcludeSensitive: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Requests) != 1 {
			t.Fatalf("eligible full-token page = %#v", page.Requests)
		}
		eligible = append(eligible, page.Requests[0].ID)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(eligible) != 2 || eligible[0] != ids["transcript"] || eligible[1] != ids["ordinary"] {
		t.Fatalf("eligible full-token queue = %v", eligible)
	}

	restricted := full
	restricted.AccessProfile = AccessProfileCuratorPreview
	restrictedPage, err := st.ListCurationRequests(ctx, restricted, MemoryCurationRequestListOptions{
		Limit: 10, ExcludeSensitive: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(restrictedPage.Requests) != 1 || restrictedPage.Requests[0].ID != ids["ordinary"] {
		t.Fatalf("restricted queue = %#v", restrictedPage.Requests)
	}
}
