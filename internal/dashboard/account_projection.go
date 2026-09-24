package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/plans"
)

const accountRows = 100
const accountTextBytes = 4096
const accountBodyBytes = 16 << 10

// Only named, typed fields pass this projection. Unknown fields, even nested
// ones, are never forwarded. Amounts use json.Number to preserve exact int64
// cents; absent and null monetary fields remain explicit null, never zero.
type accountFields map[string]string

var accountOverviewFields = accountFields{"id": "s", "display_name": "s", "status": "s", "email": "s", "created_at": "s"}
var accountTicketFields = accountFields{"id": "s", "subject": "s", "category": "s", "state": "s", "priority": "s", "opened_at": "s", "first_response_at": "s", "resolved_at": "s", "closed_at": "s", "last_activity_at": "s"}
var accountMessageFields = accountFields{"id": "s", "posted_at": "s", "author_kind": "s", "body": "body"}
var accountPendingFields = accountFields{"kind": "s", "plan": "s", "plan_name": "s", "expires": "s", "effective": "s", "requested": "s"}
var accountPlanFields = accountFields{"plan": "s", "plan_name": "s", "billing_plan": "s", "billing_plan_name": "s", "billing_available": "b", "applied": "s", "apply_pending": "b", "past_due_since": "s"}
var accountBillingFields = accountFields{"billing_available": "b", "configured": "b", "subscription_status": "s", "billing_plan": "s", "billing_plan_name": "s", "effective_plan": "s", "effective_plan_name": "s", "applied_plan": "s", "entitled_at": "s", "past_due_since": "s"}
var accountMoneyFields = accountFields{"amount_cents": "n", "currency": "s", "date": "s"}
var accountInvoiceFields = accountFields{"number": "s", "date": "s", "amount_cents": "n", "currency": "s", "status": "s"}
var accountPaymentFields = accountFields{"date": "s", "amount_cents": "n", "currency": "s", "method": "s", "status": "s"}

func accountText(s string, maxBytes int) (string, bool) {
	clean := strings.Map(func(r rune) rune {
		if (unicode.IsControl(r) && r != '\n' && r != '\t') || r == 0x2028 || r == 0x2029 || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
			return -1
		}
		return r
	}, s)
	changed := clean != s
	if len(clean) > maxBytes {
		clean = clean[:maxBytes]
		for !utf8.ValidString(clean) {
			clean = clean[:len(clean)-1]
		}
		changed = true
	}
	return clean, changed
}
func accountObject(v any) (map[string]any, error) {
	m, ok := v.(map[string]any)
	if !ok || m == nil {
		return nil, client.ErrAccountConsoleUnavailable
	}
	return m, nil
}
func accountDecode(raw json.RawMessage) (map[string]any, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var m map[string]any
	if d.Decode(&m) != nil || m == nil {
		return nil, client.ErrAccountConsoleUnavailable
	}
	return m, nil
}
func accountPick(src map[string]any, fields accountFields, truncated *bool) (map[string]any, error) {
	dst := map[string]any{}
	for key, kind := range fields {
		v, ok := src[key]
		if !ok {
			continue
		}
		if v == nil {
			dst[key] = nil
			continue
		}
		switch kind {
		case "s", "body":
			s, ok := v.(string)
			if !ok {
				return nil, client.ErrAccountConsoleUnavailable
			}
			maxBytes := accountTextBytes
			if kind == "body" {
				maxBytes = accountBodyBytes
			}
			clean, changed := accountText(s, maxBytes)
			dst[key] = clean
			*truncated = *truncated || changed
		case "b":
			b, ok := v.(bool)
			if !ok {
				return nil, client.ErrAccountConsoleUnavailable
			}
			dst[key] = b
		case "n":
			n, ok := v.(json.Number)
			if !ok {
				return nil, client.ErrAccountConsoleUnavailable
			}
			if _, err := n.Int64(); err != nil {
				return nil, client.ErrAccountConsoleUnavailable
			}
			dst[key] = n
		}
	}
	return dst, nil
}
func accountNested(src, dst map[string]any, key string, fields accountFields, truncated *bool) error {
	v, ok := src[key]
	if !ok {
		return nil
	}
	if v == nil {
		dst[key] = nil
		return nil
	}
	m, err := accountObject(v)
	if err != nil {
		return err
	}
	out, err := accountPick(m, fields, truncated)
	if err != nil {
		return err
	}
	dst[key] = out
	return nil
}
func accountArray(src map[string]any, key string) ([]any, error) {
	v, exists := src[key]
	if !exists {
		return nil, client.ErrAccountConsoleUnavailable
	}
	if v == nil {
		return []any{}, nil
	}
	a, ok := v.([]any)
	if !ok {
		return nil, client.ErrAccountConsoleUnavailable
	}
	return a, nil
}
func accountMoney(m map[string]any) {
	for _, key := range []string{"amount_cents", "currency"} {
		if _, ok := m[key]; !ok {
			m[key] = nil
		}
	}
}

