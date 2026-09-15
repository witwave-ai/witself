package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/legal"
	"github.com/witwave-ai/witself/internal/local"
)

func signupLegalTestManifest(terms, privacy string) string {
	return fmt.Sprintf(`{"terms":{"version":%q,"path":"/legal/terms"},"privacy":{"version":%q,"path":"/legal/privacy"},"dpa":{"title":"Other documents remain available"}}`, terms, privacy)
}

func writeSignupLegalTestManifest(w http.ResponseWriter, terms, privacy string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprint(w, signupLegalTestManifest(terms, privacy))
}

func assertNoSignupJournal(t *testing.T) {
	t.Helper()
	if _, err := local.ReadAccountProvisionJournal("default"); !errors.Is(err, local.ErrAccountProvisionJournalUnavailable) {
		t.Fatalf("unexpected pending journal: %v", err)
	}
}

func TestAccountCreateSelectsServedLegalVersions(t *testing.T) {
	privateAccountCreateTestHome(t)
	const terms, privacy = "terms-served-2026.09", "privacy-served-2026.09"
	if terms == legal.TermsVersion || privacy == legal.PrivacyVersion {
		t.Fatal("regression fixture must differ from compiled legal versions")
	}
	var manifestCalls, createCalls atomic.Int32
	var posted map[string]string
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/legal/versions.json":
			manifestCalls.Add(1)
			if r.Method != http.MethodGet || r.Header.Get("Accept") != "application/json" ||
				r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" ||
				r.Header.Get("Proxy-Authorization") != "" || r.URL.User != nil || r.URL.RawQuery != "" {
				t.Error("manifest request must be a credential-free public GET")
			}
			writeSignupLegalTestManifest(w, terms, privacy)
		case "/v1/accounts":
			createCalls.Add(1)
			mu.Lock()
			if err := json.NewDecoder(r.Body).Decode(&posted); err != nil {
				t.Error(err)
			}
			mu.Unlock()
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(w, `{"error":"stop before provisioning"}`)
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	// Signup must follow --endpoint, not this separate legal-command override.
	t.Setenv("WITSELF_LEGAL_URL", "https://unused.invalid/legal")
	args := append(accountCreateTestArgs(server.URL, "invite-private"), "--accept-terms")
	output := captureStdout(t, func() {
		if code := accountCreate(args); code != 1 {
			t.Fatalf("signup exit = %d", code)
		}
	})
	mu.Lock()
	defer mu.Unlock()
	if manifestCalls.Load() != 1 || createCalls.Load() != 1 ||
		posted["consent_terms_version"] != terms || posted["consent_privacy_version"] != privacy {
		t.Fatalf("served consent not sent: manifest=%d create=%d terms=%q privacy=%q", manifestCalls.Load(), createCalls.Load(), posted["consent_terms_version"], posted["consent_privacy_version"])
	}
	journal, err := local.ReadAccountProvisionJournal("default")
	if err != nil {
		t.Fatal(err)
	}
	expected, err := client.AccountCreateRequestFingerprint(server.URL, "default", "owner@example.com", "invite-private", "Owner Display", terms, privacy)
	if err != nil || journal.RequestFingerprint != expected || journal.AcceptedTermsVersion != terms || journal.AcceptedPrivacyVersion != privacy {
		t.Fatal("journal did not bind the served versions")
	}
	for _, want := range []string{terms, privacy, server.URL + "/legal/terms", server.URL + "/legal/privacy"} {
		if !strings.Contains(output, want) {
			t.Fatalf("consent output missing %q", want)
		}
	}
	if strings.Contains(output, legal.BaseURL) || strings.Contains(output, "unused.invalid") {
		t.Fatal("consent output points at an authority other than the selected endpoint")
	}
}

