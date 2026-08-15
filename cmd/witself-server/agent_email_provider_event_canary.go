package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/witwave-ai/witself/internal/server"
	"github.com/witwave-ai/witself/internal/store"
)

const agentEmailProviderEventCanaryPath = "/v1/internal/agent-email-send:provider-event"

const agentEmailProviderEventCanaryEventIDDomain = "witself.agent-email-provider-event-canary.event-id.v1"

type agentEmailProviderEventCanaryInput struct {
	AccountID          string
	SendID             string
	ExpectedAcceptedAt time.Time
}

type agentEmailProviderEventCanaryStore interface {
	PrepareAgentEmailProviderEventCanary(
		context.Context, string, string, time.Time, string,
	) (store.AgentEmailProviderEventCanaryTarget, error)
	ApplyAgentEmailOutboundProviderEvent(
		context.Context, store.AgentEmailOutboundProviderEventInput,
	) (store.AgentEmailOutboundProviderEventResult, error)
	VerifyAgentEmailProviderEventCanary(
		context.Context, store.AgentEmailProviderEventCanaryTarget, string,
	) (store.AgentEmailProviderEventCanaryVerification, error)
}

type agentEmailProviderEventCanaryResult struct {
	SendID string `json:"send_id"`
	Status string `json:"status"`
	Count  int64  `json:"count"`
	State  string `json:"state"`
}

func parseAgentEmailProviderEventCanaryCommandArgs(
	args []string,
) (agentEmailProviderEventCanaryInput, bool) {
	if len(args) != 7 {
		return agentEmailProviderEventCanaryInput{}, false
	}
	values := make(map[string]string, 3)
	jsonSeen := false
	for index := 0; index < len(args); {
		name := args[index]
		if name == "--json" {
			if jsonSeen {
				return agentEmailProviderEventCanaryInput{}, false
			}
			jsonSeen = true
			index++
			continue
		}
		if name != "--account-id" && name != "--send-id" &&
			name != "--expected-accepted-at" || index+1 >= len(args) {
			return agentEmailProviderEventCanaryInput{}, false
		}
		value := args[index+1]
		if value == "" || value != strings.TrimSpace(value) {
			return agentEmailProviderEventCanaryInput{}, false
		}
		if _, duplicated := values[name]; duplicated {
			return agentEmailProviderEventCanaryInput{}, false
		}
		values[name] = value
		index += 2
	}
	if !jsonSeen || len(values) != 3 ||
		!validAgentEmailConfigGeneratedID(values["--account-id"], "acc") ||
		!validAgentEmailConfigGeneratedID(values["--send-id"], "esnd") {
		return agentEmailProviderEventCanaryInput{}, false
	}
	expectedAcceptedAt, err := time.Parse(
		time.RFC3339Nano, values["--expected-accepted-at"],
	)
	if err != nil || values["--expected-accepted-at"] !=
		expectedAcceptedAt.UTC().Format(time.RFC3339Nano) {
		return agentEmailProviderEventCanaryInput{}, false
	}
	return agentEmailProviderEventCanaryInput{
		AccountID: values["--account-id"], SendID: values["--send-id"],
		ExpectedAcceptedAt: expectedAcceptedAt,
	}, true
}

// runAgentEmailProviderEventCanary is an explicit operator action. It opens no
// public listener and logs no private provider correlation, token, event id, or
// message content.
func runAgentEmailProviderEventCanary(
	in agentEmailProviderEventCanaryInput,
) int {
	token, present := os.LookupEnv(agentEmailProviderEventTokenEnv)
	if !present || token != strings.TrimSpace(token) ||
		len(token) < 32 || len(token) > 4096 {
		return writeAgentEmailProviderEventCanaryFailure(os.Stderr, "invalid_configuration")
	}
	dsn := dbDSN()
	if strings.TrimSpace(dsn) == "" {
		return writeAgentEmailProviderEventCanaryFailure(os.Stderr, "invalid_configuration")
	}

	ctx, stop := signal.NotifyContext(
		context.Background(), syscall.SIGINT, syscall.SIGTERM,
	)
	defer stop()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		return writeAgentEmailProviderEventCanaryFailure(os.Stderr, "database_unavailable")
	}
	defer st.Close()
	if err := st.Ping(ctx); err != nil {
		return writeAgentEmailProviderEventCanaryFailure(os.Stderr, "database_unavailable")
	}
	return runAgentEmailProviderEventCanaryWithDependencies(
		ctx, in, token, st, os.Stdout, os.Stderr,
		nil,
	)
}

