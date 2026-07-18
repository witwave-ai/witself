// Command witself-server is the Witself backend API server. It supports version
// and a serve command that binds the API, health-probe, and metrics listeners.
// The full backend is specified under docs/.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/witwave-ai/witself/internal/export"
	"github.com/witwave-ai/witself/internal/placement"
	"github.com/witwave-ai/witself/internal/server"
	"github.com/witwave-ai/witself/internal/store"
	"github.com/witwave-ai/witself/internal/version"
)

const (
	defaultBootstrapTokenFile         = "/.witself/tokens/bootstrap.token"
	factDeletionEnv                   = "WITSELF_FACT_DELETION_ENABLED"
	factDeletionMinimumSchemaVersion  = 28
	avatarStyleRolloutEnabledEnv      = "WITSELF_AVATAR_STYLE_ROLLOUT_ENABLED"
	avatarStyleRolloutBatchSizeEnv    = "WITSELF_AVATAR_STYLE_ROLLOUT_BATCH_SIZE"
	avatarStyleRolloutIntervalEnv     = "WITSELF_AVATAR_STYLE_ROLLOUT_INTERVAL"
	avatarStyleRolloutBatchTimeoutEnv = "WITSELF_AVATAR_STYLE_ROLLOUT_BATCH_TIMEOUT"
)

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
	factDeletionEnabled, err := factDeletionEnabledFromEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself-server: %v\n", err)
		return 1
	}
	if err := validateFactDeletionFeature(factDeletionEnabled, store.SchemaVersion()); err != nil {
		fmt.Fprintf(os.Stderr, "witself-server: %v\n", err)
		return 1
	}
	avatarRolloutEnabled, avatarRolloutConfig, err := avatarStyleRolloutConfigFromEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself-server: %v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg := server.ConfigFromEnv()
	var stopAvatarRolloutWorker func()
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
		cfg.AuthenticatePrincipal = func(ctx context.Context, plaintext string) (server.DomainPrincipal, bool, error) {
			p, ok, err := st.AuthenticatePrincipal(ctx, plaintext)
			if err != nil || !ok {
				return server.DomainPrincipal{}, ok, err
			}
			return toServerPrincipal(p), true, nil
		}
		configureAvatar(&cfg, st)
		cfg.CreateTranscript = func(ctx context.Context, p server.DomainPrincipal, in server.CreateTranscriptRequest) (server.Transcript, error) {
			tr, err := st.CreateTranscript(ctx, p.AccountID, p.RealmID, p.ID, store.CreateTranscriptInput{
				ExternalID: in.ExternalID,
				Title:      in.Title,
				Metadata:   in.Metadata,
			})
			if err != nil {
				return server.Transcript{}, mapTranscriptError(err)
			}
			return toServerTranscript(tr), nil
		}
		cfg.AppendTranscriptEntry = func(ctx context.Context, p server.DomainPrincipal, transcriptID string, in server.AppendTranscriptEntryRequest) (server.TranscriptEntry, error) {
			entry, err := st.AppendTranscriptEntry(ctx, p.AccountID, p.RealmID, p.ID, transcriptID, store.AppendTranscriptEntryInput{
				ExternalID:        in.ExternalID,
				Role:              in.Role,
				Body:              in.Body,
				Payload:           in.Payload,
				Model:             in.Model,
				ReplyToEntryID:    in.ReplyToEntryID,
				ReplyToExternalID: in.ReplyToExternalID,
				Artifacts:         in.Artifacts,
			})
			if err != nil {
				return server.TranscriptEntry{}, mapTranscriptError(err)
			}
			return toServerTranscriptEntry(entry), nil
		}
		cfg.AppendTranscriptEntries = func(ctx context.Context, p server.DomainPrincipal, transcriptID string, inputs []server.AppendTranscriptEntryRequest) ([]server.TranscriptEntry, error) {
			storeInputs := make([]store.AppendTranscriptEntryInput, len(inputs))
			for i, in := range inputs {
				storeInputs[i] = store.AppendTranscriptEntryInput{
					ExternalID:        in.ExternalID,
					Role:              in.Role,
					Body:              in.Body,
					Payload:           in.Payload,
					Model:             in.Model,
					ReplyToEntryID:    in.ReplyToEntryID,
					ReplyToExternalID: in.ReplyToExternalID,
					Artifacts:         in.Artifacts,
				}
			}
			entries, err := st.AppendTranscriptEntries(ctx, p.AccountID, p.RealmID, p.ID, transcriptID, storeInputs)
			if err != nil {
				return nil, mapTranscriptError(err)
			}
			out := make([]server.TranscriptEntry, len(entries))
			for i, entry := range entries {
				out[i] = toServerTranscriptEntry(entry)
			}
			return out, nil
		}
		cfg.ListTranscripts = func(ctx context.Context, p server.DomainPrincipal) ([]server.Transcript, error) {
			rows, err := st.ListTranscripts(ctx, toStorePrincipal(p))
			if err != nil {
				return nil, mapTranscriptError(err)
			}
			out := make([]server.Transcript, len(rows))
			for i, tr := range rows {
				out[i] = toServerTranscript(tr)
			}
			return out, nil
		}
		cfg.GetTranscript = func(ctx context.Context, p server.DomainPrincipal, transcriptID string) (server.Transcript, []server.TranscriptEntry, error) {
			tr, entries, err := st.GetTranscript(ctx, toStorePrincipal(p), transcriptID)
			if err != nil {
				return server.Transcript{}, nil, mapTranscriptError(err)
			}
			out := make([]server.TranscriptEntry, len(entries))
			for i, entry := range entries {
				out[i] = toServerTranscriptEntry(entry)
			}
			return toServerTranscript(tr), out, nil
		}
		cfg.GetTranscriptPage = func(ctx context.Context, p server.DomainPrincipal, transcriptID string, opts server.TranscriptPageOptions) (server.TranscriptPage, error) {
			page, err := st.GetTranscriptPage(ctx, toStorePrincipal(p), transcriptID, store.TranscriptPageOptions{
				AfterSequence: opts.AfterSequence,
				Limit:         opts.Limit,
				Tail:          opts.Tail,
			})
			if err != nil {
				return server.TranscriptPage{}, mapTranscriptError(err)
			}
			entries := make([]server.TranscriptEntry, len(page.Entries))
			for i, entry := range page.Entries {
				entries[i] = toServerTranscriptEntry(entry)
			}
			return server.TranscriptPage{
				Transcript:        toServerTranscript(page.Transcript),
				Entries:           entries,
				NextAfterSequence: page.NextAfterSequence,
			}, nil
		}
		cfg.GetTranscriptPageObservational = func(ctx context.Context, p server.DomainPrincipal, transcriptID string, opts server.TranscriptPageOptions) (server.TranscriptPage, error) {
			page, err := st.GetTranscriptPageObservational(ctx, toStorePrincipal(p), transcriptID, store.TranscriptPageOptions{
				AfterSequence: opts.AfterSequence,
				Limit:         opts.Limit,
				Tail:          opts.Tail,
			})
			if err != nil {
				return server.TranscriptPage{}, mapTranscriptError(err)
			}
			entries := make([]server.TranscriptEntry, len(page.Entries))
			for i, entry := range page.Entries {
				entries[i] = toServerTranscriptEntry(entry)
			}
			return server.TranscriptPage{
				Transcript:        toServerTranscript(page.Transcript),
				Entries:           entries,
				NextAfterSequence: page.NextAfterSequence,
			}, nil
		}
		cfg.GetUsage = func(ctx context.Context, p server.DomainPrincipal, query server.UsageQuery) (server.UsageReport, error) {
			report, err := st.GetAgentUsage(ctx, toStorePrincipal(p), store.UsageQuery{
				Since: query.Since, Until: query.Until, Bucket: query.Bucket, Dimensions: query.Dimensions,
			})
			switch {
			case errors.Is(err, store.ErrUsageInputInvalid):
				return server.UsageReport{}, fmt.Errorf("%w: %v", server.ErrBadInput, err)
			case errors.Is(err, store.ErrUsageForbidden):
				return server.UsageReport{}, server.ErrForbidden
			case err != nil:
				return server.UsageReport{}, err
			}
			return toServerUsageReport(report), nil
		}
		configureMemory(&cfg, st)
		configureMemoryCuration(&cfg, st)
		configureFactMutations(&cfg, st, factDeletionEnabled)
		cfg.GetFact = func(ctx context.Context, p server.DomainPrincipal, subject, predicate string) (server.Fact, error) {
			fact, err := st.GetFact(ctx, toStorePrincipal(p), subject, predicate)
			if err != nil {
				return server.Fact{}, mapFactError(err)
			}
			return toServerFact(fact), nil
		}
		cfg.GetFactObservational = func(ctx context.Context, p server.DomainPrincipal, subject, predicate string) (server.Fact, error) {
			fact, err := st.GetFactObservational(ctx, toStorePrincipal(p), subject, predicate)
			if err != nil {
				return server.Fact{}, mapFactError(err)
			}
			return toServerFact(fact), nil
		}
		cfg.ListFacts = func(ctx context.Context, p server.DomainPrincipal, opts server.FactListOptions) ([]server.Fact, error) {
			retrievalMode := store.FactRetrievalModeSearch
			if opts.RetrievalMode != "" {
				retrievalMode = store.FactRetrievalMode(opts.RetrievalMode)
			}
			facts, err := st.ListFacts(ctx, toStorePrincipal(p), store.FactListOptions{
				Subject: opts.Subject, PredicatePrefix: opts.PredicatePrefix,
				Limit: opts.Limit, IncludeSensitive: opts.IncludeSensitive,
				OrderByUsage: opts.OrderByUsage, UnusedOnly: opts.UnusedOnly,
				RetrievalMode: retrievalMode,
			})
			if err != nil {
				return nil, mapFactError(err)
			}
			out := make([]server.Fact, len(facts))
			for i, fact := range facts {
				out[i] = toServerFact(fact)
			}
			return out, nil
		}
		cfg.ListFactsObservational = func(ctx context.Context, p server.DomainPrincipal, opts server.FactListOptions) ([]server.Fact, error) {
			retrievalMode := store.FactRetrievalModeSearch
			if opts.RetrievalMode != "" {
				retrievalMode = store.FactRetrievalMode(opts.RetrievalMode)
			}
			facts, err := st.ListFactsObservational(ctx, toStorePrincipal(p), store.FactListOptions{
				Subject: opts.Subject, PredicatePrefix: opts.PredicatePrefix,
				Limit: opts.Limit, IncludeSensitive: opts.IncludeSensitive,
				OrderByUsage: opts.OrderByUsage, UnusedOnly: opts.UnusedOnly,
				RetrievalMode: retrievalMode,
			})
			if err != nil {
				return nil, mapFactError(err)
			}
			out := make([]server.Fact, len(facts))
			for i, fact := range facts {
				out[i] = toServerFact(fact)
			}
			return out, nil
		}
		cfg.GetFactHistory = func(ctx context.Context, p server.DomainPrincipal, factID string) ([]server.FactAssertion, error) {
			assertions, err := st.FactHistory(ctx, toStorePrincipal(p), factID)
			if err != nil {
				return nil, mapFactError(err)
			}
			out := make([]server.FactAssertion, len(assertions))
			for i, assertion := range assertions {
				out[i] = toServerFactAssertion(assertion)
			}
			return out, nil
		}
		cfg.ProposeFact = func(ctx context.Context, p server.DomainPrincipal, in server.ProposeFactRequest) (server.FactCandidate, error) {
			c, err := st.ProposeFact(ctx, toStorePrincipal(p), store.ProposeFactInput{SetFactInput: store.SetFactInput{Subject: in.Subject, Predicate: in.Predicate, ValueType: in.ValueType, Value: in.Value, Recurrence: in.Recurrence, Cardinality: in.Cardinality, Sensitive: in.Sensitive, SourceKind: store.FactSourceInference, SourceRef: in.SourceRef, Confidence: in.Confidence, ObservedAt: in.ObservedAt, ValidFrom: in.ValidFrom, ValidUntil: in.ValidUntil, RecreateDeleted: in.RecreateDeleted, IdempotencyKey: in.IdempotencyKey}, Reason: in.Reason})
			if err != nil {
				return server.FactCandidate{}, mapFactError(err)
			}
			return toServerFactCandidate(c), nil
		}
		cfg.GetFactCandidate = func(ctx context.Context, p server.DomainPrincipal, id string) (server.FactCandidate, error) {
			c, err := st.GetFactCandidate(ctx, toStorePrincipal(p), id)
			if err != nil {
				return server.FactCandidate{}, mapFactError(err)
			}
			return toServerFactCandidate(c), nil
		}
		cfg.ListFactCandidates = func(ctx context.Context, p server.DomainPrincipal, opts server.FactCandidateListOptions) ([]server.FactCandidate, error) {
			rows, err := st.ListFactCandidatesWithOptions(ctx, toStorePrincipal(p), store.FactCandidateListOptions{Status: opts.Status, Limit: opts.Limit})
			if err != nil {
				return nil, mapFactError(err)
			}
			out := make([]server.FactCandidate, len(rows))
			for i := range rows {
				out[i] = toServerFactCandidate(rows[i])
			}
			return out, nil
		}
		cfg.ConfirmFactCandidate = func(ctx context.Context, p server.DomainPrincipal, id, idempotencyKey string) (server.Fact, error) {
			f, err := st.ConfirmFactCandidateIdempotent(ctx, toStorePrincipal(p), id, idempotencyKey)
			if err != nil {
				return server.Fact{}, mapFactError(err)
			}
			return toServerFact(f), nil
		}
		cfg.RejectFactCandidate = func(ctx context.Context, p server.DomainPrincipal, id, idempotencyKey string) (server.FactCandidate, error) {
			c, err := st.RejectFactCandidateIdempotent(ctx, toStorePrincipal(p), id, idempotencyKey)
			if err != nil {
				return server.FactCandidate{}, mapFactError(err)
			}
			return toServerFactCandidate(c), nil
		}
		cfg.UpsertFactSubject = func(ctx context.Context, p server.DomainPrincipal, canonicalKey string, in server.UpsertFactSubjectRequest) (server.FactSubject, error) {
			subject, err := st.UpsertFactSubject(ctx, toStorePrincipal(p), store.UpsertFactSubjectInput{
				CanonicalKey: canonicalKey,
				DisplayName:  in.DisplayName,
			})
			if err != nil {
				return server.FactSubject{}, mapFactError(err)
			}
			return toServerFactSubject(subject), nil
		}
		cfg.AddFactSubjectAlias = func(ctx context.Context, p server.DomainPrincipal, canonicalKey, alias string) (server.FactSubject, error) {
			subject, err := st.AddFactSubjectAlias(ctx, toStorePrincipal(p), canonicalKey, alias)
			if err != nil {
				return server.FactSubject{}, mapFactError(err)
			}
			return toServerFactSubject(subject), nil
		}
		cfg.ListFactSubjects = func(ctx context.Context, p server.DomainPrincipal) ([]server.FactSubject, error) {
			subjects, err := st.ListFactSubjects(ctx, toStorePrincipal(p))
			if err != nil {
				return nil, mapFactError(err)
			}
			out := make([]server.FactSubject, len(subjects))
			for i, subject := range subjects {
				out[i] = toServerFactSubject(subject)
			}
			return out, nil
		}
		getSelfFacts := func(ctx context.Context, p server.DomainPrincipal, limit int, includeCount, observational bool) ([]server.SelfFact, int, error) {
			principal := toStorePrincipal(p)
			opts := selfHydrationFactListOptions(limit)
			var facts []store.Fact
			var err error
			if observational {
				facts, err = st.ListFactsObservational(ctx, principal, opts)
			} else {
				facts, err = st.ListFacts(ctx, principal, opts)
			}
			if err != nil {
				return nil, 0, mapFactError(err)
			}
			total := len(facts)
			if includeCount {
				total, err = st.CountFacts(ctx, principal, opts)
				if err != nil {
					return nil, 0, mapFactError(err)
				}
			} else if len(facts) == limit {
				// Without an inventory count, conservatively signal that a full
				// bounded page may have more records. The hook avoids a COUNT query
				// without ever presenting a possibly incomplete digest as complete.
				total++
			}
			out := make([]server.SelfFact, len(facts))
			for i, f := range facts {
				var value any
				_ = json.Unmarshal(f.Value, &value)
				out[i] = server.SelfFact{ID: f.ID, Name: f.Predicate, Value: value, Primary: true, Sensitive: f.Sensitive, Source: f.SourceKind}
			}
			return out, total, nil
		}
		cfg.GetSelfFacts = func(ctx context.Context, p server.DomainPrincipal, limit int, includeCount bool) ([]server.SelfFact, int, error) {
			return getSelfFacts(ctx, p, limit, includeCount, false)
		}
		cfg.GetSelfFactsObservational = func(ctx context.Context, p server.DomainPrincipal, limit int, includeCount bool) ([]server.SelfFact, int, error) {
			return getSelfFacts(ctx, p, limit, includeCount, true)
		}
		cfg.CountSelfFacts = func(ctx context.Context, p server.DomainPrincipal) (int, error) {
			count, err := st.CountFacts(ctx, toStorePrincipal(p), store.FactListOptions{Subject: "self"})
			if err != nil {
				return 0, mapFactError(err)
			}
			return count, nil
		}
		cfg.UpcomingFacts = func(ctx context.Context, p server.DomainPrincipal, opts server.UpcomingFactOptions) ([]server.FactOccurrence, error) {
			loc := time.UTC
			var err error
			if opts.Timezone != "" {
				loc, err = time.LoadLocation(opts.Timezone)
				if err != nil {
					return nil, fmt.Errorf("%w: invalid timezone", server.ErrBadInput)
				}
			}
			rows, err := st.UpcomingFacts(ctx, toStorePrincipal(p), store.UpcomingFactOptions{From: opts.From, Until: opts.Until, Location: loc, Subject: opts.Subject, PredicatePrefix: opts.PredicatePrefix, Limit: opts.Limit, IncludeSensitive: opts.IncludeSensitive})
			if err != nil {
				return nil, mapFactError(err)
			}
			out := make([]server.FactOccurrence, len(rows))
			for i, row := range rows {
				out[i] = server.FactOccurrence{Fact: toServerFact(row.Fact), OccursOn: row.OccursOn, OccursAt: row.OccursAt}
			}
			return out, nil
		}
		cfg.UpcomingFactsObservational = func(ctx context.Context, p server.DomainPrincipal, opts server.UpcomingFactOptions) ([]server.FactOccurrence, error) {
			loc := time.UTC
			var err error
			if opts.Timezone != "" {
				loc, err = time.LoadLocation(opts.Timezone)
				if err != nil {
					return nil, fmt.Errorf("%w: invalid timezone", server.ErrBadInput)
				}
			}
			rows, err := st.UpcomingFactsObservational(ctx, toStorePrincipal(p), store.UpcomingFactOptions{From: opts.From, Until: opts.Until, Location: loc, Subject: opts.Subject, PredicatePrefix: opts.PredicatePrefix, Limit: opts.Limit, IncludeSensitive: opts.IncludeSensitive})
			if err != nil {
				return nil, mapFactError(err)
			}
			out := make([]server.FactOccurrence, len(rows))
			for i, row := range rows {
				out[i] = server.FactOccurrence{Fact: toServerFact(row.Fact), OccursOn: row.OccursOn, OccursAt: row.OccursAt}
			}
			return out, nil
		}
		cfg.GetSelfMessageCheckpoint = func(ctx context.Context, p server.DomainPrincipal) (*server.SelfMessageCheckpoint, error) {
			checkpoint, err := st.GetSelfMessageCheckpoint(ctx, toStorePrincipal(p))
			if err != nil {
				return nil, mapMessageError(err)
			}
			return &server.SelfMessageCheckpoint{
				Pending:                     checkpoint.Pending,
				MailboxPending:              checkpoint.MailboxPending,
				CandidateOfferPending:       checkpoint.CandidateOfferPending,
				CoordinatorSelectionPending: checkpoint.CoordinatorSelectionPending,
				CandidateAssignmentPending:  checkpoint.CandidateAssignmentPending,
			}, nil
		}
		cfg.SendMessage = func(ctx context.Context, p server.DomainPrincipal, in server.SendMessageRequest) (server.Message, error) {
			msg, err := st.SendMessage(ctx, toStorePrincipal(p), store.SendMessageInput{
				AudienceKind:   in.To.Kind,
				ToAgent:        in.To.ID,
				ToAgents:       in.To.IDs,
				Subject:        in.Subject,
				Kind:           in.Kind,
				Body:           in.Body,
				Payload:        in.Payload,
				ThreadID:       in.ThreadID,
				IdempotencyKey: in.IdempotencyKey,
			})
			if err != nil {
				return server.Message{}, mapMessageError(err)
			}
			return toServerAgentMessage(msg), nil
		}
		cfg.CreateMessageRequest = func(ctx context.Context, p server.DomainPrincipal, in server.CreateMessageRequestRequest) (server.CreateMessageRequestResult, error) {
			result, err := st.OpenMessageRequest(ctx, toStorePrincipal(p), store.OpenMessageRequestInput{
				Subject: in.Subject, Body: in.Body, Payload: in.Payload,
				SelectionPolicy: in.SelectionPolicy, MaxAssignees: in.MaxAssignees,
				OfferWindow:    time.Duration(in.OfferWindowSeconds) * time.Second,
				ExpiresIn:      time.Duration(in.ExpiresInSeconds) * time.Second,
				IdempotencyKey: in.IdempotencyKey,
			})
			if err != nil {
				return server.CreateMessageRequestResult{}, mapMessageRequestError(err)
			}
			return server.CreateMessageRequestResult{
				Request: toServerMessageRequest(result.Request), OpeningMessage: toServerAgentMessage(result.OpeningMessage),
			}, nil
		}
		cfg.ListMessageRequests = func(ctx context.Context, p server.DomainPrincipal, opts server.MessageRequestListOptions) (server.MessageRequestPage, error) {
			page, err := st.ListMessageRequests(ctx, toStorePrincipal(p), store.MessageRequestFilter{
				State: opts.State, Phase: opts.Phase, Role: opts.Role, Limit: opts.Limit, Cursor: opts.Cursor,
			})
			if err != nil {
				return server.MessageRequestPage{}, mapMessageRequestError(err)
			}
			requests := make([]server.MessageRequest, len(page.Requests))
			for i, request := range page.Requests {
				requests[i] = toServerMessageRequest(request)
			}
			return server.MessageRequestPage{Requests: requests, NextCursor: page.NextCursor}, nil
		}
		cfg.GetMessageRequest = func(ctx context.Context, p server.DomainPrincipal, requestID string) (server.MessageRequestDetail, error) {
			detail, err := st.GetMessageRequest(ctx, toStorePrincipal(p), requestID)
			if err != nil {
				return server.MessageRequestDetail{}, mapMessageRequestError(err)
			}
			return toServerMessageRequestDetail(detail), nil
		}
		cfg.OfferMessageRequest = func(ctx context.Context, p server.DomainPrincipal, requestID string, in server.OfferMessageRequestRequest) (server.OfferMessageRequestResult, error) {
			result, err := st.OfferMessageRequest(ctx, toStorePrincipal(p), requestID, store.OfferMessageRequestInput{
				Subject: in.Subject, Body: in.Body, Payload: in.Payload, IdempotencyKey: in.IdempotencyKey,
			})
			if err != nil {
				return server.OfferMessageRequestResult{}, mapMessageRequestError(err)
			}
			return server.OfferMessageRequestResult{
				Request: toServerMessageRequest(result.Request), Offer: toServerMessageRequestOffer(result.Offer),
			}, nil
		}
		cfg.DeclineMessageRequest = func(ctx context.Context, p server.DomainPrincipal, requestID string, in server.DeclineMessageRequestRequest) (server.MessageRequest, error) {
			request, err := st.DeclineMessageRequest(ctx, toStorePrincipal(p), requestID, store.DeclineMessageRequestInput{
				IdempotencyKey: in.IdempotencyKey,
			})
			if err != nil {
				return server.MessageRequest{}, mapMessageRequestError(err)
			}
			return toServerMessageRequest(request), nil
		}
		cfg.SelectMessageRequest = func(ctx context.Context, p server.DomainPrincipal, requestID string, in server.SelectMessageRequestRequest) (server.SelectMessageRequestResult, error) {
			result, err := st.SelectMessageRequest(ctx, toStorePrincipal(p), requestID, store.SelectMessageRequestInput{
				SelectedAgentIDs: in.SelectedAgentIDs,
				Reservation:      time.Duration(in.ReservationSeconds) * time.Second,
				IdempotencyKey:   in.IdempotencyKey,
			})
			if err != nil {
				return server.SelectMessageRequestResult{}, mapMessageRequestError(err)
			}
			claims := make([]server.MessageRequestClaim, len(result.Claims))
			for i, claim := range result.Claims {
				claims[i] = toServerMessageRequestClaim(claim)
			}
			return server.SelectMessageRequestResult{
				Request: toServerMessageRequest(result.Request), Selection: toServerMessageRequestSelection(result.Selection), Claims: claims,
			}, nil
		}
		cfg.CancelMessageRequest = func(ctx context.Context, p server.DomainPrincipal, requestID string) (server.MessageRequest, error) {
			request, err := st.CancelMessageRequest(ctx, toStorePrincipal(p), requestID)
			if err != nil {
				return server.MessageRequest{}, mapMessageRequestError(err)
			}
			return toServerMessageRequest(request), nil
		}
		cfg.ClaimMessageRequest = func(ctx context.Context, p server.DomainPrincipal, requestID string, in server.ClaimMessageRequestRequest) (server.MessageRequestClaim, error) {
			claim, err := st.ClaimMessageRequest(ctx, toStorePrincipal(p), requestID, store.ClaimMessageRequestInput{
				LeaseDuration: time.Duration(in.LeaseSeconds) * time.Second, IdempotencyKey: in.IdempotencyKey,
			})
			if err != nil {
				return server.MessageRequestClaim{}, mapMessageRequestError(err)
			}
			return toServerMessageRequestClaim(claim), nil
		}
		cfg.RenewMessageRequest = func(ctx context.Context, p server.DomainPrincipal, requestID string, in server.RenewMessageRequestRequest) (server.MessageRequestClaim, error) {
			claim, err := st.RenewMessageRequest(ctx, toStorePrincipal(p), requestID, store.RenewMessageRequestInput{
				ClaimID: in.ClaimID, Generation: in.Generation,
				LeaseDuration: time.Duration(in.LeaseSeconds) * time.Second,
			})
			if err != nil {
				return server.MessageRequestClaim{}, mapMessageRequestError(err)
			}
			return toServerMessageRequestClaim(claim), nil
		}
		cfg.ReleaseMessageRequest = func(ctx context.Context, p server.DomainPrincipal, requestID string, in server.ReleaseMessageRequestRequest) (server.MessageRequestClaim, error) {
			claim, err := st.ReleaseMessageRequest(ctx, toStorePrincipal(p), requestID, store.ReleaseMessageRequestInput{
				ClaimID: in.ClaimID, Generation: in.Generation, DeterministicFailure: in.DeterministicFailure,
			})
			if err != nil {
				return server.MessageRequestClaim{}, mapMessageRequestError(err)
			}
			return toServerMessageRequestClaim(claim), nil
		}
		cfg.CompleteMessageRequest = func(ctx context.Context, p server.DomainPrincipal, requestID string, in server.CompleteMessageRequestRequest) (server.CompleteMessageRequestResult, error) {
			result, err := st.CompleteMessageRequest(ctx, toStorePrincipal(p), requestID, store.CompleteMessageRequestInput{
				ClaimID: in.ClaimID, Generation: in.Generation, Subject: in.Subject,
				Body: in.Body, Payload: in.Payload, IdempotencyKey: in.IdempotencyKey,
			})
			if err != nil {
				return server.CompleteMessageRequestResult{}, mapMessageRequestError(err)
			}
			return server.CompleteMessageRequestResult{
				Request: toServerMessageRequest(result.Request), Claim: toServerMessageRequestClaim(result.Claim),
				Message: toServerAgentMessage(result.Message),
			}, nil
		}
		cfg.ListMessages = func(ctx context.Context, p server.DomainPrincipal, opts server.MessageListOptions) (server.MessagePage, error) {
			page, err := st.ListMessages(ctx, toStorePrincipal(p), store.MessageFilter{
				Direction:   opts.Direction,
				Unread:      opts.Unread,
				Unacked:     opts.Unacked,
				OldestFirst: opts.OldestFirst,
				From:        opts.From,
				ThreadID:    opts.ThreadID,
				Kind:        opts.Kind,
				Limit:       opts.Limit,
				Cursor:      opts.Cursor,
			})
			if err != nil {
				return server.MessagePage{}, mapMessageError(err)
			}
			out := make([]server.Message, len(page.Messages))
			for i, msg := range page.Messages {
				out[i] = toServerAgentMessage(msg)
			}
			return server.MessagePage{Messages: out, NextCursor: page.NextCursor}, nil
		}
		cfg.ReplyMessage = func(ctx context.Context, p server.DomainPrincipal, parentMessageID string, in server.ReplyMessageRequest) (server.Message, error) {
			msg, err := st.ReplyMessage(ctx, toStorePrincipal(p), parentMessageID, store.ReplyMessageInput{
				Subject: in.Subject, Kind: in.Kind, Body: in.Body,
				Payload: in.Payload, IdempotencyKey: in.IdempotencyKey,
			})
			if err != nil {
				return server.Message{}, mapMessageError(err)
			}
			return toServerAgentMessage(msg), nil
		}
		cfg.ClaimMessage = func(ctx context.Context, p server.DomainPrincipal, messageID string, in server.ClaimMessageRequest) (server.MessageProcessing, error) {
			msg, err := st.ClaimMessage(ctx, toStorePrincipal(p), messageID, store.ClaimMessageInput{
				LeaseDuration:  time.Duration(in.LeaseSeconds) * time.Second,
				IdempotencyKey: in.IdempotencyKey,
			})
			if err != nil {
				return server.MessageProcessing{}, mapMessageError(err)
			}
			return toServerMessageProcessing(msg.Processing), nil
		}
		cfg.RenewMessageClaim = func(ctx context.Context, p server.DomainPrincipal, messageID string, in server.RenewMessageClaimRequest) (server.MessageProcessing, error) {
			msg, err := st.RenewMessageClaim(ctx, toStorePrincipal(p), messageID, store.RenewMessageClaimInput{
				ClaimID: in.ClaimID, ProcessingGeneration: in.Generation,
				LeaseDuration: time.Duration(in.LeaseSeconds) * time.Second,
			})
			if err != nil {
				return server.MessageProcessing{}, mapMessageError(err)
			}
			return toServerMessageProcessing(msg.Processing), nil
		}
		cfg.ReleaseMessageClaim = func(ctx context.Context, p server.DomainPrincipal, messageID string, in server.MessageClaimRequest) (server.MessageProcessing, error) {
			msg, err := st.ReleaseMessageClaim(ctx, toStorePrincipal(p), messageID, store.ReleaseMessageClaimInput{
				ClaimID: in.ClaimID, ProcessingGeneration: in.Generation,
				DeterministicFailure: in.DeterministicFailure,
			})
			if err != nil {
				return server.MessageProcessing{}, mapMessageError(err)
			}
			return toServerMessageProcessing(msg.Processing), nil
		}
		cfg.CompleteMessage = func(ctx context.Context, p server.DomainPrincipal, messageID string, in server.CompleteMessageRequest) (server.CompleteMessageResult, error) {
			result, err := st.CompleteMessage(ctx, toStorePrincipal(p), messageID, store.CompleteMessageInput{
				ClaimID: in.ClaimID, ProcessingGeneration: in.Generation,
				IdempotencyKey: in.IdempotencyKey, Subject: in.Subject, Kind: in.Kind,
				Body: in.Body, Payload: in.Payload,
			})
			if err != nil {
				return server.CompleteMessageResult{}, mapMessageError(err)
			}
			return server.CompleteMessageResult{
				Processing: toServerMessageProcessing(result.Processing),
				Message:    toServerAgentMessage(result.ResultMessage),
			}, nil
		}
		cfg.ReadMessage = func(ctx context.Context, p server.DomainPrincipal, messageID string) (server.Message, error) {
			msg, err := st.ReadMessage(ctx, toStorePrincipal(p), messageID)
			if err != nil {
				return server.Message{}, mapMessageError(err)
			}
			return toServerAgentMessage(msg), nil
		}
		cfg.AckMessage = func(ctx context.Context, p server.DomainPrincipal, messageID string) (server.Message, error) {
			msg, err := st.AckMessage(ctx, toStorePrincipal(p), messageID)
			if err != nil {
				return server.Message{}, mapMessageError(err)
			}
			return toServerAgentMessage(msg), nil
		}
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
		cfg.ListSelfPeers = func(ctx context.Context, p server.DomainPrincipal) ([]server.SelfPeer, error) {
			peers, err := st.ListAgentPeers(ctx, toStorePrincipal(p))
			if err != nil {
				return nil, err
			}
			out := make([]server.SelfPeer, len(peers))
			for i, peer := range peers {
				out[i] = server.SelfPeer{
					ID: peer.ID, Name: peer.Name, LastActivityAt: peer.LastActivityAt,
					LastRuntime: peer.LastRuntime, LastLocation: peer.LastLocation,
					LastEvent: peer.LastEvent,
				}
			}
			return out, nil
		}
		cfg.TouchAgentActivity = func(ctx context.Context, p server.DomainPrincipal, in server.AgentActivityRequest) (server.AgentActivity, error) {
			activity, err := st.TouchAgentActivity(ctx, toStorePrincipal(p), store.AgentActivityInput{
				Runtime: in.Runtime, LocationID: in.LocationID, Location: in.Location,
				Event: in.Event, EventID: in.EventID, EventOccurredAt: in.EventOccurredAt,
			})
			switch {
			case errors.Is(err, store.ErrAgentActivityInputInvalid):
				return server.AgentActivity{}, fmt.Errorf("%w: %v", server.ErrBadInput, err)
			case errors.Is(err, store.ErrAgentActivityForbidden):
				return server.AgentActivity{}, server.ErrForbidden
			case errors.Is(err, store.ErrAgentNotFound):
				return server.AgentActivity{}, server.ErrNotFound
			case err != nil:
				return server.AgentActivity{}, err
			}
			return server.AgentActivity{
				LastActivityAt: activity.LastActivityAt, LastRuntime: activity.Runtime,
				LastLocation: activity.Location, LastEvent: activity.Event,
			}, nil
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
		cfg.CreateCuratorToken = func(ctx context.Context, accountID, actorOperatorID, agentID, accessProfile, displayName string, ttl time.Duration) (string, string, string, time.Time, error) {
			tok, tokenID, agentName, expiresAt, err := st.CreateCuratorToken(
				ctx, accountID, actorOperatorID, agentID, accessProfile, displayName, ttl,
			)
			switch {
			case errors.Is(err, store.ErrAgentNotFound), errors.Is(err, store.ErrAccountNotFound):
				return "", "", "", time.Time{}, server.ErrNotFound
			case errors.Is(err, store.ErrAccountNotActive):
				return "", "", "", time.Time{}, server.ErrAccountNotActive
			case errors.Is(err, store.ErrInvalidCuratorAccessProfile),
				errors.Is(err, store.ErrInvalidCuratorTokenTTL),
				errors.Is(err, store.ErrInvalidCuratorTokenDisplayName):
				return "", "", "", time.Time{}, server.ErrBadInput
			}
			return tok, tokenID, agentName, expiresAt, err
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
			cfg.GetPlacementPolicySystem = func(ctx context.Context, accountID string) (placement.Policy, error) {
				a, err := st.GetAccount(ctx, accountID)
				if errors.Is(err, store.ErrAccountNotFound) {
					return placement.Policy{}, server.ErrNotFound
				}
				if err != nil {
					return placement.Policy{}, err
				}
				return a.PlacementPolicy, nil
			}
			cfg.SetPlacementPolicySystem = func(ctx context.Context, accountID string, policy placement.Policy) (placement.Policy, error) {
				policy, err := st.SetPlacementPolicySystem(ctx, accountID, policy)
				if errors.Is(err, store.ErrAccountNotFound) {
					return placement.Policy{}, server.ErrNotFound
				}
				return policy, err
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
		if avatarRolloutEnabled {
			workerCtx, cancelWorker := context.WithCancel(ctx)
			workerDone := make(chan error, 1)
			go func() {
				workerDone <- st.RunAvatarStyleRolloutWorker(workerCtx, avatarRolloutConfig, func(err error) {
					fmt.Fprintf(os.Stderr, "witself-server: avatar style rollout: %v\n", err)
				})
			}()
			stopAvatarRolloutWorker = func() {
				cancelWorker()
				if err := <-workerDone; err != nil {
					fmt.Fprintf(os.Stderr, "witself-server: avatar style rollout stopped: %v\n", err)
				}
			}
			fmt.Fprintf(os.Stderr, "witself-server: avatar style rollout worker enabled (batch %d, interval %s)\n",
				avatarRolloutConfig.BatchSize, avatarRolloutConfig.Interval)
		}
		fmt.Fprintf(os.Stderr, "witself-server: migrated; account %s, root operator %s ready; /readyz gates on it\n", acctID, oprID)
	} else {
		fmt.Fprintln(os.Stderr, "witself-server: no database configured (WITSELF_DATABASE_URL unset); /readyz unconditional")
	}

	runErr := server.Run(ctx, cfg)
	if stopAvatarRolloutWorker != nil {
		stopAvatarRolloutWorker()
	}
	if runErr != nil {
		fmt.Fprintf(os.Stderr, "witself-server: %v\n", runErr)
		return 1
	}
	fmt.Fprintln(os.Stderr, "witself-server: shut down cleanly")
	return 0
}

func avatarStyleRolloutConfigFromEnv() (bool, store.AvatarStyleRolloutWorkerConfig, error) {
	enabled := true
	if raw, ok := os.LookupEnv(avatarStyleRolloutEnabledEnv); ok {
		parsed, err := strconv.ParseBool(strings.TrimSpace(raw))
		if err != nil {
			return false, store.AvatarStyleRolloutWorkerConfig{}, fmt.Errorf("%s must be a boolean: %w", avatarStyleRolloutEnabledEnv, err)
		}
		enabled = parsed
	}
	cfg := store.DefaultAvatarStyleRolloutWorkerConfig()
	if raw, ok := os.LookupEnv(avatarStyleRolloutBatchSizeEnv); ok {
		parsed, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil {
			return false, store.AvatarStyleRolloutWorkerConfig{}, fmt.Errorf("%s must be an integer: %w", avatarStyleRolloutBatchSizeEnv, err)
		}
		cfg.BatchSize = parsed
	}
	if raw, ok := os.LookupEnv(avatarStyleRolloutIntervalEnv); ok {
		parsed, err := time.ParseDuration(strings.TrimSpace(raw))
		if err != nil {
			return false, store.AvatarStyleRolloutWorkerConfig{}, fmt.Errorf("%s must be a duration: %w", avatarStyleRolloutIntervalEnv, err)
		}
		cfg.Interval = parsed
	}
	if raw, ok := os.LookupEnv(avatarStyleRolloutBatchTimeoutEnv); ok {
		parsed, err := time.ParseDuration(strings.TrimSpace(raw))
		if err != nil {
			return false, store.AvatarStyleRolloutWorkerConfig{}, fmt.Errorf("%s must be a duration: %w", avatarStyleRolloutBatchTimeoutEnv, err)
		}
		cfg.BatchTimeout = parsed
	}
	if err := cfg.Validate(); err != nil {
		return false, store.AvatarStyleRolloutWorkerConfig{}, fmt.Errorf(
			"%s/%s/%s avatar style rollout configuration: %w",
			avatarStyleRolloutBatchSizeEnv, avatarStyleRolloutIntervalEnv,
			avatarStyleRolloutBatchTimeoutEnv, err)
	}
	return enabled, cfg, nil
}

func factDeletionEnabledFromEnv() (bool, error) {
	raw, ok := os.LookupEnv(factDeletionEnv)
	if !ok {
		return false, nil
	}
	enabled, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean: %w", factDeletionEnv, err)
	}
	return enabled, nil
}

