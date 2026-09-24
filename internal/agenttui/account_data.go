package agenttui

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/witwave-ai/witself/internal/dashboard"
	"github.com/witwave-ai/witself/internal/plans"
)

var accountSections = []string{"overview", "clients", "plan", "billing", "support", "access"}
var accountLabels = map[string]string{"overview": "Overview", "clients": "Clients on this device", "plan": "Plan & limits", "billing": "Billing", "support": "Support", "access": "Access"}
var accountResources = map[string]dashboard.Resource{"overview": dashboard.ResourceAccountOverview, "clients": dashboard.ResourceAccountClients, "plan": dashboard.ResourceAccountPlan, "billing": dashboard.ResourceAccountBilling, "support": dashboard.ResourceAccountSupport, "access": dashboard.ResourceAccountAccess}
var accountIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var errAccountData = errors.New("invalid account projection")

type accountContext struct {
	account, operator, role string
	sections                []string
}

func (c accountContext) available() bool { return c.account != "" }
func (c accountContext) same(d accountContext) bool {
	return c.account == d.account && c.operator == d.operator && c.role == d.role && slices.Equal(c.sections, d.sections)
}
func decodeAccountContext(raw json.RawMessage) (accountContext, error) {
	o, err := accountJSON(raw, 16<<10)
	if err != nil || !flag(o, "available") {
		return accountContext{}, errAccountData
	}
	account, accountOK := o["account_id"].(string)
	operator, operatorOK := o["operator_id"].(string)
	role, roleOK := o["role"].(string)
	if !accountOK || !operatorOK || !roleOK {
		return accountContext{}, errAccountData
	}
	c := accountContext{account: account, operator: operator, role: role}
	if !accountIDPattern.MatchString(c.account) || !accountIDPattern.MatchString(c.operator) || !slices.Contains([]string{"account_owner", "account_operator", "account_admin", "account_billing"}, c.role) {
		return accountContext{}, errAccountData
	}
	ss, ok := o["sections"].([]any)
	if !ok || len(ss) == 0 || len(ss) > 6 {
		return accountContext{}, errAccountData
	}
	last := -1
	for _, v := range ss {
		s, ok := v.(string)
		n := slices.Index(accountSections, s)
		if !ok || n <= last || (c.role == "account_operator" && (s == "plan" || s == "billing")) {
			return accountContext{}, errAccountData
		}
		last = n
		c.sections = append(c.sections, s)
	}
	return c, nil
}
func accountJSON(raw json.RawMessage, limit int) (object, error) {
	if len(raw) > limit || !json.Valid(raw) {
		return nil, errAccountData
	}
	var o object
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if d.Decode(&o) != nil || o == nil || str(o, "schema_version") != dashboard.AccountSchema {
		return nil, errAccountData
	}
	return o, nil
}

// A closed schema projects only typed scalar fields and explicitly named children.
// Unknown fields never enter cached state. Missing/null numbers stay unknown.
type accountShape struct {
	strings, numbers, bools string
	children                map[string]accountShape
	arrays                  map[string]accountShape
	stringLists             map[string][]string
}

func accountProject(src object, shape accountShape, truncated *bool) (object, error) {
	out := object{}
	for kind, keys := range []string{shape.strings, shape.numbers, shape.bools} {
		for _, key := range strings.Fields(keys) {
			v, exists := src[key]
			if !exists {
				continue
			}
			if v == nil {
				out[key] = nil
				continue
			}
			switch kind {
			case 0:
				s, ok := v.(string)
				if !ok {
					return nil, errAccountData
				}
				limit := 4096
				if key == "body" {
					limit = 16 << 10
				}
				if len(s) > limit {
					s = s[:limit]
					for !utf8.ValidString(s) {
						s = s[:len(s)-1]
					}
					*truncated = true
				}
				out[key] = clean(s)
			case 1:
				n, ok := v.(json.Number)
				if !ok {
					return nil, errAccountData
				}
				if _, err := n.Int64(); err != nil {
					return nil, errAccountData
				}
				out[key] = n
			case 2:
				b, ok := v.(bool)
				if !ok {
					return nil, errAccountData
				}
				out[key] = b
			}
		}
	}
	for key, child := range shape.children {
		v, exists := src[key]
		if !exists {
			continue
		}
		if v == nil {
			out[key] = nil
			continue
		}
		m, ok := v.(map[string]any)
		if !ok {
			return nil, errAccountData
		}
		p, err := accountProject(m, child, truncated)
		if err != nil {
			return nil, err
		}
		out[key] = p
	}
	for key, child := range shape.arrays {
		v, exists := src[key]
		if !exists {
			continue
		}
		a, ok := v.([]any)
		if v == nil {
			a = []any{}
			ok = true
		}
		if !ok {
			return nil, errAccountData
		}
		if len(a) > 100 {
			a = a[:100]
			*truncated = true
		}
		rows := []any{}
		for _, v := range a {
			m, ok := v.(map[string]any)
			if !ok {
				return nil, errAccountData
			}
			p, err := accountProject(m, child, truncated)
			if err != nil {
				return nil, err
			}
			rows = append(rows, p)
		}
		out[key] = rows
	}
	for key, allowed := range shape.stringLists {
		v, exists := src[key]
		if !exists {
			continue
		}
		a, ok := v.([]any)
		if !ok {
			return nil, errAccountData
		}
		outList := []any{}
		for _, v := range a {
			s, ok := v.(string)
			if !ok {
				return nil, errAccountData
			}
			if slices.Contains(allowed, s) && !slices.Contains(outList, any(s)) {
				outList = append(outList, s)
			}
		}
		out[key] = outList
	}
	return out, nil
}

