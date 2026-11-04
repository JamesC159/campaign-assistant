package canon

import (
	"regexp"
	"strings"
)

// Paragraph is one block of session notes with its visibility.
type Paragraph struct {
	Index  int    `json:"index"`
	Text   string `json:"text"`
	GMOnly bool   `json:"gm_only"`
}

// Chunk is a run of paragraphs small enough for one extraction call.
type Chunk struct {
	Index      int         `json:"index"`
	Paragraphs []Paragraph `json:"paragraphs"`
}

var (
	headingRe = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
	// A heading that opens a GM-only section: "## GM only", "### Secrets", "## GM notes".
	gmHeadingRe = regexp.MustCompile(`(?i)^(gm[\s-]*(only|notes?|secrets?)|secrets?|behind the screen)\b`)
	// A paragraph that is GM-only on its own: "GM: ...", "[GM] ...", "Secret: ...".
	gmPrefixRe = regexp.MustCompile(`(?i)^\s*(\[gm\]|gm:|secret:|\(gm\))\s*`)
)

// ParseNotes splits markdown notes into paragraphs and marks GM-only ones.
//
// GM-only rules:
//   - A heading matching "GM only", "GM notes", "Secrets" or "Behind the screen"
//     makes everything under it GM-only until a heading of the same or higher level.
//   - A paragraph starting with "GM:", "[GM]", "(GM)" or "Secret:" is GM-only.
//
// Headings themselves are not paragraphs. Everything else is party-visible.
func ParseNotes(notes string) []Paragraph {
	var (
		out     []Paragraph
		buf     []string
		gmLevel int // heading level that opened the current GM section; 0 = none
	)
	// Headings always flush first, so gmLevel at flush time is the section the buffered lines belong to.
	flush := func() {
		text := strings.TrimSpace(strings.Join(buf, "\n"))
		buf = buf[:0]
		if text == "" {
			return
		}
		gm := gmLevel > 0
		if gmPrefixRe.MatchString(text) {
			gm = true
			text = strings.TrimSpace(gmPrefixRe.ReplaceAllString(text, ""))
		}
		out = append(out, Paragraph{Index: len(out), Text: text, GMOnly: gm})
	}

	for _, line := range strings.Split(strings.ReplaceAll(notes, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if m := headingRe.FindStringSubmatch(trimmed); m != nil {
			flush()
			level := len(m[1])
			if gmLevel > 0 && level <= gmLevel {
				gmLevel = 0
			}
			if gmLevel == 0 && gmHeadingRe.MatchString(strings.TrimSpace(m[2])) {
				gmLevel = level
			}
			continue
		}
		if trimmed == "" {
			flush()
			continue
		}
		// A new bullet starts a new paragraph so each bullet carries its own GM prefix.
		if isBullet(trimmed) {
			flush()
			trimmed = strings.TrimSpace(trimmed[2:])
		}
		buf = append(buf, trimmed)
	}
	flush()
	return out
}

func isBullet(s string) bool {
	return strings.HasPrefix(s, "- ") || strings.HasPrefix(s, "* ") || strings.HasPrefix(s, "+ ")
}

// ChunkParagraphs groups paragraphs into chunks of at most maxChars (a single
// oversized paragraph becomes its own chunk). Paragraphs are never split, so a
// quote can always be traced to exactly one paragraph and its visibility.
func ChunkParagraphs(paras []Paragraph, maxChars int) []Chunk {
	var (
		out  []Chunk
		cur  []Paragraph
		size int
	)
	for _, p := range paras {
		if len(cur) > 0 && size+len(p.Text) > maxChars {
			out = append(out, Chunk{Index: len(out), Paragraphs: cur})
			cur, size = nil, 0
		}
		cur = append(cur, p)
		size += len(p.Text) + 2
	}
	if len(cur) > 0 {
		out = append(out, Chunk{Index: len(out), Paragraphs: cur})
	}
	return out
}

// PartyText joins the party-visible paragraphs.
func PartyText(paras []Paragraph) string {
	var b strings.Builder
	for _, p := range paras {
		if !p.GMOnly {
			b.WriteString(p.Text)
			b.WriteString("\n\n")
		}
	}
	return b.String()
}

// normalizeText folds case, whitespace and typographic quotes for quote matching.
func normalizeText(s string) string {
	s = strings.NewReplacer("’", "'", "‘", "'", "“", `"`, "”", `"`, "—", "-", "–", "-").Replace(s)
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

// locateQuote returns the paragraph of the chunk containing quote verbatim
// (ignoring case, whitespace and typographic quotes).
func locateQuote(c Chunk, quote string) (Paragraph, bool) {
	q := normalizeText(quote)
	if len(q) < 3 {
		return Paragraph{}, false
	}
	for _, p := range c.Paragraphs {
		if strings.Contains(normalizeText(p.Text), q) {
			return p, true
		}
	}
	return Paragraph{}, false
}

// containsTerm reports whether text mentions term as whole words (case-insensitive).
func containsTerm(text, term string) bool {
	t := normalizeText(term)
	if t == "" {
		return false
	}
	h := normalizeText(text)
	for i := 0; ; {
		j := strings.Index(h[i:], t)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(t)
		if (start == 0 || !isWordByte(h[start-1])) && (end == len(h) || !isWordByte(h[end])) {
			return true
		}
		i = start + 1
	}
}

func isWordByte(b byte) bool {
	return b == '_' || b == '\'' || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b >= 0x80
}
