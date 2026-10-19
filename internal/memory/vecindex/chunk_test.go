// SPDX-License-Identifier: Apache-2.0

package vecindex

import (
	"reflect"
	"strings"
	"testing"
)

// chunk builds the chunk ChunkMarkdown must produce for source.
func chunk(source, heading, text string) Chunk {
	return Chunk{ID: ChunkID(source, text), Heading: heading, Text: text}
}

func TestChunkIDIsAStableHash(t *testing.T) {
	// Pinned: the first 16 bytes of sha256("notes/a.md\x00hello world"),
	// computed outside Go with `printf 'notes/a.md\0hello world' | shasum -a 256`.
	// Persisted indexes (S2) key on this; changing it orphans every index.
	const want = "b1f4caab85aaf387d4c46d0ea00e54b2"
	if got := ChunkID("notes/a.md", "hello world"); got != want {
		t.Fatalf("ChunkID = %q, want %q", got, want)
	}
	if ChunkID("notes/b.md", "hello world") == want {
		t.Fatal("the same text in another source must get another ID")
	}
	// The separator keeps the (source, text) split unambiguous.
	if ChunkID("ab", "c") == ChunkID("a", "bc") {
		t.Fatal(`ChunkID("ab","c") == ChunkID("a","bc")`)
	}
}

func TestChunkMarkdownSplitsOnHeadings(t *testing.T) {
	doc := "intro line\n\n# Alpha\n\nalpha one.\n\nalpha two.\n## Beta\nbeta para.\n"
	got := ChunkMarkdown("d.md", doc, DefaultMaxChunk)
	want := []Chunk{
		chunk("d.md", "", "intro line"),
		chunk("d.md", "Alpha", "# Alpha\n\nalpha one.\n\nalpha two."),
		chunk("d.md", "Beta", "## Beta\n\nbeta para."),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("chunks:\n got  %q\n want %q", got, want)
	}
}

func TestChunkMarkdownSplitsALongSectionOnParagraphs(t *testing.T) {
	doc := "# H\n\naaaa aaaa\n\nbbbb bbbb\n\ncccc cccc\n"
	// "# H\n\naaaa aaaa" is 14 bytes; adding "\n\nbbbb bbbb" would make 25.
	got := ChunkMarkdown("d.md", doc, 24)
	want := []Chunk{
		chunk("d.md", "H", "# H\n\naaaa aaaa"),
		chunk("d.md", "H", "bbbb bbbb\n\ncccc cccc"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("chunks:\n got  %q\n want %q", got, want)
	}
}

func TestChunkMarkdownSplitsAnOversizedParagraph(t *testing.T) {
	para := "one two three four five six seven eight nine ten"
	got := ChunkMarkdown("d.md", para, 15)
	want := []Chunk{
		chunk("d.md", "", "one two three"),
		chunk("d.md", "", "four five six"),
		chunk("d.md", "", "seven eight"),
		chunk("d.md", "", "nine ten"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("chunks:\n got  %q\n want %q", got, want)
	}
	// A word longer than the limit is cut at a rune boundary, never inside one.
	got = ChunkMarkdown("d.md", "ééééé", 4)
	want = []Chunk{chunk("d.md", "", "éé"), chunk("d.md", "", "éé"), chunk("d.md", "", "é")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rune split:\n got  %q\n want %q", got, want)
	}
}

const fencedDoc = "# Code\n\nBefore the fence.\n\n" +
	"```go\nfunc a() {\n\n# not a heading\n\treturn // a line long enough to pass the limit\n}\n```\n\n" +
	"After the fence.\n\n" +
	"~~~\n```\nstill inside the tilde fence\n\n## also not a heading\n~~~\n"

func TestChunkMarkdownNeverSplitsAFencedCodeBlock(t *testing.T) {
	got := ChunkMarkdown("c.md", fencedDoc, 40)
	goFence := "```go\nfunc a() {\n\n# not a heading\n\treturn // a line long enough to pass the limit\n}\n```"
	tildeFence := "~~~\n```\nstill inside the tilde fence\n\n## also not a heading\n~~~"
	want := []Chunk{
		chunk("c.md", "Code", "# Code\n\nBefore the fence."),
		// Longer than 40 bytes, and still one chunk: a fence is never cut.
		chunk("c.md", "Code", goFence),
		chunk("c.md", "Code", "After the fence."),
		chunk("c.md", "Code", tildeFence),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("chunks:\n got  %q\n want %q", got, want)
	}
}

func TestChunkMarkdownKeepsAnUnclosedFenceToTheEnd(t *testing.T) {
	doc := "# A\n\n```\ncode\n\n# inside\n\nmore code\n"
	got := ChunkMarkdown("u.md", doc, 10)
	want := []Chunk{
		chunk("u.md", "A", "# A"),
		chunk("u.md", "A", "```\ncode\n\n# inside\n\nmore code"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("chunks:\n got  %q\n want %q", got, want)
	}
}

func TestChunkMarkdownIsDeterministic(t *testing.T) {
	first := ChunkMarkdown("c.md", fencedDoc, 40)
	if len(first) == 0 {
		t.Fatal("no chunks")
	}
	for i := 0; i < 50; i++ {
		if again := ChunkMarkdown("c.md", fencedDoc, 40); !reflect.DeepEqual(first, again) {
			t.Fatalf("run %d differs:\n first %q\n again %q", i, first, again)
		}
	}
	// CRLF line endings chunk exactly like LF.
	crlf := ChunkMarkdown("c.md", strings.ReplaceAll(fencedDoc, "\n", "\r\n"), 40)
	if !reflect.DeepEqual(first, crlf) {
		t.Fatalf("CRLF differs:\n LF   %q\n CRLF %q", first, crlf)
	}
	for _, c := range first {
		if c.ID != ChunkID("c.md", c.Text) {
			t.Fatalf("chunk %q has ID %q, want ChunkID(source, text)", c.Text, c.ID)
		}
	}
}

func TestChunkMarkdownOfNothingIsNoChunks(t *testing.T) {
	for _, doc := range []string{"", "\n\n", "  \n\t\n"} {
		if got := ChunkMarkdown("e.md", doc, 0); len(got) != 0 {
			t.Fatalf("ChunkMarkdown(%q) = %q, want none", doc, got)
		}
	}
	// maxBytes <= 0 means DefaultMaxChunk, not "every byte is a chunk".
	if got := ChunkMarkdown("e.md", "# T\n\nbody", 0); len(got) != 1 {
		t.Fatalf("maxBytes 0: %d chunks, want 1: %q", len(got), got)
	}
}