func TestAccountCreateLegalManifestFailuresDoNotCreateJournalOrSignup(t *testing.T) {
	valid := signupLegalTestManifest("terms-v1", "privacy-v1")
	cases := []struct {
		name   string
		body   string
		status int
	}{
		{"missing terms", `{"privacy":{"version":"v1","path":"/legal/privacy"}}`, 200},
		{"missing privacy", `{"terms":{"version":"v1","path":"/legal/terms"}}`, 200},
		{"null root", `null`, 200},
		{"array root", `[]`, 200},
		{"null entry", strings.Replace(valid, `{"version":"terms-v1","path":"/legal/terms"}`, `null`, 1), 200},
		{"wrong version type", strings.Replace(valid, `"terms-v1"`, `42`, 1), 200},
		{"blank version", signupLegalTestManifest("", "v1"), 200},
		{"unsafe version", signupLegalTestManifest("person@example.com", "v1"), 200},
		{"long version", signupLegalTestManifest(strings.Repeat("a", 65), "v1"), 200},
		{"case alias", strings.Replace(valid, `"version"`, `"Version"`, 1), 200},
		{"foreign document URL", strings.Replace(valid, `"/legal/terms"`, `"https://foreign.invalid/legal/terms"`, 1), 200},
		{"wrong document", strings.Replace(valid, `"/legal/terms"`, `"/legal/privacy"`, 1), 200},
		{"duplicate document", strings.Replace(valid, `"terms":`, `"terms":{},"terms":`, 1), 200},
		{"duplicate version", strings.Replace(valid, `"version":"terms-v1"`, `"version":"old","version":"terms-v1"`, 1), 200},
		{"duplicate escaped version", strings.Replace(valid, `"version":"terms-v1"`, `"\u0076ersion":"old","version":"terms-v1"`, 1), 200},
		{"duplicate path", strings.Replace(valid, `"path":"/legal/terms"`, `"path":"/unsafe","path":"/legal/terms"`, 1), 200},
		{"trailing JSON", valid + `{}`, 200},
		{"incomplete JSON", valid[:len(valid)-1], 200},
		{"truncated HTTP body", valid, 200},
		{"oversized valid JSON", valid + strings.Repeat(" ", maxConsentManifestBytes), 200},
		{"unavailable", valid, 503},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			privateAccountCreateTestHome(t)
			var signupCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/legal/versions.json" {
					signupCalls.Add(1)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if tc.name == "truncated HTTP body" {
					w.Header().Set("Content-Length", fmt.Sprint(len(tc.body)+1))
				}
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			args := append(accountCreateTestArgs(server.URL, "invite-private"), "--accept-terms")
			if code := accountCreate(args); code != 1 || signupCalls.Load() != 0 {
				t.Fatalf("invalid manifest created signup: exit=%d calls=%d", code, signupCalls.Load())
			}
			assertNoSignupJournal(t)
		})
	}
}

func TestAccountCreateLegalReadRespectsLocalNameGuard(t *testing.T) {
	home := privateAccountCreateTestHome(t)
	if err := local.Save("default", local.Account{ID: accountCreateTestAccountID, Email: "owner@example.com"}, accountCreateTestOperatorToken); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()
	args := append(accountCreateTestArgs(server.URL, "invite-private"), "--accept-terms")
	if code := accountCreate(args); code != 1 || calls.Load() != 0 {
		t.Fatalf("taken local name caused network access: exit=%d calls=%d", code, calls.Load())
	}
	assertNoSignupJournal(t)
	assertAccountCreateSaved(t, home)
}

func TestAccountCreateLegalReadDeadlineCoversHeadersAndBody(t *testing.T) {
	for _, bodyStarted := range []bool{false, true} {
		t.Run(fmt.Sprintf("body-started-%t", bodyStarted), func(t *testing.T) {
			privateAccountCreateTestHome(t)
			var signupCalls atomic.Int32
			started := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/legal/versions.json" {
					signupCalls.Add(1)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if bodyStarted {
					_, _ = fmt.Fprint(w, `{"terms":`)
					w.(http.Flusher).Flush()
				}
				close(started)
				<-r.Context().Done()
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			args := append(accountCreateTestArgs(server.URL, "invite-private"), "--accept-terms")
			if code := accountCreateWithContext(ctx, args); code != 1 || signupCalls.Load() != 0 || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
				t.Fatalf("deadline did not stop consent: exit=%d calls=%d context=%v", code, signupCalls.Load(), ctx.Err())
			}
			select {
			case <-started:
			default:
				t.Fatal("deadline elapsed before the intended I/O boundary was reached")
			}
			assertNoSignupJournal(t)
		})
	}
}