func validateFactDeletionFeature(enabled bool, schemaVersion int) error {
	if enabled && schemaVersion < factDeletionMinimumSchemaVersion {
		return fmt.Errorf(
			"%s=true requires store schema version %d or newer (compiled version is %d)",
			factDeletionEnv,
			factDeletionMinimumSchemaVersion,
			schemaVersion,
		)
	}
	return nil
}

func selfHydrationFactListOptions(limit int) store.FactListOptions {
	return store.FactListOptions{
		Subject:          "self",
		Limit:            limit,
		IncludeSensitive: true,
		OrderByUsage:     true,
		RetrievalMode:    store.FactRetrievalModeSelfHydration,
	}
}

func configureFactMutations(cfg *server.Config, st *store.Store, deletionEnabled bool) {
	cfg.SetFact = func(ctx context.Context, p server.DomainPrincipal, in server.SetFactRequest) (server.Fact, error) {
		if in.RecreateDeleted && !deletionEnabled {
			return server.Fact{}, fmt.Errorf(
				"%w: recreate_deleted requires %s=true",
				server.ErrBadInput,
				factDeletionEnv,
			)
		}
		fact, err := st.SetFact(ctx, toStorePrincipal(p), store.SetFactInput{
			Subject: in.Subject, Predicate: in.Predicate, ValueType: in.ValueType,
			Value: in.Value, Recurrence: in.Recurrence, Cardinality: in.Cardinality, Sensitive: in.Sensitive,
			SourceKind: store.FactSourceAgent, SourceRef: in.SourceRef, Confidence: in.Confidence,
			ObservedAt: in.ObservedAt, ConfirmedAt: in.ConfirmedAt,
			ValidFrom: in.ValidFrom, ValidUntil: in.ValidUntil,
			RecreateDeleted: in.RecreateDeleted, IdempotencyKey: in.IdempotencyKey,
		})
		if err != nil {
			return server.Fact{}, mapFactError(err)
		}
		return toServerFact(fact), nil
	}
	if !deletionEnabled {
		return
	}
	cfg.DeleteFact = func(ctx context.Context, p server.DomainPrincipal, in server.DeleteFactRequest) (server.FactDeletionReceipt, error) {
		result, err := st.DeleteFact(ctx, toStorePrincipal(p), store.DeleteFactInput{
			FactID:                      in.FactID,
			Subject:                     in.Subject,
			Predicate:                   in.Predicate,
			ExpectedResolvedAssertionID: in.ExpectedResolvedAssertionID,
			ExpectedCandidateRevision:   in.ExpectedCandidateRevision,
			IdempotencyKey:              in.IdempotencyKey,
			Apply:                       in.Apply,
		})
		if err != nil {
			return server.FactDeletionReceipt{}, mapFactError(err)
		}
		return toServerFactDeletion(result), nil
	}
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

func mapTranscriptError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrTranscriptInputInvalid):
		return wrapAsSentinel(server.ErrBadInput, store.ErrTranscriptInputInvalid, err)
	case errors.Is(err, store.ErrTranscriptExists):
		return server.ErrConflict
	case errors.Is(err, store.ErrTranscriptNotFound):
		return server.ErrNotFound
	case errors.Is(err, store.ErrTranscriptForbidden), errors.Is(err, store.ErrAgentNotFound), errors.Is(err, store.ErrAccountNotActive):
		return server.ErrForbidden
	case errors.Is(err, store.ErrAccountNotFound):
		return server.ErrNotFound
	default:
		return err
	}
}

