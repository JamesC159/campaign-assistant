package embed

import (
	"context"
	"hash/fnv"
	"strings"
	"unicode"
)

// Hash is the offline mock embedder. It hashes words and character trigrams
// into a fixed-size vector, so texts sharing spelling ("Pell" / "Oswin Pell")
// land close together. It is deterministic and needs no network, but it
// captures spelling, not meaning: "harbormaster" and "dockmaster" look unrelated.
type Hash struct{}

// Embed implements Embedder.
func (Hash) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = hashVector(t)
	}
	return out, nil
}

func hashVector(text string) []float32 {
	v := make([]float32, Dimensions)
	add := func(feature string, weight float32) {
		h := fnv.New64a()
		_, _ = h.Write([]byte(feature))
		sum := h.Sum64()
		idx := sum % Dimensions
		if sum&(1<<63) != 0 {
			weight = -weight // signed hashing reduces collision bias
		}
		v[idx] += weight
	}

	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	for _, w := range words {
		add("w:"+w, 2)
		padded := []rune(" " + w + " ")
		for i := 0; i+3 <= len(padded); i++ {
			add("t:"+string(padded[i:i+3]), 1)
		}
	}
	return normalize(v)
}
