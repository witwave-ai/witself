package supportrunner

import (
	"context"
	"errors"
	"time"

	"github.com/witwave-ai/witself/internal/client"
)

// This is the package's only dependency on internal/client. The runner and
// model layers operate on the deliberately smaller projections below so the
// support tool cannot grow into an ambient fleet-admin client.

type ticket struct {
	ID            string
	AccountID     string
	Subject       string
	Category      string
	State         string
	Priority      string
	LastMessageID string
}

type ticketMessage struct {
	ID         string
	AuthorKind string
	Body       string
}

type ticketThread struct {
	Ticket   ticket
	Messages []ticketMessage
}

type ticketListOptions struct {
	States []string
	Since  time.Time
	Limit  int
}

type retriage struct {
	Category string `json:"category"`
	Priority string `json:"priority"`
}

type ticketAPI interface {
	List(context.Context, ticketListOptions) ([]ticket, error)
	Get(context.Context, string, string) (ticketThread, error)
	ReplyAsAssistant(context.Context, string, string, string) error
	Retriage(context.Context, string, string, retriage) error
}

type httpTicketAPI struct {
	controlPlane string
	adminToken   string
}

func (a httpTicketAPI) List(ctx context.Context, opts ticketListOptions) ([]ticket, error) {
	since := opts.Since
	result, err := client.ListAdminTickets(ctx, a.controlPlane, a.adminToken, client.AdminTicketFilter{
		States: opts.States,
		Since:  &since,
		Limit:  opts.Limit,
	})
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, errors.New("admin ticket list response is missing")
	}
	return projectAdminTicketList(result)
}

func projectAdminTicketList(result *client.AdminTicketList) ([]ticket, error) {
	if result == nil {
		return nil, errors.New("admin ticket list response is missing")
	}
	// The control plane intentionally exposes partial fan-out and aggregate
	// truncation. Acting on either would turn a fleet read failure into a
	// silently incomplete queue, so fail the whole tick before any thread read
	// or mutation. Empty, non-nil slices remain the valid no-cells/no-tickets
	// response.
	statuses := make([]string, 0, len(result.Cells))
	for _, cell := range result.Cells {
		statuses = append(statuses, cell.Status)
	}
	if err := validateTicketListCompleteness(
		result.Cells != nil,
		result.Tickets != nil,
		result.AggregateCapped,
		statuses,
	); err != nil {
		return nil, err
	}
	tickets := make([]ticket, 0, len(result.Tickets))
	for _, candidate := range result.Tickets {
		tickets = append(tickets, ticketFromClient(candidate.SupportTicket))
	}
	return tickets, nil
}

func validateTicketListCompleteness(
	cellsPresent, ticketsPresent, aggregateCapped bool,
	cellStatuses []string,
) error {
	if !cellsPresent || !ticketsPresent || aggregateCapped {
		return errors.New("admin ticket list response is incomplete")
	}
	for _, status := range cellStatuses {
		if status != "ok" {
			return errors.New("admin ticket list response is incomplete")
		}
	}
	return nil
}

func (a httpTicketAPI) Get(ctx context.Context, accountID, ticketID string) (ticketThread, error) {
	result, err := client.GetAdminTicket(ctx, a.controlPlane, a.adminToken, accountID, ticketID)
	if err != nil {
		return ticketThread{}, err
	}
	if result == nil {
		return ticketThread{}, errors.New("admin ticket response is missing")
	}
	messages := make([]ticketMessage, 0, len(result.Messages))
	for _, message := range result.Messages {
		messages = append(messages, ticketMessage{
			ID:         message.ID,
			AuthorKind: message.AuthorKind,
			Body:       message.Body,
		})
	}
	return ticketThread{
		Ticket:   ticketFromClient(result.Ticket),
		Messages: messages,
	}, nil
}

func (a httpTicketAPI) ReplyAsAssistant(ctx context.Context, accountID, ticketID, body string) error {
	_, err := client.ReplyAdminTicketAsAssistant(
		ctx, a.controlPlane, a.adminToken, accountID, ticketID, body,
	)
	return err
}

func (a httpTicketAPI) Retriage(ctx context.Context, accountID, ticketID string, change retriage) error {
	_, err := client.RetriageAdminTicket(
		ctx,
		a.controlPlane,
		a.adminToken,
		accountID,
		ticketID,
		change.Category,
		change.Priority,
	)
	return err
}

func ticketFromClient(value client.SupportTicket) ticket {
	return ticket{
		ID:            value.ID,
		AccountID:     value.AccountID,
		Subject:       value.Subject,
		Category:      value.Category,
		State:         value.State,
		Priority:      value.Priority,
		LastMessageID: value.LastMessageID,
	}
}
