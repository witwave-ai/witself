package agenttui

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/witwave-ai/witself/internal/plans"
)

func accountValue(o object, key string) string { return first(str(o, key), "Unknown") }

// Only decoded, validated maps reach this formatter. Map presence carries the
// distinction between an unknown source and an omitted (uncapped) dimension.
func accountLimitValue(o object, source, key string) string {
	if o[source] == nil {
		return "Unknown"
	}
	limits := obj(o[source])
	if _, exists := limits[key]; !exists {
		return "No plan cap"
	}
	return accountValue(limits, key) + " " + accountValue(obj(o[source+"_units"]), key)
}

func accountRetentionValue(o object, key string) string {
	v, exists := o[key]
	if !exists {
		return "Unknown"
	}
	if v == nil {
		return "Indefinite"
	}
	n, ok := v.(json.Number)
	if !ok {
		return "Unknown"
	}
	days, err := n.Int64()
	if err != nil || days <= 0 || days > plans.MaxPlanLimit {
		return "Unknown"
	}
	return fmt.Sprintf("%d days", days)
}
func accountFields(d *detailWriter, o object, fields ...string) {
	for _, field := range fields {
		key, label, _ := strings.Cut(field, ":")
		if label == "" {
			label = strings.ReplaceAll(key, "_", " ")
		}
		d.kv(label, accountValue(o, key))
	}
}
func accountAmount(o object) string {
	// Exact decimal integer cents; never float conversion or cross-currency totals.
	return accountValue(o, "amount_cents") + " cents · " + accountValue(o, "currency")
}
func accountTruncation(d *detailWriter, o object) {
	if flag(o, "truncated") {
		d.dim("Truncated · only bounded records/text are shown.")
	}
}
func accountErrorText(status string, thread bool) string {
	if status == "response_too_large" {
		if thread {
			return "This thread exceeds the console read limit (4 MiB). Inspect it with the existing support CLI: witself account support show. Account access remains available."
		}
		return "This source exceeds the console read limit. Inspect it with the existing account CLI. Account access remains available."
	}
	if status == "loading" {
		return "Loading…"
	}
	return "Unavailable · this source could not be read."
}
func (m *Model) renderAccount() {
	a := &m.account
	if !a.mode || !a.context.available() {
		a.vp.SetContent("")
		return
	}
	d := &detailWriter{m: m, width: a.vp.Width}
	selectedOffset := -1
	section := m.accountSection()
	contextClipped := m.accountContextStrip(d)
	if a.status != "ready" {
		d.body(accountErrorText(a.status, false))
	} else {
		o := a.data
		switch section {
		case "overview":
			accountFields(d, obj(o["account"]), "display_name:Display name", "status:Status", "email:Contact email", "created_at:Created")
		case "clients":
			d.dim("Local metadata only · no online or remote installation status.")
			if str(o, "status") == "not_checked" {
				d.kv("This device", "Not checked")
				d.kv("Checked at", "Not checked")
				d.kv("Effective verification", "Not run")
				break
			}
			report := obj(o["report"])
			accountFields(d, report, "checked_at:Checked at", "scan_status:Scan status")
			rows := list(report, "entries")
			if len(rows) == 0 {
				d.body("No matching client records reported.")
			}
			for _, row := range rows {
				d.heading(accountValue(row, "runtime"))
				accountFields(d, row, "recorded_version:Recorded version", "executable_status:Executable", "configuration_status:Static configuration", "configuration_scope:Configuration scope", "installed_at:Recorded installation")
				d.kv("Effective verification", map[bool]string{true: "Not run", false: "Unknown"}[str(row, "effective_verification") == "not_run"])
			}
			accountTruncation(d, report)
		case "plan":
			accountFields(d, o, "plan_name:Current plan name", "plan:Current plan", "applied:Applied plan", "billing_plan_name:Billing plan name", "billing_plan:Billing plan", "billing_available:Billing available", "apply_pending:Application pending", "apply_blocked:Application blocked", "past_due_since:Past due since")
			d.heading("Pending change")
			pending := obj(o["pending"])
			if len(pending) == 0 {
				d.body("No pending change reported.")
			} else {
				accountFields(d, pending, "kind:Kind", "plan:Plan", "plan_name:Plan name", "requested:Requested", "effective:Effective", "expires:Expires")
			}
			d.heading("Limits · effective / default")
			d.dim("No plan cap still permits platform ceilings.")
			for _, key := range plans.SupportedLimitKeys() {
				d.kv(strings.ReplaceAll(key, "_", " "), accountLimitValue(o, "limits", key)+" / "+accountLimitValue(o, "limit_defaults", key))
			}
			for _, key := range []string{"features", "feature_defaults"} {
				label := map[string]string{"features": "Effective features", "feature_defaults": "Default features"}[key]
				value := "Unknown"
				if v, ok := o[key]; ok {
					value = stringsOf(v)
					if value == "" {
						value = "None reported"
					}
				}
				d.kv(label, value)
			}
			for _, key := range []string{"messaging", "email_receive", "email_send"} {
				d.heading(strings.ReplaceAll(key, "_", " "))
				accountFields(d, obj(o[key]), "enabled:Enabled", "default_enabled:Default enabled", "overridden:Overridden")
			}
			for _, key := range []string{"message_retention", "email_retention", "transcript_retention"} {
				d.heading(strings.ReplaceAll(key, "_", " "))
				retention := obj(o[key])
				d.kv("Effective", accountRetentionValue(retention, "effective_days"))
				d.kv("Default", accountRetentionValue(retention, "default_days"))
				accountFields(d, retention, "overridden:Overridden")
			}
		case "billing":
			summary := obj(o["summary"])
			d.heading("Billing summary")
			if !flag(summary, "available") {
				d.body(accountErrorText(str(summary, "error"), false))
			} else {
				accountFields(d, summary, "subscription_status:Subscription", "configured:Configured", "billing_available:Billing available", "billing_plan_name:Billing plan name", "billing_plan:Billing plan", "effective_plan_name:Effective plan name", "effective_plan:Effective plan", "applied_plan:Applied plan", "entitled_at:Entitled at", "past_due_since:Past due since")
				d.kv("Payment method", accountValue(obj(summary["payment_method"]), "label"))
				charge := obj(summary["next_charge"])
				d.kv("Next charge", accountAmount(charge))
				accountFields(d, charge, "date:Charge date")
				if pending := obj(summary["pending"]); len(pending) > 0 {
					d.heading("Pending billing change")
					accountFields(d, pending, "kind:Kind", "plan:Plan", "plan_name:Plan name", "requested:Requested", "effective:Effective", "expires:Expires")
				}
				accountTruncation(d, summary)
			}
			for _, key := range []string{"invoices", "payments"} {
				d.heading(map[string]string{"invoices": "Invoices", "payments": "Payments"}[key])
				source := obj(o[key])
				if !flag(source, "available") {
					d.body(accountErrorText(str(source, "error"), false))
					continue
				}
				rows := list(source, "entries")
				if len(rows) == 0 {
					d.body("No records reported.")
				}
				for i, row := range rows {
					label := fmt.Sprintf("Payment %d", i+1)
					if key == "invoices" {
						label = "Invoice " + first(str(row, "number"), fmt.Sprint(i+1))
					}
					d.heading(label)
					d.kv("Amount", accountAmount(row))
					accountFields(d, row, "date:Date", "status:Status")
					if key == "payments" {
						accountFields(d, row, "method:Method")
					}
				}
				accountTruncation(d, source)
			}
		case "support":
			t := &a.ticket
			t.Lock()
			id, status, data := t.id, t.status, t.data
			t.Unlock()
			if id != "" {
				d.heading("Selected thread · " + id)
				d.dim("Esc closes and clears this thread")
				if status != "ready" {
					d.body(accountErrorText(status, true))
				} else {
					ticket := obj(data["ticket"])
					for _, message := range list(data, "messages") {
						d.heading(accountValue(message, "author_kind") + " · " + accountValue(message, "posted_at"))
						d.body(str(message, "body"))
					}
					if len(list(data, "messages")) == 0 {
						d.body("No messages reported.")
					}
					d.heading("Ticket details")
					accountFields(d, ticket, "subject:Subject", "state:State", "category:Category", "priority:Priority")
					// Retain list metadata below the deliberately loaded body.
					for _, row := range list(o, "tickets") {
						if str(row, "id") == id {
							accountFields(d, row, "opened_at:Opened", "last_activity_at:Last activity", "first_response_at:First response", "resolved_at:Resolved", "closed_at:Closed")
						}
					}
					accountTruncation(d, data)
				}
			} else {
				d.dim("↑↓ select ticket · Enter deliberately opens text")
				rows := list(o, "tickets")
				if len(rows) == 0 {
					d.body("No tickets reported.")
				}
				for i, row := range rows {
					prefix := "  "
					if i == a.selected {
						prefix = "› "
						selectedOffset = len(d.lines)
						if m.height > 24 && len(d.lines) > 0 {
							selectedOffset++
						}
					}
					d.heading(prefix + accountValue(row, "subject"))
					d.kv("Ticket", accountValue(row, "id"))
					d.kv("State", accountValue(row, "state")+" · "+accountValue(row, "priority"))
					accountFields(d, row, "category:Category", "opened_at:Opened", "last_activity_at:Last activity", "first_response_at:First response", "resolved_at:Resolved", "closed_at:Closed")
				}
			}
		case "access":
			d.body("Current manager permissions apply to this account. They do not grant the selected agent account access.")
			d.heading("Available read sections")
			for _, key := range accountSections {
				if slices.Contains(a.context.sections, key) {
					d.body(accountLabels[key])
				}
			}
			d.dim("Management remains in the existing CLI/MCP workflows.")
		}
		accountTruncation(d, o)
	}
	if contextClipped {
		d.heading("Full account context · read-only")
		d.kv("Account", a.context.account)
		d.kv("Manager", a.context.operator)
		d.kv("Role", a.context.role)
		d.kv("Selected agent", m.accountAgentName())
	}
	old := a.vp.YOffset
	a.vp.SetContent(strings.Join(d.lines, "\n"))
	a.vp.SetYOffset(old)
	if a.focus == 0 && selectedOffset >= 0 && (selectedOffset < old || selectedOffset >= old+a.vp.Height) {
		a.vp.SetYOffset(selectedOffset)
	}
}
