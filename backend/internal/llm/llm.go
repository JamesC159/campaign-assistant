// Package llm defines the provider-neutral language-model interface used by the
// canon pipeline, plus a Bedrock implementation and fixture record/replay.
//
// Implementations: Bedrock (real), canon.HeuristicModel (offline mock),
// Recorder/Replayer (fixtures wrapping any Model).
package llm

import (
	"context"
	"encoding/json"
	"errors"
)

// Role is the author of a message.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Tier picks a model class. Accuracy-critical steps (extraction, contradiction
// checks) use Strong; cheap prose (recaps, tiebreaks) uses Fast.
type Tier string

const (
	TierStrong Tier = "strong"
	TierFast   Tier = "fast"
)

// Message is one conversation turn.
type Message struct {
	Role Role   `json:"role"`
	Text string `json:"text"`
}

// Tool is a function the model must call. Its arguments are the structured output.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"` // JSON Schema for the arguments
}

// Request is one model call.
type Request struct {
	// Purpose is a stable step name ("extract", "recap_party", ...). It selects the
	// mock behavior, groups usage metering, and namespaces fixtures.
	Purpose       string
	PromptVersion int
	Tier          Tier
	System        string
	Messages      []Message
	// Tool, when set, forces the model to call it; Response.ToolInput holds the arguments.
	Tool        *Tool
	MaxTokens   int
	Temperature *float32

	// Input is the structured data the prompt was rendered from. Mocks read it
	// instead of parsing prompt text. It is never sent to a provider.
	Input any
}

// Usage is token accounting for one or more calls.
type Usage struct {
	Calls        int `json:"calls"`
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Add returns the sum of two usages.
func (u Usage) Add(o Usage) Usage {
	return Usage{Calls: u.Calls + o.Calls, InputTokens: u.InputTokens + o.InputTokens, OutputTokens: u.OutputTokens + o.OutputTokens}
}

// Response is the model's answer.
type Response struct {
	Text       string          `json:"text,omitempty"`
	ToolInput  json.RawMessage `json:"tool_input,omitempty"`
	Model      string          `json:"model"`
	StopReason string          `json:"stop_reason"`
	Usage      Usage           `json:"usage"`
}

// Model completes requests. Implementations must be safe for concurrent use.
type Model interface {
	Complete(ctx context.Context, req Request) (Response, error)
}

// ErrTruncated means the model hit MaxTokens before finishing.
var ErrTruncated = errors.New("llm: response truncated at max tokens")
