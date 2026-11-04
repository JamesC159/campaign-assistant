// Package embed turns text into vectors for similarity search (entity matching,
// Ask-the-Canon retrieval). Implementations: Titan on Bedrock (real) and Hash (offline mock).
package embed

import (
	"context"
	"math"
)

// Dimensions is the vector size stored in Postgres (vector(1024)). Every
// implementation must return vectors of exactly this length.
const Dimensions = 1024

// Embedder converts texts to unit-length vectors, one per input, in order.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// Cosine returns the cosine similarity of two vectors (1 = same direction).
// It returns 0 when either vector is zero or the lengths differ.
func Cosine(a, b []float32) float32 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return float32(dot / (math.Sqrt(na) * math.Sqrt(nb)))
}

func normalize(v []float32) []float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return v
	}
	inv := float32(1 / math.Sqrt(sum))
	for i := range v {
		v[i] *= inv
	}
	return v
}
