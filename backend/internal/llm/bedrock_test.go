package llm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

type fakeConverse struct {
	got *bedrockruntime.ConverseInput
	out *bedrockruntime.ConverseOutput
	err error
}

func (f *fakeConverse) Converse(_ context.Context, in *bedrockruntime.ConverseInput, _ ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error) {
	f.got = in
	return f.out, f.err
}

func message(blocks ...types.ContentBlock) types.ConverseOutput {
	return &types.ConverseOutputMemberMessage{Value: types.Message{Role: types.ConversationRoleAssistant, Content: blocks}}
}

var testTool = &Tool{
	Name:        "record",
	Description: "Record things",
	Schema:      json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}`),
}

func TestBedrockBuildsToolRequest(t *testing.T) {
	fake := &fakeConverse{out: &bedrockruntime.ConverseOutput{
		Output:     message(&types.ContentBlockMemberToolUse{Value: types.ToolUseBlock{Name: aws.String("record"), Input: document.NewLazyDocument(map[string]any{"n": 3})}}),
		StopReason: types.StopReasonToolUse,
		Usage:      &types.TokenUsage{InputTokens: aws.Int32(120), OutputTokens: aws.Int32(15)},
	}}
	temp := float32(0)
	b := NewBedrock(fake, "strong-model", "fast-model")

	resp, err := b.Complete(context.Background(), Request{
		Purpose: "extract", Tier: TierStrong, System: "be precise",
		Messages: []Message{{Role: RoleUser, Text: "hello"}}, Tool: testTool, Temperature: &temp,
	})
	if err != nil {
		t.Fatal(err)
	}

	in := fake.got
	if aws.ToString(in.ModelId) != "strong-model" {
		t.Errorf("model = %q", aws.ToString(in.ModelId))
	}
	if sys, ok := in.System[0].(*types.SystemContentBlockMemberText); !ok || sys.Value != "be precise" {
		t.Errorf("system block wrong: %#v", in.System)
	}
	if aws.ToInt32(in.InferenceConfig.MaxTokens) != defaultMaxTokens || aws.ToFloat32(in.InferenceConfig.Temperature) != 0 {
		t.Errorf("inference config wrong: %+v", in.InferenceConfig)
	}
	choice, ok := in.ToolConfig.ToolChoice.(*types.ToolChoiceMemberTool)
	if !ok || aws.ToString(choice.Value.Name) != "record" {
		t.Errorf("tool choice should force 'record', got %#v", in.ToolConfig.ToolChoice)
	}

	var args struct{ N int }
	if err := json.Unmarshal(resp.ToolInput, &args); err != nil || args.N != 3 {
		t.Errorf("tool input = %s (%v)", resp.ToolInput, err)
	}
	if resp.Usage != (Usage{Calls: 1, InputTokens: 120, OutputTokens: 15}) {
		t.Errorf("usage = %+v", resp.Usage)
	}
}

func TestBedrockTextResponseUsesFastTier(t *testing.T) {
	fake := &fakeConverse{out: &bedrockruntime.ConverseOutput{
		Output:     message(&types.ContentBlockMemberText{Value: "Previously, "}, &types.ContentBlockMemberText{Value: "on the coast..."}),
		StopReason: types.StopReasonEndTurn,
	}}
	resp, err := NewBedrock(fake, "strong", "fast").Complete(context.Background(), Request{
		Purpose: "recap", Tier: TierFast, Messages: []Message{{Role: RoleUser, Text: "recap"}}, MaxTokens: 500,
	})
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToString(fake.got.ModelId) != "fast" || fake.got.ToolConfig != nil || fake.got.InferenceConfig.Temperature != nil {
		t.Errorf("unexpected request: model=%s tool=%v temp=%v", aws.ToString(fake.got.ModelId), fake.got.ToolConfig, fake.got.InferenceConfig.Temperature)
	}
	if resp.Text != "Previously, on the coast..." {
		t.Errorf("text = %q", resp.Text)
	}
}

func TestBedrockErrors(t *testing.T) {
	ctx := context.Background()
	msgs := []Message{{Role: RoleUser, Text: "x"}}

	t.Run("truncated", func(t *testing.T) {
		fake := &fakeConverse{out: &bedrockruntime.ConverseOutput{Output: message(&types.ContentBlockMemberText{Value: "cut"}), StopReason: types.StopReasonMaxTokens}}
		_, err := NewBedrock(fake, "s", "f").Complete(ctx, Request{Tier: TierFast, Messages: msgs})
		if !errors.Is(err, ErrTruncated) {
			t.Fatalf("want ErrTruncated, got %v", err)
		}
	})
	t.Run("tool not called", func(t *testing.T) {
		fake := &fakeConverse{out: &bedrockruntime.ConverseOutput{Output: message(&types.ContentBlockMemberText{Value: "I refuse"}), StopReason: types.StopReasonEndTurn}}
		_, err := NewBedrock(fake, "s", "f").Complete(ctx, Request{Tier: TierStrong, Messages: msgs, Tool: testTool})
		if err == nil || !strings.Contains(err.Error(), "did not call tool") {
			t.Fatalf("want tool error, got %v", err)
		}
	})
	t.Run("missing model for tier", func(t *testing.T) {
		_, err := NewBedrock(&fakeConverse{}, "s", "").Complete(ctx, Request{Tier: TierFast, Messages: msgs})
		if err == nil || !strings.Contains(err.Error(), "no model configured") {
			t.Fatalf("want config error, got %v", err)
		}
	})
	t.Run("api error is wrapped", func(t *testing.T) {
		boom := errors.New("throttled")
		_, err := NewBedrock(&fakeConverse{err: boom}, "s", "f").Complete(ctx, Request{Tier: TierFast, Messages: msgs})
		if !errors.Is(err, boom) {
			t.Fatalf("want wrapped api error, got %v", err)
		}
	})
}