func mapMessageError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrMessageInputInvalid), errors.Is(err, store.ErrMessageCursorInvalid):
		return wrapAsSentinel(server.ErrBadInput, store.ErrMessageInputInvalid, err)
	case errors.Is(err, store.ErrMessageRecipientMissing), errors.Is(err, store.ErrMessageNotFound):
		return server.ErrNotFound
	case errors.Is(err, store.ErrMessageBusy):
		return server.ErrBusy
	case errors.Is(err, store.ErrMessageClaimLost), errors.Is(err, store.ErrMessageConflict):
		return server.ErrConflict
	case errors.Is(err, store.ErrMessageForbidden), errors.Is(err, store.ErrAgentNotFound), errors.Is(err, store.ErrAccountNotActive):
		return server.ErrForbidden
	case errors.Is(err, store.ErrAccountNotFound):
		return server.ErrNotFound
	default:
		return err
	}
}

func mapMessageRequestError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrMessageRequestInputInvalid):
		return wrapAsSentinel(server.ErrBadInput, store.ErrMessageRequestInputInvalid, err)
	case errors.Is(err, store.ErrMessageRequestCursorInvalid):
		return wrapAsSentinel(server.ErrBadInput, store.ErrMessageRequestCursorInvalid, err)
	case errors.Is(err, store.ErrMessageInputInvalid), errors.Is(err, store.ErrMessageCursorInvalid):
		return wrapAsSentinel(server.ErrBadInput, store.ErrMessageInputInvalid, err)
	case errors.Is(err, store.ErrMessageRequestNotFound), errors.Is(err, store.ErrMessageRecipientMissing), errors.Is(err, store.ErrMessageNotFound):
		return server.ErrNotFound
	case errors.Is(err, store.ErrMessageRequestBusy), errors.Is(err, store.ErrMessageBusy):
		return server.ErrBusy
	case errors.Is(err, store.ErrMessageRequestClaimLost), errors.Is(err, store.ErrMessageClaimLost),
		errors.Is(err, store.ErrMessageRequestConflict), errors.Is(err, store.ErrMessageConflict):
		return server.ErrConflict
	case errors.Is(err, store.ErrMessageRequestForbidden), errors.Is(err, store.ErrMessageForbidden),
		errors.Is(err, store.ErrAgentNotFound), errors.Is(err, store.ErrAccountNotActive):
		return server.ErrForbidden
	case errors.Is(err, store.ErrAccountNotFound):
		return server.ErrNotFound
	default:
		return err
	}
}

