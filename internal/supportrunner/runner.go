package supportrunner

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"time"
)

// ticketListLimit is the control plane's bounded per-cell maximum. Fetching
// the full bounded candidate window lets local unchanged/human-owned
// suppression scan past newer tickets instead of starving older work. The
// separate Config.MaxTicketsPerTick cap still bounds thread reads and LLM use
// globally on every tick.
const ticketListLimit = 500

// Runner polls the human support queue and performs bounded first-response
// work. It is safe for one Run call; the dark slice intentionally provides no
// distributed coordination for multiple replicas.
type Runner struct {
	config     Config
	api        ticketAPI
	model      llm
	failureLog func(ticketID string)
	now        func() time.Time

	lastSeen   map[string]string
	humanOwned map[string]struct{}
	failures   atomic.Uint64
}

// New constructs a support runner after enforcing the dark gate. No HTTP or
// model dependency is constructed when Config.Enabled is false.
func New(config Config, failureLog func(ticketID string)) (*Runner, error) {
	if !config.Enabled {
		return nil, ErrDisabled
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	config.ControlPlane = strings.TrimSpace(config.ControlPlane)
	config.Model = strings.TrimSpace(config.Model)
	return newRunner(
		config,
		httpTicketAPI{controlPlane: config.ControlPlane, adminToken: config.adminToken},
		newAnthropicLLM(config.anthropicAPIKey, config.Model),
		failureLog,
	), nil
}

func newRunner(config Config, api ticketAPI, model llm, failureLog func(string)) *Runner {
	if failureLog == nil {
		failureLog = func(string) {}
	}
	return &Runner{
		config:     config,
		api:        api,
		model:      model,
		failureLog: failureLog,
		now:        time.Now,
		lastSeen:   make(map[string]string),
		humanOwned: make(map[string]struct{}),
	}
}

// Run processes one tick immediately and then continues at Config.Interval
// until ctx is canceled. Transient ticket and model failures are counted and
// do not stop the loop.
func (r *Runner) Run(ctx context.Context) error {
	if ctx == nil {
		return errors.New("support runner context is nil")
	}
	if err := r.validate(); err != nil {
		return err
	}

	for {
		if err := r.tick(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			r.recordFailure("")
		}

		timer := time.NewTimer(r.config.Interval)
		select {
		case <-ctx.Done():
			// Do not synchronously drain after Stop reports false: in Go 1.26 a
			// timer send may be racing this branch and a blocking receive can
			// deadlock shutdown. No receiver retains the timer after return.
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

// FailureCount returns the number of fail-safe ticket or polling failures
// observed by this process.
func (r *Runner) FailureCount() uint64 {
	if r == nil {
		return 0
	}
	return r.failures.Load()
}

func (r *Runner) validate() error {
	if r == nil {
		return errors.New("support runner is nil")
	}
	if err := r.config.Validate(); err != nil {
		return err
	}
	if !r.config.Enabled {
		return ErrDisabled
	}
	if r.api == nil {
		return errors.New("support runner ticket API is nil")
	}
	if r.model == nil {
		return errors.New("support runner LLM is nil")
	}
	if r.now == nil {
		return errors.New("support runner clock is nil")
	}
	if r.lastSeen == nil || r.humanOwned == nil {
		return errors.New("support runner suppression state is nil")
	}
	return nil
}

func (r *Runner) tick(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tickets, err := r.api.List(ctx, ticketListOptions{
		States: []string{ticketStateOpen, ticketStateAwaitingAdmin},
		Since:  r.now().Add(-r.config.Lookback),
		Limit:  ticketListLimit,
	})
	if err != nil {
		return err
	}

	processed := 0
	for _, candidate := range tickets {
		if processed >= r.config.MaxTicketsPerTick {
			break
		}
		if _, skip := r.humanOwned[candidate.ID]; skip {
			continue
		}
		if lastID, seen := r.lastSeen[candidate.ID]; seen && lastID == candidate.LastMessageID {
			continue
		}
		processed++
		if lastID, completed := r.processCandidate(ctx, candidate); completed {
			r.lastSeen[candidate.ID] = lastID
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return nil
}

func (r *Runner) processCandidate(ctx context.Context, candidate ticket) (string, bool) {
	thread, err := r.api.Get(ctx, candidate.AccountID, candidate.ID)
	if err != nil {
		r.recordFailure(candidate.ID)
		return "", false
	}
	if !sameTicket(candidate, thread.Ticket) || !threadIsConsistent(thread) {
		r.recordFailure(candidate.ID)
		return "", false
	}
	lastMessageID := thread.Ticket.LastMessageID

	assistantReplies := 0
	for _, message := range thread.Messages {
		switch message.AuthorKind {
		case authorKindFleetAdmin:
			r.humanOwned[candidate.ID] = struct{}{}
			return lastMessageID, true
		case authorKindAssistant:
			assistantReplies++
		}
	}
	if assistantReplies >= r.config.MaxAssistantReplies {
		return lastMessageID, true
	}
	if !isCustomerAuthor(thread.Messages[len(thread.Messages)-1].AuthorKind) {
		return lastMessageID, true
	}

	gate := deterministicGate(thread)
	if gate.Escalate {
		if gate.Retriage != (retriage{}) {
			if err := r.api.Retriage(
				ctx, candidate.AccountID, candidate.ID, gate.Retriage,
			); err != nil {
				r.recordFailure(candidate.ID)
				return "", false
			}
		}
		return lastMessageID, true
	}

	llmCtx, cancel := context.WithTimeout(ctx, r.config.LLMTimeout)
	result, err := r.model.Decide(llmCtx, thread)
	cancel()
	if err != nil {
		if ctx.Err() == nil {
			r.recordFailure(candidate.ID)
		}
		return "", false
	}
	result, err = validateDecision(result)
	if err != nil {
		r.recordFailure(candidate.ID)
		return "", false
	}
	if result.Action == decisionActionEscalate {
		return lastMessageID, true
	}

	fresh, err := r.api.Get(ctx, candidate.AccountID, candidate.ID)
	if err != nil {
		r.recordFailure(candidate.ID)
		return "", false
	}
	if !sameTicket(candidate, fresh.Ticket) ||
		!isEligibleState(fresh.Ticket.State) ||
		fresh.Ticket.LastMessageID != thread.Ticket.LastMessageID {
		return lastMessageID, true
	}
	if err := r.api.ReplyAsAssistant(
		ctx, candidate.AccountID, candidate.ID, result.ReplyBody,
	); err != nil {
		r.recordFailure(candidate.ID)
		return "", false
	}
	if result.Retriage != (retriage{}) {
		if err := r.api.Retriage(
			ctx, candidate.AccountID, candidate.ID, result.Retriage,
		); err != nil {
			r.recordFailure(candidate.ID)
		}
	}
	return lastMessageID, true
}

func sameTicket(candidate, fetched ticket) bool {
	return candidate.ID != "" &&
		candidate.AccountID != "" &&
		fetched.ID == candidate.ID &&
		fetched.AccountID == candidate.AccountID
}

func threadIsConsistent(thread ticketThread) bool {
	if !isEligibleState(thread.Ticket.State) ||
		thread.Ticket.LastMessageID == "" ||
		len(thread.Messages) == 0 {
		return false
	}
	last := thread.Messages[len(thread.Messages)-1]
	return last.ID != "" && last.ID == thread.Ticket.LastMessageID
}

func (r *Runner) recordFailure(ticketID string) {
	r.failures.Add(1)
	r.failureLog(ticketID)
}
