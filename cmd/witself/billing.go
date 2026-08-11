package main

import (
	"context"
	"flag"
	"fmt"
	neturl "net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/witwave-ai/witself/internal/client"
)

// billingCmd exposes the provider-neutral billing read/setup surface. Plan
// changes stay under `witself plan`; raw payment credentials never cross this
// command.
func billingCmd(args []string) int {
	if len(args) == 0 {
		return billingShow(nil)
	}
	if strings.HasPrefix(args[0], "-") {
		return billingShow(args)
	}
	switch args[0] {
	case "show":
		return billingShow(args[1:])
	case "invoices":
		return billingInvoices(args[1:])
	case "payments":
		return billingPayments(args[1:])
	case "portal":
		return billingPortal(args[1:])
	case "setup":
		return billingSetup(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "witself billing: unknown subcommand %q\n", args[0])
		fmt.Fprintln(os.Stderr, "usage: witself billing show|invoices|payments|portal|setup")
		return 2
	}
}

func billingShow(args []string) int {
	fs := flag.NewFlagSet("billing show", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account := accountFlag(fs)
	endpoint := billingEndpointFlag(fs)
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: witself billing show [--account NAME] [--endpoint URL] [--json]")
		return 2
	}
	ctx := context.Background()
	accountID, token, controlPlane, err := billingContext(ctx, *account, *endpoint)
	if err != nil {
		return billingCLIError(err)
	}
	summary, err := client.GetBillingSummary(ctx, controlPlane, accountID, token)
	if err != nil {
		return billingCLIError(err)
	}
	if *jsonOut {
		return printJSON(summary)
	}
	printBillingSummary(summary)
	return 0
}