var accountTicketShape = accountShape{strings: "id subject category state priority opened_at first_response_at resolved_at closed_at last_activity_at"}
var accountPendingShape = accountShape{strings: "kind plan plan_name expires effective requested"}

func accountDataShape(r dashboard.Resource) (accountShape, bool) {
	s := accountShape{bools: "available truncated"}
	switch r {
	case dashboard.ResourceAccountOverview:
		s.children = map[string]accountShape{"account": {strings: "id display_name status email created_at"}}
	case dashboard.ResourceAccountPlan:
		s.strings = "plan plan_name billing_plan billing_plan_name applied past_due_since"
		s.bools += " billing_available apply_pending apply_blocked"
		s.children = map[string]accountShape{"pending": accountPendingShape}
		for _, k := range []string{"messaging", "email_receive", "email_send"} {
			s.children[k] = accountShape{bools: "default_enabled enabled overridden"}
		}
		for _, k := range []string{"message_retention", "email_retention", "transcript_retention"} {
			s.children[k] = accountShape{numbers: "default_days effective_days", bools: "overridden"}
		}
		for _, k := range []string{"limits", "limit_defaults"} {
			keys := strings.Join(plans.SupportedLimitKeys(), " ")
			s.children[k] = accountShape{numbers: keys}
			s.children[k+"_units"] = accountShape{strings: keys}
		}
		s.stringLists = map[string][]string{"features": plans.SupportedFeatureKeys(), "feature_defaults": plans.SupportedFeatureKeys()}
	case dashboard.ResourceAccountBilling:
		summary := accountShape{strings: "error subscription_status billing_plan billing_plan_name effective_plan effective_plan_name applied_plan entitled_at past_due_since", bools: "available truncated billing_available configured", children: map[string]accountShape{"pending": accountPendingShape, "payment_method": {strings: "label"}, "next_charge": {strings: "currency date", numbers: "amount_cents"}}}
		s.children = map[string]accountShape{"summary": summary, "invoices": {strings: "error", bools: "available truncated", arrays: map[string]accountShape{"entries": {strings: "number date currency status", numbers: "amount_cents"}}}, "payments": {strings: "error", bools: "available truncated", arrays: map[string]accountShape{"entries": {strings: "date currency method status", numbers: "amount_cents"}}}}
	case dashboard.ResourceAccountSupport:
		s.arrays = map[string]accountShape{"tickets": accountTicketShape}
	case dashboard.ResourceAccountSupportTicket:
		s.children = map[string]accountShape{"ticket": accountTicketShape}
		s.arrays = map[string]accountShape{"messages": {strings: "id posted_at author_kind body"}}
	case dashboard.ResourceAccountClients, dashboard.ResourceAccountClientsScan:
		s.strings = "status"
		s.children = map[string]accountShape{"report": {strings: "schema_version device_label checked_at scan_status", bools: "truncated", arrays: map[string]accountShape{"entries": {strings: "runtime recorded_version executable_status configuration_status configuration_scope effective_verification installed_at"}}}}
	default:
		return s, false
	}
	return s, true
}
func decodeAccount(r dashboard.Resource, raw json.RawMessage, c accountContext, id string) (object, error) {
	limit := 2 << 20
	if r == dashboard.ResourceAccountSupportTicket {
		limit = 4 << 20
	}
	src, err := accountJSON(raw, limit)
	if err != nil || !flag(src, "available") {
		return nil, errAccountData
	}
	shape, ok := accountDataShape(r)
	if !ok {
		return nil, errAccountData
	}
	truncated := flag(src, "truncated")
	out, err := accountProject(src, shape, &truncated)
	if err != nil {
		return nil, err
	}
	out["truncated"] = truncated
	switch r {
	case dashboard.ResourceAccountPlan:
		for _, key := range []string{"limits", "limit_defaults"} {
			if out[key] == nil {
				continue
			}
			limits := map[string]int64{}
			for dimension, value := range obj(out[key]) {
				n, ok := value.(json.Number)
				if !ok { // Null is invalid; only an omitted dimension has no cap.
					return nil, errAccountData
				}
				limits[dimension], err = n.Int64()
				if err != nil {
					return nil, errAccountData
				}
			}
			if plans.ValidateLimits(limits) != nil {
				return nil, errAccountData
			}
		}
	case dashboard.ResourceAccountOverview:
		if str(obj(src["account"]), "id") != c.account {
			return nil, errAccountData
		}
	case dashboard.ResourceAccountSupport:
		if _, ok := out["tickets"]; !ok {
			return nil, errAccountData
		}
		for _, row := range list(src, "tickets") {
			if !accountIDPattern.MatchString(str(row, "id")) {
				return nil, errAccountData
			}
		}
	case dashboard.ResourceAccountSupportTicket:
		if str(obj(src["ticket"]), "id") != id {
			return nil, errAccountData
		}
		if _, ok := out["messages"]; !ok {
			return nil, errAccountData
		}
	case dashboard.ResourceAccountBilling:
		for _, key := range []string{"invoices", "payments"} {
			section := obj(out[key])
			if flag(section, "available") {
				if _, ok := section["entries"]; !ok {
					return nil, errAccountData
				}
			}
		}
	case dashboard.ResourceAccountClients, dashboard.ResourceAccountClientsScan:
		report := obj(out["report"])
		status := str(out, "status")
		if status != "not_checked" && status != "checked" {
			return nil, errAccountData
		}
		if status == "checked" {
			if _, ok := report["entries"]; !ok {
				return nil, errAccountData
			}
		}
		if status == "checked" && (str(report, "schema_version") != dashboard.AccountClientsSchema || str(report, "device_label") != "This device") {
			return nil, errAccountData
		}
	}
	return out, nil
}
