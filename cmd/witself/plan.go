package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/local"
	"github.com/witwave-ai/witself/internal/plans"
)

// planCmd dispatches `witself plan <verb>`: list, status, upgrade, downgrade,
// cancel.
// Talks to the CONTROL PLANE (plans are Cloud-side). Self-hosted deployments
// have no CP — the CLI errors gracefully then.
func planCmd(args []string) int {
	if len(args) == 0 {
		return planStatus(nil)
	}
	switch args[0] {
	case "list":
		return planList(args[1:])
	case "status":
		return planStatus(args[1:])
	case "upgrade":
		return planChangeCLI("upgrade", args[1:])
	case "downgrade":
		return planChangeCLI("downgrade", args[1:])
	case "cancel":
		return planCancelCLI(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "witself plan: unknown subcommand %q\n", args[0])
		fmt.Fprintln(os.Stderr, "usage: witself plan list|status|upgrade|downgrade|cancel")
		return 2
	}
}

func planList(args []string) int {
	fs := flag.NewFlagSet("plan list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	endpoint := fs.String("endpoint", defaultControlPlane, "control-plane URL")
	availableOnly := fs.Bool("available-only", false, "show only plans available today")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: witself plan list [--available-only] [--json] [--endpoint URL]")
		return 2
	}
	catalog, err := client.GetPlanCatalog(context.Background(), *endpoint)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: read plan catalog: %s\n", planCLIColumn(err.Error()))
		return 1
	}
	selected := make([]plans.Plan, 0, len(catalog.Plans))
	for _, plan := range catalog.Plans {
		if !*availableOnly || plan.Available {
			selected = append(selected, plan)
		}
	}
	if *jsonOut {
		return printJSON(struct {
			SchemaVersion string       `json:"schema_version"`
			Updated       string       `json:"updated"`
			Currency      string       `json:"currency"`
			Plans         []plans.Plan `json:"plans"`
		}{plans.SchemaVersion, catalog.Updated, catalog.Currency, selected})
	}
	printPlanCatalog(catalog, selected)
	return 0
}

