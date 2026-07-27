// Package usage holds the cost-observability pricing table (design 003).
//
// Cost is computed in micro-USD (µUSD). A convenient identity keeps the table
// readable: because 1 USD = 1e6 µUSD and prices are quoted per 1e6 tokens, the
// µUSD cost of one token equals the model's USD-per-1M-tokens price. So a price
// of $3 / 1M input tokens is exactly 3 µUSD per input token.
//
// The table ships as an editable default. Prices drift; unknown models return
// (0, false) so the UI shows tokens only, flagged with a tilde, rather than
// inventing a dollar figure. Overrides are applied via SetOverrides.
package usage

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Price is the per-1M-token price (USD) for each token class, which is also the
// per-token cost in µUSD (see package doc).
type Price struct {
	In         float64 // uncached input
	Out        float64 // generated output
	CacheRead  float64 // input served from prompt cache
	CacheWrite float64 // input written to prompt cache
}

// defaultTable maps a lowercased model-name prefix to its price. Longest-prefix
// match wins, so "claude-opus-4-8" resolves against "claude-opus". Values are
// approximate public list prices at time of writing and are overridable.
var defaultTable = map[string]Price{
	// Anthropic
	"claude-opus":      {In: 15, Out: 75, CacheRead: 1.5, CacheWrite: 18.75},
	"claude-sonnet":    {In: 3, Out: 15, CacheRead: 0.3, CacheWrite: 3.75},
	"claude-haiku":     {In: 0.8, Out: 4, CacheRead: 0.08, CacheWrite: 1},
	"claude-3-5-haiku": {In: 0.8, Out: 4, CacheRead: 0.08, CacheWrite: 1},
	"claude-3-opus":    {In: 15, Out: 75, CacheRead: 1.5, CacheWrite: 18.75},
	// OpenAI
	"gpt-4o-mini":  {In: 0.15, Out: 0.6},
	"gpt-4o":       {In: 2.5, Out: 10},
	"gpt-4.1-mini": {In: 0.4, Out: 1.6},
	"gpt-4.1":      {In: 2, Out: 8},
	"o3-mini":      {In: 1.1, Out: 4.4},
	"o3":           {In: 2, Out: 8},
	// Google
	"gemini-1.5-flash": {In: 0.075, Out: 0.3},
	"gemini-1.5-pro":   {In: 1.25, Out: 5},
	"gemini-2.0-flash": {In: 0.1, Out: 0.4},
	"gemini-2.5-pro":   {In: 1.25, Out: 10},
	// Local (Ollama) — free at the wire; recorded as tokens-only spend.
	"ollama": {In: 0, Out: 0},
}

var (
	overrideMu sync.RWMutex
	overrides  = map[string]Price{}
)

// SetOverrides installs config-supplied pricing entries (keyed by lowercased
// model prefix). They take precedence over the default table. Passing nil
// clears overrides.
func SetOverrides(m map[string]Price) {
	overrideMu.Lock()
	defer overrideMu.Unlock()
	overrides = map[string]Price{}
	for k, v := range m {
		overrides[strings.ToLower(k)] = v
	}
}

// lookup returns the price for a model by longest-prefix match, checking
// overrides first, then the default table.
func lookup(model string) (Price, bool) {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return Price{}, false
	}
	overrideMu.RLock()
	ov := overrides
	overrideMu.RUnlock()

	best := ""
	var bestPrice Price
	found := false
	consider := func(table map[string]Price) {
		for prefix, price := range table {
			if strings.HasPrefix(m, prefix) && len(prefix) > len(best) {
				best, bestPrice, found = prefix, price, true
			}
		}
	}
	consider(ov)
	// Only fall back to defaults if overrides didn't match; but a longer
	// default prefix should still win, so consider both and keep the longest.
	consider(defaultTable)
	return bestPrice, found
}

// CostMicroUSD computes the µUSD cost of a call. The bool is false when the
// model is unpriced (unknown), in which case cost is 0 and the caller should
// flag the figure rather than treat 0 as free.
func CostMicroUSD(model string, in, out, cacheRead, cacheWrite int) (int64, bool) {
	p, ok := lookup(model)
	if !ok {
		return 0, false
	}
	cost := float64(in)*p.In + float64(out)*p.Out +
		float64(cacheRead)*p.CacheRead + float64(cacheWrite)*p.CacheWrite
	if cost < 0 {
		cost = 0
	}
	return int64(cost + 0.5), true
}

// FormatUSD renders a µUSD amount as a dollar string like "$1.42".
func FormatUSD(microUSD int64) string {
	return fmt.Sprintf("$%.2f", float64(microUSD)/1e6)
}

// KnownModels returns the sorted list of priced model prefixes (for docs/tests).
func KnownModels() []string {
	out := make([]string, 0, len(defaultTable))
	for k := range defaultTable {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
