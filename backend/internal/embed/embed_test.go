package embed

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
)

// contract checks behavior every Embedder must have.
func contract(t *testing.T, e Embedder) {
	t.Helper()
	vs, err := e.Embed(context.Background(), []string{"Oswin Pell", "the harbormaster Oswin Pell", "Thornwood forest"})
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 3 {
		t.Fatalf("want 3 vectors, got %d", len(vs))
	}
	for i, v := range vs {
		if len(v) != Dimensions {
			t.Fatalf("vector %d has %d dims", i, len(v))
		}
		var norm float64
		for _, x := range v {
			norm += float64(x) * float64(x)
		}
		if math.Abs(norm-1) > 1e-3 {
			t.Errorf("vector %d not unit length (|v|²=%f)", i, norm)
		}
	}
}

func TestHashContract(t *testing.T) { contract(t, Hash{}) }

func TestHashSimilarity(t *testing.T) {
	vs, _ := Hash{}.Embed(context.Background(), []string{"Oswin Pell", "Harbormaster Oswin Pell", "Captain Rhosyn Dace", "Oswin Pell"})
	same, related, unrelated := Cosine(vs[0], vs[3]), Cosine(vs[0], vs[1]), Cosine(vs[0], vs[2])
	if same < 0.999 {
		t.Errorf("identical text should be ~1, got %f", same)
	}
	if !(related > unrelated+0.3) {
		t.Errorf("shared name should score well above unrelated: related=%f unrelated=%f", related, unrelated)
	}
}

func TestCosineEdgeCases(t *testing.T) {
	if Cosine(nil, nil) != 0 || Cosine([]float32{1}, []float32{1, 2}) != 0 || Cosine([]float32{0, 0}, []float32{1, 0}) != 0 {
		t.Fatal("degenerate inputs must return 0")
	}
}

type fakeInvoke struct {
	mu     sync.Mutex
	inputs []titanRequest
	err    error
	dims   int
}

func (f *fakeInvoke) InvokeModel(_ context.Context, in *bedrockruntime.InvokeModelInput, _ ...func(*bedrockruntime.Options)) (*bedrockruntime.InvokeModelOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	var req titanRequest
	if err := json.Unmarshal(in.Body, &req); err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.inputs = append(f.inputs, req)
	f.mu.Unlock()

	dims := f.dims
	if dims == 0 {
		dims = req.Dimensions
	}
	v := make([]float32, dims)
	v[len(req.InputText)%dims] = 1 // unit vector that depends on the input
	body, _ := json.Marshal(titanResponse{Embedding: v})
	if aws.ToString(in.ContentType) != "application/json" {
		return nil, errors.New("wrong content type")
	}
	return &bedrockruntime.InvokeModelOutput{Body: body}, nil
}

func TestTitanContract(t *testing.T) {
	fake := &fakeInvoke{}
	contract(t, Titan{Client: fake, ModelID: DefaultTitanModel, Concurrency: 2})
	for _, in := range fake.inputs {
		if in.Dimensions != Dimensions || !in.Normalize {
			t.Errorf("titan request must ask for %d normalized dims: %+v", Dimensions, in)
		}
	}
}

func TestTitanErrors(t *testing.T) {
	if _, err := (Titan{Client: &fakeInvoke{err: errors.New("throttled")}, ModelID: "m"}).Embed(context.Background(), []string{"a"}); err == nil {
		t.Error("API errors must propagate")
	}
	if _, err := (Titan{Client: &fakeInvoke{dims: 256}, ModelID: "m"}).Embed(context.Background(), []string{"a"}); err == nil {
		t.Error("wrong dimensions must be rejected")
	}
}