func printPlanCatalog(catalog *plans.Catalog, entries []plans.Plan) {
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	writeRow := func(label string, values func(plans.Plan) string) {
		_, _ = fmt.Fprint(tw, label)
		for _, plan := range entries {
			_, _ = fmt.Fprintf(tw, "\t%s", values(plan))
		}
		_, _ = fmt.Fprintln(tw)
	}
	_, _ = fmt.Fprint(tw, "CAPABILITY")
	for _, plan := range entries {
		name := planCLIColumn(plan.Name)
		if plan.Recommended {
			name += "*"
		}
		_, _ = fmt.Fprintf(tw, "\t%s", name)
	}
	_, _ = fmt.Fprintln(tw)
	writeRow("Plan ID", func(plan plans.Plan) string { return planCLIColumn(plan.ID) })
	writeRow("Monthly price", func(plan plans.Plan) string { return formatPlanPrice(catalog.Currency, plan) })
	writeRow("Availability", func(plan plans.Plan) string {
		if plan.Available {
			return "Available"
		}
		return "Coming soon"
	})
	writeRow("Realms", func(plan plans.Plan) string { return formatPlanLimit(plan, plans.RealmLimit) })
	writeRow("Agents / realm", func(plan plans.Plan) string { return formatPlanLimit(plan, plans.AgentPerRealmLimit) })
	writeRow("Active memories / agent", func(plan plans.Plan) string { return formatPlanLimit(plan, plans.StoredMemoryLimit) })
	writeRow("Current facts / agent", func(plan plans.Plan) string { return formatPlanLimit(plan, plans.StoredFactLimit) })
	writeRow("Transcript retention", func(plan plans.Plan) string {
		return formatPlanRetention(plan, plans.TranscriptRetentionDaysPolicy, false)
	})
	writeRow("Secrets / agent", func(plan plans.Plan) string { return formatPlanLimit(plan, plans.StoredSecretLimit) })
	writeRow("Messaging", func(plan plans.Plan) string { return formatPlanFeature(plan, plans.MessagingFeature) })
	writeRow("Message retention", func(plan plans.Plan) string {
		return formatPlanRetention(plan, plans.MessageRetentionDaysPolicy, !plan.HasFeature(plans.MessagingFeature))
	})
	writeRow("Receive-email entitlement", func(plan plans.Plan) string { return formatPlanFeature(plan, plans.AgentEmailReceiveFeature) })
	writeRow("Send-email entitlement", func(plan plans.Plan) string { return formatPlanFeature(plan, plans.AgentEmailSendFeature) })
	writeRow("Email retention", func(plan plans.Plan) string {
		return formatPlanRetention(plan, plans.AgentEmailRetentionDaysPolicy, !plan.HasFeature(plans.AgentEmailReceiveFeature))
	})
	writeRow("Maximum raw email", func(plan plans.Plan) string {
		return formatPlanByteLimit(plan, plans.AgentEmailMaxRawBytesLimit)
	})
	writeRow("Attachment storage / account", func(plan plans.Plan) string {
		return formatPlanByteLimit(plan, plans.AgentEmailAttachmentStorageBytesLimit)
	})
	writeRow("Realm email aliases / realm", func(plan plans.Plan) string {
		return formatPlanLimit(plan, plans.AgentEmailRealmAliasesPerRealmLimit)
	})
	writeRow("Custom inbound domains", func(plan plans.Plan) string {
		value := formatPlanLimit(plan, plans.AgentEmailCustomDomainsPerAccountLimit)
		if plan.ID == "enterprise" && value == "0" {
			return "Contracted (0 default)"
		}
		return value
	})
	_ = tw.Flush()

	for _, plan := range entries {
		if plan.Recommended {
			fmt.Printf("\n* %s — %s\n", planCLIColumn(plan.Badge), planCLIColumn(plan.Name))
			break
		}
	}
	fmt.Printf("Catalog updated %s. Missing caps mean no catalog cap; missing policy days mean indefinite retention.\n", planCLIColumn(catalog.Updated))
	fmt.Println("Enterprise is not purchasable and requires explicit contracted overrides before customer sale.")
	fmt.Println("Entitlements do not prove delivery readiness; managed email routing and account rollout gates remain separate.")
}

func formatPlanPrice(currency string, plan plans.Plan) string {
	currency = planCLIColumn(currency)
	switch {
	case plan.PriceMonthly != nil:
		if currency == "USD" {
			return fmt.Sprintf("$%s / month", formatPlanInteger(*plan.PriceMonthly))
		}
		return fmt.Sprintf("%s %s / month", currency, formatPlanInteger(*plan.PriceMonthly))
	case plan.PriceMonthlyMin != nil:
		if currency == "USD" {
			return fmt.Sprintf("$%s+ / month", formatPlanInteger(*plan.PriceMonthlyMin))
		}
		return fmt.Sprintf("%s %s+ / month", currency, formatPlanInteger(*plan.PriceMonthlyMin))
	default:
		return "Contact us"
	}
}

func formatPlanLimit(plan plans.Plan, key string) string {
	value, ok := plan.Limits[key]
	if !ok {
		return "No catalog cap"
	}
	return formatPlanInteger(value)
}

func formatPlanInteger(value int64) string {
	raw := strconv.FormatInt(value, 10)
	if len(raw) <= 3 {
		return raw
	}
	first := len(raw) % 3
	if first == 0 {
		first = 3
	}
	var out strings.Builder
	out.Grow(len(raw) + len(raw)/3)
	out.WriteString(raw[:first])
	for i := first; i < len(raw); i += 3 {
		out.WriteByte(',')
		out.WriteString(raw[i : i+3])
	}
	return out.String()
}

