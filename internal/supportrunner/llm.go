package supportrunner

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

const (
	ticketStateOpen          = "open"
	ticketStateAwaitingAdmin = "awaiting_admin"

	authorKindOwner      = "owner"
	authorKindOperator   = "operator"
	authorKindFleetAdmin = "fleet_admin"
	authorKindAssistant  = "assistant"

	ticketCategoryTechnical = "technical"
	ticketCategoryBilling   = "billing"
	ticketCategorySecurity  = "security"
	ticketCategoryOther     = "other"

	ticketPriorityLow    = "low"
	ticketPriorityNormal = "normal"
	ticketPriorityHigh   = "high"
	ticketPriorityUrgent = "urgent"

	decisionActionReply    = "reply"
	decisionActionEscalate = "escalate"

	maxReplyBodyBytes = 64 * 1024
)

var (
	legalCategories = []string{
		ticketCategoryTechnical,
		ticketCategoryBilling,
		ticketCategorySecurity,
		ticketCategoryOther,
	}
	legalPriorities = []string{
		ticketPriorityLow,
		ticketPriorityNormal,
		ticketPriorityHigh,
		ticketPriorityUrgent,
	}
)

type decision struct {
	Action         string   `json:"action"`
	ReplyBody      string   `json:"reply_body"`
	Retriage       retriage `json:"retriage"`
	EscalateReason string   `json:"escalate_reason"`
}

type llm interface {
	Decide(context.Context, ticketThread) (decision, error)
}

func validateDecision(value decision) (decision, error) {
	if value.Action != decisionActionReply && value.Action != decisionActionEscalate {
		return decision{}, fmt.Errorf("unknown decision action %q", value.Action)
	}
	if value.Retriage.Category != "" && !slices.Contains(legalCategories, value.Retriage.Category) {
		return decision{}, fmt.Errorf("unknown retriage category %q", value.Retriage.Category)
	}
	if value.Retriage.Priority != "" && !slices.Contains(legalPriorities, value.Retriage.Priority) {
		return decision{}, fmt.Errorf("unknown retriage priority %q", value.Retriage.Priority)
	}
	if value.Action == decisionActionEscalate {
		// Escalation is deliberately silent. Discard any model-supplied reply
		// or re-triage data so this branch can never mutate the ticket.
		value.ReplyBody = ""
		value.Retriage = retriage{}
		return value, nil
	}

	value.ReplyBody = strings.TrimSpace(value.ReplyBody)
	if value.ReplyBody == "" {
		return decision{}, errors.New("reply body is empty")
	}
	if len(value.ReplyBody) > maxReplyBodyBytes {
		return decision{}, fmt.Errorf("reply body exceeds %d bytes", maxReplyBodyBytes)
	}
	return value, nil
}

func isEligibleState(state string) bool {
	return state == ticketStateOpen || state == ticketStateAwaitingAdmin
}

func isCustomerAuthor(kind string) bool {
	return kind == authorKindOwner || kind == authorKindOperator
}

func decisionInputSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"action": map[string]any{
				"type": "string",
				"enum": []string{decisionActionReply, decisionActionEscalate},
			},
			"reply_body": map[string]any{
				"type": "string",
			},
			"retriage": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]any{
					"category": map[string]any{
						"type": "string",
						"enum": append([]string{""}, legalCategories...),
					},
					"priority": map[string]any{
						"type": "string",
						"enum": append([]string{""}, legalPriorities...),
					},
				},
				"required": []string{"category", "priority"},
			},
			"escalate_reason": map[string]any{
				"type": "string",
			},
		},
		"required": []string{"action", "reply_body", "retriage", "escalate_reason"},
	}
}
