// SPDX-License-Identifier: Apache-2.0

package policy

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	"gopkg.in/yaml.v3"
)

// Cross-session memory (design 010 §3a). The section is optional:
//
//	memory:
//	  enabled: true          # false: no writes, nothing injected
//	  inject: true           # false: entries kept and listable, never injected
//	  max_inject_bytes: 4096 # the injected block's cap; the lower value wins
//	  semantic:              # design 010 §3 semantic recall; OFF unless enabled
//	    enabled: true
//	    model: nomic-embed-text
//	    dim: 768
//	    embedder_url: http://127.0.0.1:11434  # default; a project file may only name loopback
//	    allow_remote: false  # ORG file only: lets a non-loopback https embedder in
//
// Strictest wins against an org file, and a policy that failed to load turns
// memory off (MemoryRules), like every other rule class here.

const (
	// DefaultMemoryInjectBytes matches memory.DefaultInjectBytes.
	DefaultMemoryInjectBytes = 4096
	// maxMemoryInjectBytes bounds what a policy may allow.
	maxMemoryInjectBytes = 16384
	// maxSemanticDim bounds memory.semantic.dim.
	maxSemanticDim = 8192
)

// SemanticSpec is the memory.semantic: section as written.
type SemanticSpec struct {
	Enabled     *bool  `yaml:"enabled"`
	EmbedderURL string `yaml:"embedder_url"`
	Model       string `yaml:"model"`
	Dim         int    `yaml:"dim"`
	AllowRemote *bool  `yaml:"allow_remote"`
	// AutoRecall / AutoRecallBytes: S5, per-turn recall into the prompt.
	AutoRecall      *bool `yaml:"auto_recall"`
	AutoRecallBytes int   `yaml:"auto_recall_bytes"`
}

// MemorySpec is the memory: section as written. Nil pointers / zero mean
// "not set".
type MemorySpec struct {
	Enabled        *bool         `yaml:"enabled"`
	Inject         *bool         `yaml:"inject"`
	MaxInjectBytes int           `yaml:"max_inject_bytes"`
	Semantic       *SemanticSpec `yaml:"semantic"`
	set            bool
}

// MemoryRules is the resolved memory policy.
type MemoryRules struct {
	Enabled        bool
	Inject         bool
	MaxInjectBytes int
	// Reason is set when memory is off because the policy failed to load.
	Reason string
	// Semantic is the resolved semantic-recall policy (design 010 §3).
	Semantic SemanticRules
}

// SemanticRules is the resolved memory.semantic policy. The zero value is off.
type SemanticRules struct {
	Enabled     bool
	EmbedderURL string
	Model       string
	Dim         int
	AllowRemote bool
	// AutoRecall adds the best-matching memories to each prompt (design 010
	// §3 S5); off unless a file enables it on top of semantic.
	AutoRecall      bool
	AutoRecallBytes int
}

// DefaultAutoRecallBytes caps the per-turn recall block unless policy lowers it.
const DefaultAutoRecallBytes = 1500

// maxAutoRecallBytes bounds memory.semantic.auto_recall_bytes.
const maxAutoRecallBytes = 8192