func billingInvoices(args []string) int {
	fs := flag.NewFlagSet("billing invoices", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account := accountFlag(fs)
	endpoint := billingEndpointFlag(fs)
	jsonOut := jsonFlag(fs)
	openPDF := fs.Bool("pdf", false, "open a provider-hosted invoice PDF")
	invoiceNumber := fs.String("invoice", "", "invoice number to open or show (default with --pdf: newest)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || (*jsonOut && *openPDF) {
		fmt.Fprintln(os.Stderr, "usage: witself billing invoices [--account NAME] [--endpoint URL] [--invoice NUMBER] [--pdf | --json]")
		return 2
	}
	ctx := context.Background()
	accountID, token, controlPlane, err := billingContext(ctx, *account, *endpoint)
	if err != nil {
		return billingCLIError(err)
	}
	out, err := client.GetBillingInvoices(ctx, controlPlane, accountID, token)
	if err != nil {
		return billingCLIError(err)
	}
	selected := filterBillingInvoices(out.Invoices, *invoiceNumber)
	if *invoiceNumber != "" && len(selected) == 0 {
		return billingCLIError(fmt.Errorf("invoice %q was not found in recent history", *invoiceNumber))
	}
	if *jsonOut {
		out.Invoices = selected
		return printJSON(out)
	}
	printBillingInvoices(selected)
	if !*openPDF {
		return 0
	}
	invoice, err := selectBillingInvoicePDF(selected)
	if err != nil {
		return billingCLIError(err)
	}
	if err := openBillingURL(invoice.PDFURL); err != nil {
		return billingCLIError(err)
	}
	fmt.Printf("opened invoice %s\n", billingCLIColumn(invoice.Number))
	return 0
}

func billingPayments(args []string) int {
	fs := flag.NewFlagSet("billing payments", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account := accountFlag(fs)
	endpoint := billingEndpointFlag(fs)
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: witself billing payments [--account NAME] [--endpoint URL] [--json]")
		return 2
	}
	ctx := context.Background()
	accountID, token, controlPlane, err := billingContext(ctx, *account, *endpoint)
	if err != nil {
		return billingCLIError(err)
	}
	out, err := client.GetBillingPayments(ctx, controlPlane, accountID, token)
	if err != nil {
		return billingCLIError(err)
	}
	if *jsonOut {
		return printJSON(out)
	}
	printBillingPayments(out.Payments)
	return 0
}

func billingPortal(args []string) int {
	fs := flag.NewFlagSet("billing portal", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account := accountFlag(fs)
	endpoint := billingEndpointFlag(fs)
	open := fs.Bool("open", false, "open the provider-hosted portal in the default browser")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || (*jsonOut && *open) {
		fmt.Fprintln(os.Stderr, "usage: witself billing portal [--account NAME] [--endpoint URL] [--open | --json]")
		return 2
	}
	ctx := context.Background()
	accountID, token, controlPlane, err := billingContext(ctx, *account, *endpoint)
	if err != nil {
		return billingCLIError(err)
	}
	action, err := client.CreateBillingPortal(ctx, controlPlane, accountID, token)
	if err != nil {
		return billingCLIError(err)
	}
	return renderBillingAction(action, *open, *jsonOut, "billing portal is ready")
}

func billingSetup(args []string) int {
	fs := flag.NewFlagSet("billing setup", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account := accountFlag(fs)
	endpoint := billingEndpointFlag(fs)
	email := fs.String("email", "", "billing email used on first provider contact")
	open := fs.Bool("open", false, "open the provider-hosted setup flow in the default browser")
	reason := fs.String("reason", "", "reason recorded with the billing operation")
	idempotencyKey := fs.String("idempotency-key", "", "unique retry-safe operation key")
	confirmed := fs.Bool("yes", false, "confirm the billing operation")
	dryRun := fs.Bool("dry-run", false, "preview without creating a receipt or calling the provider")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	const usage = "usage: witself billing setup --reason TEXT (--dry-run | --idempotency-key KEY --yes) [--account NAME] [--endpoint URL] [--email EMAIL] [--open | --json]"
	if fs.NArg() != 0 || (*jsonOut && *open) || (*dryRun && *open) {
		if *dryRun && *open {
			fmt.Fprintln(os.Stderr, "witself: --dry-run cannot be combined with --open")
		}
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	if !validBillingMutationCLIFlags(
		*reason, *idempotencyKey, *confirmed, *dryRun, usage,
	) {
		return 2
	}
	ctx := context.Background()
	accountID, token, controlPlane, err := billingContext(ctx, *account, *endpoint)
	if err != nil {
		return billingCLIError(err)
	}
	if *dryRun {
		preview, err := client.PreviewBillingMutation(
			ctx, controlPlane, accountID, token, client.BillingMutationSetup, "", *email,
			strings.TrimSpace(*reason))
		if err != nil {
			return billingCLIError(err)
		}
		return printBillingMutationPreview(preview, *jsonOut)
	}
	action, err := client.CreateBillingSetup(
		ctx, controlPlane, accountID, token, *email,
		client.BillingMutationOptions{
			Reason: strings.TrimSpace(*reason), Confirmed: *confirmed,
			IdempotencyKey: strings.TrimSpace(*idempotencyKey),
		})
	if err != nil {
		return billingCLIError(err)
	}
	return renderBillingAction(action, *open, *jsonOut, "payment method is already on file")
}

func renderBillingAction(action client.BillingAction, open, jsonOut bool, doneMessage string) int {
	if action.Kind == "action" {
		if _, err := validatedBillingURL(action.URL); err != nil {
			return billingCLIError(err)
		}
	}
	if jsonOut {
		doc := map[string]any{
			"schema_version": "witself.v0",
			"kind":           action.Kind,
		}
		if action.URL != "" {
			doc["url"] = action.URL
		}
		if action.Operation != "" {
			doc["operation_id"] = action.OperationID
			doc["operation"] = action.Operation
			doc["actor_id"] = action.ActorID
			doc["actor_role"] = action.ActorRole
			doc["confirmed"] = action.Confirmed
			doc["replayed"] = action.Replayed
		}
		return printJSON(doc)
	}
	if action.Kind == "done" {
		fmt.Println(doneMessage)
		return 0
	}
	if open {
		if err := openBillingURL(action.URL); err != nil {
			return billingCLIError(err)
		}
		fmt.Println("opened provider-hosted billing flow")
		return 0
	}
	fmt.Println(billingCLIColumn(action.URL))
	return 0
}

func printBillingSummary(summary client.BillingSummary) {
	availability := "unavailable on this control plane"
	if summary.BillingAvailable {
		availability = "available"
	}
	configured := "not configured"
	if summary.Configured {
		configured = "configured"
	}
	fmt.Printf("billing:      %s; %s\n", availability, configured)
	fmt.Printf("subscription: %s — %s\n",
		formatPlanIdentity(summary.BillingPlan, summary.BillingPlanName),
		billingCLIColumn(summary.SubscriptionStatus))
	if summary.EffectivePlan != summary.BillingPlan {
		fmt.Printf("effective:    %s (account override; billing remains %s)\n",
			formatPlanIdentity(summary.EffectivePlan, summary.EffectivePlanName),
			formatPlanIdentity(summary.BillingPlan, summary.BillingPlanName))
	}
	if summary.AppliedPlan != "" && summary.AppliedPlan != summary.EffectivePlan {
		fmt.Printf("applied:      %s (converging)\n", billingCLIColumn(summary.AppliedPlan))
	}
	if summary.EntitledAt != nil {
		fmt.Printf("entitled-at:  %s\n", summary.EntitledAt.UTC().Format(time.RFC3339))
	}
	if summary.PastDueSince != nil {
		fmt.Printf("past-due:     since %s\n", summary.PastDueSince.UTC().Format(time.RFC3339))
	}
	if summary.PaymentMethod == nil {
		fmt.Println("payment:      none on file")
	} else {
		fmt.Printf("payment:      %s\n", billingCLIColumn(summary.PaymentMethod.Label))
	}
	if summary.NextCharge == nil {
		fmt.Println("next charge:  none reported")
	} else {
		fmt.Printf("next charge:  %s on %s\n",
			formatBillingAmount(summary.NextCharge.AmountCents, summary.NextCharge.Currency),
			summary.NextCharge.Date.UTC().Format(time.RFC3339))
	}
	if pending := summary.Pending; pending != nil {
		fmt.Printf("pending:      %s → %s\n", billingCLIColumn(pending.Kind),
			formatPlanIdentity(pending.Plan, pending.PlanName))
		if pending.Effective != nil {
			fmt.Printf("effective-at: %s\n", pending.Effective.UTC().Format(time.RFC3339))
		}
		if pending.Expires != nil {
			fmt.Printf("expires-at:   %s\n", pending.Expires.UTC().Format(time.RFC3339))
		}
	}
}

func printBillingInvoices(invoices []client.BillingInvoice) {
	if len(invoices) == 0 {
		fmt.Println("no invoices")
		return
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "DATE\tNUMBER\tSTATUS\tAMOUNT\tDOCUMENT")
	for _, invoice := range invoices {
		document := "-"
		if invoice.PDFURL != "" {
			document = "PDF available"
		} else if invoice.HostedURL != "" {
			document = "hosted invoice available"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			invoice.Date.UTC().Format(time.RFC3339), billingCLIColumn(invoice.Number),
			billingCLIColumn(invoice.Status),
			formatBillingAmount(invoice.AmountCents, invoice.Currency), document)
	}
	_ = tw.Flush()
}

func printBillingPayments(payments []client.BillingPayment) {
	if len(payments) == 0 {
		fmt.Println("no payments")
		return
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "DATE\tSTATUS\tAMOUNT\tMETHOD\tRECEIPT")
	for _, payment := range payments {
		receipt := "-"
		if payment.ReceiptURL != "" {
			receipt = "available"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			payment.Date.UTC().Format(time.RFC3339), billingCLIColumn(payment.Status),
			formatBillingAmount(payment.AmountCents, payment.Currency),
			billingCLIColumn(payment.Method), receipt)
	}
	_ = tw.Flush()
}

func filterBillingInvoices(invoices []client.BillingInvoice, number string) []client.BillingInvoice {
	number = strings.TrimSpace(number)
	if number == "" {
		return invoices
	}
	for _, invoice := range invoices {
		if invoice.Number == number {
			return []client.BillingInvoice{invoice}
		}
	}
	return []client.BillingInvoice{}
}

func selectBillingInvoicePDF(invoices []client.BillingInvoice) (client.BillingInvoice, error) {
	if len(invoices) == 0 || strings.TrimSpace(invoices[0].PDFURL) == "" {
		return client.BillingInvoice{}, fmt.Errorf(
			"no provider-hosted PDF is available for the selected invoice",
		)
	}
	return invoices[0], nil
}

func formatBillingAmount(cents int64, currency string) string {
	abs := uint64(cents)
	sign := ""
	if cents < 0 {
		sign = "-"
		abs = uint64(-(cents + 1)) + 1
	}
	return fmt.Sprintf("%s %s%d.%02d", strings.ToUpper(billingCLIColumn(currency)),
		sign, abs/100, abs%100)
}

func validatedBillingURL(raw string) (*neturl.URL, error) {
	if raw == "" || strings.TrimSpace(raw) != raw || billingCLIColumn(raw) != raw ||
		strings.ContainsRune(raw, '\\') {
		return nil, fmt.Errorf("billing provider returned an unsafe hosted URL")
	}
	decoded, err := neturl.PathUnescape(raw)
	if err != nil || billingCLIColumn(decoded) != decoded || strings.ContainsRune(decoded, '\\') {
		return nil, fmt.Errorf("billing provider returned an unsafe hosted URL")
	}
	parsed, err := neturl.Parse(raw)
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") || parsed.Host == "" ||
		parsed.Hostname() == "" || parsed.Opaque != "" || parsed.User != nil {
		return nil, fmt.Errorf("billing provider returned an unsafe hosted URL")
	}
	return parsed, nil
}

func openBillingURL(raw string) error {
	if _, err := validatedBillingURL(raw); err != nil {
		return err
	}
	if err := launchBrowser(raw); err != nil {
		return fmt.Errorf("open billing URL: %w", err)
	}
	return nil
}

func billingCLIColumn(value string) string {
	return planCLIColumn(value)
}

func billingCLIError(err error) int {
	fmt.Fprintf(os.Stderr, "witself: billing: %s\n", billingCLIColumn(err.Error()))
	return 1
}

func billingEndpointFlag(fs *flag.FlagSet) *string {
	return fs.String("endpoint", "", "account cell URL for billing capability discovery")
}

func billingContext(
	ctx context.Context,
	accountName, cellEndpoint string,
) (accountID, token, controlPlane string, err error) {
	return planContext(ctx, accountName, cellEndpoint)
}