func projectAccount(resource Resource, raw json.RawMessage, accountID, id string) (map[string]any, error) {
	src, err := accountDecode(raw)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"available": true}
	truncated, _ := src["truncated"].(bool)
	switch resource {
	case ResourceAccountOverview:
		m, err := accountObject(src["account"])
		if err != nil || m["id"] != accountID {
			return nil, client.ErrAccountConsoleUnavailable
		}
		out["account"], err = accountPick(m, accountOverviewFields, &truncated)
		if err != nil {
			return nil, err
		}
	case ResourceAccountPlan:
		if s, ok := src["plan"].(string); !ok || strings.TrimSpace(s) == "" {
			return nil, client.ErrAccountConsoleUnavailable
		}
		out, err = accountPick(src, accountPlanFields, &truncated)
		if err != nil {
			return nil, err
		}
		out["available"] = true
		// The CP stores joined cell violation prose here. Expose only whether
		// application is blocked, never the opaque report or its identifiers.
		if value, exists := src["apply_blocked"]; exists {
			text, ok := value.(string)
			if !ok {
				return nil, client.ErrAccountConsoleUnavailable
			}
			out["apply_blocked"] = strings.TrimSpace(text) != ""
		}
		if err := accountNested(src, out, "pending", accountPendingFields, &truncated); err != nil {
			return nil, err
		}
		for _, key := range []string{"messaging", "email_receive", "email_send"} {
			if err := accountNested(src, out, key, accountFields{"default_enabled": "b", "enabled": "b", "overridden": "b"}, &truncated); err != nil {
				return nil, err
			}
		}
		for _, key := range []string{"message_retention", "email_retention", "transcript_retention"} {
			if err := accountNested(src, out, key, accountFields{"default_days": "n", "effective_days": "n", "overridden": "b"}, &truncated); err != nil {
				return nil, err
			}
		}
		for _, key := range []string{"limits", "limit_defaults"} {
			if src[key] == nil {
				continue
			}
			m, err := accountObject(src[key])
			if err != nil {
				return nil, err
			}
			fields := accountFields{}
			for _, key := range plans.SupportedLimitKeys() {
				fields[key] = "n"
			}
			safe, err := accountPick(m, fields, &truncated)
			if err != nil {
				return nil, err
			}
			limits := make(map[string]int64, len(safe))
			for dimension, value := range safe {
				n, ok := value.(json.Number)
				if !ok { // Explicit null is invalid; only an omitted key has no plan cap.
					return nil, client.ErrAccountConsoleUnavailable
				}
				limits[dimension], err = n.Int64()
				if err != nil {
					return nil, client.ErrAccountConsoleUnavailable
				}
			}
			if plans.ValidateLimits(limits) != nil {
				return nil, client.ErrAccountConsoleUnavailable
			}
			out[key] = safe
			// A present valid map supplies the full dimension vocabulary, even
			// when every dimension has no plan cap. Do not invent numeric caps.
			units := map[string]string{}
			for _, key := range plans.SupportedLimitKeys() {
				units[key] = accountLimitUnit(key)
			}
			out[key+"_units"] = units
		}
		for _, key := range []string{"features", "feature_defaults"} {
			if _, ok := src[key]; !ok {
				continue
			}
			a, err := accountArray(src, key)
			if err != nil {
				return nil, err
			}
			safe := []string{}
			for _, v := range a {
				s, ok := v.(string)
				if !ok {
					return nil, client.ErrAccountConsoleUnavailable
				}
				if slices.Contains(plans.SupportedFeatureKeys(), s) && !slices.Contains(safe, s) {
					safe = append(safe, s)
				}
			}
			out[key] = safe
		}
	case ResourceAccountSupport, ResourceAccountSupportTicket:
		if resource == ResourceAccountSupportTicket {
			m, err := accountObject(src["ticket"])
			if err != nil || m["account_id"] != accountID || m["id"] != id {
				return nil, client.ErrAccountConsoleUnavailable
			}
			out["ticket"], err = accountPick(m, accountTicketFields, &truncated)
			if err != nil {
				return nil, err
			}
		}
		key, fields := "tickets", accountTicketFields
		if resource == ResourceAccountSupportTicket {
			key, fields = "messages", accountMessageFields
		}
		rows, err := accountArray(src, key)
		if err != nil {
			return nil, err
		}
		if len(rows) > accountRows {
			truncated = true
			if resource == ResourceAccountSupportTicket {
				// Store.GetTicket returns oldest first; keep recent replies
				// accessible without changing their chronological order.
				rows = rows[len(rows)-accountRows:]
			} else {
				rows = rows[:accountRows]
			}
		}
		safe := make([]any, 0, len(rows))
		for _, v := range rows {
			m, err := accountObject(v)
			if err != nil || m["account_id"] != accountID {
				return nil, client.ErrAccountConsoleUnavailable
			}
			rowID, ok := m["id"].(string)
			if !ok || !readerID(rowID) || (resource == ResourceAccountSupportTicket && m["ticket_id"] != id) {
				return nil, client.ErrAccountConsoleUnavailable
			}
			row, err := accountPick(m, fields, &truncated)
			if err != nil {
				return nil, err
			}
			safe = append(safe, row)
		}
		out[key] = safe
	default:
		return nil, client.ErrAccountConsoleForbidden
	}
	out["truncated"] = truncated
	return out, nil
}
func accountLimitUnit(key string) string {
	if strings.Contains(key, "bytes") {
		if strings.HasSuffix(key, "minute") {
			return "bytes/minute"
		}
		return "bytes"
	}
	if strings.HasSuffix(key, "minute") {
		return "operations/minute"
	}
	return "count"
}

