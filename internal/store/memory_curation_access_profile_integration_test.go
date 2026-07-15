package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"
)

func TestMemoryCurationCuratorSensitiveScopePostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}

	t.Run("restricted profiles cannot request or consume sensitive curation", func(t *testing.T) {
		ctx, st, full := newMemoryCurationAccessProfileStore(t, dsn)
		preview := full
		preview.AccessProfile = AccessProfileCuratorPreview
		apply := full
		apply.AccessProfile = AccessProfileCuratorApply

		for _, restricted := range []Principal{preview, apply} {
			if _, err := st.RequestCuration(ctx, restricted, RequestMemoryCurationInput{
				Scope:          MemoryCurationScope{IncludeSensitive: true},
				CoalescingKey:  "restricted_request",
				TriggerReason:  "manual_refine",
				IdempotencyKey: "restricted-request-" + restricted.AccessProfile,
			}); !errors.Is(err, ErrMemoryCurationForbidden) {
				t.Fatalf("%s request error = %v", restricted.AccessProfile, err)
			}
		}

		requested, err := st.RequestCuration(ctx, full, RequestMemoryCurationInput{
			Scope:          MemoryCurationScope{IncludeSensitive: true},
			CoalescingKey:  "sensitive_scope",
			TriggerReason:  "manual_refine",
			IdempotencyKey: "sensitive-request-full",
		})
		if err != nil {
			t.Fatal(err)
		}
		if !requested.Request.Scope.IncludeSensitive {
			t.Fatal("persisted request lost include_sensitive")
		}
		nonSensitive, err := st.RequestCuration(ctx, full, RequestMemoryCurationInput{
			Scope: MemoryCurationScope{
				Sources: []string{MemoryCurationSourceMemory}, IncludeSensitive: false,
			},
			CoalescingKey:  "ordinary_scope",
			TriggerReason:  "manual_refine",
			IdempotencyKey: "ordinary-request-full",
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, restricted := range []Principal{preview, apply} {
			page, err := st.ListCurationRequests(ctx, restricted, MemoryCurationRequestListOptions{Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Requests) != 1 || page.Requests[0].ID != nonSensitive.Request.ID ||
				page.Requests[0].Scope.IncludeSensitive {
				t.Fatalf("%s due requests = %#v", restricted.AccessProfile, page.Requests)
			}
		}

		for _, restricted := range []Principal{preview, apply} {
			if _, err := st.StartCuration(ctx, restricted, StartMemoryCurationInput{
				RequestID:      requested.Request.ID,
				LeaseDuration:  time.Minute,
				IdempotencyKey: "sensitive-start-" + restricted.AccessProfile,
			}); !errors.Is(err, ErrMemoryCurationForbidden) {
				t.Fatalf("%s start error = %v", restricted.AccessProfile, err)
			}
		}
		persisted, err := st.GetCurationRequest(ctx, full, requested.Request.ID)
		if err != nil {
			t.Fatal(err)
		}
		status, err := st.GetCurationStatus(ctx, full, "")
		if err != nil {
			t.Fatal(err)
		}
		if persisted.State != MemoryCurationRequestQueued || persisted.ClaimedRunID != "" ||
			status.Lane.ActiveRunID != "" {
			t.Fatalf("restricted start mutated request/lane: request=%#v lane=%#v", persisted, status.Lane)
		}

		started, err := st.StartCuration(ctx, full, StartMemoryCurationInput{
			RequestID: requested.Request.ID, LeaseDuration: time.Minute,
			IdempotencyKey: "sensitive-start-full",
		})
		if err != nil {
			t.Fatal(err)
		}
		draft := marshalEmptyCurationPlanForAccessProfile(t)
		for _, restricted := range []Principal{preview, apply} {
			if _, err := st.GetCurationRunInputs(ctx, restricted, started.Run.ID,
				started.Run.FencingGeneration, "", 0); !errors.Is(err, ErrMemoryCurationForbidden) {
				t.Fatalf("%s inputs error = %v", restricted.AccessProfile, err)
			}
			if _, err := st.PlanCuration(ctx, restricted, started.Run.ID,
				PlanMemoryCurationInput{
					FencingGeneration: started.Run.FencingGeneration,
					Draft:             draft,
					IdempotencyKey:    "sensitive-plan-" + restricted.AccessProfile,
				}); !errors.Is(err, ErrMemoryCurationForbidden) {
				t.Fatalf("%s plan error = %v", restricted.AccessProfile, err)
			}
		}

		planned, err := st.PlanCuration(ctx, full, started.Run.ID, PlanMemoryCurationInput{
			FencingGeneration: started.Run.FencingGeneration,
			Draft:             draft,
			IdempotencyKey:    "sensitive-plan-full",
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, restricted := range []Principal{preview, apply} {
			if _, err := st.ApplyCuration(ctx, restricted, started.Run.ID,
				ApplyMemoryCurationInput{
					FencingGeneration: started.Run.FencingGeneration,
					PlanRevision:      planned.Plan.PlanRevision,
					PlanHash:          planned.Receipt.PlanHash,
					IdempotencyKey:    "sensitive-apply-" + restricted.AccessProfile,
				}); !errors.Is(err, ErrMemoryCurationForbidden) {
				t.Fatalf("%s apply error = %v", restricted.AccessProfile, err)
			}
		}
		run, err := st.GetCurationRun(ctx, full, started.Run.ID)
		if err != nil {
			t.Fatal(err)
		}
		persisted, err = st.GetCurationRequest(ctx, full, requested.Request.ID)
		if err != nil {
			t.Fatal(err)
		}
		if run.State != MemoryCurationRunPlanned || persisted.State != MemoryCurationRequestClaimed {
			t.Fatalf("restricted calls mutated planned curation: run=%#v request=%#v", run, persisted)
		}
	})

	t.Run("non-sensitive curator apply profile can complete the path", func(t *testing.T) {
		ctx, st, full := newMemoryCurationAccessProfileStore(t, dsn)
		curator := full
		curator.AccessProfile = AccessProfileCuratorApply

		requested, err := st.RequestCuration(ctx, full, RequestMemoryCurationInput{
			Scope: MemoryCurationScope{
				Sources: []string{MemoryCurationSourceMemory}, IncludeSensitive: false,
			},
			CoalescingKey:  "non_sensitive_scope",
			TriggerReason:  "manual_refine",
			IdempotencyKey: "non-sensitive-request-full",
		})
		if err != nil {
			t.Fatal(err)
		}
		started, err := st.StartCuration(ctx, curator, StartMemoryCurationInput{
			RequestID: requested.Request.ID, LeaseDuration: time.Minute,
			IdempotencyKey: "non-sensitive-start-curator",
		})
		if err != nil {
			t.Fatal(err)
		}
		page, err := st.GetCurationRunInputs(ctx, curator, started.Run.ID,
			started.Run.FencingGeneration, "", 0)
		if err != nil {
			t.Fatal(err)
		}
		if page.Run.ID != started.Run.ID {
			t.Fatalf("input page run = %q, want %q", page.Run.ID, started.Run.ID)
		}
		planned, err := st.PlanCuration(ctx, curator, started.Run.ID,
			PlanMemoryCurationInput{
				FencingGeneration: started.Run.FencingGeneration,
				Draft:             marshalEmptyCurationPlanForAccessProfile(t),
				IdempotencyKey:    "non-sensitive-plan-curator",
			})
		if err != nil {
			t.Fatal(err)
		}
		applied, err := st.ApplyCuration(ctx, curator, started.Run.ID,
			ApplyMemoryCurationInput{
				FencingGeneration: started.Run.FencingGeneration,
				PlanRevision:      planned.Plan.PlanRevision,
				PlanHash:          planned.Receipt.PlanHash,
				IdempotencyKey:    "non-sensitive-apply-curator",
			})
		if err != nil {
			t.Fatal(err)
		}
		if applied.Run.State != MemoryCurationRunApplied ||
			applied.Request.State != MemoryCurationRequestFulfilled {
			t.Fatalf("non-sensitive curator result = %#v", applied)
		}
	})
}

func TestMemoryCurationRestrictedProfilesCannotSeeOrOperateTranscriptScopePostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx, st, full := newMemoryCurationAccessProfileStore(t, dsn)
	preview := full
	preview.AccessProfile = AccessProfileCuratorPreview
	apply := full
	apply.AccessProfile = AccessProfileCuratorApply
	restrictedPrincipals := []Principal{preview, apply}

	transcript, err := st.CreateTranscript(ctx, full.AccountID, full.RealmID, full.ID,
		CreateTranscriptInput{ExternalID: "restricted-transcript-boundary"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendTranscriptEntry(ctx, full.AccountID, full.RealmID, full.ID,
		transcript.ID, AppendTranscriptEntryInput{
			ExternalID: "restricted-transcript-entry", Role: TranscriptRoleUser,
			Body: "private transcript content must never reach a restricted curator",
		}); err != nil {
		t.Fatal(err)
	}

	fullPage, err := st.ListCurationRequests(ctx, full, MemoryCurationRequestListOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var transcriptRequest MemoryCurationRequest
	for _, request := range fullPage.Requests {
		if curationHasSource(request.Scope, MemoryCurationSourceTranscript) {
			transcriptRequest = request
			break
		}
	}
	if transcriptRequest.ID == "" || transcriptRequest.Scope.IncludeSensitive {
		t.Fatalf("automatic transcript request = %#v", transcriptRequest)
	}

	for _, restricted := range restrictedPrincipals {
		page, err := st.ListCurationRequests(ctx, restricted, MemoryCurationRequestListOptions{Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Requests) != 0 {
			t.Fatalf("%s listed transcript work = %#v", restricted.AccessProfile, page.Requests)
		}
		if _, err := st.GetCurationRequest(ctx, restricted, transcriptRequest.ID); !errors.Is(err, ErrMemoryCurationForbidden) {
			t.Fatalf("%s request read error = %v", restricted.AccessProfile, err)
		}
		if _, err := st.GetCurationStatus(ctx, restricted, ""); !errors.Is(err, ErrMemoryCurationForbidden) {
			t.Fatalf("%s queued status error = %v", restricted.AccessProfile, err)
		}
		if _, err := st.StartCuration(ctx, restricted, StartMemoryCurationInput{
			RequestID: transcriptRequest.ID, LeaseDuration: time.Minute,
			IdempotencyKey: "transcript-start-" + restricted.AccessProfile,
		}); !errors.Is(err, ErrMemoryCurationForbidden) {
			t.Fatalf("%s start error = %v", restricted.AccessProfile, err)
		}
	}

	started, err := st.StartCuration(ctx, full, StartMemoryCurationInput{
		RequestID: transcriptRequest.ID, LeaseDuration: time.Minute,
		IdempotencyKey: "transcript-start-full",
	})
	if err != nil {
		t.Fatal(err)
	}
	planned, err := st.PlanCuration(ctx, full, started.Run.ID, PlanMemoryCurationInput{
		FencingGeneration: started.Run.FencingGeneration,
		Draft:             marshalEmptyCurationPlanForAccessProfile(t),
		IdempotencyKey:    "transcript-plan-full",
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, restricted := range restrictedPrincipals {
		profile := restricted.AccessProfile
		if _, err := st.GetCurationRequest(ctx, restricted, transcriptRequest.ID); !errors.Is(err, ErrMemoryCurationForbidden) {
			t.Fatalf("%s claimed request read error = %v", profile, err)
		}
		if _, err := st.GetCurationRun(ctx, restricted, started.Run.ID); !errors.Is(err, ErrMemoryCurationForbidden) {
			t.Fatalf("%s run read error = %v", profile, err)
		}
		if _, err := st.GetCurationStatus(ctx, restricted, started.Run.ID); !errors.Is(err, ErrMemoryCurationForbidden) {
			t.Fatalf("%s exact status error = %v", profile, err)
		}
		if _, err := st.GetCurationStatus(ctx, restricted, ""); !errors.Is(err, ErrMemoryCurationForbidden) {
			t.Fatalf("%s active status error = %v", profile, err)
		}
		if _, err := st.GetCurationRunInputs(ctx, restricted, started.Run.ID,
			started.Run.FencingGeneration, "", 50); !errors.Is(err, ErrMemoryCurationForbidden) {
			t.Fatalf("%s inputs error = %v", profile, err)
		}
		if _, err := st.RenewCuration(ctx, restricted, started.Run.ID, RenewMemoryCurationInput{
			FencingGeneration: started.Run.FencingGeneration, Extension: time.Minute,
			IdempotencyKey: "transcript-renew-" + profile,
		}); !errors.Is(err, ErrMemoryCurationForbidden) {
			t.Fatalf("%s renew error = %v", profile, err)
		}
		if _, err := st.PlanCuration(ctx, restricted, started.Run.ID, PlanMemoryCurationInput{
			FencingGeneration: started.Run.FencingGeneration,
			Draft:             marshalEmptyCurationPlanForAccessProfile(t),
			IdempotencyKey:    "transcript-plan-" + profile,
		}); !errors.Is(err, ErrMemoryCurationForbidden) {
			t.Fatalf("%s plan error = %v", profile, err)
		}
		if _, err := st.ApplyCuration(ctx, restricted, started.Run.ID, ApplyMemoryCurationInput{
			FencingGeneration: started.Run.FencingGeneration,
			PlanRevision:      planned.Plan.PlanRevision, PlanHash: planned.Receipt.PlanHash,
			IdempotencyKey: "transcript-apply-" + profile,
		}); !errors.Is(err, ErrMemoryCurationForbidden) {
			t.Fatalf("%s apply error = %v", profile, err)
		}
		if _, err := st.AbandonCuration(ctx, restricted, started.Run.ID, FinishMemoryCurationInput{
			FencingGeneration: started.Run.FencingGeneration,
			Reason:            "worker_abandoned", IdempotencyKey: "transcript-abandon-" + profile,
		}); !errors.Is(err, ErrMemoryCurationForbidden) {
			t.Fatalf("%s abandon error = %v", profile, err)
		}
	}

	run, err := st.GetCurationRun(ctx, full, started.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	request, err := st.GetCurationRequest(ctx, full, transcriptRequest.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != MemoryCurationRunPlanned || request.State != MemoryCurationRequestClaimed ||
		request.ClaimedRunID != run.ID {
		t.Fatalf("restricted calls changed transcript run: run=%#v request=%#v", run, request)
	}
}

func TestMemoryCurationPreviewAbandonPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx, st, full := newMemoryCurationAccessProfileStore(t, dsn)

	previewRequest, err := st.RequestCuration(ctx, full, RequestMemoryCurationInput{
		CoalescingKey: "preview_success", TriggerReason: "manual_refine",
		MaxAttempts: 1, IdempotencyKey: "preview-success-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	previewRun, err := st.StartCuration(ctx, full, StartMemoryCurationInput{
		RequestID: previewRequest.Request.ID, LeaseDuration: time.Minute,
		IdempotencyKey: "preview-success-start",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.PlanCuration(ctx, full, previewRun.Run.ID, PlanMemoryCurationInput{
		FencingGeneration: previewRun.Run.FencingGeneration,
		Draft:             marshalEmptyCurationPlanForAccessProfile(t),
		IdempotencyKey:    "preview-success-plan",
	}); err != nil {
		t.Fatal(err)
	}
	before := time.Now().UTC()
	if _, err := st.AbandonCuration(ctx, full, previewRun.Run.ID, FinishMemoryCurationInput{
		FencingGeneration: previewRun.Run.FencingGeneration,
		Reason:            "preview_complete",
		IdempotencyKey:    "preview-success-abandon",
	}); err != nil {
		t.Fatal(err)
	}
	persisted, err := st.GetCurationRequest(ctx, full, previewRequest.Request.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.State != MemoryCurationRequestRetryWait || persisted.AttemptCount != 0 ||
		persisted.DueAt.Before(before.Add(23*time.Hour)) {
		t.Fatalf("preview request consumed retry budget or missed cooldown: %#v", persisted)
	}

	openRequest, err := st.RequestCuration(ctx, full, RequestMemoryCurationInput{
		CoalescingKey: "preview_spoof", TriggerReason: "manual_refine",
		MaxAttempts: 2, IdempotencyKey: "preview-spoof-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	openRun, err := st.StartCuration(ctx, full, StartMemoryCurationInput{
		RequestID: openRequest.Request.ID, LeaseDuration: time.Minute,
		IdempotencyKey: "preview-spoof-start",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AbandonCuration(ctx, full, openRun.Run.ID, FinishMemoryCurationInput{
		FencingGeneration: openRun.Run.FencingGeneration,
		Reason:            "preview_complete",
		IdempotencyKey:    "preview-spoof-abandon",
	}); err != nil {
		t.Fatal(err)
	}
	persisted, err = st.GetCurationRequest(ctx, full, openRequest.Request.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.State != MemoryCurationRequestRetryWait || persisted.AttemptCount != 1 {
		t.Fatalf("unplanned preview reason bypassed retry budget: %#v", persisted)
	}
}

func newMemoryCurationAccessProfileStore(
	t *testing.T,
	dsn string,
) (context.Context, *Store, Principal) {
	t.Helper()
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	p := provisionMemoryCurationApplyPrincipal(ctx, t, st)
	p.AccessProfile = AccessProfileFull
	return ctx, st, p
}

func marshalEmptyCurationPlanForAccessProfile(t *testing.T) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(MemoryCurationPlanDraft{
		Schema:        MemoryCurationPlanSchemaV1,
		DraftRevision: 1,
		Actions:       []MemoryCurationPlanAction{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