func parseMemory(node *yaml.Node) (MemorySpec, error) {
	var spec MemorySpec
	if node == nil || node.Kind == 0 {
		return spec, nil
	}
	if node.Kind != yaml.MappingNode {
		return spec, errors.New("memory: must be a mapping with enabled, inject and max_inject_bytes")
	}
	raw, err := yaml.Marshal(node)
	if err != nil {
		return spec, fmt.Errorf("memory: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&spec); err != nil {
		return MemorySpec{}, fmt.Errorf("memory: %w", err)
	}
	for _, kv := range pairs(node) {
		if kv == "max_inject_bytes" && (spec.MaxInjectBytes <= 0 || spec.MaxInjectBytes > maxMemoryInjectBytes) {
			return MemorySpec{}, fmt.Errorf("memory.max_inject_bytes must be 1..%d", maxMemoryInjectBytes)
		}
	}
	if s := spec.Semantic; s != nil {
		if s.AutoRecallBytes < 0 || s.AutoRecallBytes > maxAutoRecallBytes {
			return MemorySpec{}, fmt.Errorf("memory.semantic.auto_recall_bytes must be 1..%d", maxAutoRecallBytes)
		}
		if s.Dim < 0 || s.Dim > maxSemanticDim {
			return MemorySpec{}, fmt.Errorf("memory.semantic.dim must be 1..%d", maxSemanticDim)
		}
		if s.Enabled != nil && *s.Enabled && (strings.TrimSpace(s.Model) == "" || s.Dim == 0) {
			return MemorySpec{}, errors.New("memory.semantic: enabled needs model and dim")
		}
	}
	spec.set = true
	return spec, nil
}

// pairs lists a mapping node's keys.
func pairs(node *yaml.Node) []string {
	var keys []string
	for i := 0; i+1 < len(node.Content); i += 2 {
		keys = append(keys, node.Content[i].Value)
	}
	return keys
}

// MemoryRules resolves the memory policy. A nil policy (none loaded yet) and
// a policy with no memory: section give the defaults; a policy that failed to
// load gives memory off, with the reason.
func (p *Policy) MemoryRules() MemoryRules {
	r := MemoryRules{Enabled: true, Inject: true, MaxInjectBytes: DefaultMemoryInjectBytes}
	if p == nil {
		return r
	}
	if p.Err != nil {
		return MemoryRules{Reason: "the policy failed to load (fail-closed); fix it and run ##policy reload"}
	}
	for _, s := range []MemorySpec{p.Memory, p.OrgMemory} {
		if s.Enabled != nil && !*s.Enabled {
			r.Enabled = false
		}
		if s.Inject != nil && !*s.Inject {
			r.Inject = false
		}
		if s.MaxInjectBytes > 0 && s.MaxInjectBytes < r.MaxInjectBytes {
			r.MaxInjectBytes = s.MaxInjectBytes
		}
	}
	if !r.Enabled {
		r.Inject = false
	}
	r.Semantic = resolveSemantic(p.Memory.Semantic, p.OrgMemory.Semantic, r.Enabled)
	return r
}

// resolveSemantic: on only when a file turns it on, no file turns it off and
// memory itself is on. The org's model and dim win where it sets them.
//
// The project file ships with the repository, so it can never send memory
// text off the machine: a remote embedder needs allow_remote: true from the
// ORG file, and a project embedder_url counts only when it is loopback. The
// project file may still turn allow_remote off or pick a local port.
func resolveSemantic(project, org *SemanticSpec, memoryOn bool) SemanticRules {
	var r SemanticRules
	var on, off, remoteOff, autoOn, autoOff bool
	autoBytes := DefaultAutoRecallBytes
	orgRemote := false
	for _, s := range []*SemanticSpec{project, org} {
		if s == nil {
			continue
		}
		if s.Enabled != nil {
			on, off = on || *s.Enabled, off || !*s.Enabled
		}
		if s.AllowRemote != nil && !*s.AllowRemote {
			remoteOff = true
		}
		if s.AutoRecall != nil {
			autoOn, autoOff = autoOn || *s.AutoRecall, autoOff || !*s.AutoRecall
		}
		if s.AutoRecallBytes > 0 && s.AutoRecallBytes < autoBytes {
			autoBytes = s.AutoRecallBytes
		}
		if s.Model != "" {
			r.Model = s.Model
		}
		if s.Dim > 0 {
			r.Dim = s.Dim
		}
	}
	if project != nil && loopbackURL(project.EmbedderURL) {
		r.EmbedderURL = project.EmbedderURL
	}
	if org != nil {
		if org.EmbedderURL != "" {
			r.EmbedderURL = org.EmbedderURL
		}
		orgRemote = org.AllowRemote != nil && *org.AllowRemote
	}
	if !memoryOn || !on || off || r.Model == "" || r.Dim == 0 {
		return SemanticRules{}
	}
	r.Enabled = true
	r.AllowRemote = orgRemote && !remoteOff
	if autoOn && !autoOff {
		r.AutoRecall, r.AutoRecallBytes = true, autoBytes
	}
	return r
}

// loopbackURL reports whether u names this machine (127.0.0.0/8, ::1 or
// localhost). Anything unparsable is not loopback.
func loopbackURL(u string) bool {
	parsed, err := url.Parse(strings.TrimSpace(u))
	if err != nil || parsed.Host == "" {
		return false
	}
	h := parsed.Hostname()
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func (p *Policy) memorySummary() string {
	if !p.Memory.set && !p.OrgMemory.set {
		return ""
	}
	m := p.MemoryRules()
	return fmt.Sprintf("  memory:        enabled=%v inject=%v max_inject_bytes=%d\n", m.Enabled, m.Inject, m.MaxInjectBytes)
}
