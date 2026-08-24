package supportrunner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/option"
)

func TestAnthropicRequestShapeAndDecisionParse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected request", http.StatusNotFound)
			return
		}
		if got := r.Header.Get("X-Api-Key"); got != "test-api-key" {
			t.Errorf("X-Api-Key = %q", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		assertAnthropicRequestShape(t, body)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
  "id":"msg_1",
  "type":"message",
  "role":"assistant",
  "model":"test-model",
  "content":[{
    "type":"tool_use",
    "id":"toolu_1",
    "name":"submit_support_decision",
    "input":{
      "action":"reply",
      "reply_body":"Use the documented verification command.",
      "retriage":{"category":"technical","priority":"normal"},
      "escalate_reason":""
    }
  }],
  "stop_reason":"tool_use",
  "stop_sequence":null,
  "usage":{"input_tokens":10,"output_tokens":10}
}`))
	}))
	defer server.Close()

	model := newAnthropicLLMWithOptions(
		"test-api-key",
		"test-model",
		option.WithBaseURL(server.URL),
		option.WithMaxRetries(0),
	)
	got, err := model.Decide(context.Background(), runnerThread(
		"tkt_1",
		"acc_1",
		[]ticketMessage{{ID: "tkm_1", AuthorKind: authorKindOwner, Body: "How?"}},
	))
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	want := decision{
		Action:    decisionActionReply,
		ReplyBody: "Use the documented verification command.",
		Retriage:  retriage{Category: ticketCategoryTechnical, Priority: ticketPriorityNormal},
	}
	if got != want {
		t.Fatalf("decision = %+v, want %+v", got, want)
	}
}

func TestAnthropicRefusalFailsBeforeContentParse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
  "id":"msg_refusal",
  "type":"message",
  "role":"assistant",
  "model":"test-model",
  "content":[{"type":"text","text":"customer-controlled text must not escape"}],
  "stop_reason":"refusal",
  "stop_sequence":null,
  "usage":{"input_tokens":1,"output_tokens":1}
}`))
	}))
	defer server.Close()

	model := newAnthropicLLMWithOptions(
		"test-api-key", "test-model",
		option.WithBaseURL(server.URL), option.WithMaxRetries(0),
	)
	_, err := model.Decide(context.Background(), runnerThread(
		"tkt_1", "acc_1",
		[]ticketMessage{{ID: "tkm_1", AuthorKind: authorKindOwner, Body: "question"}},
	))
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("error = %v, want refusal", err)
	}
}

