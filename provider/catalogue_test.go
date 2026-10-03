package provider

import (
	"errors"
	"testing"
	"time"
)

func catalogueFixtures() []ModelRef {
	return []ModelRef{
		{Model: "free-working", Provider: "openrouter", PriceKnown: true, Free: true, Availability: ModelAvailable},
		{Model: "free-unknown", Provider: "openrouter", PriceKnown: true, Free: true, Availability: ModelUnknown},
		{Model: "free-limited", Provider: "openrouter", PriceKnown: true, Free: true, Availability: ModelLimited},
		{Model: "paid-working", Provider: "groq", PriceKnown: true, Availability: ModelAvailable},
		{Model: "price-unknown", Provider: "local", Availability: ModelAvailable},
		{Model: "paid-unknown", Provider: "groq", PriceKnown: true, Availability: ModelUnknown},
		{Model: "paid-limited", Provider: "groq", PriceKnown: true, Availability: ModelLimited},
		{Model: "free-unavailable", Provider: "openrouter", PriceKnown: true, Free: true, Availability: ModelUnavailable},
	}
}

func modelOrder(refs []ModelRef) []string {
	out := make([]string, len(refs))
	for i, ref := range refs {
		out[i] = ref.Model
	}

	return out
}

func equalOrder(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}

	return true
}