func formatPlanRetention(plan plans.Plan, key string, cleanupOnly bool) string {
	days, ok := plan.Policies[key]
	if !ok {
		return "Indefinite"
	}
	value := fmt.Sprintf("%s days", formatPlanInteger(days))
	if cleanupOnly {
		value += " cleanup"
	}
	return value
}

func formatPlanFeature(plan plans.Plan, feature string) string {
	if plan.HasFeature(feature) {
		return "Enabled"
	}
	return "Disabled"
}

func formatPlanByteLimit(plan plans.Plan, key string) string {
	value, ok := plan.Limits[key]
	if !ok {
		return "No catalog cap"
	}
	if value == 0 {
		return "0"
	}
	for _, unit := range []struct {
		bytes int64
		name  string
	}{{1 << 30, "GiB"}, {1 << 20, "MiB"}, {1 << 10, "KiB"}} {
		if value >= unit.bytes && value%unit.bytes == 0 {
			return fmt.Sprintf("%s %s", formatPlanInteger(value/unit.bytes), unit.name)
		}
	}
	return fmt.Sprintf("%s bytes", formatPlanInteger(value))
}

// planContext resolves the account and asks its current cell for the
// authoritative billing control-plane route before forwarding the operator
// token. Directory lookup and the value-free capability read carry no operator
// credential. The token first authenticates to the selected account cell, then
// goes only to the endpoint advertised by that account-verified cell.
func planContext(
	ctx context.Context,
	accountName, cellEndpoint string,
) (accountID, token, controlPlane string, err error) {
	return planContextWithLocator(
		ctx, accountName, cellEndpoint, client.LookupAccount,
	)
}

func planContextWithLocator(
	ctx context.Context,
	accountName, cellEndpoint string,
	locate accountLocator,
) (accountID, token, controlPlane string, err error) {
	name, acct, tok, err := local.Resolve(accountName)
	if err != nil {
		return "", "", "", err
	}
	if strings.TrimSpace(cellEndpoint) == "" {
		directory := defaultControlPlane
		// Preserve the existing staging override only as an unauthenticated
		// directory audience. The operator token still goes exclusively to the
		// located cell and the billing endpoint that cell advertises.
		if configured := strings.TrimSpace(os.Getenv("WITSELF_CONTROL_PLANE")); configured != "" {
			directory = configured
		}
		_, cellEndpoint, err = locate(ctx, directory, acct.ID)
		if err != nil {
			return "", "", "", fmt.Errorf(
				"locate account %q (%s): %w", name, acct.ID, err,
			)
		}
	}
	capability, err := client.GetBillingCapability(ctx, cellEndpoint, acct.ID, tok)
	if err != nil {
		return "", "", "", fmt.Errorf("discover billing capability: %w", err)
	}
	if !capability.Supported {
		reason := strings.TrimSpace(capability.Reason)
		if reason == "" {
			reason = "not configured"
		}
		return "", "", "", fmt.Errorf("billing is not supported for this account (%s)", reason)
	}
	return acct.ID, tok, capability.Endpoint, nil
}

func planStatus(args []string) int {
	fs := flag.NewFlagSet("plan status", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account := accountFlag(fs)
	endpoint := fs.String("endpoint", "", "account cell URL for billing capability discovery")
	full := fs.Bool("full", false, "show the effective feature set and every known limit and policy")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: witself plan status [--account NAME] [--endpoint CELL_URL] [--full] [--json]")
		return 2
	}
	ctx := context.Background()
	acctID, tok, cp, err := planContext(ctx, *account, *endpoint)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %s\n", planCLIColumn(err.Error()))
		return 1
	}
	status, err := client.GetPlan(ctx, cp, acctID, tok)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %s\n", planCLIColumn(err.Error()))
		return 1
	}
	if *jsonOut {
		return printJSON(status)
	}
	printPlanStatus(status, *full)
	return 0
}

