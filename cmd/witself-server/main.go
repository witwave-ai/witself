// Command witself-server is the Witself backend API server. It supports version
// and a serve command that binds the API, health-probe, and metrics listeners.
// The full backend is specified under docs/.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/witwave-ai/witself/internal/export"
	"github.com/witwave-ai/witself/internal/placement"
	"github.com/witwave-ai/witself/internal/server"
	"github.com/witwave-ai/witself/internal/store"
	"github.com/witwave-ai/witself/internal/version"
)

const defaultBootstrapTokenFile = "/.witself/tokens/bootstrap.token"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage(os.Stdout)
		return 0
	}
	switch args[0] {
	case "version", "--version", "-v":
		fmt.Println(version.String("witself-server"))
		return 0
	case "help", "--help", "-h":
		usage(os.Stdout)
		return 0
	case "serve":
		return serve()
	default:
		fmt.Fprintf(os.Stderr, "witself-server: unknown command %q\n\n", args[0])
		usage(os.Stderr)
		return 2
	}
}

func serve() int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg := server.ConfigFromEnv()
	if dsn := dbDSN(); dsn != "" {
		st, err := store.Open(ctx, dsn)
		if err != nil {
			fmt.Fprintf(os.Stderr, "witself-server: database: %v\n", err)
			return 1
		}
		defer st.Close()
		if err := st.Migrate(); err != nil {
			fmt.Fprintf(os.Stderr, "witself-server: %v\n", err)
			return 1
		}
		acctID, err := st.EnsureDefaultAccount(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "witself-server: %v\n", err)
			return 1
		}
		cfg.AccountID = acctID
		oprID, err := st.EnsureRootOperator(ctx, acctID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "witself-server: %v\n", err)
			return 1
		}
		bt, err := bootstrapToken()
		if err != nil {
			fmt.Fprintf(os.Stderr, "witself-server: %v\n", err)
			return 1
		}
		if bt != "" {
			ttl, err := bootstrapTokenTTL()
			if err != nil {
				fmt.Fprintf(os.Stderr, "witself-server: %v\n", err)
				return 1
			}
			if err := st.AdoptBootstrapToken(ctx, acctID, oprID, bt, ttl); err != nil {
				fmt.Fprintf(os.Stderr, "witself-server: %v\n", err)
				return 1
			}
			fmt.Fprintf(os.Stderr, "witself-server: bootstrap token adopted (ttl %s)\n", ttl)
		}
		cfg.Login = func(ctx context.Context, bt string) (string, string, bool, error) {
			ot, oid, err := st.ExchangeBootstrap(ctx, bt)
			if errors.Is(err, store.ErrInvalidBootstrap) {
				return "", "", false, nil
			}
			if err != nil {
				return "", "", false, err
			}
			return ot, oid, true, nil
		}
		cfg.Authenticate = st.AuthenticateOperator
		cfg.CreateRealm = func(ctx context.Context, accountID, name string) (server.Realm, error) {
			r, err := st.CreateRealm(ctx, accountID, name)
			if errors.Is(err, store.ErrRealmExists) {
				return server.Realm{}, server.ErrConflict
			}
			if errors.Is(err, store.ErrPlanLimitReached) {
				return server.Realm{}, planLimitError(err)
			}
			if err != nil {
				return server.Realm{}, err
			}
			return server.Realm{ID: r.ID, Name: r.Name}, nil
		}
		cfg.ListRealms = func(ctx context.Context, accountID string) ([]server.Realm, error) {
			rs, err := st.ListRealms(ctx, accountID)
			if err != nil {
				return nil, err
			}
			out := make([]server.Realm, len(rs))
			for i, r := range rs {
				out[i] = server.Realm{ID: r.ID, Name: r.Name}
			}
			return out, nil
		}
		cfg.DeleteRealm = func(ctx context.Context, accountID, realmID string) error {
			err := st.DeleteRealm(ctx, accountID, realmID)
			switch {
			case errors.Is(err, store.ErrRealmNotFound):
				return server.ErrNotFound
			case errors.Is(err, store.ErrRealmNotEmpty):
				return server.ErrConflict
			default:
				return err
			}
		}
		cfg.CreateAgent = func(ctx context.Context, accountID, realmID, name string) (server.Agent, error) {
			a, err := st.CreateAgent(ctx, accountID, realmID, name)
			switch {
			case errors.Is(err, store.ErrRealmNotFound):
				return server.Agent{}, server.ErrNotFound
			case errors.Is(err, store.ErrAgentExists):
				return server.Agent{}, server.ErrConflict
			case errors.Is(err, store.ErrPlanLimitReached):
				return server.Agent{}, planLimitError(err)
			case err != nil:
				return server.Agent{}, err
			}
			return server.Agent{ID: a.ID, Name: a.Name}, nil
		}
		cfg.ListAgents = func(ctx context.Context, accountID, realmID string) ([]server.Agent, error) {
			as, err := st.ListAgents(ctx, accountID, realmID)
			if err != nil {
				return nil, err
			}
			out := make([]server.Agent, len(as))
			for i, a := range as {
				out[i] = server.Agent{ID: a.ID, Name: a.Name}
			}
			return out, nil
		}
		cfg.DeleteAgent = func(ctx context.Context, accountID, realmID, agentID string) error {
			err := st.DeleteAgent(ctx, accountID, realmID, agentID)
			if errors.Is(err, store.ErrAgentNotFound) {
				return server.ErrNotFound
			}
			return err
		}
		cfg.CreateAgentToken = func(ctx context.Context, accountID, actorOperatorID, agentID string) (string, string, string, error) {
			tok, tokenID, agentName, err := st.CreateAgentToken(ctx, accountID, actorOperatorID, agentID)
			switch {
			case errors.Is(err, store.ErrAgentNotFound), errors.Is(err, store.ErrAccountNotFound):
				return "", "", "", server.ErrNotFound
			case errors.Is(err, store.ErrAccountNotActive):
				return "", "", "", server.ErrAccountNotActive
			}
			return tok, tokenID, agentName, err
		}
		cfg.RenameAccount = func(ctx context.Context, accountID, operatorID, displayName string) error {
			err := st.UpdateAccountDisplayName(ctx, accountID, operatorID, displayName)
			switch {
			case errors.Is(err, store.ErrAccountNotFound):
				return server.ErrNotFound
			case errors.Is(err, store.ErrNotAccountOwner):
				return server.ErrNotAccountOwner
			}
			return err
		}
		cfg.CreateOperatorToken = func(ctx context.Context, accountID, operatorID, displayName string, ttl *time.Duration) (string, string, *time.Time, error) {
			tok, tokenID, expiresAt, err := st.CreateOperatorToken(ctx, accountID, operatorID, displayName, ttl)
			switch {
			case errors.Is(err, store.ErrOperatorNotFound), errors.Is(err, store.ErrAccountNotFound):
				return "", "", nil, server.ErrNotFound
			case errors.Is(err, store.ErrAccountNotActive):
				return "", "", nil, server.ErrAccountNotActive
			}
			return tok, tokenID, expiresAt, err
		}
		cfg.ListAccountEvents = func(ctx context.Context, accountID, operatorID string, filter server.EventFilter) (server.EventPage, error) {
			page, err := st.ListAccountEvents(ctx, accountID, operatorID, store.EventFilter{
				Since:  filter.Since,
				Until:  filter.Until,
				Verb:   filter.Verb,
				Limit:  filter.Limit,
				Cursor: filter.Cursor,
			})
			if errors.Is(err, store.ErrNotAccountOwner) {
				return server.EventPage{}, server.ErrNotAccountOwner
			}
			if errors.Is(err, store.ErrBadEventCursor) {
				return server.EventPage{}, fmt.Errorf("%w: %v", server.ErrBadInput, err)
			}
			if err != nil {
				return server.EventPage{}, err
			}
			out := make([]server.Event, len(page.Events))
			for i, e := range page.Events {
				out[i] = server.Event{
					ID:         e.ID,
					AccountID:  e.AccountID,
					OccurredAt: e.OccurredAt,
					ActorKind:  e.ActorKind,
					ActorID:    e.ActorID,
					Verb:       e.Verb,
					Metadata:   e.Metadata,
				}
			}
			return server.EventPage{Events: out, NextCursor: page.NextCursor}, nil
		}
		cfg.ListOperators = func(ctx context.Context, accountID string) ([]server.Operator, error) {
			ops, err := st.ListOperators(ctx, accountID)
			if err != nil {
				return nil, err
			}
			out := make([]server.Operator, len(ops))
			for i, op := range ops {
				out[i] = serverOperator(op)
			}
			return out, nil
		}
		cfg.CreateOperator = func(ctx context.Context, accountID, actorOperatorID, displayName, tokenDisplayName string, ttl *time.Duration) (server.Operator, string, *time.Time, error) {
			op, tok, expiresAt, err := st.CreateOperator(ctx, accountID, actorOperatorID, displayName, tokenDisplayName, ttl)
			if errors.Is(err, store.ErrAccountNotActive) {
				return server.Operator{}, "", nil, server.ErrAccountNotActive
			}
			if err != nil {
				return server.Operator{}, "", nil, err
			}
			return serverOperator(op), tok, expiresAt, nil
		}
		cfg.DeleteOperator = func(ctx context.Context, accountID, actorOperatorID, targetOperatorID string) error {
			err := st.DeleteOperator(ctx, accountID, actorOperatorID, targetOperatorID)
			switch {
			case errors.Is(err, store.ErrOperatorNotFound):
				return server.ErrNotFound
			case errors.Is(err, store.ErrCannotDeleteSelf):
				return server.ErrCannotDeleteSelf
			case errors.Is(err, store.ErrCannotDeleteRootOperator):
				return server.ErrCannotDeleteRoot
			case errors.Is(err, store.ErrLastOperator):
				return server.ErrLastOperator
			default:
				return err
			}
		}
		cfg.RevokeToken = func(ctx context.Context, accountID, actorOperatorID, tokenID string) error {
			err := st.RevokeToken(ctx, accountID, actorOperatorID, tokenID)
			if errors.Is(err, store.ErrTokenNotFound) {
				return server.ErrNotFound
			}
			return err
		}
		cfg.CloseAccount = func(ctx context.Context, accountID, operatorID, reason string) error {
			err := st.CloseAccount(ctx, accountID, operatorID, reason)
			switch {
			case errors.Is(err, store.ErrNotAccountOwner):
				return server.ErrNotAccountOwner
			case errors.Is(err, store.ErrCannotCloseDefault):
				return server.ErrCannotCloseDefault
			default:
				return err
			}
		}
		cfg.GetAccount = func(ctx context.Context, accountID string) (server.AccountRecord, error) {
			a, err := st.GetAccount(ctx, accountID)
			if errors.Is(err, store.ErrAccountNotFound) {
				return server.AccountRecord{}, server.ErrNotFound
			}
			if err != nil {
				return server.AccountRecord{}, err
			}
			return server.AccountRecord{
				ID:              a.ID,
				Email:           a.Email,
				DisplayName:     a.DisplayName,
				Status:          a.Status,
				CreatedAt:       a.CreatedAt,
				ClosedAt:        a.ClosedAt,
				ClosedReason:    a.ClosedReason,
				SuspendedAt:     a.SuspendedAt,
				SuspendedFor:    a.SuspendedFor,
				SuspendedReason: a.SuspendedReason,
				SupportPolicy:   a.SupportPolicy,
				Plan:            a.Plan,
				PlanLimits:      a.PlanLimits,
				PlanFeatures:    a.PlanFeatures,
				PlacementPolicy: a.PlacementPolicy,
			}, nil
		}
		cfg.GetPlacementPolicy = func(ctx context.Context, accountID, operatorID string) (placement.Policy, error) {
			policy, err := st.GetPlacementPolicy(ctx, accountID, operatorID)
			switch {
			case errors.Is(err, store.ErrAccountNotFound):
				return placement.Policy{}, server.ErrNotFound
			case errors.Is(err, store.ErrNotAccountOwner):
				return placement.Policy{}, server.ErrNotAccountOwner
			default:
				return policy, err
			}
		}
		cfg.SetPlacementPolicy = func(ctx context.Context, accountID, operatorID string, policy placement.Policy) (placement.Policy, error) {
			policy, err := st.SetPlacementPolicy(ctx, accountID, operatorID, policy)
			switch {
			case errors.Is(err, store.ErrAccountNotFound):
				return placement.Policy{}, server.ErrNotFound
			case errors.Is(err, store.ErrNotAccountOwner):
				return placement.Policy{}, server.ErrNotAccountOwner
			case errors.Is(err, store.ErrAccountNotActive):
				return placement.Policy{}, server.ErrAccountNotActive
			default:
				return policy, err
			}
		}
		cfg.SuspendAccountOwner = func(ctx context.Context, accountID, operatorID, reason string) error {
			err := st.SuspendAccountOwner(ctx, accountID, operatorID, reason)
			switch {
			case errors.Is(err, store.ErrAccountNotFound):
				return server.ErrNotFound
			case errors.Is(err, store.ErrNotAccountOwner):
				return server.ErrNotAccountOwner
			case errors.Is(err, store.ErrCannotCloseDefault):
				return server.ErrCannotCloseDefault
			case errors.Is(err, store.ErrAccountNotActive):
				return server.ErrAccountNotActive
			}
			return err
		}
		cfg.SuspendAccountSystem = func(ctx context.Context, accountID, category, reason string) error {
			err := st.SuspendAccountSystem(ctx, accountID, category, reason)
			switch {
			case errors.Is(err, store.ErrAccountNotFound):
				return server.ErrNotFound
			case errors.Is(err, store.ErrCannotCloseDefault):
				return server.ErrCannotCloseDefault
			case errors.Is(err, store.ErrAccountNotActive):
				return server.ErrConflict
			}
			return err
		}
		cfg.StreamAccountExport = func(ctx context.Context, accountID string, w io.Writer) error {
			cellName := os.Getenv("WITSELF_CELL_NAME")
			err := st.ExportAccount(ctx, accountID, cellName, version.Version, w)
			switch {
			case errors.Is(err, store.ErrAccountNotFound):
				return server.ErrNotFound
			case errors.Is(err, store.ErrAccountNotExportable):
				return server.ErrConflict
			}
			return err
		}
		cfg.ResumeAccountOwner = func(ctx context.Context, accountID, operatorID string) error {
			err := st.ResumeAccountOwner(ctx, accountID, operatorID)
			switch {
			case errors.Is(err, store.ErrAccountNotFound):
				return server.ErrNotFound
			case errors.Is(err, store.ErrNotAccountOwner):
				return server.ErrNotAccountOwner
			case errors.Is(err, store.ErrAccountNotSuspended):
				return server.ErrAccountNotSuspended
			case errors.Is(err, store.ErrCannotSelfResume):
				return server.ErrCannotSelfResume
			}
			return err
		}
		cfg.OpenSupportTicket = func(ctx context.Context, in server.OpenTicketRequest) (server.SupportTicket, server.SupportTicketMessage, error) {
			t, m, err := st.OpenTicket(ctx, store.OpenTicketInput{
				AccountID:  in.AccountID,
				OperatorID: in.OperatorID,
				Subject:    in.Subject,
				Category:   in.Category,
				Priority:   in.Priority,
				Body:       in.Body,
			})
			if err := mapSupportError(err); err != nil {
				return server.SupportTicket{}, server.SupportTicketMessage{}, err
			}
			return toServerTicket(t), toServerMessage(m), nil
		}
		cfg.ListSupportTickets = func(ctx context.Context, accountID, operatorID string) ([]server.SupportTicket, error) {
			ts, err := st.ListTickets(ctx, accountID, operatorID)
			if err := mapSupportError(err); err != nil {
				return nil, err
			}
			out := make([]server.SupportTicket, len(ts))
			for i, t := range ts {
				out[i] = toServerTicket(t)
			}
			return out, nil
		}
		cfg.GetSupportTicket = func(ctx context.Context, accountID, operatorID, ticketID string) (server.SupportTicket, []server.SupportTicketMessage, error) {
			t, ms, err := st.GetTicket(ctx, accountID, operatorID, ticketID)
			if err := mapSupportError(err); err != nil {
				return server.SupportTicket{}, nil, err
			}
			out := make([]server.SupportTicketMessage, len(ms))
			for i, m := range ms {
				out[i] = toServerMessage(m)
			}
			return toServerTicket(t), out, nil
		}
		cfg.ReplySupportTicket = func(ctx context.Context, accountID, operatorID, ticketID, body string) (server.SupportTicketMessage, error) {
			m, err := st.ReplyToTicket(ctx, accountID, operatorID, ticketID, body)
			if err := mapSupportError(err); err != nil {
				return server.SupportTicketMessage{}, err
			}
			return toServerMessage(m), nil
		}
		cfg.ChangeSupportTicketState = func(ctx context.Context, in server.ChangeTicketStateRequest) (server.SupportTicket, error) {
			t, err := st.ChangeTicketState(ctx, store.ChangeTicketStateInput{
				AccountID:  in.AccountID,
				OperatorID: in.OperatorID,
				TicketID:   in.TicketID,
				NewState:   in.NewState,
			})
			if err := mapSupportError(err); err != nil {
				return server.SupportTicket{}, err
			}
			return toServerTicket(t), nil
		}
		cfg.ListAdminTickets = func(ctx context.Context, accountID string) ([]server.SupportTicket, error) {
			ts, err := st.ListTicketsAdmin(ctx, accountID)
			if err := mapSupportError(err); err != nil {
				return nil, err
			}
			out := make([]server.SupportTicket, len(ts))
			for i, t := range ts {
				out[i] = toServerTicket(t)
			}
			return out, nil
		}
		cfg.GetAdminTicket = func(ctx context.Context, accountID, ticketID string) (server.SupportTicket, []server.SupportTicketMessage, error) {
			t, ms, err := st.GetTicketAdmin(ctx, accountID, ticketID)
			if err := mapSupportError(err); err != nil {
				return server.SupportTicket{}, nil, err
			}
			out := make([]server.SupportTicketMessage, len(ms))
			for i, m := range ms {
				out[i] = toServerMessage(m)
			}
			return toServerTicket(t), out, nil
		}
		cfg.ReplyAdminTicket = func(ctx context.Context, in server.ReplyAdminTicketRequest) (server.SupportTicketMessage, error) {
			m, err := st.ReplyAdminTicket(ctx, store.ReplyAdminInput{
				AccountID:   in.AccountID,
				AdminHandle: in.AdminHandle,
				TicketID:    in.TicketID,
				Body:        in.Body,
			})
			if err := mapSupportError(err); err != nil {
				return server.SupportTicketMessage{}, err
			}
			return toServerMessage(m), nil
		}
		cfg.ChangeAdminTicketState = func(ctx context.Context, in server.ChangeAdminTicketStateRequest) (server.SupportTicket, error) {
			t, err := st.ChangeAdminTicketState(ctx, store.ChangeAdminStateInput{
				AccountID:   in.AccountID,
				AdminHandle: in.AdminHandle,
				TicketID:    in.TicketID,
				NewState:    in.NewState,
			})
			if err := mapSupportError(err); err != nil {
				return server.SupportTicket{}, err
			}
			return toServerTicket(t), nil
		}
		cfg.ListAdminTicketsAll = func(ctx context.Context, in server.ListAdminTicketsAllRequest) (server.ListAdminTicketsAllResult, error) {
			res, err := st.ListTicketsAdminAll(ctx, store.ListAdminAllInput{
				States:    in.States,
				Since:     in.Since,
				Limit:     in.Limit,
				PageToken: in.PageToken,
			})
			if err := mapSupportError(err); err != nil {
				return server.ListAdminTicketsAllResult{}, err
			}
			out := make([]server.SupportTicket, len(res.Tickets))
			for i, t := range res.Tickets {
				out[i] = toServerTicket(t)
			}
			return server.ListAdminTicketsAllResult{
				Tickets:       out,
				NextPageToken: res.NextPageToken,
			}, nil
		}
		cfg.ListAdminEventsAll = func(ctx context.Context, filter server.EventFilter) (server.EventPage, error) {
			page, err := st.ListEventsAdminAll(ctx, store.EventFilter{
				Since:  filter.Since,
				Until:  filter.Until,
				Verb:   filter.Verb,
				Limit:  filter.Limit,
				Cursor: filter.Cursor,
			})
			if errors.Is(err, store.ErrBadEventCursor) {
				return server.EventPage{}, fmt.Errorf("%w: %v", server.ErrBadInput, err)
			}
			if err != nil {
				return server.EventPage{}, err
			}
			out := make([]server.Event, len(page.Events))
			for i, e := range page.Events {
				out[i] = server.Event{
					ID:         e.ID,
					AccountID:  e.AccountID,
					OccurredAt: e.OccurredAt,
					ActorKind:  e.ActorKind,
					ActorID:    e.ActorID,
					Verb:       e.Verb,
					Metadata:   e.Metadata,
				}
			}
			return server.EventPage{Events: out, NextCursor: page.NextCursor}, nil
		}
		cfg.GetAdminSupportPolicy = func(ctx context.Context, accountID string) (string, error) {
			p, err := st.GetSupportPolicyAdmin(ctx, accountID)
			if err := mapSupportError(err); err != nil {
				return "", err
			}
			return p, nil
		}
		cfg.SetAdminSupportPolicy = func(ctx context.Context, in server.SetAdminSupportPolicyRequest) (server.SetAdminSupportPolicyResult, error) {
			res, err := st.SetSupportPolicyAdmin(ctx, store.SetSupportPolicyAdminInput{
				AccountID:   in.AccountID,
				AdminHandle: in.AdminHandle,
				NewPolicy:   in.NewPolicy,
			})
			if err := mapSupportError(err); err != nil {
				return server.SetAdminSupportPolicyResult{}, err
			}
			return server.SetAdminSupportPolicyResult{
				AccountID:  res.AccountID,
				PolicyFrom: res.PolicyFrom,
				PolicyTo:   res.PolicyTo,
			}, nil
		}
		cfg.LogAccountEvent = func(ctx context.Context, accountID, verb, actorKind string, metadata map[string]any) error {
			err := st.LogEvent(ctx, store.EventInput{
				AccountID: accountID,
				ActorKind: actorKind,
				Verb:      verb,
				Metadata:  metadata,
			})
			switch {
			case errors.Is(err, store.ErrAccountNotFound):
				return server.ErrNotFound
			case errors.Is(err, store.ErrUnknownVerb),
				errors.Is(err, store.ErrBadEventMetadata):
				return fmt.Errorf("%w: %v", server.ErrBadInput, err)
			}
			return err
		}
		cfg.ImportAccountArchive = func(ctx context.Context, accountID string, body io.Reader) (server.ImportSummary, error) {
			m, err := st.ImportAccount(ctx, accountID, body)
			switch {
			case errors.Is(err, store.ErrAccountExists):
				return server.ImportSummary{}, server.ErrConflict
			case errors.Is(err, export.ErrArchiveTooNew):
				return server.ImportSummary{}, server.ErrArchiveTooNew
			case errors.Is(err, store.ErrArchiveAccountMismatch),
				errors.Is(err, store.ErrArchiveContent),
				errors.Is(err, export.ErrCorrupt):
				return server.ImportSummary{}, server.ErrBadArchive
			case err != nil:
				return server.ImportSummary{}, err
			}
			return server.ImportSummary{
				AccountID:     m.AccountID,
				Status:        m.Status,
				SchemaVersion: m.SchemaVersion,
			}, nil
		}
		cfg.ResumeAccountSystem = func(ctx context.Context, accountID, category string) error {
			err := st.ResumeAccountSystem(ctx, accountID, category)
			switch {
			case errors.Is(err, store.ErrAccountNotFound):
				return server.ErrNotFound
			case errors.Is(err, store.ErrAccountNotSuspended):
				return server.ErrAccountNotSuspended
			case errors.Is(err, store.ErrResumeWrongCategory):
				return server.ErrResumeWrongCategory
			}
			return err
		}
		cfg.SetAccountPlan = func(ctx context.Context, accountID, plan string, limits map[string]int64, features []string) error {
			err := st.SetAccountPlan(ctx, accountID, plan, limits, features)
			if errors.Is(err, store.ErrAccountNotFound) {
				return server.ErrNotFound
			}
			return err
		}
		// Surface the deployment account's applied plan in /v1/capabilities.
		if acctID := cfg.AccountID; acctID != "" {
			cfg.PlanInfo = func(ctx context.Context) (string, map[string]int64, []string, error) {
				a, err := st.GetAccount(ctx, acctID)
				if err != nil {
					return "", nil, nil, err
				}
				return a.Plan, a.PlanLimits, a.PlanFeatures, nil
			}
		}
		if pt := strings.TrimSpace(os.Getenv("WITSELF_PROVISION_TOKEN")); pt != "" {
			// Account provisioning: the control-plane -> cell trust link. The
			// bootstrap tokens minted per signup are short-lived — the CLI
			// exchanges them within seconds.
			const provisionBootstrapTTL = time.Hour
			cfg.ProvisionToken = pt
			cfg.ProvisionAccount = func(ctx context.Context, email, displayName string) (server.ProvisionedAccount, error) {
				p, err := st.ProvisionAccount(ctx, email, displayName, provisionBootstrapTTL)
				if err != nil {
					return server.ProvisionedAccount{}, err
				}
				return server.ProvisionedAccount{
					AccountID:      p.AccountID,
					OperatorID:     p.OperatorID,
					Email:          p.Email,
					Status:         p.Status,
					BootstrapToken: p.BootstrapToken,
				}, nil
			}
			cfg.ReapAccount = func(ctx context.Context, accountID string) (bool, error) {
				reaped, err := st.ReapPendingAccount(ctx, accountID, "activation window expired")
				switch {
				case errors.Is(err, store.ErrAccountNotFound):
					return false, server.ErrNotFound
				case errors.Is(err, store.ErrAccountActive):
					return false, server.ErrConflict
				}
				return reaped, err
			}
			cfg.ActivateAccount = func(ctx context.Context, accountID string) (bool, error) {
				activated, err := st.ActivateAccount(ctx, accountID)
				switch {
				case errors.Is(err, store.ErrAccountNotFound):
					return false, server.ErrNotFound
				case errors.Is(err, store.ErrAccountNotActivatable):
					return false, server.ErrConflict
				}
				return activated, err
			}
			cfg.AccountContact = func(ctx context.Context, accountID string) (server.AccountRecord, error) {
				a, err := st.GetAccount(ctx, accountID)
				if errors.Is(err, store.ErrAccountNotFound) {
					return server.AccountRecord{}, server.ErrNotFound
				}
				if err != nil {
					return server.AccountRecord{}, err
				}
				return server.AccountRecord{ID: a.ID, Email: a.Email, Status: a.Status}, nil
			}
			cfg.UpdateAccountEmail = func(ctx context.Context, accountID, operatorID, newEmail string) error {
				err := st.UpdateAccountEmail(ctx, accountID, operatorID, newEmail)
				switch {
				case errors.Is(err, store.ErrAccountNotFound):
					return server.ErrNotFound
				case errors.Is(err, store.ErrNotAccountOwner):
					return server.ErrNotAccountOwner
				case errors.Is(err, store.ErrAccountNotActive):
					return server.ErrConflict
				}
				return err
			}
			cfg.UndoAccountEmail = func(ctx context.Context, accountID, expectedCurrent, newEmail string) error {
				err := st.UndoAccountEmail(ctx, accountID, expectedCurrent, newEmail)
				switch {
				case errors.Is(err, store.ErrAccountNotFound):
					return server.ErrNotFound
				case errors.Is(err, store.ErrAccountNotActive):
					return server.ErrConflict
				case errors.Is(err, store.ErrConflictingUndo):
					return server.ErrEmailChangedSinceUndo
				}
				return err
			}
			cfg.RecoverAccount = func(ctx context.Context, accountID string) (server.ProvisionedAccount, error) {
				p, err := st.RecoverAccount(ctx, accountID, provisionBootstrapTTL)
				switch {
				case errors.Is(err, store.ErrAccountNotFound):
					return server.ProvisionedAccount{}, server.ErrNotFound
				case errors.Is(err, store.ErrAccountNotActive):
					return server.ProvisionedAccount{}, server.ErrConflict
				case err != nil:
					return server.ProvisionedAccount{}, err
				}
				return server.ProvisionedAccount{
					AccountID:      p.AccountID,
					OperatorID:     p.OperatorID,
					Email:          p.Email,
					Status:         p.Status,
					BootstrapToken: p.BootstrapToken,
				}, nil
			}
			fmt.Fprintln(os.Stderr, "witself-server: account provisioning enabled (WITSELF_PROVISION_TOKEN set)")
		}
		cfg.Ready = st.Ping
		fmt.Fprintf(os.Stderr, "witself-server: migrated; account %s, root operator %s ready; /readyz gates on it\n", acctID, oprID)
	} else {
		fmt.Fprintln(os.Stderr, "witself-server: no database configured (WITSELF_DATABASE_URL unset); /readyz unconditional")
	}

	if err := server.Run(ctx, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "witself-server: %v\n", err)
		return 1
	}
	fmt.Fprintln(os.Stderr, "witself-server: shut down cleanly")
	return 0
}