func TestAccountCreateLegalRedirectCannotChangeAuthority(t *testing.T) {
	privateAccountCreateTestHome(t)
	var foreignCalls, signupCalls atomic.Int32
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		foreignCalls.Add(1)
		writeSignupLegalTestManifest(w, "foreign-terms", "foreign-privacy")
	}))
	defer foreign.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/legal/versions.json" {
			signupCalls.Add(1)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		http.Redirect(w, r, foreign.URL+"/legal/versions.json", http.StatusFound)
	}))
	defer server.Close()
	args := append(accountCreateTestArgs(server.URL, "invite-private"), "--accept-terms")
	if code := accountCreate(args); code != 1 || foreignCalls.Load() != 0 || signupCalls.Load() != 0 {
		t.Fatalf("redirect crossed authority: exit=%d foreign=%d signup=%d", code, foreignCalls.Load(), signupCalls.Load())
	}
	assertNoSignupJournal(t)
}

func TestSignupLegalReadAllowsBoundedSameAuthorityRedirect(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/legal/versions.json" {
			http.Redirect(w, r, "/manifest", http.StatusFound)
			return
		}
		writeSignupLegalTestManifest(w, "v1", "v2")
	}))
	defer server.Close()
	terms, privacy, err := signupLegalVersions(context.Background(), server.URL+"/legal")
	if err != nil || terms != "v1" || privacy != "v2" || calls.Load() != 2 {
		t.Fatalf("same-authority redirect failed: %q/%q calls=%d err=%v", terms, privacy, calls.Load(), err)
	}
}

func TestSignupLegalReadBoundsRedirectLoop(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Redirect(w, r, "/legal/versions.json", http.StatusFound)
	}))
	defer server.Close()
	if _, _, err := signupLegalVersions(context.Background(), server.URL+"/legal"); err == nil || calls.Load() != 5 {
		t.Fatalf("redirect loop not bounded: calls=%d err=%v", calls.Load(), err)
	}
}

func TestSignupLegalBaseRejectsCredentialsAndNonAuthorityURLs(t *testing.T) {
	for _, endpoint := range []string{
		"https://owner:private-value@example.com", "https://example.com?token=private-value", "https://example.com/#private-value",
		"file:///private-value", "https:///missing-host", "//example.com", "https://example.com?",
	} {
		if _, err := signupLegalBase(endpoint); err == nil || strings.Contains(err.Error(), "private-value") {
			t.Fatalf("unsafe endpoint accepted or exposed: %v", err)
		}
	}
	if base, err := signupLegalBase(" http://127.0.0.1:1234/custom/api/ "); err != nil || base != "http://127.0.0.1:1234/legal" {
		t.Fatalf("selected endpoint authority = %q / %v", base, err)
	}
}

