package embed

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"golang.org/x/sync/errgroup"
)

// InvokeAPI is the slice of the Bedrock runtime client Titan uses.
type InvokeAPI interface {
	InvokeModel(ctx context.Context, in *bedrockruntime.InvokeModelInput, optFns ...func(*bedrockruntime.Options)) (*bedrockruntime.InvokeModelOutput, error)
}

// DefaultTitanModel is Amazon Titan Text Embeddings V2, which supports 1024 dimensions.
const DefaultTitanModel = "amazon.titan-embed-text-v2:0"

// Titan embeds text with Amazon Titan on Bedrock. Titan takes one text per
// call, so Embed fans out with bounded concurrency.
type Titan struct {
	Client      InvokeAPI
	ModelID     string
	Concurrency int // default 8
}

type titanRequest struct {
	InputText  string `json:"inputText"`
	Dimensions int    `json:"dimensions"`
	Normalize  bool   `json:"normalize"`
}

type titanResponse struct {
	Embedding []float32 `json:"embedding"`
}

// Embed implements Embedder.
func (t Titan) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	g, ctx := errgroup.WithContext(ctx)
	limit := t.Concurrency
	if limit <= 0 {
		limit = 8
	}
	g.SetLimit(limit)
	for i, text := range texts {
		g.Go(func() error {
			v, err := t.embedOne(ctx, text)
			if err != nil {
				return fmt.Errorf("embed text %d: %w", i, err)
			}
			out[i] = v
			return nil
		})
	}
	return out, g.Wait()
}

func (t Titan) embedOne(ctx context.Context, text string) ([]float32, error) {
	body, err := json.Marshal(titanRequest{InputText: text, Dimensions: Dimensions, Normalize: true})
	if err != nil {
		return nil, err
	}
	res, err := t.Client.InvokeModel(ctx, &bedrockruntime.InvokeModelInput{
		ModelId:     aws.String(t.ModelID),
		ContentType: aws.String("application/json"),
		Accept:      aws.String("application/json"),
		Body:        body,
	})
	if err != nil {
		return nil, err
	}
	var parsed titanResponse
	if err := json.Unmarshal(res.Body, &parsed); err != nil {
		return nil, fmt.Errorf("parse titan response: %w", err)
	}
	if len(parsed.Embedding) != Dimensions {
		return nil, fmt.Errorf("titan returned %d dimensions, want %d", len(parsed.Embedding), Dimensions)
	}
	return parsed.Embedding, nil
}