func mapFactError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrFactInputInvalid):
		return fmt.Errorf("%w: %v", server.ErrBadInput, err)
	case errors.Is(err, store.ErrFactNotFound):
		return server.ErrNotFound
	case errors.Is(err, store.ErrFactConflict):
		return server.ErrConflict
	case errors.Is(err, store.ErrFactIdempotencyConflict):
		return server.ErrIdempotencyConflict
	case errors.Is(err, store.ErrFactDeleted):
		return server.ErrFactDeleted
	case errors.Is(err, store.ErrFactForbidden), errors.Is(err, store.ErrAccountNotActive), errors.Is(err, store.ErrAgentNotFound):
		return server.ErrForbidden
	default:
		return err
	}
}

func toStorePrincipal(p server.DomainPrincipal) store.Principal {
	return store.Principal{
		Kind:           p.Kind,
		ID:             p.ID,
		TokenID:        p.TokenID,
		AccessProfile:  p.AccessProfile,
		TokenExpiresAt: p.TokenExpiresAt,
		AccountID:      p.AccountID,
		RealmID:        p.RealmID,
		AgentName:      p.AgentName,
		RealmName:      p.RealmName,
		AccountStatus:  p.AccountStatus,
	}
}

func toServerPrincipal(p store.Principal) server.DomainPrincipal {
	return server.DomainPrincipal{
		Kind:           p.Kind,
		ID:             p.ID,
		TokenID:        p.TokenID,
		AccessProfile:  p.AccessProfile,
		TokenExpiresAt: p.TokenExpiresAt,
		AccountID:      p.AccountID,
		RealmID:        p.RealmID,
		AgentName:      p.AgentName,
		RealmName:      p.RealmName,
		AccountStatus:  p.AccountStatus,
	}
}

