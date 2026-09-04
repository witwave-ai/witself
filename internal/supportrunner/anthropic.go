package supportrunner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/witwave-ai/witself/internal/jsonstrict"
)

const anthropicMaxTokens = 4096

type anthropicLLM struct {
	client anthropic.Client
	model  string
}

func newAnthropicLLM(apiKey, model string) llm {
	return newAnthropicLLMWithOptions(apiKey, model)
}

func newAnthropicLLMWithOptions(apiKey, model string, options ...option.RequestOption) *anthropicLLM {
	options = append([]option.RequestOption{option.WithAPIKey(apiKey)}, options...)
	return &anthropicLLM{
		client: anthropic.NewClient(options...),
		model:  model,
	}
}

func (a *anthropicLLM) Decide(ctx context.Context, thread ticketThread) (decision, error) {
	prompt, err := userPrompt(thread)
	if err != nil {
		return decision{}, fmt.Errorf("build support prompt: %w", err)
	}

	schema := decisionInputSchema()
	tool := anthropic.ToolUnionParamOfTool(anthropic.ToolInputSchemaParam{
		Properties: schema["properties"],
		Required:   schema["required"].([]string),
		ExtraFields: map[string]any{
			"additionalProperties": schema["additionalProperties"],
		},
	}, decisionToolName)
	tool.OfTool.Description = anthropic.String(
		"Return the support assistant's single bounded decision for this ticket.",
	)
	tool.OfTool.Strict = anthropic.Bool(true)

	message, err := a.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(a.model),
		MaxTokens: anthropicMaxTokens,
		System:    []anthropic.TextBlockParam{{Text: systemPrompt()}},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(prompt)),
		},
		Tools:      []anthropic.ToolUnionParam{tool},
		ToolChoice: anthropic.ToolChoiceParamOfTool(decisionToolName),
	})
	if err != nil {
		return decision{}, err
	}
	if message == nil {
		return decision{}, errors.New("anthropic response is missing")
	}
	if message.StopReason == anthropic.StopReasonRefusal {
		return decision{}, errors.New("anthropic refused the support decision")
	}

	var input any
	toolUses := 0
	for _, block := range message.Content {
		value, ok := block.AsAny().(anthropic.ToolUseBlock)
		if !ok {
			return decision{}, errors.New("anthropic returned non-tool content")
		}
		if value.Name != decisionToolName {
			return decision{}, errors.New("anthropic returned an unexpected tool")
		}
		toolUses++
		input = value.Input
	}
	if toolUses != 1 {
		return decision{}, fmt.Errorf("anthropic returned %d decision tools", toolUses)
	}

	raw, err := json.Marshal(input)
	if err != nil {
		return decision{}, fmt.Errorf("marshal Anthropic tool input: %w", err)
	}
	return decodeAnthropicDecision(raw)
}

type anthropicDecisionWire struct {
	Action         *string                `json:"action"`
	ReplyBody      *string                `json:"reply_body"`
	Retriage       *anthropicRetriageWire `json:"retriage"`
	EscalateReason *string                `json:"escalate_reason"`
}

type anthropicRetriageWire struct {
	Category *string `json:"category"`
	Priority *string `json:"priority"`
}

func decodeAnthropicDecision(raw []byte) (decision, error) {
	var wire anthropicDecisionWire
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return decision{}, fmt.Errorf("decode Anthropic tool input: %w", err)
	}
	if err := jsonstrict.RequireEOF(decoder); err != nil {
		if errors.Is(err, jsonstrict.ErrTrailingValue) {
			return decision{}, errors.New("anthropic tool input has trailing JSON")
		}
		return decision{}, fmt.Errorf("decode trailing Anthropic tool input: %w", err)
	}
	if wire.Action == nil {
		return decision{}, errors.New("anthropic tool input is missing or null required field action")
	}
	if wire.ReplyBody == nil {
		return decision{}, errors.New("anthropic tool input is missing or null required field reply_body")
	}
	if wire.Retriage == nil {
		return decision{}, errors.New("anthropic tool input is missing or null required field retriage")
	}
	if wire.Retriage.Category == nil {
		return decision{}, errors.New("anthropic retriage input is missing or null required field category")
	}
	if wire.Retriage.Priority == nil {
		return decision{}, errors.New("anthropic retriage input is missing or null required field priority")
	}
	if wire.EscalateReason == nil {
		return decision{}, errors.New("anthropic tool input is missing or null required field escalate_reason")
	}
	return decision{
		Action:         *wire.Action,
		ReplyBody:      *wire.ReplyBody,
		Retriage:       retriage{Category: *wire.Retriage.Category, Priority: *wire.Retriage.Priority},
		EscalateReason: *wire.EscalateReason,
	}, nil
}