func TestAccountCreatePreservesConsentlessJournalWhenFlagAdded(t *testing.T) {
	privateAccountCreateTestHome(t)
	var manifestCalls, signupCalls atomic.Int32
	var posted map[string]string
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/legal/versions.json" {
			manifestCalls.Add(1)
			writeSignupLegalTestManifest(w, "new-terms", "new-privacy")
			return
		}
		signupCalls.Add(1)
		mu.Lock()
		if err := json.NewDecoder(r.Body).Decode(&posted); err != nil {
			t.Error(err)
		}
		mu.Unlock()
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"error":"stop before provisioning"}`)
	}))
	defer server.Close()
	fingerprint, err := client.AccountCreateRequestFingerprint(server.URL, "default", "owner@example.com", "invite-private", "Owner Display", "", "")
	if err != nil {
		t.Fatal(err)
	}
	before, _, err := local.BeginAccountProvisionJournal("default", fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	args := append(accountCreateTestArgs(server.URL, "invite-private"), "--accept-terms")
	if code := accountCreate(args); code != 1 || manifestCalls.Load() != 0 || signupCalls.Load() != 1 {
		t.Fatalf("legacy journal was upgraded: exit=%d manifest=%d signup=%d", code, manifestCalls.Load(), signupCalls.Load())
	}
	after, err := local.ReadAccountProvisionJournal("default")
	mu.Lock()
	defer mu.Unlock()
	if err != nil || before != after || posted["provision_id"] != before.ProvisionID {
		t.Fatal("legacy journal or provision id changed")
	}
	for _, field := range []string{"consent_terms_version", "consent_privacy_version"} {
		if _, present := posted[field]; present {
			t.Fatalf("legacy signup acquired %s", field)
		}
	}
}

func TestAccountCreateConsentfulCredentialRecoveryDoesNotReadManifest(t *testing.T) {
	for _, alreadySaved := range []bool{false, true} {
		t.Run(fmt.Sprintf("already-saved-%t", alreadySaved), func(t *testing.T) {
			home := privateAccountCreateTestHome(t)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			defer server.Close()
			fingerprint, err := client.AccountCreateRequestFingerprint(server.URL, "default", "owner@example.com", "invite-private", "Owner Display", "accepted-terms", "accepted-privacy")
			if err != nil {
				t.Fatal(err)
			}
			record, _, err := local.BeginAccountProvisionJournalWithConsent("default", fingerprint, "accepted-terms", "accepted-privacy")
			if err != nil {
				t.Fatal(err)
			}
			if err := local.SaveAccountProvisionCredential("default", fingerprint, record.ProvisionID, accountCreateTestAccountID, accountCreateTestOperatorToken); err != nil {
				t.Fatal(err)
			}
			if alreadySaved {
				if err := local.SaveProvisionedAccountDurable("default", local.Account{ID: accountCreateTestAccountID, Email: "owner@example.com"}, accountCreateTestOperatorToken); err != nil {
					t.Fatal(err)
				}
			}
			args := append(accountCreateTestArgs(server.URL, "invite-private"), "--accept-terms")
			if code := accountCreate(args); code != 0 || calls.Load() != 0 {
				t.Fatalf("credential recovery contacted endpoint: exit=%d calls=%d", code, calls.Load())
			}
			assertNoSignupJournal(t)
			assertAccountCreateSaved(t, home)
		})
	}
}

func TestAccountCreateLegalReadPreservesConcurrentJournalWinner(t *testing.T) {
	privateAccountCreateTestHome(t)
	var signupCalls atomic.Int32
	var winner local.AccountProvisionJournal
	var mu sync.Mutex
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/legal/versions.json" {
			signupCalls.Add(1)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		// Another caller wins after the initial read but before this caller's
		// Begin. Exercise the real local publication/lock path, not a mock.
		fingerprint, err := client.AccountCreateRequestFingerprint(server.URL, "default", "owner@example.com", "invite-private", "Owner Display", "winner-terms", "winner-privacy")
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		mu.Lock()
		winner, _, err = local.BeginAccountProvisionJournalWithConsent("default", fingerprint, "winner-terms", "winner-privacy")
		mu.Unlock()
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		writeSignupLegalTestManifest(w, "served-terms", "served-privacy")
	}))
	defer server.Close()
	args := append(accountCreateTestArgs(server.URL, "invite-private"), "--accept-terms")
	if code := accountCreate(args); code != 1 || signupCalls.Load() != 0 {
		t.Fatalf("concurrent winner was not fenced: exit=%d calls=%d", code, signupCalls.Load())
	}
	retained, err := local.ReadAccountProvisionJournal("default")
	mu.Lock()
	defer mu.Unlock()
	if err != nil || winner.ProvisionID == "" || retained != winner {
		t.Fatal("concurrent winner's journal changed")
	}
}

func TestSignupLegalRedirectRejectsDowngradeAndCredentials(t *testing.T) {
	for _, target := range []string{"http://example.com/legal/versions.json", "https://owner:private-value@example.com/legal/versions.json"} {
		t.Run(strings.SplitN(target, ":", 2)[0], func(t *testing.T) {
			var calls int
			original := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = original })
			http.DefaultTransport = accountCreateRoundTripFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				return &http.Response{
					StatusCode: http.StatusFound,
					Header:     http.Header{"Location": []string{target}},
					Body:       io.NopCloser(strings.NewReader("")),
					Request:    request,
				}, nil
			})
			if _, _, err := signupLegalVersions(context.Background(), "https://example.com/legal"); err == nil || calls != 1 || strings.Contains(err.Error(), "private-value") {
				t.Fatalf("unsafe redirect sent or leaked: calls=%d err=%v", calls, err)
			}
		})
	}
}