// Billing subreads retain independent availability; a failed history is never
// represented as a successful empty collection. No financial values inferred.
func (c *accountCollector) billing(ctx context.Context) (map[string]any, error) {
	// Spend at most half the remaining handler budget on concurrent subreads,
	// leaving the parent context live for the final authority check.
	budget := readerTimeout / 2
	if deadline, ok := ctx.Deadline(); ok {
		budget = min(budget, time.Until(deadline)/2)
	}
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		key       string
		projected map[string]any
		err       error
	}
	sections := []struct {
		key      string
		resource client.AccountConsoleResource
		fields   accountFields
	}{
		{"summary", client.AccountConsoleBillingSummary, accountBillingFields},
		{"invoices", client.AccountConsoleInvoices, accountInvoiceFields},
		{"payments", client.AccountConsolePayments, accountPaymentFields},
	}
	results := make(chan result, len(sections))
	for _, section := range sections {
		go func() {
			subctx, cancel := context.WithTimeout(workCtx, budget)
			defer cancel()
			raw, err := client.ReadAccountConsole(subctx, *c.manager, section.resource, "")
			var projected map[string]any
			if err == nil {
				projected, err = projectAccountBilling(raw, section.key, section.fields)
			}
			if err != nil {
				code := "unavailable"
				if errors.Is(err, client.ErrAccountConsoleResponseTooLarge) {
					code = "response_too_large"
				}
				projected = map[string]any{"available": false, "error": code}
			}
			results <- result{section.key, projected, err}
		}()
	}
	out := map[string]any{"available": true}
	for range sections {
		section := <-results
		if errors.Is(section.err, client.ErrAccountConsoleForbidden) {
			return nil, section.err
		}
		out[section.key] = section.projected
	}
	return out, nil
}
func projectAccountBilling(raw json.RawMessage, key string, fields accountFields) (map[string]any, error) {
	src, err := accountDecode(raw)
	if err != nil {
		return nil, err
	}
	truncated, _ := src["truncated"].(bool)
	out := map[string]any{"available": true}
	if key == "summary" {
		for _, key := range []string{"billing_plan", "effective_plan"} {
			if s, ok := src[key].(string); !ok || strings.TrimSpace(s) == "" {
				return nil, client.ErrAccountConsoleUnavailable
			}
		}
		out, err = accountPick(src, fields, &truncated)
		if err != nil {
			return nil, err
		}
		out["available"] = true
		for _, key := range []string{"payment_method", "next_charge", "pending"} {
			fields := accountPendingFields
			if key == "payment_method" {
				fields = accountFields{"label": "s"}
			}
			if key == "next_charge" {
				fields = accountMoneyFields
			}
			out[key] = nil
			if err := accountNested(src, out, key, fields, &truncated); err != nil {
				return nil, err
			}
		}
		if charge, ok := out["next_charge"].(map[string]any); ok {
			accountMoney(charge)
		}
	} else {
		rows, err := accountArray(src, key)
		if err != nil {
			return nil, err
		}
		if len(rows) > accountRows {
			truncated = true
			rows = rows[:accountRows]
		}
		safe := make([]any, 0, len(rows))
		for _, v := range rows {
			m, err := accountObject(v)
			if err != nil {
				return nil, err
			}
			row, err := accountPick(m, fields, &truncated)
			if err != nil {
				return nil, err
			}
			accountMoney(row)
			safe = append(safe, row)
		}
		out["entries"] = safe
	}
	out["truncated"] = truncated
	return out, nil
}

