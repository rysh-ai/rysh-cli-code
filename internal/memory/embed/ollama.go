// SPDX-License-Identifier: Apache-2.0

// Package embed holds the embedders behind semantic memory (design 010 §3,
// slice S3). The only one is a local Ollama server, reached on loopback only:
// memory text never leaves the machine to be embedded. A run against a real
// model needs a local Ollama install and is the founder's act; the tests use
// a loopback stub.
package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/memory/vecindex"
)

// DefaultOllamaURL is Ollama's default listen address.
const DefaultOllamaURL = "http://127.0.0.1:11434"

const (
	defaultBatch    = 32
	defaultTimeout  = 60 * time.Second
	maxResponseSize = 64 << 20
)

// ErrNotLoopback: an address that is not on this machine's loopback was
// given or reached. The embedder is air-gapped by construction.
var ErrNotLoopback = errors.New("embed: not a loopback address")

type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embedResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
}

// Ollama embeds texts with a model served by a local Ollama (POST /api/embed).
type Ollama struct {
	endpoint string
	model    string
	dim      int
	batch    int
	client   *http.Client
}

var _ vecindex.Embedder = (*Ollama)(nil)

// Option configures NewOllama.
type Option func(*options)

type options struct{ allowRemote bool }

// AllowRemoteHost lifts the loopback rule for a server the user configured
// explicitly. It is never a default: memory text then leaves the machine, so
// the URL must be https with no credentials in it. Proxy and redirects stay
// off.
func AllowRemoteHost() Option { return func(o *options) { o.allowRemote = true } }

// NewOllama returns an embedder for model, whose vectors have dimension dim,
// at baseURL ("" means DefaultOllamaURL). By default baseURL must be plain
// http(s) on a loopback host (127.0.0.0/8, ::1 or localhost) with no
// credentials; anything else is ErrNotLoopback, and the dialer refuses every
// non-loopback address, so a DNS answer cannot route it off the machine
// either. AllowRemoteHost is the only way past that. The client never uses a
// proxy and follows no redirect.
func NewOllama(baseURL, model string, dim int, opts ...Option) (*Ollama, error) {
	var cfg options
	for _, opt := range opts {
		opt(&cfg)
	}
	if baseURL == "" {
		baseURL = DefaultOllamaURL
	}
	if strings.TrimSpace(model) == "" {
		return nil, errors.New("embed: model is required")
	}
	if dim <= 0 {
		return nil, fmt.Errorf("%w: dimension %d", vecindex.ErrDimension, dim)
	}
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Hostname() == "" {
		return nil, fmt.Errorf("%w: not a usable embedder URL", ErrNotLoopback)
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second, Control: loopbackOnly}
	if !loopbackHost(u.Hostname()) {
		if !cfg.allowRemote {
			return nil, fmt.Errorf("%w: the embedder only talks to a local server unless a remote host is explicitly allowed", ErrNotLoopback)
		}
		if u.Scheme != "https" {
			return nil, errors.New("embed: a remote embedder must use https")
		}
		dialer.Control = nil
	}
	client := &http.Client{
		Timeout: defaultTimeout,
		Transport: &http.Transport{
			Proxy:       nil, // HTTP(S)_PROXY would carry memory text off the machine
			DialContext: dialer.DialContext,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("embed: redirect refused")
		},
	}
	return &Ollama{
		endpoint: strings.TrimRight(u.String(), "/") + "/api/embed",
		model:    model,
		dim:      dim,
		batch:    defaultBatch,
		client:   client,
	}, nil
}

// Dim implements vecindex.Embedder.
func (o *Ollama) Dim() int { return o.dim }

// ID names the model and dimension, for the index file (vecindex.Save/Load):
// a different model or dimension forces a reindex.
func (o *Ollama) ID() string { return fmt.Sprintf("ollama:%s:%d", o.model, o.dim) }

// Embed implements vecindex.Embedder, in batches, preserving order. A vector
// of the wrong length is an error wrapping vecindex.ErrDimension. Errors never
// quote the input texts.
func (o *Ollama) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += o.batch {
		end := min(start+o.batch, len(texts))
		vecs, err := o.embedBatch(ctx, texts[start:end])
		if err != nil {
			return nil, err
		}
		out = append(out, vecs...)
	}
	return out, nil
}

func (o *Ollama) embedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	body, err := json.Marshal(embedRequest{Model: o.model, Input: texts})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed: ollama request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embed: ollama answered %s", resp.Status)
	}
	var er embedResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseSize)).Decode(&er); err != nil {
		return nil, fmt.Errorf("embed: ollama response: %w", err)
	}
	if len(er.Embeddings) != len(texts) {
		return nil, fmt.Errorf("embed: ollama returned %d vectors for %d texts", len(er.Embeddings), len(texts))
	}
	for i, v := range er.Embeddings {
		if len(v) != o.dim {
			return nil, fmt.Errorf("%w: ollama vector %d has %d components, want %d", vecindex.ErrDimension, i, len(v), o.dim)
		}
	}
	return er.Embeddings, nil
}

// loopbackHost reports whether a URL host is this machine's loopback.
func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// loopbackOnly is the dialer's Control hook: it runs on the resolved address
// just before connect, and refuses anything that is not a loopback IP.
func loopbackOnly(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNotLoopback, err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return ErrNotLoopback
	}
	return nil
}