// TestSortGroupsByAvailabilityAndPrice pins the availability ordering: free
// and working first, unavailable last, whatever the price.
func TestSortGroupsByAvailabilityAndPrice(t *testing.T) {
	got := modelOrder(Sort(catalogueFixtures(), "", nil))

	want := []string{
		"free-working",
		"free-unknown",
		"free-limited",
		"paid-working",
		"price-unknown",
		"paid-unknown",
		"paid-limited",
		"free-unavailable",
	}

	if !equalOrder(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

// TestSortOrdersInsideAGroup covers the relevance rules: what the user runs
// now, what they bookmarked, observed health and latency.
func TestSortOrdersInsideAGroup(t *testing.T) {
	refs := []ModelRef{
		{Model: "aaa", PriceKnown: true, Free: true, Availability: ModelAvailable, Failures: 3},
		{Model: "bbb", PriceKnown: true, Free: true, Availability: ModelAvailable, Latency: 10 * time.Second},
		{Model: "ccc", PriceKnown: true, Free: true, Availability: ModelAvailable},
		{Model: "ddd", PriceKnown: true, Free: true, Availability: ModelAvailable},
		{Model: "eee", PriceKnown: true, Free: true, Availability: ModelAvailable, Latency: 200 * time.Millisecond},
	}

	got := modelOrder(Sort(refs, "ccc", []string{"ddd"}))
	want := []string{"ccc", "ddd", "eee", "bbb", "aaa"}

	if !equalOrder(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

// TestSortIsStable keeps the picker order reproducible, which is what makes a
// model button's index safe to resolve later.
func TestSortIsStable(t *testing.T) {
	refs := catalogueFixtures()
	first := modelOrder(Sort(refs, "", nil))
	second := modelOrder(Sort(refs, "", nil))

	if !equalOrder(first, second) {
		t.Errorf("the order changed between runs: %v vs %v", first, second)
	}
}

func filterFixtures() []ModelRef {
	return []ModelRef{
		{
			Model: "qwen/qwen3-coder:free", Provider: "openrouter", DisplayName: "Qwen3 Coder",
			PriceKnown: true, Free: true, Coding: true, ContextLength: 262144, Availability: ModelAvailable,
		},
		{
			Model: "gemini-2.5-flash", Provider: "google",
			PriceKnown: true, Vision: true, Fast: true, ContextLength: 1048576, Availability: ModelUnknown,
		},
		{
			Model: "deepseek-r1", Provider: "openrouter",
			PriceKnown: true, Reasoning: true, Availability: ModelLimited,
		},
	}
}

// TestFilterCoversEveryToggle checks the compact inline filters.
func TestFilterCoversEveryToggle(t *testing.T) {
	refs := filterFixtures()

	cases := []struct {
		name   string
		filter ModelFilter
		want   []string
	}{
		{"free", ModelFilter{Free: true}, []string{"qwen/qwen3-coder:free"}},
		{"working", ModelFilter{Working: true}, []string{"qwen/qwen3-coder:free"}},
		{"fast", ModelFilter{Fast: true}, []string{"gemini-2.5-flash"}},
		{"reasoning", ModelFilter{Reasoning: true}, []string{"deepseek-r1"}},
		{"coding", ModelFilter{Coding: true}, []string{"qwen/qwen3-coder:free"}},
		{"vision", ModelFilter{Vision: true}, []string{"gemini-2.5-flash"}},
		{"long context", ModelFilter{LongContext: true}, []string{"qwen/qwen3-coder:free", "gemini-2.5-flash"}},
		{"favourites", ModelFilter{Favourites: []string{"deepseek-r1"}}, []string{"deepseek-r1"}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := modelOrder(Filter(refs, testCase.filter))
			if !equalOrder(got, testCase.want) {
				t.Errorf("filter %s = %v, want %v", testCase.name, got, testCase.want)
			}
		})
	}
}

// TestSearchMatchesNameIdProviderAndCapability covers `/models qwen`.
func TestSearchMatchesNameIdProviderAndCapability(t *testing.T) {
	refs := filterFixtures()

	cases := map[string][]string{
		"qwen":       {"qwen/qwen3-coder:free"},
		"coder":      {"qwen/qwen3-coder:free"},
		"openrouter": {"qwen/qwen3-coder:free", "deepseek-r1"},
		"google":     {"gemini-2.5-flash"},
		"vision":     {"gemini-2.5-flash"},
		"free":       {"qwen/qwen3-coder:free"},
		"flash gem":  {"gemini-2.5-flash"},
		"missing":    {},
	}

	for query, want := range cases {
		got := modelOrder(Filter(refs, ModelFilter{Query: query}))
		if !equalOrder(got, want) {
			t.Errorf("search %q = %v, want %v", query, got, want)
		}
	}
}

// TestFilterBitsRoundTrip keeps the callback encoding lossless.
func TestFilterBitsRoundTrip(t *testing.T) {
	filter := ModelFilter{Free: true, Vision: true, LongContext: true}
	restored := FilterFromBits(filter.Bits(), nil, "")

	if !restored.Free || !restored.Vision || !restored.LongContext {
		t.Errorf("round trip lost a toggle: %+v", restored)
	}
	if restored.Working || restored.Coding || restored.Reasoning || restored.Fast {
		t.Errorf("round trip added a toggle: %+v", restored)
	}
}

// TestCountAndTopFree covers the header numbers and the free block.
func TestCountAndTopFree(t *testing.T) {
	refs := []ModelRef{
		{Model: "a", PriceKnown: true, Free: true, Availability: ModelAvailable},
		{Model: "b", PriceKnown: true, Free: true, Availability: ModelUnknown},
		{Model: "c", PriceKnown: true, Free: true, Availability: ModelUnavailable},
		{Model: "d", PriceKnown: true, Availability: ModelLimited},
		{Model: "e", Availability: ModelUnknown},
	}

	counts := Count(refs)
	if counts.Total != 5 || counts.Free != 3 {
		t.Errorf("counts = %+v", counts)
	}
	if counts.Working != 1 || counts.Limited != 1 || counts.Unavailable != 1 || counts.Unknown != 2 {
		t.Errorf("status counts = %+v", counts)
	}

	top := modelOrder(TopFree(Sort(refs, "", nil), 8))
	if !equalOrder(top, []string{"a", "b"}) {
		t.Errorf("top free = %v, want the two usable free models", top)
	}
	if limited := TopFree(Sort(refs, "", nil), 1); len(limited) != 1 {
		t.Errorf("top free ignored the limit: %d", len(limited))
	}
}

// TestModelStatusRecoversAfterASuccess keeps one failure from blacklisting a
// model for good.
func TestModelStatusRecoversAfterASuccess(t *testing.T) {
	chain, err := NewChain([]Config{{
		Name: "groq", BaseURL: "https://example.invalid/v1", APIKey: "test-key", Models: []string{"one", "two"},
	}})
	if err != nil {
		t.Fatalf("NewChain: %v", err)
	}

	chain.RecordModelFailure("groq", "one", errors.New("upstream said no"))

	if got := chain.ModelAvailability("groq", "one"); got != ModelUnavailable {
		t.Fatalf("availability after a failure = %q", got)
	}
	if got := chain.ModelFailures("groq", "one"); got != 1 {
		t.Fatalf("failures = %d, want 1", got)
	}

	chain.RecordModelSuccess("groq", "one", 250*time.Millisecond)

	if got := chain.ModelAvailability("groq", "one"); got != ModelAvailable {
		t.Fatalf("availability after a success = %q, want available", got)
	}
	if got := chain.ModelFailures("groq", "one"); got != 0 {
		t.Fatalf("the failure streak was not cleared: %d", got)
	}
	latency, ok := chain.ModelLatency("one")
	if !ok || latency != 250*time.Millisecond {
		t.Fatalf("latency = %v (%v), want 250ms", latency, ok)
	}

	catalogue := chain.ModelCatalog()
	if len(catalogue) != 2 {
		t.Fatalf("catalogue has %d models, want 2", len(catalogue))
	}
	for _, ref := range catalogue {
		if ref.Model == "one" {
			if ref.Availability != ModelAvailable || ref.Latency == 0 {
				t.Errorf("the catalogue lost the health data: %+v", ref)
			}
		}
	}
}

// TestAutoRoutingAvoidsUnhealthyModels is the /auto path: a model that just
// failed is ranked below one that has not been tried recently.
func TestAutoRoutingAvoidsUnhealthyModels(t *testing.T) {
	chain, err := NewChain([]Config{{
		Name:    "groq",
		BaseURL: "https://example.invalid/v1",
		APIKey:  "test-key",
		Models:  []string{"llama-3.3-70b-versatile", "llama-3.1-8b-instant"},
	}})
	if err != nil {
		t.Fatalf("NewChain: %v", err)
	}

	candidates := chain.CandidatesFor(Preference{Auto: true}, &UsageProfile{})
	if len(candidates) == 0 || candidates[0].Model != "llama-3.3-70b-versatile" {
		t.Fatalf("healthy candidates = %+v", candidates)
	}

	chain.RecordModelFailure("groq", "llama-3.3-70b-versatile", errors.New("model overloaded"))

	after := chain.CandidatesFor(Preference{Auto: true}, &UsageProfile{})
	if len(after) != 1 || after[0].Model != "llama-3.1-8b-instant" {
		t.Fatalf("candidates after a failure = %+v", after)
	}

	ranked := chain.RecommendAmong(
		[]Candidate{
			{Provider: "groq", Model: "llama-3.3-70b-versatile"},
			{Provider: "groq", Model: "llama-3.1-8b-instant"},
		},
		UsageProfile{},
	)
	if ranked.Model != "llama-3.1-8b-instant" {
		t.Errorf("ranking picked the unavailable model %q", ranked.Model)
	}
}