func toServerAgentMessage(msg store.Message) server.Message {
	return server.Message{
		ID: msg.ID, AccountID: msg.AccountID, RealmID: msg.RealmID,
		From: server.MessageAgent{
			Kind: "agent", AgentID: msg.From.ID, AgentName: msg.From.Name,
		},
		To: server.MessageRecipient{
			Kind: msg.To.Kind, AgentID: msg.To.ID, AgentName: msg.To.Name, Count: msg.To.Count,
		},
		Subject: msg.Subject, Kind: msg.Kind, Body: msg.Body, Payload: msg.Payload,
		ThreadID: msg.ThreadID, ReplyToMessageID: msg.ReplyToMessageID, CausalDepth: msg.CausalDepth,
		CreatedAt: msg.CreatedAt,
		Delivery: server.MessageDelivery{
			State: msg.Delivery.State, DeliveredAt: msg.Delivery.DeliveredAt,
		},
		ReadState: server.MessageReadState{
			State: msg.ReadState.State, ReadAt: msg.ReadState.ReadAt, AckedAt: msg.ReadState.AckedAt,
		},
		Processing: toServerMessageProcessing(msg.Processing),
	}
}

func toServerMessageRequest(request store.MessageRequest) server.MessageRequest {
	return server.MessageRequest{
		ID: request.ID, AccountID: request.AccountID, RealmID: request.RealmID,
		OpeningMessageID: request.OpeningMessageID,
		Coordinator: server.MessageAgent{
			Kind: "agent", AgentID: request.Coordinator.ID, AgentName: request.Coordinator.Name,
		},
		SelectionPolicy: request.SelectionPolicy, State: request.State, Phase: request.Phase,
		MaxAssignees: request.MaxAssignees, CandidateCount: request.CandidateCount,
		OfferCount: request.OfferCount, DeclineCount: request.DeclineCount,
		SelectedAgentIDs: request.SelectedAgentIDs, SelectionGeneration: request.SelectionGeneration,
		OfferDeadline: request.OfferDeadline, ExpiresAt: request.ExpiresAt,
		CreatedAt: request.CreatedAt, UpdatedAt: request.UpdatedAt,
		CompletedAt: request.CompletedAt, CancelledAt: request.CancelledAt, ExpiredAt: request.ExpiredAt,
	}
}

