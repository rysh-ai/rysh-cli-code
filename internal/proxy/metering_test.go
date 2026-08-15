// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Metering correctness (design 001 §4.2 / §4.4).
//
// Ceilings are only as good as the meter. Each test here covers a way the meter
// used to read ZERO while the request itself succeeded — the worst possible
// failure shape, because nothing looks broken: `##cost` just under-reports and
// no budget can ever bind.

// TestMetering_GzippedResponseIsStillMetered is the Node-CLI case. Every
// Node-based agent CLI sends `Accept-Encoding: gzip, deflate` by default; the
// proxy forwarded that header verbatim, so net/http did not auto-decompress and
// the usage scan ran over gzip bytes.
func TestMetering_GzippedResponseIsStillMetered(t *testing.T) {
	var sawAcceptEncoding string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAcceptEncoding = r.Header.Get("Accept-Encoding")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		_, _ = io.WriteString(gz, `{"model":"claude-opus-4-8","usage":`+
			`{"input_tokens":1234,"output_tokens":567}}`)
		_ = gz.Close()
	}))
	defer upstream.Close()

	srv := New(nil, nil, nil, map[string]string{"anthropic": upstream.URL}, false)
	if _, err := srv.StartPrivate(""); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Stop()

	req, _ := http.NewRequest(http.MethodPost,
		srv.BaseURL()+"/anthropic/pane-1/v1/messages",
		strings.NewReader(`{"model":"claude-opus-4-8","messages":[]}`))
	// Exactly what a Node CLI sends, and what used to defeat the meter.
	req.Header.Set("Accept-Encoding", "gzip, deflate")
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	// The client still gets a readable body...
	if !strings.Contains(string(body), "output_tokens") {
		t.Fatalf("client got an unreadable body: %q", body)
	}
	// ...and, crucially, the tokens were counted.
	audits := srv.RecentAudits(10)
	if len(audits) != 1 {
		t.Fatalf("audits = %d, want 1", len(audits))
	}
	if audits[0].InTokens != 1234 || audits[0].OutTokens != 567 {
		t.Fatalf("usage = %d/%d, want 1234/567 — a compressed response metered as zero",
			audits[0].InTokens, audits[0].OutTokens)
	}
	// Non-vacuous: the upstream really was asked for (and sent) gzip.
	if !strings.Contains(sawAcceptEncoding, "gzip") {
		t.Fatalf("upstream Accept-Encoding = %q, want it to include gzip — "+
			"this test would prove nothing against an identity response", sawAcceptEncoding)
	}
}

// TestInjectStreamUsageOptions covers the rewrite in isolation, including the
// cases where NOT rewriting is the correct answer.
func TestInjectStreamUsageOptions(t *testing.T) {
	cases := []struct {
		name, path, body string
		want             string
	}{
		{
			name: "streaming chat completion gets the usage option",
			path: "v1/chat/completions",
			body: `{"model":"gpt-4o","stream":true,"messages":[]}`,
			want: `{"model":"gpt-4o","stream":true,"messages":[],"stream_options":{"include_usage":true}}`,
		},
		{
			name: "non-streaming request is untouched (usage is already reported)",
			path: "v1/chat/completions",
			body: `{"model":"gpt-4o","messages":[]}`,
			want: `{"model":"gpt-4o","messages":[]}`,
		},
		{
			name: "a caller that already asked keeps its own options",
			path: "v1/chat/completions",
			body: `{"stream":true,"stream_options":{"include_usage":false}}`,
			want: `{"stream":true,"stream_options":{"include_usage":false}}`,
		},
		{
			name: "the responses API does not accept stream_options",
			path: "v1/responses",
			body: `{"model":"gpt-4o","stream":true}`,
			want: `{"model":"gpt-4o","stream":true}`,
		},
		{
			name: "a body that is not a JSON object is left exactly as it came",
			path: "v1/chat/completions",
			body: `not json "stream": true`,
			want: `not json "stream": true`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := string(openaiDialect{}.AdaptRequestBody(c.path, []byte(c.body)))
			if got != c.want {
				t.Fatalf("got  %s\nwant %s", got, c.want)
			}
		})
	}
}

