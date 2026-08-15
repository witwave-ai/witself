package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/agentemailoutbound"
	"github.com/witwave-ai/witself/internal/store"
)

type agentEmailReceiptReplayStore interface {
	Ping(context.Context) error
	LoadAgentEmailOutboundReceiptReplay(
		context.Context,
		store.AgentEmailOutboundReceiptReplayInput,
	) (store.AgentEmailOutboundDispatch, error)
	Close()
}

type agentEmailReceiptReplayStoreOpener func(
	context.Context,
	string,
) (agentEmailReceiptReplayStore, error)

type agentEmailReceiptReplayOptions struct {
	accountID            string
	sendID               string
	expectedAcceptedAt   time.Time
	expectedAttemptCount int64
}

func runAgentEmailReceiptReplay(args []string) int {
	return runAgentEmailReceiptReplayWith(
		context.Background(),
		args,
		os.LookupEnv,
		os.Stdout,
		os.Stderr,
		func(ctx context.Context, dsn string) (agentEmailReceiptReplayStore, error) {
			return store.Open(ctx, dsn)
		},
		nil,
	)
}

func runAgentEmailReceiptReplayWith(
	ctx context.Context,
	args []string,
	lookup func(string) (string, bool),
	stdout io.Writer,
	stderr io.Writer,
	openStore agentEmailReceiptReplayStoreOpener,
	httpClient *http.Client,
) int {
	options, help, err := parseAgentEmailReceiptReplayOptions(args, stderr)
	if help {
		return 0
	}
	if err != nil {
		fmt.Fprintln(stderr, "witself-worker: invalid agent-email receipt-replay command")
		return 2
	}
	dsn := dbDSN(lookup)
	if dsn == "" {
		fmt.Fprintln(stderr, "witself-worker: WITSELF_DATABASE_URL is required (falls back to DATABASE_URL)")
		return 1
	}
	client, err := agentEmailReceiptReplayClientFromEnv(lookup, httpClient)
	if err != nil {
		fmt.Fprintln(stderr, "witself-worker: agent-email receipt-replay signing configuration is invalid")
		return 1
	}
	st, err := openStore(ctx, dsn)
	if err != nil {
		fmt.Fprintln(stderr, "witself-worker: agent-email receipt-replay database configuration is invalid")
		return 1
	}
	defer st.Close()
	if err := st.Ping(ctx); err != nil {
		fmt.Fprintln(stderr, "witself-worker: agent-email receipt-replay database is unavailable")
		return 1
	}
	source, err := st.LoadAgentEmailOutboundReceiptReplay(
		ctx,
		store.AgentEmailOutboundReceiptReplayInput{
			AccountID:            options.accountID,
			SendID:               options.sendID,
			ExpectedAcceptedAt:   options.expectedAcceptedAt,
			ExpectedAttemptCount: options.expectedAttemptCount,
		},
	)
	if err != nil {
		if errors.Is(err, store.ErrAgentEmailOutboundReceiptReplayRefused) {
			fmt.Fprintln(stderr, "witself-worker: local receipt assertions were not satisfied")
		} else {
			fmt.Fprintln(stderr, "witself-worker: local receipt read failed")
		}
		return 1
	}
	dispatch := agentEmailOutboundWireDispatch(source)
	if err := dispatch.Validate(); err != nil {
		fmt.Fprintln(stderr, "witself-worker: immutable local dispatch is invalid")
		return 1
	}
	proof, err := client.ReplayReceipt(ctx, dispatch)
	if err != nil {
		fmt.Fprintln(stderr, "witself-worker: edge receipt proof failed")
		return 1
	}
	if proof.RoutePending {
		fmt.Fprintln(stderr, "witself-worker: edge receipt route is not settled")
		return 1
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(proof); err != nil {
		fmt.Fprintln(stderr, "witself-worker: could not encode value-free receipt proof")
		return 1
	}
	return 0
}

func parseAgentEmailReceiptReplayOptions(
	args []string,
	stderr io.Writer,
) (agentEmailReceiptReplayOptions, bool, error) {
	flags := flag.NewFlagSet("agent-email receipt-replay", flag.ContinueOnError)
	// Flag parsing must never echo a malformed supplied value; it could have
	// been pasted from the wrong operator field. Help is emitted separately as
	// a fixed, value-free string.
	flags.SetOutput(io.Discard)
	var accountID, sendID, acceptedAt string
	var expectedAttemptCount int64
	var jsonOutput bool
	flags.StringVar(&accountID, "account-id", "", "exact account id")
	flags.StringVar(&sendID, "send-id", "", "exact outbound send id")
	flags.StringVar(&acceptedAt, "expected-accepted-at", "", "exact RFC3339Nano acceptance time")
	flags.Int64Var(&expectedAttemptCount, "expected-attempt-count", 0, "exact initial attempt count")
	flags.BoolVar(&jsonOutput, "json", false, "emit only the value-free JSON proof")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(stderr, "witself-worker agent-email receipt-replay --account-id ID --send-id ID --expected-accepted-at RFC3339NANO --expected-attempt-count 1 --json")
			return agentEmailReceiptReplayOptions{}, true, nil
		}
		return agentEmailReceiptReplayOptions{}, false, err
	}
	parsedAcceptedAt, err := time.Parse(time.RFC3339Nano, acceptedAt)
	if err != nil || accountID == "" || accountID != strings.TrimSpace(accountID) ||
		sendID == "" || sendID != strings.TrimSpace(sendID) ||
		acceptedAt != strings.TrimSpace(acceptedAt) || expectedAttemptCount != 1 ||
		!jsonOutput || flags.NArg() != 0 {
		return agentEmailReceiptReplayOptions{}, false, errors.New("missing or invalid assertion")
	}
	return agentEmailReceiptReplayOptions{
		accountID:            accountID,
		sendID:               sendID,
		expectedAcceptedAt:   parsedAcceptedAt,
		expectedAttemptCount: expectedAttemptCount,
	}, false, nil
}