func toServerMessageRequestCandidate(candidate store.MessageRequestCandidate) server.MessageRequestCandidate {
	return server.MessageRequestCandidate{
		Agent: server.MessageAgent{
			Kind: "agent", AgentID: candidate.Agent.ID, AgentName: candidate.Agent.Name,
		},
		ResponseState: candidate.ResponseState, OfferMessageID: candidate.OfferMessageID,
		RespondedAt: candidate.RespondedAt, CreatedAt: candidate.CreatedAt,
	}
}

func toServerMessageRequestOffer(offer store.MessageRequestOffer) server.MessageRequestOffer {
	return server.MessageRequestOffer{
		Agent: server.MessageAgent{
			Kind: "agent", AgentID: offer.Agent.ID, AgentName: offer.Agent.Name,
		},
		Message: toServerAgentMessage(offer.Message), OfferedAt: offer.OfferedAt,
	}
}

func toServerMessageRequestSelection(selection store.MessageRequestSelection) server.MessageRequestSelection {
	return server.MessageRequestSelection{
		ID: selection.ID, Generation: selection.Generation,
		Coordinator: server.MessageAgent{
			Kind: "agent", AgentID: selection.Coordinator.ID, AgentName: selection.Coordinator.Name,
		},
		SelectedAgentIDs: selection.SelectedAgentIDs, CreatedAt: selection.CreatedAt,
	}
}