func planChangeCLI(verb string, args []string) int {
	fs := flag.NewFlagSet("plan "+verb, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account := accountFlag(fs)
	endpoint := fs.String("endpoint", "", "account cell URL for billing capability discovery")
	email := fs.String("email", "", "billing email (used on first purchase)")
	reason := fs.String("reason", "", "reason recorded with the billing operation")
	idempotencyKey := fs.String("idempotency-key", "", "unique retry-safe operation key")
	confirmed := fs.Bool("yes", false, "confirm the billing operation")
	dryRun := fs.Bool("dry-run", false, "preview without creating a receipt or calling the provider")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	reasonSpec := "--reason TEXT"
	if verb == "downgrade" {
		reasonSpec = "[--reason TEXT]"
	}
	usage := fmt.Sprintf("usage: witself plan %s %s (--dry-run | --idempotency-key KEY --yes) [--account NAME] [--endpoint CELL_URL] [--email E] [--json] TARGET_PLAN", verb, reasonSpec)
	if fs.NArg() != 1 || !validBillingMutationCLIFlags(
		*reason, *idempotencyKey, *confirmed, *dryRun, usage, verb != "downgrade",
	) {
		if fs.NArg() != 1 {
			fmt.Fprintln(os.Stderr, usage)
		}
		return 2
	}
	if verb == "downgrade" && strings.TrimSpace(*reason) == "" {
		*reason = "customer requested cancellation"
	}
	target := fs.Arg(0)
	ctx := context.Background()
	acctID, tok, cp, err := planContext(ctx, *account, *endpoint)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %s\n", planCLIColumn(err.Error()))
		return 1
	}
	catalog, err := client.GetPlanCatalog(ctx, cp)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: read plan catalog: %s\n", planCLIColumn(err.Error()))
		return 1
	}
	targetID, targetName, err := resolvePlanTarget(catalog, target)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %s\n", planCLIColumn(err.Error()))
		return 2
	}
	operation := client.BillingMutationUpgrade
	if verb == "downgrade" {
		operation = client.BillingMutationDowngrade
	}
	if *dryRun {
		preview, err := client.PreviewBillingMutation(
			ctx, cp, acctID, tok, operation, targetID, *email,
			strings.TrimSpace(*reason))
		if err != nil {
			fmt.Fprintf(os.Stderr, "witself: %s\n", planCLIColumn(err.Error()))
			return 1
		}
		return printBillingMutationPreview(preview, *jsonOut)
	}
	options := client.BillingMutationOptions{
		Reason: strings.TrimSpace(*reason), Confirmed: *confirmed,
		IdempotencyKey: strings.TrimSpace(*idempotencyKey),
	}
	var out client.PlanOutcome
	switch verb {
	case "upgrade":
		out, err = client.UpgradePlan(ctx, cp, acctID, tok, targetID, *email, options)
	case "downgrade":
		out, err = client.DowngradePlan(ctx, cp, acctID, tok, targetID, *email, options)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %s\n", planCLIColumn(err.Error()))
		return 1
	}
	if *jsonOut {
		return printBillingMutationOutcome(out)
	}
	printPlanOutcome(out, targetName)
	return 0
}

func resolvePlanTarget(catalog *plans.Catalog, target string) (string, string, error) {
	target = strings.TrimSpace(target)
	for _, plan := range catalog.Plans {
		if strings.EqualFold(target, plan.ID) || strings.EqualFold(target, plan.Name) {
			return plan.ID, plan.Name, nil
		}
	}
	choices := make([]string, 0, len(catalog.Plans))
	for _, plan := range catalog.Plans {
		choices = append(choices, fmt.Sprintf("%s (%s)", plan.Name, plan.ID))
	}
	return "", "", fmt.Errorf("unknown plan %q; choose %s", target, strings.Join(choices, ", "))
}