// TestMetering_StreamingOpenAIRequestAsksForUsage is the same fix observed on
// the wire, which is where it matters: the bytes the provider receives must
// carry the option, and the usage block that comes back must be counted.
func TestMetering_StreamingOpenAIRequestAsksForUsage(t *testing.T) {
	var captured string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		captured = string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		// What OpenAI sends only when include_usage was requested.
		_, _ = io.WriteString(w, "data: {\"model\":\"gpt-4o\",\"choices\":[]}\n\n"+
			"data: {\"usage\":{\"prompt_tokens\":900,\"completion_tokens\":120}}\n\n"+
			"data: [DONE]\n\n")
	}))
	defer upstream.Close()

	srv := New(nil, nil, nil, map[string]string{"openai": upstream.URL}, false)
	if _, err := srv.StartPrivate(""); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Stop()

	resp, err := http.Post(srv.BaseURL()+"/openai/pane-1/v1/chat/completions",
		"application/json",
		strings.NewReader(`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	if !strings.Contains(captured, `"stream_options":{"include_usage":true}`) {
		t.Fatalf("the option never reached the provider: %s", captured)
	}
	audits := srv.RecentAudits(10)
	if len(audits) != 1 || audits[0].InTokens != 900 || audits[0].OutTokens != 120 {
		t.Fatalf("streaming usage not metered: %+v", audits)
	}
}

// TestUsageCapture_Boundary pins the 32 KiB head/tail window. The window is
// what makes usage extractable from a long stream without buffering it, and it
// had no boundary test at all — so an off-by-one on either edge would have
// silently dropped the usage block that arrives last.
func TestUsageCapture_Boundary(t *testing.T) {
	const limit = 1 << 10

	write := func(c *usageCapture, chunks ...string) {
		for _, s := range chunks {
			if _, err := c.Write([]byte(s)); err != nil {
				t.Fatalf("write: %v", err)
			}
		}
	}

	t.Run("head keeps exactly the first limit bytes", func(t *testing.T) {
		c := &usageCapture{limit: limit}
		write(c, strings.Repeat("a", limit-1), "HEADEDGE", strings.Repeat("b", 4*limit))
		if len(c.head) != limit {
			t.Fatalf("head = %d bytes, want %d", len(c.head), limit)
		}
		// The byte that straddles the boundary is kept, the next one is not.
		if c.head[limit-1] != 'H' {
			t.Fatalf("head does not end at the boundary byte: %q", c.head[limit-4:])
		}
	})

	t.Run("tail keeps exactly the last limit bytes", func(t *testing.T) {
		c := &usageCapture{limit: limit}
		write(c, strings.Repeat("a", 10*limit), "TAILEDGE")
		if len(c.tail) != limit {
			t.Fatalf("tail = %d bytes, want %d", len(c.tail), limit)
		}
		if !strings.HasSuffix(string(c.tail), "TAILEDGE") {
			t.Fatalf("tail lost the final bytes: %q", c.tail[len(c.tail)-16:])
		}
	})

	t.Run("a short response is not duplicated into both halves", func(t *testing.T) {
		const body = `{"usage":{"input_tokens":7,"output_tokens":9}}`
		c := &usageCapture{limit: limit}
		write(c, body)
		if got := c.combined(); got != body {
			t.Fatalf("combined = %q, want the body exactly once — a repeated "+
				"window makes every occurrence count double", got)
		}
		in, out, _, _, _ := parseUsage(c.combined())
		if in != 7 || out != 9 {
			t.Fatalf("usage = %d/%d, want 7/9", in, out)
		}
	})

	t.Run("overlapping windows are stitched, not repeated", func(t *testing.T) {
		// Longer than one window, shorter than two: the halves overlap, and the
		// overlap must appear once.
		body := strings.Repeat("q", limit) + "MIDDLE" + strings.Repeat("w", limit/2)
		c := &usageCapture{limit: limit}
		write(c, body)
		if got := c.combined(); got != body {
			t.Fatalf("combined lost or repeated bytes: len=%d, want %d", len(got), len(body))
		}
	})

	t.Run("usage in the last chunk of a long stream survives", func(t *testing.T) {
		c := &usageCapture{limit: limit}
		write(c,
			`{"type":"message_start","message":{"model":"m","usage":{"input_tokens":11}}}`,
			strings.Repeat("x", 50*limit),
			`{"type":"message_delta","usage":{"output_tokens":22}}`)
		in, out, _, _, model := parseUsage(c.combined())
		if in != 11 || out != 22 || model != "m" {
			t.Fatalf("usage = in%d out%d model%q, want 11/22/m", in, out, model)
		}
	})

	t.Run("the head/tail seam cannot fabricate a token count", func(t *testing.T) {
		// Splitting a number across the seam must not read as a valid count:
		// combined() joins with a newline precisely so the two halves cannot be
		// spliced into a field that was never sent.
		c := &usageCapture{limit: 16}
		write(c, `{"output_tokens":1`, strings.Repeat("y", 64), `23456}`)
		_, out, _, _, _ := parseUsage(c.combined())
		if out != 0 {
			t.Fatalf("out = %d, want 0 — a truncated number was read as a count", out)
		}
	})

	t.Run("the real 32 KiB default", func(t *testing.T) {
		// The value ServeHTTP uses, exercised end to end so the constant and the
		// behaviour cannot drift apart.
		c := &usageCapture{limit: 32 << 10}
		head := fmt.Sprintf(`{"model":"m","usage":{"input_tokens":%d}}`, 4242)
		write(c, head, strings.Repeat("z", 1<<20), `{"usage":{"output_tokens":8484}}`)
		in, out, _, _, _ := parseUsage(c.combined())
		if in != 4242 || out != 8484 {
			t.Fatalf("usage = %d/%d over a 1 MiB stream, want 4242/8484", in, out)
		}
	})
}
