package main

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/local"
)

func TestBillingSummaryKeepsBillingAndEffectivePlansDistinct(t *testing.T) {
	now := time.Date(2026, 8, 10, 20, 0, 0, 0, time.UTC)
	summary := client.BillingSummary{
		BillingAvailable:   true,
		Configured:         true,
		SubscriptionStatus: "active",
		BillingPlan:        "free",
		BillingPlanName:    "Personal",
		EffectivePlan:      "enterprise",
		EffectivePlanName:  "Enterprise",
		AppliedPlan:        "enterprise",
		EntitledAt:         &now,
		PaymentMethod:      &client.BillingPaymentMethod{Label: "visa ****4242"},
		NextCharge: &client.BillingCharge{
			Date: now.AddDate(0, 1, 0), AmountCents: 2500, Currency: "usd",
		},
	}
	stdout, stderr, code := capturePlanCLI(t, func() int {
		printBillingSummary(summary)
		return 0
	})
	if code != 0 || stderr != "" {
		t.Fatalf("summary = %d stderr=%q", code, stderr)
	}
	for _, want := range []string{
		"Personal (free) — active",
		"Enterprise (enterprise) (account override; billing remains Personal (free))",
		"visa ****4242",
		"USD 25.00",
		"entitled-at:",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("summary omitted %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "$30") {
		t.Fatalf("summary fabricated catalog price:\n%s", stdout)
	}
}

func TestBillingHumanTablesNeutralizeProviderControls(t *testing.T) {
	now := time.Date(2026, 8, 10, 20, 0, 0, 0, time.UTC)
	invoices := []client.BillingInvoice{{
		Number: "INV\x1b[2J\nforged", Date: now, AmountCents: 3000,
		Currency: "u\u202esd\u2069", Status: "paid\u009b31m\u2028forged",
		PDFURL: "https://billing.example/invoice.pdf",
	}}
	payments := []client.BillingPayment{{
		Date: now, AmountCents: 3000, Currency: "usd", Status: "succeeded\x1b[2J",
		Method: "visa\t****4242\u2029spoof", ReceiptURL: "https://billing.example/receipt",
	}}
	stdout, stderr, code := capturePlanCLI(t, func() int {
		printBillingInvoices(invoices)
		printBillingPayments(payments)
		return 0
	})
	if code != 0 || stderr != "" {
		t.Fatalf("tables = %d stderr=%q", code, stderr)
	}
	if strings.ContainsAny(stdout, "\x1b\u009b\t\u202e\u2069\u2028\u2029") {
		t.Fatalf("billing tables retained terminal controls: %q", stdout)
	}
	for _, want := range []string{"INV[2J forged", "paid31m forged", "visa ****4242 spoof"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("sanitized tables omitted %q:\n%s", want, stdout)
		}
	}
}