func planCancelCLI(args []string) int {
	fs := flag.NewFlagSet("plan cancel", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account := accountFlag(fs)
	endpoint := fs.String("endpoint", "", "account cell URL for billing capability discovery")
	reason := fs.String("reason", "", "reason recorded with the billing operation")
	idempotencyKey := fs.String("idempotency-key", "", "unique retry-safe operation key")
	confirmed := fs.Bool("yes", false, "confirm the billing operation")
	dryRun := fs.Bool("dry-run", false, "preview without creating a receipt or calling the provider")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	const usage = "usage: witself plan cancel [--reason TEXT] (--dry-run | --idempotency-key KEY --yes) [--account NAME] [--endpoint CELL_URL] [--json]"
	if strings.TrimSpace(*reason) == "" {
		*reason = "customer requested cancellation of the pending change"
	}
	if fs.NArg() != 0 || !validBillingMutationCLIFlags(
		*reason, *idempotencyKey, *confirmed, *dryRun, usage, false,
	) {
		if fs.NArg() != 0 {
			fmt.Fprintln(os.Stderr, usage)
		}
		return 2
	}
	ctx := context.Background()
	acctID, tok, cp, err := planContext(ctx, *account, *endpoint)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %s\n", planCLIColumn(err.Error()))
		return 1
	}
	if *dryRun {
		preview, err := client.PreviewBillingMutation(
			ctx, cp, acctID, tok, client.BillingMutationCancel, "", "",
			strings.TrimSpace(*reason))
		if err != nil {
			fmt.Fprintf(os.Stderr, "witself: %s\n", planCLIColumn(err.Error()))
			return 1
		}
		return printBillingMutationPreview(preview, *jsonOut)
	}
	out, err := client.CancelPlanChange(ctx, cp, acctID, tok,
		client.BillingMutationOptions{
			Reason: strings.TrimSpace(*reason), Confirmed: *confirmed,
			IdempotencyKey: strings.TrimSpace(*idempotencyKey),
		})
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %s\n", planCLIColumn(err.Error()))
		return 1
	}
	if *jsonOut {
		return printBillingMutationOutcome(out)
	}
	return printPlanCancelOutcome(out)
}

func printPlanCancelOutcome(out client.PlanOutcome) int {
	if out.Kind == "resolved" {
		fmt.Println("pending change was already resolved; no cancellation was applied")
	} else {
		fmt.Println("cancelled the pending plan change — your current subscription continues and will keep renewing monthly")
		fmt.Println("(to stop renewal: witself plan downgrade free)")
	}
	return 0
}

func validBillingMutationCLIFlags(
	reason, idempotencyKey string,
	confirmed, dryRun bool,
	usage string,
	reasonRequired bool,
) bool {
	if reasonRequired && strings.TrimSpace(reason) == "" {
		fmt.Fprintln(os.Stderr, "witself: --reason is required")
		fmt.Fprintln(os.Stderr, usage)
		return false
	}
	if dryRun {
		if confirmed || strings.TrimSpace(idempotencyKey) != "" {
			fmt.Fprintln(os.Stderr, "witself: --dry-run cannot be combined with --yes or --idempotency-key")
			fmt.Fprintln(os.Stderr, usage)
			return false
		}
		return true
	}
	if !confirmed || strings.TrimSpace(idempotencyKey) == "" {
		fmt.Fprintln(os.Stderr, "witself: applying a billing change requires --yes and --idempotency-key")
		fmt.Fprintln(os.Stderr, usage)
		return false
	}
	if idempotencyKey != strings.TrimSpace(idempotencyKey) {
		fmt.Fprintln(os.Stderr, "witself: --idempotency-key cannot have leading or trailing whitespace")
		fmt.Fprintln(os.Stderr, usage)
		return false
	}
	return true
}