func runAgentEmailProviderEventCanaryWithDependencies(
	ctx context.Context,
	in agentEmailProviderEventCanaryInput,
	token string,
	st agentEmailProviderEventCanaryStore,
	stdout, stderr io.Writer,
	listen func(network, address string) (net.Listener, error),
) int {
	if len(token) < 32 || len(token) > 4096 || token != strings.TrimSpace(token) {
		return writeAgentEmailProviderEventCanaryFailure(stderr, "invalid_configuration")
	}
	eventID := agentEmailProviderEventCanaryEventID(in)
	target, err := st.PrepareAgentEmailProviderEventCanary(
		ctx, in.AccountID, in.SendID, in.ExpectedAcceptedAt, eventID,
	)
	if err != nil {
		return writeAgentEmailProviderEventCanaryFailure(stderr, "preflight_failed")
	}

	exactBody, changedBody, err :=
		agentEmailProviderEventCanaryRequestBodies(in, target, eventID)
	if err != nil {
		return writeAgentEmailProviderEventCanaryFailure(stderr, "request_encoding_failed")
	}

	handler, err := server.AgentEmailOutboundProviderEventHTTPHandler(
		token,
		func(ctx context.Context, event server.AgentEmailOutboundProviderEvent) error {
			_, applyErr := st.ApplyAgentEmailOutboundProviderEvent(
				ctx, store.AgentEmailOutboundProviderEventInput{
					Provider: store.AgentEmailOutboundCloudflareProvider,
					EventID:  event.EventID, ProviderMessageID: event.ProviderMessageID,
					EventClass: event.EventClass, OccurredAt: event.OccurredAt,
				},
			)
			return mapAgentEmailOutboundProviderEventError(applyErr)
		},
	)
	if err != nil {
		return writeAgentEmailProviderEventCanaryFailure(stderr, "handler_setup_failed")
	}
	if listen == nil {
		listen = net.Listen
	}
	listener, err := listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return writeAgentEmailProviderEventCanaryFailure(stderr, "listener_failed")
	}
	if address, ok := listener.Addr().(*net.TCPAddr); !ok ||
		address.IP == nil || !address.IP.IsLoopback() {
		_ = listener.Close()
		return writeAgentEmailProviderEventCanaryFailure(stderr, "listener_failed")
	}

	httpServer := &http.Server{
		Handler: handler, ReadHeaderTimeout: 2 * time.Second,
	}
	serveDone := make(chan error, 1)
	go func() {
		serveErr := httpServer.Serve(listener)
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
		serveDone <- serveErr
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
		select {
		case <-serveDone:
		case <-shutdownCtx.Done():
		}
	}()

	client := newAgentEmailProviderEventCanaryHTTPClient()
	defer client.CloseIdleConnections()
	targetURL := "http://" + listener.Addr().String() + agentEmailProviderEventCanaryPath
	for _, request := range []struct {
		body       []byte
		wantStatus int
	}{
		{body: exactBody, wantStatus: http.StatusNoContent},
		{body: exactBody, wantStatus: http.StatusNoContent},
		{body: changedBody, wantStatus: http.StatusConflict},
	} {
		status, postErr := postAgentEmailProviderEventCanary(
			ctx, client, targetURL, token, request.body,
		)
		if postErr != nil || status != request.wantStatus {
			return writeAgentEmailProviderEventCanaryFailure(stderr, "http_probe_failed")
		}
	}

	verification, err := st.VerifyAgentEmailProviderEventCanary(
		ctx, target, eventID,
	)
	if err != nil || verification.SendID != in.SendID ||
		verification.State != store.AgentEmailOutboundDelivered ||
		verification.ProviderEventReceiptCount != 1 ||
		verification.EmailSentUsageEventCount != 1 {
		return writeAgentEmailProviderEventCanaryFailure(stderr, "verification_failed")
	}
	result := agentEmailProviderEventCanaryResult{
		SendID: in.SendID, Status: "passed", Count: 1,
		State: verification.State,
	}
	if err := json.NewEncoder(stdout).Encode(result); err != nil {
		return writeAgentEmailProviderEventCanaryFailure(stderr, "result_encoding_failed")
	}
	return 0
}

func newAgentEmailProviderEventCanaryHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{Proxy: nil},
		Timeout:   10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("provider-event canary redirects are disabled")
		},
	}
}

func postAgentEmailProviderEventCanary(
	ctx context.Context,
	client *http.Client,
	targetURL, token string,
	body []byte,
) (int, error) {
	request, err := http.NewRequestWithContext(
		ctx, http.MethodPost, targetURL, bytes.NewReader(body),
	)
	if err != nil {
		return 0, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return 0, err
	}
	defer func() { _ = response.Body.Close() }()
	_, copyErr := io.Copy(io.Discard, io.LimitReader(response.Body, 32*1024+1))
	return response.StatusCode, copyErr
}

func agentEmailProviderEventCanaryEventID(
	in agentEmailProviderEventCanaryInput,
) string {
	canonical := agentEmailProviderEventCanaryEventIDDomain + "\x00" +
		in.AccountID + "\x00" + in.SendID + "\x00" +
		in.ExpectedAcceptedAt.UTC().Format(time.RFC3339Nano)
	digest := sha256.Sum256([]byte(canonical))
	return "witself-canary-" + hex.EncodeToString(digest[:])
}

func agentEmailProviderEventCanaryRequestBodies(
	in agentEmailProviderEventCanaryInput,
	target store.AgentEmailProviderEventCanaryTarget,
	eventID string,
) ([]byte, []byte, error) {
	if eventID != agentEmailProviderEventCanaryEventID(in) {
		return nil, nil, errors.New("provider-event canary identity mismatch")
	}
	event := server.AgentEmailOutboundProviderEvent{
		SchemaVersion: server.AgentEmailOutboundProviderEventSchema,
		EventID:       eventID, ProviderMessageID: target.ProviderMessageID,
		EventClass: store.AgentEmailOutboundProviderEventDelivered,
		OccurredAt: in.ExpectedAcceptedAt.UTC(),
	}
	exactBody, err := json.Marshal(event)
	if err != nil {
		return nil, nil, err
	}
	changedEvent := event
	changedEvent.EventClass = store.AgentEmailOutboundProviderEventDeferred
	changedBody, err := json.Marshal(changedEvent)
	if err != nil {
		return nil, nil, err
	}
	return exactBody, changedBody, nil
}

func writeAgentEmailProviderEventCanaryFailure(w io.Writer, reason string) int {
	_, _ = fmt.Fprintf(
		w, "witself-server: agent-email provider-event canary failed (reason=%s)\n",
		reason,
	)
	return 1
}