func agentEmailReceiptReplayClientFromEnv(
	lookup func(string) (string, bool),
	httpClient *http.Client,
) (agentemailoutbound.Client, error) {
	baseEndpoint, ok := lookup(agentEmailOutboundDispatchEndpointEnv)
	if !ok || strings.TrimSpace(baseEndpoint) == "" {
		return agentemailoutbound.Client{}, errors.New("dispatch endpoint is required")
	}
	replayEndpoint, err := agentEmailReceiptReplayEndpoint(baseEndpoint)
	if err != nil {
		return agentemailoutbound.Client{}, err
	}
	keyID, ok := lookup(agentEmailOutboundDispatchKeyIDEnv)
	if !ok || strings.TrimSpace(keyID) == "" {
		return agentemailoutbound.Client{}, errors.New("dispatch key id is required")
	}
	encodedKey, ok := lookup(agentEmailOutboundDispatchPrivateKeyEnv)
	if !ok || strings.TrimSpace(encodedKey) == "" {
		return agentemailoutbound.Client{}, errors.New("dispatch private key is required")
	}
	privateKey, err := agentemailoutbound.ParsePrivateKey(encodedKey)
	if err != nil {
		return agentemailoutbound.Client{}, err
	}
	timeout := defaultAgentEmailOutboundWorkerConfig().ProviderTimeout
	if raw, ok := lookup(agentEmailOutboundProviderTimeoutEnv); ok {
		timeout, err = parseDurationEnv(agentEmailOutboundProviderTimeoutEnv, raw)
		if err != nil || timeout < time.Second || timeout > time.Minute {
			return agentemailoutbound.Client{}, errors.New("provider timeout is invalid")
		}
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: timeout}
	}
	client := agentemailoutbound.Client{
		Endpoint:   replayEndpoint,
		Audience:   agentemailoutbound.ReceiptReplayAudience,
		KeyID:      strings.TrimSpace(keyID),
		PrivateKey: privateKey,
		HTTPClient: httpClient,
	}
	if err := client.ValidateReceiptReplay(); err != nil {
		return agentemailoutbound.Client{}, err
	}
	return client, nil
}

func agentEmailReceiptReplayEndpoint(base string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(base))
	if err != nil || parsed.Path != agentemailoutbound.DispatchPath {
		return "", errors.New("dispatch endpoint is invalid")
	}
	parsed.Path = agentemailoutbound.ReceiptReplayPath
	return parsed.String(), nil
}