// printPlanStatus renders the record — designed to say the truth: current
// plan, what's applied, and anything pending (a URL to resume, an effective
// date, an apply-blocked reason).
func printPlanStatus(s client.PlanStatus, full bool) {
	fmt.Printf("plan:     %s\n", formatPlanIdentity(s.Plan, s.PlanName))
	if s.BillingPlan != "" && s.BillingPlan != s.Plan {
		fmt.Printf("billing:  %s   (effective plan override: %s)\n",
			formatPlanIdentity(s.BillingPlan, s.BillingPlanName),
			formatPlanIdentity(s.Plan, s.PlanName))
	}
	if s.Applied != s.Plan {
		appliedName := ""
		switch s.Applied {
		case s.Plan:
			appliedName = s.PlanName
		case s.BillingPlan:
			appliedName = s.BillingPlanName
		}
		applied := formatPlanIdentity(s.Applied, appliedName)
		if strings.TrimSpace(s.Applied) == "" {
			applied = "none"
		}
		fmt.Printf("applied:  %s   (converging — retry or wait)\n", applied)
	}
	if s.ApplyPending && s.Applied == s.Plan {
		fmt.Println("snapshot: apply pending")
	}
	if s.Transcript != nil {
		fmt.Printf("transcripts: %s\n", formatPlanRetentionStatus(*s.Transcript))
	}
	if s.Messaging != nil {
		fmt.Printf("messaging:   %s\n", formatPlanFeatureStatus(*s.Messaging))
	}
	if s.MessageRetention != nil {
		fmt.Printf("messages:    %s retention\n", formatPlanRetentionStatus(*s.MessageRetention))
	}
	if s.EmailReceive != nil {
		fmt.Printf("email entitlement: %s\n", formatPlanFeatureStatus(*s.EmailReceive))
		fmt.Println("email delivery:    not reported by plan status (separate rollout gates)")
	}
	if s.EmailSend != nil {
		fmt.Printf("email sending:     %s\n", formatPlanFeatureStatus(*s.EmailSend))
	}
	if s.EmailRetention != nil {
		fmt.Printf("email data:  %s retention\n", formatPlanRetentionStatus(*s.EmailRetention))
	}
	if s.ApplyBlocked != "" {
		fmt.Printf("blocked:  %s\n", planCLIColumn(s.ApplyBlocked))
	}
	if s.PastDueSince != nil {
		fmt.Printf("past-due: since %s\n", s.PastDueSince.Format(time.RFC3339))
	}
	if p := s.Pending; p != nil {
		fmt.Println()
		switch p.Kind {
		case "upgrade":
			fmt.Printf("pending:  upgrade → %s (awaiting payment)\n",
				formatPlanIdentity(p.Plan, p.PlanName))
			if p.URL != "" {
				fmt.Printf("  resume: %s\n", planCLIColumn(p.URL))
			}
			if p.Expires != nil {
				fmt.Printf("  expires: %s\n", p.Expires.Format(time.RFC3339))
			}
		case "downgrade":
			fmt.Printf("pending:  downgrade → %s (scheduled)\n",
				formatPlanIdentity(p.Plan, p.PlanName))
			if p.Effective != nil {
				fmt.Printf("  effective: %s\n", p.Effective.Format(time.RFC3339))
			}
			fmt.Println("  resume paid plan (undo this scheduled downgrade): witself plan cancel --idempotency-key KEY --yes")
		case "contact":
			fmt.Printf("pending:  interest in %s recorded — we'll be in touch\n",
				formatPlanIdentity(p.Plan, p.PlanName))
		}
	}
	if full {
		printPlanStatusDetails(s)
	}
}

func formatPlanIdentity(id, name string) string {
	id = planCLIColumn(id)
	name = planCLIColumn(name)
	if strings.TrimSpace(name) == "" || name == id {
		return id
	}
	return fmt.Sprintf("%s (%s)", name, id)
}

func planCLIColumn(value string) string {
	return tabSafe(safeText(value))
}

