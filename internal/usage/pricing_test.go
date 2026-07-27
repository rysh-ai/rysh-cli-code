package usage

import "testing"

func TestCostMicroUSD_KnownModels(t *testing.T) {
	// claude-opus: 1000 in * 15 + 100 out * 75 = 15000 + 7500 = 22500 µUSD.
	if got, ok := CostMicroUSD("claude-opus-4-8", 1000, 100, 0, 0); !ok || got != 22500 {
		t.Fatalf("claude-opus: got (%d,%v), want (22500,true)", got, ok)
	}
	// gpt-4o: 1M input * $2.5/1M = 2_500_000 µUSD = $2.50.
	if got, ok := CostMicroUSD("gpt-4o-2024-08", 1_000_000, 0, 0, 0); !ok || got != 2_500_000 {
		t.Fatalf("gpt-4o: got (%d,%v), want (2500000,true)", got, ok)
	}
}

func TestCostMicroUSD_LongestPrefixWins(t *testing.T) {
	// "gpt-4o-mini" must match the more specific entry, not "gpt-4o".
	got, ok := CostMicroUSD("gpt-4o-mini", 1_000_000, 0, 0, 0)
	if !ok || got != 150_000 { // $0.15
		t.Fatalf("gpt-4o-mini: got (%d,%v), want (150000,true)", got, ok)
	}
}

func TestCostMicroUSD_UnknownModel(t *testing.T) {
	if got, ok := CostMicroUSD("some-unlisted-model", 1000, 1000, 0, 0); ok || got != 0 {
		t.Fatalf("unknown: got (%d,%v), want (0,false)", got, ok)
	}
	if got, ok := CostMicroUSD("", 1000, 1000, 0, 0); ok || got != 0 {
		t.Fatalf("empty model: got (%d,%v), want (0,false)", got, ok)
	}
}

func TestCostMicroUSD_OllamaKnownButFree(t *testing.T) {
	// Local models are priced at 0 but KNOWN (not tilde-flagged).
	got, ok := CostMicroUSD("ollama-llama3.1", 10_000, 5_000, 0, 0)
	if !ok || got != 0 {
		t.Fatalf("ollama: got (%d,%v), want (0,true)", got, ok)
	}
}

func TestCostMicroUSD_CacheTokens(t *testing.T) {
	// claude-sonnet: cacheRead 0.3, cacheWrite 3.75 per 1M.
	got, ok := CostMicroUSD("claude-sonnet-4", 0, 0, 1_000_000, 1_000_000)
	if !ok || got != 4_050_000 { // 300000 + 3750000
		t.Fatalf("sonnet cache: got (%d,%v), want (4050000,true)", got, ok)
	}
}

func TestOverridesTakePrecedence(t *testing.T) {
	t.Cleanup(func() { SetOverrides(nil) })
	SetOverrides(map[string]Price{"claude-opus": {In: 1, Out: 1}})
	// Override wins for the overridden prefix.
	if got, _ := CostMicroUSD("claude-opus-4-8", 1000, 1000, 0, 0); got != 2000 {
		t.Fatalf("override: got %d, want 2000", got)
	}
	// Non-overridden models still use the default table.
	if got, ok := CostMicroUSD("gpt-4o", 1_000_000, 0, 0, 0); !ok || got != 2_500_000 {
		t.Fatalf("non-overridden after override: got (%d,%v)", got, ok)
	}
}

func TestFormatUSD(t *testing.T) {
	cases := map[int64]string{0: "$0.00", 1_420_000: "$1.42", 9_800_000: "$9.80", 500: "$0.00"}
	for in, want := range cases {
		if got := FormatUSD(in); got != want {
			t.Errorf("FormatUSD(%d) = %q, want %q", in, got, want)
		}
	}
}