func toServerMessageRequestClaim(claim store.MessageRequestClaim) server.MessageRequestClaim {
	return server.MessageRequestClaim{
		ClaimID: claim.ClaimID, RequestID: claim.RequestID, SelectionID: claim.SelectionID,
		Agent: server.MessageAgent{
			Kind: "agent", AgentID: claim.Agent.ID, AgentName: claim.Agent.Name,
		},
		State: claim.State, Generation: claim.Generation, FailureCount: claim.FailureCount,
		LeaseExpiresAt: claim.LeaseExpiresAt, ResultMessageID: claim.ResultMessageID,
		SelectedAt: claim.SelectedAt, ClaimedAt: claim.ClaimedAt, ReleasedAt: claim.ReleasedAt,
		CompletedAt: claim.CompletedAt, CancelledAt: claim.CancelledAt, UpdatedAt: claim.UpdatedAt,
	}
}

func toServerMessageRequestDetail(detail store.MessageRequestDetail) server.MessageRequestDetail {
	candidates := make([]server.MessageRequestCandidate, len(detail.Candidates))
	for i, candidate := range detail.Candidates {
		candidates[i] = toServerMessageRequestCandidate(candidate)
	}
	offers := make([]server.MessageRequestOffer, len(detail.Offers))
	for i, offer := range detail.Offers {
		offers[i] = toServerMessageRequestOffer(offer)
	}
	selections := make([]server.MessageRequestSelection, len(detail.Selections))
	for i, selection := range detail.Selections {
		selections[i] = toServerMessageRequestSelection(selection)
	}
	claims := make([]server.MessageRequestClaim, len(detail.Claims))
	for i, claim := range detail.Claims {
		claims[i] = toServerMessageRequestClaim(claim)
	}
	return server.MessageRequestDetail{
		Request: toServerMessageRequest(detail.Request), OpeningMessage: toServerAgentMessage(detail.OpeningMessage),
		Candidates: candidates, Offers: offers, Selections: selections, Claims: claims,
	}
}