func formatPlanFeatureStatus(status client.PlanFeatureStatus) string {
	effective := "disabled"
	if status.Enabled {
		effective = "enabled"
	}
	if !status.Overridden {
		return effective
	}
	planDefault := "disabled"
	if status.DefaultEnabled {
		planDefault = "enabled"
	}
	return fmt.Sprintf("%s (account override; plan default %s)", effective, planDefault)
}

func formatPlanRetentionStatus(status client.PlanRetentionStatus) string {
	effective := "indefinite"
	if status.EffectiveDays != nil {
		effective = fmt.Sprintf("%s days", formatPlanInteger(*status.EffectiveDays))
	}
	if !status.Overridden {
		return effective
	}
	planDefault := "indefinite"
	if status.DefaultDays != nil {
		planDefault = fmt.Sprintf("%s days", formatPlanInteger(*status.DefaultDays))
	}
	return fmt.Sprintf("%s (account override; plan default %s)", effective, planDefault)
}

func printPlanStatusDetails(status client.PlanStatus) {
	features := append([]string(nil), status.Features...)
	sort.Strings(features)
	fmt.Println("\neffective features:")
	if len(features) == 0 {
		fmt.Println("  none")
	} else {
		for _, feature := range features {
			marker := ""
			if !containsPlanString(status.FeatureDefaults, feature) {
				marker = " (account override)"
			}
			fmt.Printf("  %s%s\n", planCLIColumn(feature), marker)
		}
	}
	printPlanValueMap("effective limits", planStatusLimitKeys,
		status.Limits, status.LimitDefaults, "no plan cap")
	printPlanValueMap("effective policies", planStatusPolicyKeys,
		status.Policies, status.PolicyDefaults, "indefinite")
}

var planStatusLimitKeys = []string{
	plans.RealmLimit,
	plans.AgentPerRealmLimit,
	plans.AgentLimit,
	plans.StoredMemoryLimit,
	plans.StoredFactLimit,
	plans.StoredSecretLimit,
	plans.MessageSentPerAgentMinuteLimit,
	plans.MessageDeliveredPerRealmMinuteLimit,
	plans.MessageDeliveredPerRecipientMinuteLimit,
	plans.AgentEmailMaxRawBytesLimit,
	plans.AgentEmailAttachmentStorageBytesLimit,
	plans.AgentEmailRealmAliasesPerRealmLimit,
	plans.AgentEmailCustomDomainsPerAccountLimit,
	plans.AgentEmailSentPerAgentMinuteLimit,
	plans.AgentEmailSentPerRealmMinuteLimit,
	plans.AgentEmailReceivedPerSenderMinuteLimit,
	plans.AgentEmailReceivedPerRecipientMinuteLimit,
	plans.AgentEmailReceivedPerRealmMinuteLimit,
	plans.AgentEmailReceivedBytesPerSenderMinuteLimit,
	plans.AgentEmailReceivedBytesPerRecipientMinuteLimit,
	plans.AgentEmailReceivedBytesPerRealmMinuteLimit,
}

var planStatusPolicyKeys = []string{
	plans.TranscriptRetentionDaysPolicy,
	plans.MessageRetentionDaysPolicy,
	plans.MessagingEntitlementVersionPolicy,
	plans.AgentEmailRetentionDaysPolicy,
	plans.AgentEmailEntitlementVersionPolicy,
}

