package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/agentemail"
)

func TestAgentEmailCellVerdictContract(t *testing.T) {
	rawContract, err := os.ReadFile("../../infra/cloudflare/agent-email/test/cell-verdict-contract.json")
	if err != nil {
		t.Fatal(err)
	}
	var contract struct {
		Schema string `json:"schema"`
		Cases  []struct {
			Name        string `json:"name"`
			Status      int    `json:"status"`
			Verdict     string `json:"verdict"`
			CellOutcome string `json:"cell_outcome"`
			Edge        string `json:"edge"`
			EdgeOutcome string `json:"edge_outcome"`
			EdgeStatus  int    `json:"edge_status"`
		} `json:"cases"`
	}
	decoder := json.NewDecoder(bytes.NewReader(rawContract))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&contract); err != nil {
		t.Fatalf("decode cell verdict contract: %v", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		t.Fatal("cell verdict contract has trailing JSON")
	}
	if contract.Schema != "witself.agent-email.cell-verdict-contract.v1" || len(contract.Cases) != 17 {
		t.Fatal("cell verdict contract schema or case count changed")
	}
	errorsByName := map[string]error{
		"accepted":                    nil,
		"accepted_attachment_omitted": ErrAgentEmailAttachmentOmitted,
		"feature_disabled":            ErrAgentEmailFeatureDisabled,
		"over_size":                   ErrAgentEmailRawSizeExceeded,
		"storage_full":                ErrAgentEmailDatabaseCapacity,
		"unknown_recipient":           ErrAgentEmailUnknownRecipient,
		"not_found":                   ErrNotFound,
		"rate_limit_impossible":       &AgentEmailRateLimitError{Retryable: false},
		"retry_canary_rejected":       ErrAgentEmailRetryCanaryPermanent,
		"rate_limited":                &AgentEmailRateLimitError{Retryable: true},
		"rate_limited_untyped":        ErrAgentEmailRateLimited,
		"receive_disabled":            ErrAgentEmailReceiveDisabled,
		"retry_canary_temporary":      ErrAgentEmailRetryCanaryTemporary,
		"receive_unavailable":         ErrAgentEmailPilotUnavailable,
		"forbidden":                   ErrForbidden,
		"unmapped_error":              context.DeadlineExceeded,
		"cohort_deferred": errors.Join(
			ErrAgentEmailCohortDeferred,
			errors.New("acc_private_identifier agent-private@example.test"),
		),
	}
	seen := make(map[string]bool, len(contract.Cases))
	for _, row := range contract.Cases {
		if _, exists := errorsByName[row.Name]; !exists || seen[row.Name] {
			t.Fatal("cell verdict contract has an unknown or duplicate case name")
		}
		seen[row.Name] = true
	}
	if len(seen) != len(errorsByName) {
		t.Fatal("cell verdict contract is missing an ingest outcome")
	}
	pilot, privateKey := testAgentEmailPilotConfig(t)
	raw := []byte("From: sender@example.test\r\nTo: pilot@example.test\r\nSubject: code\r\n\r\n123456\r\n")
	metadata := testAgentEmailRelayMetadata(raw, pilot, "pilot-key")
	for _, row := range contract.Cases {
		t.Run(row.Name, func(t *testing.T) {
			ingestErr := errorsByName[row.Name]
			handler := apiMux(Config{
				AgentEmailPilot: pilot,
				IngestAgentEmailPilot: func(context.Context, agentemail.RelayMetadata, []byte) error {
					return ingestErr
				},
			})
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, testAgentEmailIngestRequest(t, raw, metadata, privateKey))
			assertAgentEmailVerdict(t, response, row.Status, row.Verdict)
			if got := agentEmailIngestMetricOutcome(ingestErr); got != row.CellOutcome {
				t.Errorf("cell metric outcome = %q, want %q", got, row.CellOutcome)
			}
			switch row.Edge {
			case "accept", "defer", "reject", "reject_too_large":
			default:
				t.Fatal("cell verdict contract has an unknown edge disposition")
			}
			wantRetryAfter := row.Name == "rate_limited" || row.Name == "rate_limited_untyped"
			if (response.Header().Get("Retry-After") != "") != wantRetryAfter {
				t.Error("Retry-After presence differs from the contract")
			}
			if row.Name == "cohort_deferred" && (strings.Contains(response.Body.String(), "acc_private_identifier") ||
				strings.Contains(response.Body.String(), "@")) {
				t.Error("cohort deferral response exposed a private value")
			}
		})
	}
}
