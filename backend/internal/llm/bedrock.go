package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

// ConverseAPI is the slice of the Bedrock runtime client this package uses.
// *bedrockruntime.Client satisfies it; tests use a fake.
type ConverseAPI interface {
	Converse(ctx context.Context, in *bedrockruntime.ConverseInput, optFns ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error)
}

// Bedrock calls models through the Bedrock Converse API.
type Bedrock struct {
	client ConverseAPI
	models map[Tier]string
}

// NewBedrock maps each tier to a Bedrock model or inference-profile ID.
// Forced tool use requires a model that supports Converse tool choice "tool".
func NewBedrock(client ConverseAPI, strongModelID, fastModelID string) *Bedrock {
	return &Bedrock{client: client, models: map[Tier]string{TierStrong: strongModelID, TierFast: fastModelID}}
}

const defaultMaxTokens = 4096

// Complete implements Model.
func (b *Bedrock) Complete(ctx context.Context, req Request) (Response, error) {
	in, err := b.buildInput(req)
	if err != nil {
		return Response{}, err
	}
	out, err := b.client.Converse(ctx, in)
	if err != nil {
		return Response{}, fmt.Errorf("bedrock converse (%s): %w", req.Purpose, err)
	}
	return parseOutput(req, aws.ToString(in.ModelId), out)
}

func (b *Bedrock) buildInput(req Request) (*bedrockruntime.ConverseInput, error) {
	modelID := b.models[req.Tier]
	if modelID == "" {
		return nil, fmt.Errorf("bedrock: no model configured for tier %q", req.Tier)
	}
	if len(req.Messages) == 0 {
		return nil, errors.New("bedrock: request has no messages")
	}

	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}
	in := &bedrockruntime.ConverseInput{
		ModelId:         aws.String(modelID),
		InferenceConfig: &types.InferenceConfiguration{MaxTokens: aws.Int32(int32(maxTokens))}, //nolint:gosec // bounded by callers
	}
	if req.Temperature != nil {
		in.InferenceConfig.Temperature = req.Temperature
	}
	if req.System != "" {
		in.System = []types.SystemContentBlock{&types.SystemContentBlockMemberText{Value: req.System}}
	}
	for _, m := range req.Messages {
		role := types.ConversationRoleUser
		if m.Role == RoleAssistant {
			role = types.ConversationRoleAssistant
		}
		in.Messages = append(in.Messages, types.Message{
			Role:    role,
			Content: []types.ContentBlock{&types.ContentBlockMemberText{Value: m.Text}},
		})
	}

	if req.Tool != nil {
		var schema any
		if err := json.Unmarshal(req.Tool.Schema, &schema); err != nil {
			return nil, fmt.Errorf("bedrock: tool %q schema is not valid JSON: %w", req.Tool.Name, err)
		}
		in.ToolConfig = &types.ToolConfiguration{
			Tools: []types.Tool{&types.ToolMemberToolSpec{Value: types.ToolSpecification{
				Name:        aws.String(req.Tool.Name),
				Description: aws.String(req.Tool.Description),
				InputSchema: &types.ToolInputSchemaMemberJson{Value: document.NewLazyDocument(schema)},
			}}},
			ToolChoice: &types.ToolChoiceMemberTool{Value: types.SpecificToolChoice{Name: aws.String(req.Tool.Name)}},
		}
	}
	return in, nil
}

func parseOutput(req Request, modelID string, out *bedrockruntime.ConverseOutput) (Response, error) {
	resp := Response{Model: modelID, StopReason: string(out.StopReason), Usage: Usage{Calls: 1}}
	if u := out.Usage; u != nil {
		resp.Usage.InputTokens = int(aws.ToInt32(u.InputTokens))
		resp.Usage.OutputTokens = int(aws.ToInt32(u.OutputTokens))
	}

	msg, ok := out.Output.(*types.ConverseOutputMemberMessage)
	if !ok {
		return resp, fmt.Errorf("bedrock (%s): unexpected output type %T", req.Purpose, out.Output)
	}
	var text strings.Builder
	for _, block := range msg.Value.Content {
		switch b := block.(type) {
		case *types.ContentBlockMemberText:
			text.WriteString(b.Value)
		case *types.ContentBlockMemberToolUse:
			if req.Tool == nil || aws.ToString(b.Value.Name) != req.Tool.Name || b.Value.Input == nil {
				continue
			}
			raw, err := b.Value.Input.MarshalSmithyDocument()
			if err != nil {
				return resp, fmt.Errorf("bedrock (%s): decode tool input: %w", req.Purpose, err)
			}
			resp.ToolInput = raw
		}
	}
	resp.Text = text.String()

	if out.StopReason == types.StopReasonMaxTokens {
		return resp, fmt.Errorf("bedrock (%s): %w", req.Purpose, ErrTruncated)
	}
	if req.Tool != nil && resp.ToolInput == nil {
		return resp, fmt.Errorf("bedrock (%s): model did not call tool %q (stop reason %q)", req.Purpose, req.Tool.Name, out.StopReason)
	}
	return resp, nil
}