func containsPlanString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func printPlanValueMap(title string, known []string, effective, defaults map[string]int64, missing string) {
	keys := make(map[string]struct{}, len(known)+len(effective)+len(defaults))
	ordered := make([]string, 0, len(known)+len(effective)+len(defaults))
	for _, key := range known {
		if _, duplicate := keys[key]; duplicate {
			continue
		}
		keys[key] = struct{}{}
		ordered = append(ordered, key)
	}
	extras := make([]string, 0, len(effective)+len(defaults))
	for key := range effective {
		if _, present := keys[key]; !present {
			keys[key] = struct{}{}
			extras = append(extras, key)
		}
	}
	for key := range defaults {
		if _, present := keys[key]; !present {
			keys[key] = struct{}{}
			extras = append(extras, key)
		}
	}
	sort.Strings(extras)
	ordered = append(ordered, extras...)
	fmt.Printf("\n%s:\n", title)
	if len(ordered) == 0 {
		fmt.Printf("  none (%s)\n", missing)
		return
	}
	for _, key := range ordered {
		value, ok := effective[key]
		effectiveText := missing
		if ok {
			effectiveText = formatPlanInteger(value)
		}
		defaultValue, defaultOK := defaults[key]
		if ok == defaultOK && (!ok || value == defaultValue) {
			fmt.Printf("  %s: %s\n", planCLIColumn(key), effectiveText)
			continue
		}
		defaultText := missing
		if defaultOK {
			defaultText = formatPlanInteger(defaultValue)
		}
		fmt.Printf("  %s: %s (account override; plan default %s)\n",
			planCLIColumn(key), effectiveText, defaultText)
	}
}

// printPlanOutcome renders what a change resolved to. The one CLI path branches
// on Kind — the shape the `done | needs_action(url)` adapter guarantees.
func printPlanOutcome(out client.PlanOutcome, targetName string) {
	plan := formatPlanIdentity(out.Plan, targetName)
	switch out.Kind {
	case "done":
		fmt.Printf("upgraded to %s\n", plan)
	case "action":
		fmt.Printf("complete your %s upgrade at:\n  %s\n", plan, planCLIColumn(out.URL))
		fmt.Println("(this link expires; re-run to get a new one)")
		fmt.Println("this is an automatically renewing monthly subscription at the price shown at checkout;")
		fmt.Println("cancel anytime with `witself plan downgrade free` — see https://self.witwave.ai/legal/refunds")
	case "scheduled":
		fmt.Printf("downgrade to %s scheduled for %s\n", plan, out.Effective.Format(time.RFC3339))
		fmt.Println("no further charges will occur; to undo and resume the paid plan before then: witself plan cancel --idempotency-key KEY --yes")
	case "contact":
		fmt.Printf("interest in %s recorded — we'll be in touch\n", plan)
	default:
		fmt.Printf("%s (kind=%s)\n", plan, planCLIColumn(out.Kind))
	}
}

func printBillingMutationOutcome(out client.PlanOutcome) int {
	doc := map[string]any{
		"schema_version": "witself.v0",
		"operation_id":   out.OperationID,
		"operation":      out.Operation,
		"actor_id":       out.ActorID,
		"actor_role":     out.ActorRole,
		"confirmed":      out.Confirmed,
		"replayed":       out.Replayed,
		"kind":           out.Kind,
	}
	if out.Plan != "" {
		doc["plan"] = out.Plan
	}
	if out.URL != "" {
		doc["url"] = out.URL
	}
	if !out.Effective.IsZero() {
		doc["effective"] = out.Effective
	}
	if out.Kind == "cancelled" {
		doc["cancelled"] = true
	}
	return printJSON(doc)
}

func printBillingMutationPreview(preview client.BillingMutationPreview, jsonOut bool) int {
	if jsonOut {
		return printJSON(preview)
	}
	fmt.Printf("operation:    %s\n", planCLIColumn(string(preview.Operation)))
	if preview.Plan != "" {
		fmt.Printf("plan:         %s\n", planCLIColumn(preview.Plan))
	}
	if preview.Allowed {
		fmt.Println("allowed:      yes")
	} else {
		fmt.Println("allowed:      no")
	}
	if preview.ConfirmationRequired {
		fmt.Println("confirmation: required for apply")
	} else {
		fmt.Println("confirmation: not required")
	}
	for _, effect := range preview.Effects {
		fmt.Printf("effect:       %s\n", planCLIColumn(effect))
	}
	for _, violation := range preview.Violations {
		fmt.Printf("violation:    %s\n", planCLIColumn(violation))
	}
	return 0
}