func serverOperator(op store.Operator) server.Operator {
	out := server.Operator{
		ID:          op.ID,
		DisplayName: op.DisplayName,
		Role:        op.Role,
		IsRoot:      op.IsRoot,
		CreatedAt:   op.CreatedAt,
		UpdatedAt:   op.UpdatedAt,
		Tokens:      make([]server.OperatorToken, len(op.Tokens)),
	}
	for i, tok := range op.Tokens {
		out.Tokens[i] = server.OperatorToken{
			ID:          tok.ID,
			DisplayName: tok.DisplayName,
			CreatedAt:   tok.CreatedAt,
			ExpiresAt:   tok.ExpiresAt,
		}
	}
	return out
}

// dbDSN resolves the Postgres DSN from the environment, preferring the
// WITSELF_-prefixed name and falling back to the conventional DATABASE_URL.
func dbDSN() string {
	if v := os.Getenv("WITSELF_DATABASE_URL"); v != "" {
		return v
	}
	return os.Getenv("DATABASE_URL")
}

// bootstrapToken resolves first-operator bootstrap material from a token file,
// preferring an explicit path but also checking the deployment well-known path.
// WITSELF_BOOTSTRAP_TOKEN remains as a local/dev fallback.
func bootstrapToken() (string, error) {
	if path := os.Getenv("WITSELF_BOOTSTRAP_TOKEN_FILE"); path != "" {
		return readTokenFile(path, true)
	}
	if tok := strings.TrimSpace(os.Getenv("WITSELF_BOOTSTRAP_TOKEN")); tok != "" {
		return tok, nil
	}
	return readTokenFile(defaultBootstrapTokenFile, false)
}

