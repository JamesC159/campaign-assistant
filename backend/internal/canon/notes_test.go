package canon

import (
	"strings"
	"testing"
)

func TestParseNotesMarksGMOnlyText(t *testing.T) {
	notes := `# Session 1

The party met Oswin.

GM: Oswin is lying.

## GM only

The cult is behind it.

### Sub-detail still under GM only

The cult meets at night.

## Loot

- A silver key
- [GM] The key is cursed
- Secret: a second key exists
`
	got := ParseNotes(notes)
	want := []struct {
		text string
		gm   bool
	}{
		{"The party met Oswin.", false},
		{"Oswin is lying.", true},
		{"The cult is behind it.", true},
		{"The cult meets at night.", true},
		{"A silver key", false},
		{"The key is cursed", true},
		{"a second key exists", true},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d paragraphs, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].Text != w.text || got[i].GMOnly != w.gm {
			t.Errorf("paragraph %d = %q (gm=%v), want %q (gm=%v)", i, got[i].Text, got[i].GMOnly, w.text, w.gm)
		}
	}
}

func TestChunkParagraphsNeverSplitsParagraphs(t *testing.T) {
	paras := []Paragraph{{Index: 0, Text: strings.Repeat("a", 60)}, {Index: 1, Text: strings.Repeat("b", 60)}, {Index: 2, Text: strings.Repeat("c", 300)}}
	chunks := ChunkParagraphs(paras, 130)
	if len(chunks) != 2 || len(chunks[0].Paragraphs) != 2 || len(chunks[1].Paragraphs) != 1 {
		t.Fatalf("unexpected chunking: %+v", chunks)
	}
}

func TestLocateQuote(t *testing.T) {
	c := Chunk{Paragraphs: []Paragraph{{Index: 0, Text: "Rhosyn’s crew   sails at dawn."}, {Index: 1, Text: "Secret plans.", GMOnly: true}}}
	if p, ok := locateQuote(c, "rhosyn's crew sails"); !ok || p.Index != 0 {
		t.Error("quote should match despite case, spacing and curly apostrophe")
	}
	if p, ok := locateQuote(c, "Secret plans"); !ok || !p.GMOnly {
		t.Error("quote in GM paragraph should report that paragraph")
	}
	if _, ok := locateQuote(c, "sails at dusk"); ok {
		t.Error("invented quote must not match")
	}
}

func TestContainsTermMatchesWholeWords(t *testing.T) {
	cases := []struct {
		text, term string
		want       bool
	}{
		{"Lady Maren Ashcombe arrived", "maren", true},
		{"The Marengo river", "maren", false},
		{"a hooded stranger watched", "Hooded Stranger", true},
		{"Pell's boat", "pell", false}, // possessive is part of the word token; NormalizeName strips it for names
		{"", "x", false},
	}
	for _, c := range cases {
		if got := containsTerm(c.text, c.term); got != c.want {
			t.Errorf("containsTerm(%q, %q) = %v, want %v", c.text, c.term, got, c.want)
		}
	}
}