func toServerMessageProcessing(processing store.MessageProcessing) server.MessageProcessing {
	return server.MessageProcessing{
		State: processing.State, ClaimID: processing.ClaimID, Generation: processing.Generation,
		FailureCount:   processing.FailureCount,
		LeaseExpiresAt: processing.LeaseExpiresAt, CompletedAt: processing.CompletedAt,
		ResultMessageID: processing.ResultMessageID,
	}
}

func toServerTranscript(tr store.Transcript) server.Transcript {
	return server.Transcript{
		ID:           tr.ID,
		AccountID:    tr.AccountID,
		RealmID:      tr.RealmID,
		OwnerAgentID: tr.OwnerAgentID,
		ExternalID:   tr.ExternalID,
		Title:        tr.Title,
		Metadata:     tr.Metadata,
		CreatedAt:    tr.CreatedAt,
		UpdatedAt:    tr.UpdatedAt,
	}
}

func toServerFact(f store.Fact) server.Fact {
	return server.Fact{
		ID: f.ID, SubjectID: f.SubjectID, Subject: f.Subject,
		Predicate: f.Predicate, Cardinality: f.Cardinality, Sensitive: f.Sensitive,
		ResolvedAssertionID: f.ResolvedAssertionID, ValueType: f.ValueType, Value: f.Value,
		Recurrence: f.Recurrence,
		SourceKind: f.SourceKind, SourceRef: f.SourceRef, Confidence: f.Confidence,
		ObservedAt: f.ObservedAt, ConfirmedAt: f.ConfirmedAt, ValidFrom: f.ValidFrom,
		ValidUntil: f.ValidUntil, CreatedAt: f.CreatedAt, UpdatedAt: f.UpdatedAt,
		UsageCount: f.UsageCount, LastUsedAt: f.LastUsedAt,
	}
}

func toServerFactDeletion(result store.DeleteFactResult) server.FactDeletionReceipt {
	state := "active"
	if result.Applied {
		state = "deleted"
	}
	return server.FactDeletionReceipt{
		FactID:              result.FactID,
		ReceiptID:           result.ReceiptID,
		SubjectID:           result.SubjectID,
		Subject:             result.Subject,
		Predicate:           result.Predicate,
		Sensitive:           result.Sensitive,
		AssertionCount:      result.AssertionCount,
		CandidateCount:      result.CandidateCount,
		CandidateRevision:   result.CandidateRevision,
		UsageCount:          result.UsageCount,
		ResolvedAssertionID: result.PriorResolvedAssertionID,
		DeletionState:       state,
		DeletedAt:           result.DeletedAt,
		Applied:             result.Applied,
		Replayed:            result.Replayed,
	}
}

func toServerFactAssertion(a store.FactAssertion) server.FactAssertion {
	return server.FactAssertion{
		ID: a.ID, FactID: a.FactID, ValueType: a.ValueType, Value: a.Value,
		Recurrence: a.Recurrence,
		SourceKind: a.SourceKind, SourceRef: a.SourceRef, Confidence: a.Confidence,
		ObservedAt: a.ObservedAt, ConfirmedAt: a.ConfirmedAt, ValidFrom: a.ValidFrom,
		ValidUntil: a.ValidUntil, SupersedesID: a.SupersedesID, CreatedAt: a.CreatedAt,
	}
}

func toServerFactCandidate(c store.FactCandidate) server.FactCandidate {
	return server.FactCandidate{ID: c.ID, Subject: c.Subject, Predicate: c.Predicate, ValueType: c.ValueType, Value: c.Value, Recurrence: c.Recurrence, Cardinality: c.Cardinality, Sensitive: c.Sensitive, SourceRef: c.SourceRef, Confidence: c.Confidence, ObservedAt: c.ObservedAt, ValidFrom: c.ValidFrom, ValidUntil: c.ValidUntil, Reason: c.Reason, Status: c.Status, ConflictFactID: c.ConflictFactID, ObservedAssertionID: c.ObservedAssertionID, ResolvedFactID: c.ResolvedFactID, ProposedAt: c.ProposedAt, DecidedAt: c.DecidedAt}
}

func toServerFactSubject(s store.FactSubject) server.FactSubject {
	return server.FactSubject{
		ID:           s.ID,
		CanonicalKey: s.CanonicalKey,
		DisplayName:  s.DisplayName,
		Aliases:      s.Aliases,
		CreatedAt:    s.CreatedAt,
		UpdatedAt:    s.UpdatedAt,
	}
}

func toServerTranscriptEntry(entry store.TranscriptEntry) server.TranscriptEntry {
	return server.TranscriptEntry{
		ID:                entry.ID,
		AccountID:         entry.AccountID,
		TranscriptID:      entry.TranscriptID,
		RealmID:           entry.RealmID,
		RecordedByAgentID: entry.RecordedByAgentID,
		Sequence:          entry.Sequence,
		ExternalID:        entry.ExternalID,
		Role:              entry.Role,
		Body:              entry.Body,
		Payload:           entry.Payload,
		Model:             entry.Model,
		ReplyToEntryID:    entry.ReplyToEntryID,
		Artifacts:         entry.Artifacts,
		CreatedAt:         entry.CreatedAt,
	}
}

func toServerUsageReport(report store.UsageReport) server.UsageReport {
	points := make([]server.UsagePoint, len(report.Points))
	for i, point := range report.Points {
		points[i] = server.UsagePoint{
			Dimension: point.Dimension, Unit: point.Unit, BucketStart: point.BucketStart,
			Quantity: point.Quantity, EventCount: point.EventCount,
		}
	}
	totals := make([]server.UsageTotal, len(report.Totals))
	for i, total := range report.Totals {
		totals[i] = server.UsageTotal{
			Dimension: total.Dimension, Unit: total.Unit,
			Quantity: total.Quantity, EventCount: total.EventCount,
		}
	}
	return server.UsageReport{
		AccountID: report.AccountID, RealmID: report.RealmID, RealmName: report.RealmName,
		AgentID: report.AgentID, AgentName: report.AgentName,
		Since: report.Since, Until: report.Until, Bucket: report.Bucket,
		Points: points, Totals: totals,
	}
}