func TestAnthropicRejectsMalformedToolInput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
  "id":"msg_bad",
  "type":"message",
  "role":"assistant",
  "model":"test-model",
  "content":[{
    "type":"tool_use",
    "id":"toolu_bad",
    "name":"submit_support_decision",
    "input":{
      "action":"reply",
      "reply_body":"answer",
      "retriage":{"category":"","priority":""},
      "escalate_reason":"",
      "unexpected":"must fail closed"
    }
  }],
  "stop_reason":"tool_use",
  "stop_sequence":null,
  "usage":{"input_tokens":1,"output_tokens":1}
}`))
	}))
	defer server.Close()

	model := newAnthropicLLMWithOptions(
		"test-api-key", "test-model",
		option.WithBaseURL(server.URL), option.WithMaxRetries(0),
	)
	_, err := model.Decide(context.Background(), runnerThread(
		"tkt_1", "acc_1",
		[]ticketMessage{{ID: "tkm_1", AuthorKind: authorKindOwner, Body: "question"}},
	))
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("error = %v, want strict parse failure", err)
	}
}

func TestAnthropicRejectsMissingRequiredToolFields(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name: "top level",
			input: `{
  "action":"reply",
  "reply_body":"answer",
  "retriage":{"category":"","priority":""}
}`,
			want: "escalate_reason",
		},
		{
			name: "nested retriage",
			input: `{
  "action":"reply",
  "reply_body":"answer",
  "retriage":{"category":""},
  "escalate_reason":""
}`,
			want: "priority",
		},
		{
			name: "null top-level field",
			input: `{
  "action":null,
  "reply_body":"answer",
  "retriage":{"category":"","priority":""},
  "escalate_reason":""
}`,
			want: "action",
		},
		{
			name: "null retriage object",
			input: `{
  "action":"reply",
  "reply_body":"answer",
  "retriage":null,
  "escalate_reason":""
}`,
			want: "retriage",
		},
		{
			name: "null nested field",
			input: `{
  "action":"reply",
  "reply_body":"answer",
  "retriage":{"category":"","priority":null},
  "escalate_reason":""
}`,
			want: "priority",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{
  "id":"msg_missing",
  "type":"message",
  "role":"assistant",
  "model":"test-model",
  "content":[{
    "type":"tool_use",
    "id":"toolu_missing",
    "name":"submit_support_decision",
    "input":` + test.input + `
  }],
  "stop_reason":"tool_use",
  "stop_sequence":null,
  "usage":{"input_tokens":1,"output_tokens":1}
}`))
			}))
			defer server.Close()

			model := newAnthropicLLMWithOptions(
				"test-api-key", "test-model",
				option.WithBaseURL(server.URL), option.WithMaxRetries(0),
			)
			_, err := model.Decide(context.Background(), runnerThread(
				"tkt_1", "acc_1",
				[]ticketMessage{{ID: "tkm_1", AuthorKind: authorKindOwner, Body: "question"}},
			))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want missing %s", err, test.want)
			}
		})
	}
}

func assertAnthropicRequestShape(t *testing.T, body map[string]any) {
	t.Helper()
	if body["model"] != "test-model" || body["max_tokens"] != float64(anthropicMaxTokens) {
		t.Errorf("model/max_tokens = %#v/%#v", body["model"], body["max_tokens"])
	}
	toolChoice := objectAt(t, body, "tool_choice")
	if toolChoice["type"] != "tool" || toolChoice["name"] != decisionToolName {
		t.Errorf("tool_choice = %#v", toolChoice)
	}
	tools := arrayAt(t, body, "tools")
	if len(tools) != 1 {
		t.Fatalf("tools = %#v", tools)
	}
	tool, ok := tools[0].(map[string]any)
	if !ok {
		t.Fatalf("tool = %#v", tools[0])
	}
	if tool["name"] != decisionToolName || tool["strict"] != true {
		t.Errorf("tool identity/strict = %#v", tool)
	}
	schema := objectAt(t, tool, "input_schema")
	if schema["type"] != "object" || schema["additionalProperties"] != false {
		t.Errorf("input schema is not strict: %#v", schema)
	}
	properties := objectAt(t, schema, "properties")
	retriageSchema := objectAt(t, objectAt(t, properties, "retriage"), "properties")
	if retriageSchema["category"] == nil || retriageSchema["priority"] == nil {
		t.Errorf("retriage schema = %#v", retriageSchema)
	}
	system := arrayAt(t, body, "system")
	if len(system) != 1 {
		t.Fatalf("system blocks = %#v", system)
	}
	systemBlock, ok := system[0].(map[string]any)
	if !ok || !strings.Contains(systemBlock["text"].(string), "# Support Policy") {
		t.Errorf("system prompt does not embed support policy")
	}
	messages := arrayAt(t, body, "messages")
	if len(messages) != 1 {
		t.Fatalf("messages = %#v", messages)
	}
}

func objectAt(t *testing.T, object map[string]any, key string) map[string]any {
	t.Helper()
	value, ok := object[key].(map[string]any)
	if !ok {
		t.Fatalf("%s = %#v, want object", key, object[key])
	}
	return value
}

func arrayAt(t *testing.T, object map[string]any, key string) []any {
	t.Helper()
	value, ok := object[key].([]any)
	if !ok {
		t.Fatalf("%s = %#v, want array", key, object[key])
	}
	return value
}
