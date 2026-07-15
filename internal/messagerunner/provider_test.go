package messagerunner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/client"
)

func TestCommandProviderSanitizesEnvironmentAndReturnsStrictResult(t *testing.T) {
	t.Setenv("WITSELF_TOKEN", "parent-secret")
	t.Setenv("WITSELF_TOKEN_FILE", "/private/token")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "ambient-cloud-secret")
	provider := helperCommandProvider(t, "environment")
	provider.Env = append(provider.Env, "ADAPTER_SETTING=present", "WITSELF_ENDPOINT=https://private.example")

	result, err := provider.Invoke(context.Background(), validTurnEnvelope())
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != OutcomeResult {
		t.Fatalf("outcome = %q, want result", result.Outcome)
	}
	if result.Body != "token=false token_file=false endpoint=false ambient=false session=true custom=present" {
		t.Fatalf("unexpected helper environment: %q", result.Body)
	}
}

func TestMessageProviderAmbientCredentialsAreProviderScoped(t *testing.T) {
	base := []string{
		"PATH=/bin", "CLAUDE_CODE_OAUTH_TOKEN=claude-secret", "ANTHROPIC_API_KEY=anthropic-secret",
		"XAI_API_KEY=grok-secret", "AWS_SECRET_ACCESS_KEY=cloud-secret",
	}
	claude := strings.Join(messageProviderEnvironment(providerEnvironmentClaude, base, nil), "\n")
	grok := strings.Join(messageProviderEnvironment(providerEnvironmentGrok, base, nil), "\n")
	generic := strings.Join(messageProviderEnvironment(providerEnvironmentGeneric, base, nil), "\n")
	if !strings.Contains(claude, "CLAUDE_CODE_OAUTH_TOKEN=claude-secret") ||
		!strings.Contains(claude, "ANTHROPIC_API_KEY=anthropic-secret") || strings.Contains(claude, "XAI_API_KEY") {
		t.Fatalf("claude environment = %q", claude)
	}
	if !strings.Contains(grok, "XAI_API_KEY=grok-secret") || strings.Contains(grok, "CLAUDE_CODE") || strings.Contains(grok, "ANTHROPIC") {
		t.Fatalf("grok environment = %q", grok)
	}
	if strings.Contains(generic, "secret") || strings.Contains(generic, "API_KEY") || strings.Contains(generic, "OAUTH") {
		t.Fatalf("generic environment = %q", generic)
	}
}

func TestCommandProviderStripsProcessingFence(t *testing.T) {
	provider := helperCommandProvider(t, "inspect-fence")
	envelope := validTurnEnvelope()
	envelope.Message.Processing = client.MessageProcessing{
		State: "claimed", ClaimID: "mcl_private_fence", Generation: 7,
	}
	result, err := provider.Invoke(context.Background(), envelope)
	if err != nil {
		t.Fatal(err)
	}
	if result.Body != "fence=false" {
		t.Fatalf("provider observed processing fence: %q", result.Body)
	}
}

func TestCommandProviderRejectsTrailingData(t *testing.T) {
	provider := helperCommandProvider(t, "trailing")
	_, err := provider.Invoke(context.Background(), validTurnEnvelope())
	if !errors.Is(err, ErrProviderOutputInvalid) {
		t.Fatalf("error = %v, want invalid output", err)
	}
}

func TestCommandProviderRejectsUnknownFields(t *testing.T) {
	provider := helperCommandProvider(t, "unknown-field")
	_, err := provider.Invoke(context.Background(), validTurnEnvelope())
	if !errors.Is(err, ErrProviderOutputInvalid) {
		t.Fatalf("error = %v, want invalid output", err)
	}
}

func TestCommandProviderDoesNotReturnStderr(t *testing.T) {
	provider := helperCommandProvider(t, "failure")
	_, err := provider.Invoke(context.Background(), validTurnEnvelope())
	if err == nil {
		t.Fatal("expected provider failure")
	}
	if strings.Contains(err.Error(), "message-body-secret") {
		t.Fatalf("provider error disclosed stderr: %v", err)
	}
}

func TestCommandProviderEnforcesProcessOutputLimit(t *testing.T) {
	provider := helperCommandProvider(t, "large-output")
	provider.MaxOutputBytes = 32
	_, err := provider.Invoke(context.Background(), validTurnEnvelope())
	if err == nil || !strings.Contains(err.Error(), "output exceeded") {
		t.Fatalf("error = %v, want output limit", err)
	}
}

func TestCommandProviderEnforcesProcessStderrLimit(t *testing.T) {
	provider := helperCommandProvider(t, "large-stderr")
	provider.MaxStderrBytes = 32
	_, err := provider.Invoke(context.Background(), validTurnEnvelope())
	if err == nil || !strings.Contains(err.Error(), "stderr exceeded") {
		t.Fatalf("error = %v, want stderr limit", err)
	}
}

func TestCommandProviderEnforcesAllowedOutcome(t *testing.T) {
	provider := helperCommandProvider(t, "question")
	envelope := validTurnEnvelope()
	envelope.Policy.AllowedOutcomes = []string{OutcomeResult}
	_, err := provider.Invoke(context.Background(), envelope)
	if !errors.Is(err, ErrProviderResultInvalid) {
		t.Fatalf("error = %v, want invalid result", err)
	}
}