func TestBillingActionRequiresExplicitSafeHTTPSURL(t *testing.T) {
	for _, raw := range []string{
		"javascript:alert(1)",
		"file:///tmp/invoice",
		"http://billing.example/portal",
		"https://user:secret@billing.example/portal",
		"https://billing.example/portal\nhttps://forged.example",
		"https://billing.example/\u202espoof",
		"https://billing.example/%0ahttps://forged.example",
		"https://billing.example/%E2%80%AEspoof",
		"https://billing.example/%5c@forged.example",
	} {
		stdout, stderr, code := capturePlanCLI(t, func() int {
			return renderBillingAction(client.BillingAction{Kind: "action", URL: raw}, false, false, "done")
		})
		if code != 1 || stdout != "" || !strings.Contains(stderr, "unsafe hosted URL") {
			t.Errorf("URL %q = code %d stdout=%q stderr=%q", raw, code, stdout, stderr)
		}
	}

	stdout, stderr, code := capturePlanCLI(t, func() int {
		return renderBillingAction(client.BillingAction{
			Kind: "action", URL: "https://billing.example/portal?session=public",
		}, false, false, "done")
	})
	if code != 0 || stderr != "" || strings.TrimSpace(stdout) != "https://billing.example/portal?session=public" {
		t.Fatalf("safe URL = code %d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestBillingActionJSONAndAmountEdges(t *testing.T) {
	stdout, stderr, code := capturePlanCLI(t, func() int {
		return renderBillingAction(client.BillingAction{
			OperationID: "bop_setup", Operation: client.BillingMutationSetup,
			ActorID: "opr_owner", ActorRole: "account_owner", Confirmed: true,
			Kind: "action", URL: "https://billing.example/setup",
		}, false, true, "done")
	})
	if code != 0 || stderr != "" {
		t.Fatalf("JSON action = %d stderr=%q", code, stderr)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil ||
		doc["kind"] != "action" || doc["url"] != "https://billing.example/setup" ||
		doc["operation_id"] != "bop_setup" || doc["operation"] != "billing_setup" ||
		doc["confirmed"] != true || doc["replayed"] != false {
		t.Fatalf("JSON action = %v, %v", doc, err)
	}
	if got := formatBillingAmount(math.MinInt64, "usd"); got != "USD -92233720368547758.08" {
		t.Fatalf("MinInt64 amount = %q", got)
	}
}

func TestBillingPDFSelectionNeverSkipsTheNewestInvoice(t *testing.T) {
	invoices := []client.BillingInvoice{
		{Number: "newest-draft"},
		{Number: "older-paid", PDFURL: "https://billing.example/older.pdf"},
	}
	if invoice, err := selectBillingInvoicePDF(invoices); err == nil {
		t.Fatalf("selected older invoice %+v", invoice)
	}
	invoices[0].PDFURL = "https://billing.example/newest.pdf"
	invoice, err := selectBillingInvoicePDF(invoices)
	if err != nil || invoice.Number != "newest-draft" {
		t.Fatalf("selected invoice = %+v, %v", invoice, err)
	}
}

func TestBillingRootHelpAdvertisesImplementedSurface(t *testing.T) {
	var out strings.Builder
	usage(&out)
	if !strings.Contains(out.String(), "witself billing show|invoices|payments|portal|setup") {
		t.Fatalf("help omitted billing surface:\n%s", out.String())
	}
}

func TestBillingTopLevelFlagsDelegateToSummary(t *testing.T) {
	// A top-level flag is a summary flag, rather than an unknown subcommand.
	// An unknown flag stops in the summary FlagSet before any local account or
	// network state is consulted, keeping this dispatch test hermetic.
	_, stderr, code := capturePlanCLI(t, func() int {
		return billingCmd([]string{"--not-a-summary-flag"})
	})
	if code != 2 || strings.Contains(stderr, "unknown subcommand") ||
		!strings.Contains(stderr, "flag provided but not defined") {
		t.Fatalf("billing top-level flags = %d stderr=%q", code, stderr)
	}
}

func TestBillingSetupGuardFlagsFailBeforeAccountOrNetworkAccess(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "reason required", args: []string{"--dry-run"}, want: "--reason is required"},
		{
			name: "preview cannot open",
			args: []string{"--reason", "test", "--dry-run", "--open"},
			want: "--dry-run cannot be combined with --open",
		},
		{
			name: "apply guards required",
			args: []string{"--reason", "test", "--yes"},
			want: "requires --yes and --idempotency-key",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code := capturePlanCLI(t, func() int {
				return billingSetup(tc.args)
			})
			if code != 2 || stdout != "" || !strings.Contains(stderr, tc.want) {
				t.Fatalf("code=%d stdout=%q stderr=%q; want %q", code, stdout, stderr, tc.want)
			}
		})
	}
}

func TestBillingContextUsesCellAdvertisedCredentialAudience(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	if err := local.Save("default", local.Account{ID: "acct_self_hosted"}, "operator-private"); err != nil {
		t.Fatal(err)
	}
	cell := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/whoami" {
			if r.Header.Get("Authorization") != "Bearer operator-private" {
				t.Fatalf("whoami auth=%q", r.Header.Get("Authorization"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": "witself.v0",
				"principal": map[string]string{
					"operator_id": "opr_self_hosted",
					"account_id":  "acct_self_hosted",
				},
			})
			return
		}
		if r.URL.Path != "/v1/capabilities" || r.Header.Get("Authorization") != "" {
			t.Fatalf("capability request = %s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.v0",
			"backend":        map[string]string{"kind": "self-hosted"},
			"account":        map[string]string{"id": "acct_self_hosted"},
			"billing": map[string]any{
				"supported": true,
				"endpoint":  "https://private-billing.example/",
			},
		})
	}))
	t.Cleanup(cell.Close)
	ctx := context.Background()
	accountID, token, endpoint, err := billingContext(ctx, "default", cell.URL)
	if err != nil || accountID != "acct_self_hosted" || token != "operator-private" ||
		endpoint != "https://private-billing.example" {
		t.Fatalf("explicit billing context = %q %q %q %v", accountID, token, endpoint, err)
	}
	accountID, token, endpoint, err = planContextWithLocator(
		ctx, "default", "", func(_ context.Context, directory, accountID string) (string, string, error) {
			if directory != defaultControlPlane || accountID != "acct_self_hosted" {
				t.Fatalf("directory lookup = %q %q", directory, accountID)
			}
			return "cell", cell.URL, nil
		})
	if err != nil || accountID != "acct_self_hosted" || token != "operator-private" ||
		endpoint != "https://private-billing.example" {
		t.Fatalf("located billing context = %q %q %q %v", accountID, token, endpoint, err)
	}
	t.Setenv("WITSELF_CONTROL_PLANE", "https://staging-directory.example")
	accountID, token, endpoint, err = planContextWithLocator(
		ctx, "default", "", func(_ context.Context, directory, accountID string) (string, string, error) {
			if directory != "https://staging-directory.example" || accountID != "acct_self_hosted" {
				t.Fatalf("configured directory lookup = %q %q", directory, accountID)
			}
			return "cell", cell.URL, nil
		})
	if err != nil || accountID != "acct_self_hosted" || token != "operator-private" ||
		endpoint != "https://private-billing.example" {
		t.Fatalf("configured billing context = %q %q %q %v", accountID, token, endpoint, err)
	}
}