func readTokenFile(path string, required bool) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if !required && errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("read bootstrap token file %s: %w", path, err)
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", fmt.Errorf("bootstrap token file %s is empty", path)
	}
	return tok, nil
}

func bootstrapTokenTTL() (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv("WITSELF_BOOTSTRAP_TOKEN_TTL"))
	if raw == "" {
		raw = "24h"
	}
	ttl, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("parse WITSELF_BOOTSTRAP_TOKEN_TTL: %w", err)
	}
	if ttl <= 0 {
		return 0, fmt.Errorf("WITSELF_BOOTSTRAP_TOKEN_TTL must be positive")
	}
	return ttl, nil
}

func usage(w io.Writer) {
	usageLine(w, "witself-server — the Witself backend API server")
	usageLine(w)
	usageLine(w, "Usage:")
	usageLine(w, "  witself-server version    Print version information")
	usageLine(w, "  witself-server serve      Run the API, health, and metrics listeners")
	usageLine(w)
	usageLine(w, "Listeners (override with env):")
	usageLine(w, "  WITSELF_API_ADDR      default :8080  (/v1 API)")
	usageLine(w, "  WITSELF_HEALTH_ADDR   default :8081  (/livez /readyz /startupz)")
	usageLine(w, "  WITSELF_METRICS_ADDR  default :9090  (/metrics)")
	usageLine(w)
	usageLine(w, "Database (optional; when set, /readyz gates on it):")
	usageLine(w, "  WITSELF_DATABASE_URL  Postgres DSN (falls back to DATABASE_URL)")
	usageLine(w)
	usageLine(w, "Bootstrap (optional first-operator setup):")
	usageLine(w, "  WITSELF_BOOTSTRAP_TOKEN_FILE  token file path (default /.witself/tokens/bootstrap.token)")
	usageLine(w, "  WITSELF_PROVISION_TOKEN       enables POST /v1/accounts (control-plane account provisioning)")
	usageLine(w, "  WITSELF_BOOTSTRAP_TOKEN_TTL   token lifetime after adoption (default 24h)")
}