// Keep this private DTO independent of clientinventory. CLI translates its enums
// and times; validation here is a separate callback trust boundary.
var accountClientRuntimes = [...]string{"codex", "claude-code", "grok-build", "cursor", "openclaw", "antigravity", "copilot", "dsh"}
var accountClientVersion = regexp.MustCompile(`^v?(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})(-(alpha|beta|rc)(\.(0|[1-9][0-9]{0,8}))?)?$`)
var accountCursorVersion = regexp.MustCompile(`^[1-9][0-9]{3}\.[0-9]{2}\.[0-9]{2}-[0-9a-f]{7,16}$`)

func validAccountClientVersion(runtime, version string) bool {
	if version == "" {
		return true
	}
	if len(version) > 48 {
		return false
	}
	if accountClientVersion.MatchString(version) {
		return true
	}
	if runtime != "cursor" || !accountCursorVersion.MatchString(version) {
		return false
	}
	_, err := time.Parse("2006.01.02", version[:10])
	return err == nil
}
func validAccountClientTime(value string) bool {
	if len(value) > len("2006-01-02T15:04:05.999999999+00:00") {
		return false
	}
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || t.IsZero() || t.Format(time.RFC3339Nano) != value {
		return false
	}
	// time.Parse accepts a +24:00 offset, outside RFC3339's hour range.
	if !strings.HasSuffix(value, "Z") && value[len(value)-5:len(value)-3] > "23" {
		return false
	}
	return true
}
func sanitizeAccountClients(in AccountClientsReport) (*AccountClientsReport, error) {
	if in.SchemaVersion != AccountClientsSchema || in.DeviceLabel != "This device" ||
		!slices.Contains([]string{"complete", "partial", "unavailable"}, in.ScanStatus) ||
		!validAccountClientTime(in.CheckedAt) || len(in.Entries) > len(accountClientRuntimes) {
		return nil, client.ErrAccountConsoleUnavailable
	}
	out := in
	out.Entries = make([]AccountClientEntry, 0, len(in.Entries))
	seen := make(map[string]bool, len(in.Entries))
	for _, row := range in.Entries {
		if !slices.Contains(accountClientRuntimes[:], row.Runtime) || seen[row.Runtime] ||
			!validAccountClientVersion(row.Runtime, row.RecordedVersion) ||
			!slices.Contains([]string{"present", "missing", "unchecked"}, row.ExecutableStatus) ||
			!slices.Contains([]string{"match", "incomplete", "changed", "unavailable", "unsupported"}, row.ConfigurationStatus) ||
			!slices.Contains([]string{"none", "mcp_registration"}, row.ConfigurationScope) ||
			row.EffectiveVerification != "not_run" ||
			(row.InstalledAt != "" && !validAccountClientTime(row.InstalledAt)) {
			return nil, client.ErrAccountConsoleUnavailable
		}
		seen[row.Runtime] = true
		out.Entries = append(out.Entries, row)
	}
	if out.Truncated && out.ScanStatus == "complete" {
		out.ScanStatus = "partial"
	}
	return &out, nil
}
