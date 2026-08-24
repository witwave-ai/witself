package supportrunner

import (
	"regexp"
	"strings"
)

var (
	refundGateRE   = regexp.MustCompile(`\b(?:refund|chargeback|dispute)\b`)
	legalGateRE    = regexp.MustCompile(`\b(?:legal|lawyer|attorney|subpoena|court)\b`)
	deletionGateRE = regexp.MustCompile(
		`\b(?:delete\s+my\s+data|data\s+deletion|erasure|right\s+to\s+be\s+forgotten|gdpr)\b`,
	)
	securityGateRE = regexp.MustCompile(`\b(?:vulnerability|breach|hacked|compromised)\b`)
	humanGateRE    = regexp.MustCompile(
		`\b(?:speak\s+to\s+a\s+human|talk\s+to\s+a\s+human|real\s+person)\b`,
	)
)

type gateResult struct {
	Escalate bool
	Retriage retriage
}

func deterministicGate(thread ticketThread) gateResult {
	result := gateResult{}
	if thread.Ticket.Category == ticketCategorySecurity {
		result.Escalate = true
		result.Retriage.Priority = ticketPriorityUrgent
	}
	if thread.Ticket.Category == ticketCategoryBilling {
		result.Escalate = true
	}

	var text strings.Builder
	text.WriteString(thread.Ticket.Subject)
	for _, message := range thread.Messages {
		if !isCustomerAuthor(message.AuthorKind) {
			continue
		}
		text.WriteByte('\n')
		text.WriteString(message.Body)
	}
	corpus := strings.ToLower(text.String())
	if refundGateRE.MatchString(corpus) ||
		legalGateRE.MatchString(corpus) ||
		deletionGateRE.MatchString(corpus) ||
		humanGateRE.MatchString(corpus) {
		result.Escalate = true
	}
	if securityGateRE.MatchString(corpus) {
		result.Escalate = true
		result.Retriage.Category = ticketCategorySecurity
		result.Retriage.Priority = ticketPriorityUrgent
	}
	return result
}