func usageLine(w io.Writer, args ...any) {
	_, _ = fmt.Fprintln(w, args...)
}

// mapSupportError translates the store's support-ticket sentinels into
// the server package's sentinels so the HTTP layer can pick the right
// status code without importing the store package.
func mapSupportError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrSupportDisabled):
		return wrapAsSentinel(server.ErrSupportDisabled, store.ErrSupportDisabled, err)
	case errors.Is(err, store.ErrTicketNotFound):
		return server.ErrTicketNotFound
	case errors.Is(err, store.ErrTicketStateInvalid):
		return wrapAsSentinel(server.ErrTicketStateInvalid, store.ErrTicketStateInvalid, err)
	case errors.Is(err, store.ErrTicketInputInvalid):
		return wrapAsSentinel(server.ErrTicketInputInvalid, store.ErrTicketInputInvalid, err)
	case errors.Is(err, store.ErrNotAccountOwner):
		return server.ErrNotAccountOwner
	case errors.Is(err, store.ErrAccountNotActive):
		return server.ErrAccountNotActive
	case errors.Is(err, store.ErrAccountNotFound):
		return server.ErrNotFound
	case errors.Is(err, store.ErrBadEventCursor):
		return fmt.Errorf("%w: %v", server.ErrBadInput, err)
	}
	return err
}

