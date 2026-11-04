// Package providers builds the AI clients for the configured PROVIDER.
package providers

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"

	"campaign-assistant/backend/internal/canon"
	"campaign-assistant/backend/internal/config"
	"campaign-assistant/backend/internal/embed"
	"campaign-assistant/backend/internal/llm"
)

// AI is the model and embedder pair the pipeline needs.
type AI struct {
	Model    llm.Model
	Embedder embed.Embedder
}

// New returns offline mocks for PROVIDER=mock and Bedrock clients for PROVIDER=aws.
// AWS credentials come from the standard chain (env, shared config, SSO, instance role).
func New(ctx context.Context, cfg config.Config) (AI, error) {
	switch cfg.Provider {
	case config.ProviderMock:
		return AI{Model: canon.HeuristicModel{}, Embedder: embed.Hash{}}, nil
	case config.ProviderAWS:
		awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
			awsconfig.WithRegion(cfg.AWSRegion),
			awsconfig.WithRetryMode(aws.RetryModeAdaptive), // backs off on Bedrock throttling
			awsconfig.WithRetryMaxAttempts(6),
		)
		if err != nil {
			return AI{}, fmt.Errorf("load AWS config: %w", err)
		}
		client := bedrockruntime.NewFromConfig(awsCfg)
		return AI{
			Model:    llm.NewBedrock(client, cfg.BedrockStrongModel, cfg.BedrockFastModel),
			Embedder: embed.Titan{Client: client, ModelID: cfg.BedrockEmbedModel},
		}, nil
	default:
		return AI{}, fmt.Errorf("unknown provider %q", cfg.Provider)
	}
}

// WithFixtures wraps the model to record responses into recordDir and/or
// replay them from replayDir (falling back to the real model on a miss when
// both are set). Empty dirs leave the model unchanged.
func WithFixtures(m llm.Model, recordDir, replayDir string) llm.Model {
	if recordDir != "" {
		m = llm.Recorder{Next: m, Dir: recordDir}
	}
	if replayDir != "" {
		var fallback llm.Model
		if recordDir != "" {
			fallback = m
		}
		m = llm.Replayer{Dir: replayDir, Fallback: fallback}
	}
	return m
}
