package agenttui

import (
	"encoding/json"

	"github.com/witwave-ai/witself/internal/dashboard"
)

// Explicit fixture authorization, independent of any real account or home.
func demoAccountContext() object {
	return object{"schema_version": dashboard.AccountSchema, "available": true, "account_id": "acct_studio_demo", "operator_id": "op_manager_demo", "role": "account_owner", "sections": accountSections}
}
func (d *demoSource) accountRead(r dashboard.ReadRequest) (json.RawMessage, error) {
	out := object{"schema_version": dashboard.AccountSchema, "available": true}
	switch r.Resource {
	case dashboard.ResourceAccountContext, dashboard.ResourceAccountAccess:
		return encode(demoAccountContext())
	case dashboard.ResourceAccountOverview:
		out["account"] = object{"id": "acct_studio_demo", "display_name": "Studio · synthetic account", "status": "active", "email": "manager@example.test", "created_at": "2026-09-01T09:00:00Z"}
	case dashboard.ResourceAccountClients, dashboard.ResourceAccountClientsScan:
		d.mu.Lock()
		defer d.mu.Unlock()
		if r.Resource == dashboard.ResourceAccountClientsScan {
			d.accountChecked = true
		}
		out["status"] = "not_checked"
		if d.accountChecked {
			out["status"] = "checked"
			out["report"] = object{"schema_version": dashboard.AccountClientsSchema, "device_label": "This device", "checked_at": demoTime, "scan_status": "partial", "entries": []any{
				object{"runtime": "codex", "recorded_version": "0.1.0", "executable_status": "present", "configuration_status": "match", "configuration_scope": "mcp_registration", "effective_verification": "not_run", "installed_at": "2026-09-01T09:00:00Z"},
				object{"runtime": "dsh", "recorded_version": "", "executable_status": "unchecked", "configuration_status": "unsupported", "configuration_scope": "none", "effective_verification": "not_run"},
			}}
		}
	case dashboard.ResourceAccountPlan:
		out["plan"] = "studio"
		out["plan_name"] = "Studio (synthetic)"
		out["applied"] = "starter"
		out["billing_plan"] = "studio"
		out["billing_plan_name"] = "Studio"
		out["billing_available"] = true
		out["apply_pending"] = true
		out["apply_blocked"] = false
		out["pending"] = object{"kind": "downgrade", "plan": "starter", "plan_name": "Starter", "requested": demoTime, "effective": "2026-10-01T00:00:00Z"}
		out["limits"] = object{"agents": 10, "stored_fact": 1000}
		out["limit_defaults"] = object{"agents": 5, "stored_fact": 1000}
		out["limits_units"] = object{"agents": "count", "stored_fact": "count"}
		out["limit_defaults_units"] = out["limits_units"]
		out["features"] = []string{"memory", "facts", "messaging"}
		out["feature_defaults"] = []string{"memory", "facts"}
		for _, key := range []string{"messaging", "email_receive", "email_send"} {
			out[key] = object{"enabled": true, "default_enabled": false, "overridden": true}
		}
		for _, key := range []string{"message_retention", "email_retention", "transcript_retention"} {
			out[key] = object{"effective_days": 30, "default_days": 14, "overridden": true}
		}
	case dashboard.ResourceAccountBilling:
		out["summary"] = object{"available": true, "configured": true, "billing_available": true, "subscription_status": "active", "billing_plan": "studio", "billing_plan_name": "Studio", "effective_plan": "studio", "effective_plan_name": "Studio", "applied_plan": "starter", "payment_method": object{"label": "Demo card · 4242"}, "next_charge": object{"amount_cents": nil, "currency": "USD", "date": "2026-10-01"}}
		out["invoices"] = object{"available": true, "truncated": true, "entries": []any{object{"number": "DEMO-001", "date": "2026-09-01", "amount_cents": 0, "currency": "USD", "status": "paid"}, object{"number": "DEMO-002", "date": "2026-09-02", "amount_cents": 12345, "currency": "EUR", "status": "paid"}}}
		out["payments"] = object{"available": false, "error": "unavailable"}
	case dashboard.ResourceAccountSupport:
		out["tickets"] = []any{object{"id": "tkt_demo_guide", "subject": "Field guide question", "category": "question", "state": "open", "priority": "normal", "opened_at": demoTime, "last_activity_at": demoTime}, object{"id": "tkt_demo_limits", "subject": "Understanding retention", "category": "question", "state": "resolved", "priority": "normal", "opened_at": demoTime}}
	case dashboard.ResourceAccountSupportTicket:
		if r.ID != "tkt_demo_guide" && r.ID != "tkt_demo_limits" {
			return nil, &dashboard.ReaderError{Code: "forbidden"}
		}
		out["ticket"] = object{"id": r.ID, "subject": "Synthetic support thread", "state": "open", "category": "question", "priority": "normal"}
		out["messages"] = []any{object{"id": "reply_demo_1", "author_kind": "account_operator", "posted_at": demoTime, "body": "How do the account limits apply to this workspace?"}, object{"id": "reply_demo_2", "author_kind": "support", "posted_at": demoTime, "body": "The Account area shows the current plan and applied plan separately. Pending changes appear with their effective date.\n\nThis is synthetic support text, loaded only after Enter. Escape or leaving clears it."}}
	default:
		return nil, &dashboard.ReaderError{Code: "unsupported"}
	}
	return encode(out)
}
