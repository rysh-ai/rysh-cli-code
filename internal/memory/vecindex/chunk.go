// SPDX-License-Identifier: Apache-2.0

package vecindex

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode/utf8"
)

// DefaultMaxChunk is the chunk size limit, in bytes, used when none is given.
const DefaultMaxChunk = 1500

// ChunkID is the stable ID of text from source: the first 16 bytes of
// sha256(source + "\x00" + text), in hex. It depends on nothing else, so it is
// the same across runs, machines and releases.
func ChunkID(source, text string) string {
	h := sha256.New()
	h.Write([]byte(source))
	h.Write([]byte{0})
	h.Write([]byte(text))
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// ChunkMarkdown splits a markdown document into chunks, deterministically.
//
// Headings are hard boundaries: a chunk never spans two sections. Within a
// section the blocks (the heading line, each paragraph, each fenced code
// block) are packed in order into chunks of at most maxBytes, joined by a
// blank line. A paragraph longer than maxBytes is cut at line breaks, then at
// spaces, then at rune boundaries. A fenced code block is never cut: one
// longer than maxBytes becomes a chunk of its own, over the limit. A fence
// that is never closed runs to the end of the document. Lines inside a fence
// are not headings. maxBytes <= 0 means DefaultMaxChunk.
func ChunkMarkdown(source, text string, maxBytes int) []Chunk {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxChunk
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")

	var out []Chunk
	heading := ""
	var cur []string // blocks of the chunk being packed
	size := 0
	flush := func() {
		if len(cur) > 0 {
			t := strings.Join(cur, "\n\n")
			out = append(out, Chunk{ID: ChunkID(source, t), Heading: heading, Text: t})
		}
		cur, size = nil, 0
	}
	add := func(block string) {
		if len(cur) > 0 && size+2+len(block) > maxBytes {
			flush()
		}
		if len(cur) > 0 {
			size += 2
		}
		cur = append(cur, block)
		size += len(block)
	}

	for _, b := range markdownBlocks(text) {
		switch {
		case b.heading != "":
			flush()
			heading = b.heading
			add(b.text)
		case b.fence:
			if len(b.text) > maxBytes {
				flush()
				add(b.text)
				flush()
			} else {
				add(b.text)
			}
		default:
			for _, piece := range splitParagraph(b.text, maxBytes) {
				add(piece)
			}
		}
	}
	flush()
	return out
}

type block struct {
	text    string
	heading string // set for an ATX heading line: its text without the #s
	fence   bool
}

// markdownBlocks splits text into heading lines, paragraphs and fenced code
// blocks, in document order.
func markdownBlocks(text string) []block {
	lines := strings.Split(text, "\n")
	var out []block
	var para []string
	endPara := func() {
		if len(para) > 0 {
			out = append(out, block{text: strings.Join(para, "\n")})
			para = nil
		}
	}
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if ch, n, ok := fenceOpen(line); ok {
			endPara()
			j := i + 1
			for j < len(lines) && !fenceClose(lines[j], ch, n) {
				j++
			}
			end := j // the closing line, or len(lines) if the fence never closes
			if end == len(lines) {
				for end > i+1 && strings.TrimSpace(lines[end-1]) == "" {
					end--
				}
				out = append(out, block{text: strings.Join(lines[i:end], "\n"), fence: true})
			} else {
				out = append(out, block{text: strings.Join(lines[i:end+1], "\n"), fence: true})
			}
			i = j
			continue
		}
		if h, ok := atxHeading(line); ok {
			endPara()
			out = append(out, block{text: strings.TrimSpace(line), heading: h})
			continue
		}
		if strings.TrimSpace(line) == "" {
			endPara()
			continue
		}
		para = append(para, line)
	}
	endPara()
	return out
}

// indent3 strips up to three leading spaces; ok is false for four or more,
// which markdown reads as an indented code line rather than a fence or heading.
func indent3(line string) (string, bool) {
	for i := 0; i < 4; i++ {
		if i == len(line) || line[i] != ' ' {
			return line[i:], true
		}
	}
	return "", false
}

func fenceOpen(line string) (ch byte, n int, ok bool) {
	s, ok := indent3(line)
	if !ok || len(s) < 3 || (s[0] != '`' && s[0] != '~') {
		return 0, 0, false
	}
	ch = s[0]
	for n < len(s) && s[n] == ch {
		n++
	}
	if n < 3 || (ch == '`' && strings.IndexByte(s[n:], '`') >= 0) {
		return 0, 0, false
	}
	return ch, n, true
}

func fenceClose(line string, ch byte, n int) bool {
	s, ok := indent3(line)
	if !ok {
		return false
	}
	m := 0
	for m < len(s) && s[m] == ch {
		m++
	}
	return m >= n && strings.TrimSpace(s[m:]) == ""
}

func atxHeading(line string) (string, bool) {
	s, ok := indent3(line)
	if !ok {
		return "", false
	}
	n := 0
	for n < len(s) && s[n] == '#' {
		n++
	}
	if n == 0 || n > 6 || (n < len(s) && s[n] != ' ' && s[n] != '\t') {
		return "", false
	}
	h := strings.TrimSpace(s[n:])
	if t := strings.TrimRight(h, "#"); t != h && (t == "" || strings.HasSuffix(t, " ")) {
		h = strings.TrimSpace(t) // closing #s
	}
	if h == "" {
		h = strings.Repeat("#", n) // an empty heading still opens a section
	}
	return h, true
}

// splitParagraph cuts p into pieces of at most max bytes: at line breaks
// first, then at spaces, then at rune boundaries.
func splitParagraph(p string, max int) []string {
	if len(p) <= max {
		return []string{p}
	}
	var out []string
	cur := ""
	for _, line := range strings.Split(p, "\n") {
		for _, piece := range splitLine(line, max) {
			switch {
			case cur == "":
				cur = piece
			case len(cur)+1+len(piece) <= max:
				cur += "\n" + piece
			default:
				out = append(out, cur)
				cur = piece
			}
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func splitLine(line string, max int) []string {
	var out []string
	cur := ""
	for _, w := range strings.Fields(line) {
		for len(w) > max { // a word longer than the limit: cut at a rune boundary
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
			cut := max
			for cut > 0 && !utf8.RuneStart(w[cut]) {
				cut--
			}
			if cut == 0 { // max is smaller than one rune; take the whole rune
				_, cut = utf8.DecodeRuneInString(w)
			}
			out = append(out, w[:cut])
			w = w[cut:]
		}
		switch {
		case w == "":
		case cur == "":
			cur = w
		case len(cur)+1+len(w) <= max:
			cur += " " + w
		default:
			out = append(out, cur)
			cur = w
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
