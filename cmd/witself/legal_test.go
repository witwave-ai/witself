package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/client"
)

func captureStdout(t *testing.T, run func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string)
	go func() {
		buf := make([]byte, 1<<16)
		total := ""
		for {
			n, readErr := r.Read(buf)
			total += string(buf[:n])
			if readErr != nil {
				break
			}
		}
		done <- total
	}()
	run()
	w.Close()
	os.Stdout = old
	return <-done
}

func newLegalStub(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/versions.json", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"terms":{"title":"Terms of Service","version":"2026-08-31","path":"/legal/terms"},"privacy":{"title":"Privacy Policy","version":"2026-08-31","path":"/legal/privacy"}}`)
	})
	mux.HandleFunc("/terms", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("format") != "md" {
			t.Errorf("document fetch must request markdown, got query %q", r.URL.RawQuery)
		}
		w.Header().Set("content-type", "text/markdown")
		fmt.Fprint(w, "# Witself Terms of Service\n\n**Version 2026-08-31**\n")
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func TestLegalListPrintsVersionsFromManifest(t *testing.T) {
	server := newLegalStub(t)
	out := captureStdout(t, func() {
		if code := legalList(server.URL); code != 0 {
			t.Errorf("legalList = %d, want 0", code)
		}
	})
	for _, want := range []string{"terms", "privacy", "2026-08-31", "Terms of Service"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output missing %q:\n%s", want, out)
		}
	}
}

func TestLegalShowFetchesMarkdownAndCitesTheURL(t *testing.T) {
	server := newLegalStub(t)
	out := captureStdout(t, func() {
		if code := legalShow(server.URL, "terms-of-service"); code != 0 {
			t.Errorf("legalShow = %d, want 0", code)
		}
	})
	if !strings.Contains(out, "# Witself Terms of Service") {
		t.Errorf("show output missing document body:\n%s", out)
	}
	if !strings.Contains(out, server.URL+"/terms") {
		t.Errorf("show output must cite the published URL:\n%s", out)
	}
}

func TestLegalShowRejectsUnknownDocument(t *testing.T) {
	if code := legalShow("http://127.0.0.1:0", "warranty"); code != 2 {
		t.Fatalf("legalShow(unknown) = %d, want 2", code)
	}
}

// TestPlanCancelOutcomeWarnsBillingResumes pins the consumer-protection fix:
// undoing a scheduled downgrade re-arms monthly renewal, and the output must
// say so instead of the old bare "cancelled".
func TestPlanCancelOutcomeWarnsBillingResumes(t *testing.T) {
	out := captureStdout(t, func() {
		printPlanCancelOutcome(client.PlanOutcome{Kind: "cancelled"})
	})
	if !strings.Contains(out, "keep renewing monthly") ||
		!strings.Contains(out, "witself plan downgrade free") {
		t.Fatalf("cancel outcome must warn that billing resumes:\n%s", out)
	}
}

// TestBillingMutationReasonOptionalOnlyForCancellation pins the click-to-
// cancel posture: purchases still demand an explicit reason, cancellation
// paths must not gate the consumer on providing one.
func TestBillingMutationReasonOptionalOnlyForCancellation(t *testing.T) {
	if validBillingMutationCLIFlags("", "k", true, false, "usage", true) {
		t.Fatal("purchase direction must still require --reason")
	}
	if !validBillingMutationCLIFlags("", "k", true, false, "usage", false) {
		t.Fatal("cancellation direction must not require --reason")
	}
}