func TestCommandProviderHonorsContextCancellation(t *testing.T) {
	provider := helperCommandProvider(t, "wait")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := provider.Invoke(ctx, validTurnEnvelope())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context canceled", err)
	}
}

func TestValidateTurnEnvelopeRequiresRecipientIdentity(t *testing.T) {
	envelope := validTurnEnvelope()
	envelope.Message.To.AgentID = "another-agent"
	if err := validateTurnEnvelope(envelope); err == nil {
		t.Fatal("expected mismatched recipient to be rejected")
	}
}

func TestLimitedBufferCapsRetainedContent(t *testing.T) {
	buffer := &limitedBuffer{limit: 4}
	n, err := buffer.Write([]byte("abcdef"))
	if err != nil || n != 6 {
		t.Fatalf("write = (%d, %v), want (6, nil)", n, err)
	}
	if got := buffer.String(); got != "abcd" || !buffer.exceeded {
		t.Fatalf("buffer = %q exceeded=%v", got, buffer.exceeded)
	}
}

func helperCommandProvider(t *testing.T, mode string) CommandProvider {
	t.Helper()
	return CommandProvider{
		Executable: os.Args[0],
		Args: []string{
			"-test.run=^TestMessageProviderHelperProcess$",
			"--",
			mode,
		},
		Env:     []string{"GO_WANT_MESSAGE_PROVIDER_HELPER=1"},
		Timeout: 5 * time.Second,
	}
}

func validTurnEnvelope() TurnEnvelope {
	return TurnEnvelope{
		Schema: TurnEnvelopeSchemaV1,
		Identity: client.SelfIdentity{
			AccountID: "account-1",
			RealmID:   "realm-1",
			AgentID:   "bob",
			AgentName: "Bob",
		},
		Message: client.Message{
			ID:        "message-1",
			AccountID: "account-1",
			RealmID:   "realm-1",
			From: client.MessageAgent{
				Kind:      "agent",
				AgentID:   "scott",
				AgentName: "Scott",
			},
			To: client.MessageRecipient{
				Kind:      "agent",
				AgentID:   "bob",
				AgentName: "Bob",
			},
			Kind: "request",
			Body: "message-body-secret",
		},
		Policy: TurnPolicy{
			AllowedOutcomes:    []string{OutcomeQuestion, OutcomeResult, OutcomeDecline, OutcomeEscalate},
			MaximumBodyBytes:   64 * 1024,
			MessageIsUntrusted: true,
			CurrentTurn:        1,
			MaximumTurns:       12,
		},
	}
}

func TestMessageProviderHelperProcess(_ *testing.T) {
	if os.Getenv("GO_WANT_MESSAGE_PROVIDER_HELPER") != "1" {
		return
	}
	mode := os.Args[len(os.Args)-1]
	result := TurnResult{Schema: TurnResultSchemaV1, Outcome: OutcomeResult, Body: "done"}
	switch mode {
	case "environment":
		result.Body = fmt.Sprintf(
			"token=%t token_file=%t endpoint=%t ambient=%t session=%t custom=%s",
			os.Getenv("WITSELF_TOKEN") != "",
			os.Getenv("WITSELF_TOKEN_FILE") != "",
			os.Getenv("WITSELF_ENDPOINT") != "",
			os.Getenv("AWS_SECRET_ACCESS_KEY") != "",
			os.Getenv("WITSELF_MESSAGE_RUNNER_SESSION") == "1",
			os.Getenv("ADAPTER_SETTING"),
		)
	case "trailing":
		encodeHelperResult(result)
		_, _ = fmt.Fprint(os.Stdout, " trailing")
		os.Exit(0)
	case "unknown-field":
		_, _ = fmt.Fprint(os.Stdout, `{"schema":"witself.message-turn-result.v1","outcome":"result","body":"done","authority":"forged"}`)
		os.Exit(0)
	case "failure":
		_, _ = fmt.Fprint(os.Stderr, "message-body-secret")
		os.Exit(2)
	case "large-output":
		_, _ = fmt.Fprint(os.Stdout, strings.Repeat("x", 4096))
		os.Exit(0)
	case "large-stderr":
		_, _ = fmt.Fprint(os.Stderr, strings.Repeat("x", 4096))
		encodeHelperResult(result)
		os.Exit(0)
	case "question":
		result.Outcome = OutcomeQuestion
		result.Body = "I need clarification"
	case "inspect-fence":
		input, err := io.ReadAll(os.Stdin)
		if err != nil {
			os.Exit(5)
		}
		result.Body = fmt.Sprintf("fence=%t", strings.Contains(string(input), "mcl_private_fence"))
	case "wait":
		time.Sleep(10 * time.Second)
	default:
		os.Exit(3)
	}
	encodeHelperResult(result)
	os.Exit(0)
}

func encodeHelperResult(result TurnResult) {
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		os.Exit(4)
	}
}