// wrapAsSentinel returns an error whose errors.Is matches the server
// sentinel but whose Error() reads as "<server-sentinel>: <detail>",
// where detail is inner's text with the store sentinel's message
// stripped off. This avoids the double-print you get from
// fmt.Errorf("%w: %v", server.ErrX, inner) when server.ErrX and the
// store sentinel share the same Error() string — the previous form
// produced messages like "invalid support ticket input: invalid
// support ticket input: subject required".
func wrapAsSentinel(serverSentinel, storeSentinel, inner error) error {
	detail := strings.TrimPrefix(inner.Error(), storeSentinel.Error())
	detail = strings.TrimPrefix(detail, ": ")
	if detail == "" {
		return serverSentinel
	}
	return fmt.Errorf("%w: %s", serverSentinel, detail)
}

func toServerTicket(t store.Ticket) server.SupportTicket {
	return server.SupportTicket{
		ID:              t.ID,
		AccountID:       t.AccountID,
		OpenedAt:        t.OpenedAt,
		OpenedByKind:    t.OpenedByKind,
		OpenedByID:      t.OpenedByID,
		Subject:         t.Subject,
		Category:        t.Category,
		State:           t.State,
		Priority:        t.Priority,
		FirstResponseAt: t.FirstResponseAt,
		ResolvedAt:      t.ResolvedAt,
		ClosedAt:        t.ClosedAt,
		LastActivityAt:  t.LastActivityAt,
		LastMessageID:   t.LastMessageID,
		Correlation:     t.Correlation,
		Metadata:        t.Metadata,
	}
}

// planLimitError translates the store's plan-limit refusal into the server
// sentinel while keeping the store's human-readable detail ("realms 1/1 on
// the free plan") — the HTTP layer surfaces the message verbatim.
func planLimitError(err error) error {
	detail := strings.TrimPrefix(err.Error(), store.ErrPlanLimitReached.Error()+": ")
	return fmt.Errorf("%w: %s", server.ErrPlanLimit, detail)
}

func toServerMessage(m store.TicketMessage) server.SupportTicketMessage {
	return server.SupportTicketMessage{
		ID:          m.ID,
		TicketID:    m.TicketID,
		AccountID:   m.AccountID,
		PostedAt:    m.PostedAt,
		AuthorKind:  m.AuthorKind,
		AuthorID:    m.AuthorID,
		Body:        m.Body,
		Attachments: m.Attachments,
		Metadata:    m.Metadata,
	}
}
