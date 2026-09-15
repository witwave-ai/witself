package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/local"
	"github.com/witwave-ai/witself/internal/signuplegal"
)

func cliLegalRefusal(body map[string]string, next string) signuplegal.Refusal {
	return signuplegal.Refusal{SchemaVersion: signuplegal.RefusalSchema, Code: signuplegal.RefusalCode,
		Error: signuplegal.RefusalError, ProvisionID: body["provision_id"], RequestFingerprint: strings.Repeat("a", 64),
		ConsentTermsVersion: body["consent_terms_version"], ConsentPrivacyVersion: body["consent_privacy_version"],
		RefusalID: "refusal-" + body["provision_id"], RefusalRevision: 1,
		RequiredTermsVersion: "terms-" + next, RequiredPrivacyVersion: "privacy-" + next}
}
func cliLegalAck(request signuplegal.ReconsentRequest) signuplegal.Ack {
	return signuplegal.Ack{SchemaVersion: signuplegal.AckSchema, Status: "registered", Refusal: request.Refusal,
		TransitionID: request.TransitionID, Candidate: request.Candidate, CandidateRequestFingerprint: strings.Repeat("b", 64)}
}

func TestAccountCreateRefusalStopsAndRequiresLaterExplicitAcceptance(t *testing.T) {
	privateAccountCreateTestHome(t)
	var mu sync.Mutex
	var manifests, creates, transitions int
	var firstID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/legal/versions.json":
			manifests++
			version := "v1"
			if manifests > 1 {
				version = "v2"
			}
			writeSignupLegalTestManifest(w, "terms-"+version, "privacy-"+version)
		case r.URL.Path == "/v1/accounts":
			creates++
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if creates == 1 {
				firstID = body["provision_id"]
			} else if body["provision_id"] == firstID {
				t.Error("successor reused refused ID")
			}
			next := "v2"
			if creates > 1 {
				next = "v3"
			}
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(cliLegalRefusal(body, next))
		case strings.HasSuffix(r.URL.Path, ":reconsent"):
			transitions++
			var request signuplegal.ReconsentRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			if request.Validate() != nil || r.URL.Path != "/v1/account-signups/"+firstID+":reconsent" {
				t.Error("transition route/core mismatch")
			}
			// The candidate and acceptance are already durable before the first
			// mutation. The command may not merely keep them in process memory.
			journal, err := local.ReadAccountProvisionJournal("default")
			if err != nil || journal.Reconsent == nil || journal.Reconsent.Candidate != request.Candidate || journal.Reconsent.TransitionID != request.TransitionID {
				t.Error("candidate was not durable before registration")
			}
			_ = json.NewEncoder(w).Encode(cliLegalAck(request))
		default:
			t.Errorf("unexpected request path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	args := accountCreateTestArgs(server.URL, "invite-private")
	accepted := append(append([]string{}, args...), "--accept-terms")
	if code := accountCreate(accepted); code != 1 {
		t.Fatalf("first refusal exit = %d", code)
	}
	first, err := local.ReadAccountProvisionJournal("default")
	if err != nil || first.LegalRefusal == nil || first.Reconsent != nil {
		t.Fatalf("refusal journal = %#v, %v", first, err)
	}
	mu.Lock()
	if manifests != 1 || creates != 1 || transitions != 0 {
		t.Error("first failure refreshed or transitioned in the same invocation")
	}
	mu.Unlock()
	if code := accountCreate(args); code != 1 {
		t.Fatalf("no acceptance exit = %d", code)
	}
	if code := accountCreate(append(accountCreateTestArgs(server.URL, "changed-invite"), "--accept-terms")); code != 1 {
		t.Fatalf("changed identity exit = %d", code)
	}
	mu.Lock()
	if manifests != 1 || creates != 1 || transitions != 0 {
		t.Error("missing acceptance or changed core performed remote work")
	}
	mu.Unlock()
	if code := accountCreate(accepted); code != 1 {
		t.Fatalf("second refusal exit = %d", code)
	}
	second, err := local.ReadAccountProvisionJournal("default")
	if err != nil || second.LegalRefusal == nil || second.Reconsent != nil || second.ProvisionID == first.ProvisionID || second.AcceptedTermsVersion != "terms-v2" {
		t.Fatalf("successor refusal was not saved as one bounded current attempt: %#v, %v", second, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if manifests != 2 || creates != 2 || transitions != 1 {
		t.Error("new stale successor caused an acceptance loop")
	}
}

func TestAccountCreateAmbiguousReconsentReplaysOneDurableCandidateWithoutRefresh(t *testing.T) {
	privateAccountCreateTestHome(t)
	var mu sync.Mutex
	var manifests, creates, transitions int
	var firstTransition signuplegal.ReconsentRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/legal/versions.json":
			manifests++
			version := "v1"
			if manifests > 1 {
				version = "v2"
			}
			writeSignupLegalTestManifest(w, "terms-"+version, "privacy-"+version)
		case r.URL.Path == "/v1/accounts":
			creates++
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if creates == 1 {
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(cliLegalRefusal(body, "v2"))
			} else {
				if body["provision_id"] != firstTransition.Candidate.ProvisionID {
					t.Error("signup did not use registered candidate")
				}
				w.WriteHeader(http.StatusBadRequest)
			}
		case strings.HasSuffix(r.URL.Path, ":reconsent"):
			transitions++
			var request signuplegal.ReconsentRequest
			_ = json.NewDecoder(r.Body).Decode(&request)
			if transitions == 1 {
				firstTransition = request
				// Remote registration committed, but no usable acknowledgement
				// reached this invocation. The client must keep its original ID.
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			if request != firstTransition {
				t.Error("ambiguous registration changed its exact request")
			}
			_ = json.NewEncoder(w).Encode(cliLegalAck(request))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	args := accountCreateTestArgs(server.URL, "invite-private")
	accepted := append(append([]string{}, args...), "--accept-terms")
	for attempt := 1; attempt <= 2; attempt++ {
		if code := accountCreate(accepted); code != 1 {
			t.Fatalf("account create attempt %d exit = %d; expected refusal then ambiguous transition", attempt, code)
		}
	}
	pending, err := local.ReadAccountProvisionJournal("default")
	if err != nil || pending.Reconsent == nil {
		t.Fatalf("pending transition lost: %v", err)
	}
	if code := accountCreate(args); code != 1 {
		t.Fatalf("replayed successor exit = %d", code)
	}
	promoted, err := local.ReadAccountProvisionJournal("default")
	if err != nil || promoted.LegalRefusal != nil || promoted.Reconsent != nil || promoted.ProvisionID != pending.Reconsent.Candidate.ProvisionID || promoted.RequestFingerprint != pending.Reconsent.RequestFingerprint {
		t.Fatalf("exact acknowledged candidate not promoted: %#v, %v", promoted, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if manifests != 2 || creates != 2 || transitions != 2 {
		t.Error("pending retry refreshed consent or repeated signup before registration")
	}
}

func TestAccountCreateCredentialWinnerSurvivesStaleRefusalAndRecoversOffline(t *testing.T) {
	privateAccountCreateTestHome(t)
	var mu sync.Mutex
	var manifests, creates int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/legal/versions.json" {
			manifests++
			writeSignupLegalTestManifest(w, "terms-v1", "privacy-v1")
			return
		}
		if r.URL.Path != "/v1/accounts" {
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		creates++
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		journal, err := local.ReadAccountProvisionJournal("default")
		if err != nil {
			t.Error(err)
		}
		if err := local.SaveAccountProvisionCredential("default", journal.RequestFingerprint, journal.ProvisionID,
			accountCreateTestAccountID, accountCreateTestOperatorToken); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(cliLegalRefusal(body, "v2"))
	}))
	defer server.Close()
	args := append(accountCreateTestArgs(server.URL, "invite-private"), "--accept-terms")
	if accountCreate(args) != 1 {
		t.Fatal("stale refusal should stop")
	}
	journal, err := local.ReadAccountProvisionJournal("default")
	if err != nil || journal.OperatorToken != accountCreateTestOperatorToken || journal.LegalRefusal != nil {
		t.Fatal("credential winner was overwritten")
	}
	if accountCreate(args) != 0 {
		t.Fatal("credential winner did not recover offline")
	}
	mu.Lock()
	defer mu.Unlock()
	if manifests != 1 || creates != 1 {
		t.Fatal("credential recovery made another network request")
	}
}

func TestAccountCreateSuccessfulSuccessorSavesCredentialAndRecoversOffline(t *testing.T) {
	home := privateAccountCreateTestHome(t)
	var mu sync.Mutex
	var manifests, creates, transitions, bootstraps int
	var candidate signuplegal.Candidate
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/legal/versions.json":
			manifests++
			version := "v1"
			if manifests > 1 {
				version = "v2"
			}
			writeSignupLegalTestManifest(w, "terms-"+version, "privacy-"+version)
		case r.URL.Path == "/v1/accounts":
			creates++
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if creates == 1 {
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(cliLegalRefusal(body, "v2"))
				return
			}
			journal, err := local.ReadAccountProvisionJournal("default")
			if err != nil || journal.LegalRefusal != nil || journal.Reconsent != nil || journal.ProvisionID != candidate.ProvisionID ||
				body["provision_id"] != candidate.ProvisionID || body["consent_terms_version"] != candidate.ConsentTermsVersion || body["consent_privacy_version"] != candidate.ConsentPrivacyVersion {
				t.Error("candidate signup occurred before exact durable promotion")
			}
			writeAccountCreateTestResponse(w, server.URL, body["provision_id"], body["email"], 1)
		case strings.HasSuffix(r.URL.Path, ":reconsent"):
			transitions++
			var request signuplegal.ReconsentRequest
			_ = json.NewDecoder(r.Body).Decode(&request)
			candidate = request.Candidate
			_ = json.NewEncoder(w).Encode(cliLegalAck(request))
		case r.URL.Path == "/v1/auth/bootstrap":
			bootstraps++
			// The real credential journal remains writable while local account
			// publication is deliberately interrupted after remote bootstrap.
			if err := writeAccountCreatePrivateTestFile(filepath.Join(home, "tokens"), []byte("blocked")); err != nil {
				t.Error(err)
			}
			writeAccountCreateBootstrapResponse(w)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	args := append(accountCreateTestArgs(server.URL, "invite-private"), "--accept-terms")
	for attempt := 1; attempt <= 2; attempt++ {
		if code := accountCreate(args); code != 1 {
			t.Fatalf("account create attempt %d exit = %d; expected refusal, then a local-save interruption after successful candidate signup", attempt, code)
		}
	}
	journal, err := local.ReadAccountProvisionJournal("default")
	if err != nil || journal.ProvisionID != candidate.ProvisionID || journal.AccountID != accountCreateTestAccountID || journal.OperatorToken != accountCreateTestOperatorToken || journal.LegalRefusal != nil || journal.Reconsent != nil {
		t.Fatal("successful successor credential was not durably retained")
	}
	if err := os.Remove(filepath.Join(home, "tokens")); err != nil {
		t.Fatal(err)
	}
	if accountCreate(args) != 0 {
		t.Fatal("successor credential did not recover offline")
	}
	assertAccountCreateSaved(t, home)
	assertNoSignupJournal(t)
	mu.Lock()
	defer mu.Unlock()
	if manifests != 2 || creates != 2 || transitions != 1 || bootstraps != 1 {
		t.Fatal("offline successor recovery repeated remote work")
	}
}

func TestAccountCreatePendingReconsentRequiresDurabilityBeforeRemoteRegistration(t *testing.T) {
	privateAccountCreateTestHome(t)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()
	fingerprint, err := client.AccountCreateRequestFingerprint(server.URL, "default", "owner@example.com", "invite-private", "Owner Display", "terms-v1", "privacy-v1")
	if err != nil {
		t.Fatal(err)
	}
	journal, _, err := local.BeginAccountProvisionJournalWithConsent("default", fingerprint, "terms-v1", "privacy-v1")
	if err != nil {
		t.Fatal(err)
	}
	refusal := cliLegalRefusal(map[string]string{"provision_id": journal.ProvisionID, "consent_terms_version": "terms-v1", "consent_privacy_version": "privacy-v1"}, "v2")
	journal, err = local.RecordAccountProvisionLegalRefusal("default", journal, refusal)
	if err != nil {
		t.Fatal(err)
	}
	newFingerprint, err := client.AccountCreateRequestFingerprint(server.URL, "default", "owner@example.com", "invite-private", "Owner Display", "terms-v2", "privacy-v2")
	if err != nil {
		t.Fatal(err)
	}
	journal, err = local.BeginAccountProvisionReconsent("default", journal, newFingerprint, "terms-v2", "privacy-v2")
	if err != nil {
		t.Fatal(err)
	}
	publications := 0
	_, err = resumeAccountLegalReconsentWithBegin(context.Background(), "default", server.URL, "owner@example.com", "invite-private", "Owner Display", false, journal,
		func(name string, expected local.AccountProvisionJournal, hash, terms, privacy string) (local.AccountProvisionJournal, error) {
			publications++
			if name != "default" || expected.ProvisionID != journal.ProvisionID || expected.Reconsent == nil || *expected.Reconsent != *journal.Reconsent || hash != newFingerprint || terms != "terms-v2" || privacy != "privacy-v2" {
				t.Error("pending durability replay changed its saved choice")
			}
			return local.AccountProvisionJournal{}, local.ErrAccountProvisionJournalStorage
		})
	if !errors.Is(err, local.ErrAccountProvisionJournalStorage) || publications != 1 || calls != 0 {
		t.Fatal("pending sync failure allowed a remote registration")
	}
	after, err := local.ReadAccountProvisionJournal("default")
	if err != nil || after.Reconsent == nil || *after.Reconsent != *journal.Reconsent {
		t.Fatal("failed sync lost the elected pending candidate")
	}
}
