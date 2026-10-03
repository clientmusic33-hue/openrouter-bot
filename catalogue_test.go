package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"openrouter-bot/config"
	"openrouter-bot/internal/models"
	"openrouter-bot/provider"
	"openrouter-bot/ui"
	"openrouter-bot/user"
)

const cataloguePayload = `{"data":[
	{"id":"qwen/qwen3-coder:free","name":"Qwen3 Coder","context_length":262144,
	 "pricing":{"prompt":"0","completion":"0"},
	 "architecture":{"modality":"text+image->text","input_modalities":["text","image"]},
	 "supported_parameters":["tools","reasoning"]},
	{"id":"deepseek/deepseek-chat","name":"DeepSeek Chat","context_length":65536,
	 "pricing":{"prompt":"0.0000003","completion":"0.0000006"}},
	{"id":"mystery/model","context_length":8192}
]}`

// newCatalogueApp builds the slice of the bot the catalogue screens need: a
// chain with one provider, backed by a live model list.
func newCatalogueApp(t *testing.T, payload string) *app {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(server.Close)

	chain, err := provider.NewChain([]provider.Config{{
		Name:    "testllm",
		BaseURL: server.URL,
		APIKey:  "test-key",
		Models:  []string{"deepseek/deepseek-chat"},
	}})
	if err != nil {
		t.Fatalf("NewChain: %v", err)
	}

	catalogue := models.New()
	if _, err := catalogue.Discover(context.Background(), models.CacheKey("testllm", ""), server.URL, "test-key"); err != nil {
		t.Fatalf("Discover: %v", err)
	}

	return &app{ctx: context.Background(), chain: chain, modelCatalog: catalogue}
}

func testTracker(t *testing.T) (*user.UsageTracker, *config.Config) {
	t.Helper()

	conf := &config.Config{}

	return user.NewUsageTracker("42", "tester", t.TempDir(), conf, user.NewHistory()), conf
}

func findEntry(entries []ui.CatalogueEntry, model string) (ui.CatalogueEntry, bool) {
	for _, entry := range entries {
		if entry.Ref.Model == model {
			return entry, true
		}
	}

	return ui.CatalogueEntry{}, false
}

// TestCatalogueEntriesCarryLiveMetadata covers the merge of the provider's
// model list with the chain's health data.
func TestCatalogueEntriesCarryLiveMetadata(t *testing.T) {
	a := newCatalogueApp(t, cataloguePayload)

	entries := a.catalogueEntries()
	if len(entries) != 3 {
		t.Fatalf("catalogue has %d models, want 3", len(entries))
	}

	free, ok := findEntry(entries, "qwen/qwen3-coder:free")
	if !ok {
		t.Fatal("the free model is missing from the catalogue")
	}
	if !free.Ref.Free || !free.Ref.PriceKnown {
		t.Errorf("zero pricing was not detected: %+v", free.Ref)
	}
	if !free.Ref.Vision || !free.Ref.Reasoning {
		t.Errorf("declared capabilities were dropped: %+v", free.Ref)
	}
	if free.Ref.ContextLength != 262144 {
		t.Errorf("context = %d", free.Ref.ContextLength)
	}
	if free.ProviderIndex != 0 {
		t.Errorf("the button index lost its provider: %+v", free)
	}

	// Free and untried models come first: that is the ordering /models shows.
	if entries[0].Ref.Model != "qwen/qwen3-coder:free" {
		t.Errorf("first model = %q", entries[0].Ref.Model)
	}
}

// TestFreeModelsListsOnlyZeroPricedModels covers /get_models.
func TestFreeModelsListsOnlyZeroPricedModels(t *testing.T) {
	a := newCatalogueApp(t, cataloguePayload)

	text := a.freeModelsText()
	if !strings.Contains(text, "qwen/qwen3-coder:free") {
		t.Errorf("the free model is missing: %q", text)
	}
	if strings.Contains(text, "deepseek/deepseek-chat") {
		t.Errorf("a paid model was listed as free: %q", text)
	}
	if strings.Contains(text, "mystery/model") {
		t.Errorf("a model with unknown pricing was listed as free: %q", text)
	}
}

// TestCatalogueScreenShowsTopFreeAndStatuses covers the /models header.
func TestCatalogueScreenShowsTopFreeAndStatuses(t *testing.T) {
	a := newCatalogueApp(t, cataloguePayload)
	tracker, conf := testTracker(t)

	screen := a.catalogueScreen(conf, tracker, provider.ModelFilter{}, 0)

	for _, want := range []string{
		"TOP FREE &amp; AVAILABLE",
		"qwen/qwen3-coder:free",
		"🆓 1 free",
		"<b>testllm</b>",
		"⚪ 3 unknown",
	} {
		if !strings.Contains(screen.Text, want) {
			t.Errorf("catalogue screen is missing %q:\n%s", want, screen.Text)
		}
	}
}

// TestCatalogueFallsBackToConfiguredModels covers a provider whose model list
// could never be read: the configured models stay visible and nothing is
// claimed to be free.
func TestCatalogueFallsBackToConfiguredModels(t *testing.T) {
	chain, err := provider.NewChain([]provider.Config{{
		Name: "testllm", BaseURL: "https://example.invalid/v1", APIKey: "test-key",
		Models: []string{"deepseek/deepseek-chat"},
	}})
	if err != nil {
		t.Fatalf("NewChain: %v", err)
	}

	a := &app{ctx: context.Background(), chain: chain, modelCatalog: models.New()}
	tracker, conf := testTracker(t)

	if _, ok := findEntry(a.catalogueEntries(), "deepseek/deepseek-chat"); !ok {
		t.Fatal("the configured model disappeared from the catalogue")
	}

	screen := a.catalogueScreen(conf, tracker, provider.ModelFilter{}, 0)
	if !strings.Contains(screen.Text, "1 models") {
		t.Errorf("the fallback model is not counted:\n%s", screen.Text)
	}
	if strings.Contains(screen.Text, "TOP FREE") {
		t.Errorf("a model with unknown pricing was called free:\n%s", screen.Text)
	}
	if !strings.Contains(screen.Text, "never refreshed") {
		t.Errorf("the missing refresh time is not reported:\n%s", screen.Text)
	}
}

// TestCatalogueListScreenSearchesAndPaginates covers `/models qwen` and the
// page clamping a stale button can produce.
func TestCatalogueListScreenSearchesAndPaginates(t *testing.T) {
	a := newCatalogueApp(t, cataloguePayload)
	tracker, conf := testTracker(t)

	found := a.catalogueListScreen(conf, tracker, provider.ModelFilter{Query: "qwen"}, 0)
	if !strings.Contains(found.Text, "qwen/qwen3-coder:free") {
		t.Errorf("the search result is missing:\n%s", found.Text)
	}
	if strings.Contains(found.Text, "deepseek/deepseek-chat") {
		t.Errorf("the search returned another provider's model:\n%s", found.Text)
	}
	if !strings.Contains(found.Text, "1 match(es) · page 1/1") {
		t.Errorf("the result count is wrong:\n%s", found.Text)
	}

	capability := a.catalogueListScreen(conf, tracker, provider.ModelFilter{Query: "coding"}, 0)
	if !strings.Contains(capability.Text, "qwen/qwen3-coder:free") {
		t.Errorf("searching a capability found nothing:\n%s", capability.Text)
	}

	clamped := a.catalogueListScreen(conf, tracker, provider.ModelFilter{}, 99)
	if !strings.Contains(clamped.Text, "page 1/1") {
		t.Errorf("an out of range page was not clamped:\n%s", clamped.Text)
	}

	none := a.catalogueListScreen(conf, tracker, provider.ModelFilter{Query: "nothing-here"}, 0)
	if !strings.Contains(none.Text, "No model matches") {
		t.Errorf("an empty search has no explanation:\n%s", none.Text)
	}
}

// TestModelDetailsShowsOnlyKnownData keeps invented metadata out of the
// details screen.
func TestModelDetailsShowsOnlyKnownData(t *testing.T) {
	a := newCatalogueApp(t, cataloguePayload)
	tracker, conf := testTracker(t)

	entries := a.catalogueEntries()
	free, _ := findEntry(entries, "qwen/qwen3-coder:free")
	unknown, _ := findEntry(entries, "mystery/model")

	details := a.modelDetailsScreen(conf, tracker, free.ProviderIndex, free.ModelIndex)
	for _, want := range []string{"🆓 FREE", "262k", "Vision: yes", "Reasoning: yes", "testllm"} {
		if !strings.Contains(details.Text, want) {
			t.Errorf("details are missing %q:\n%s", want, details.Text)
		}
	}

	plain := a.modelDetailsScreen(conf, tracker, unknown.ProviderIndex, unknown.ModelIndex)
	if !strings.Contains(plain.Text, "❔ PRICE UNKNOWN") {
		t.Errorf("unknown pricing is not reported:\n%s", plain.Text)
	}
	for _, absent := range []string{"Vision:", "Reasoning:", "Coding:", "Latency:", "Last seen:"} {
		if strings.Contains(plain.Text, absent) {
			t.Errorf("details invented %q:\n%s", absent, plain.Text)
		}
	}
}

// TestGlobalRefreshIsSingleFlight covers the guard behind /refresh_models.
func TestGlobalRefreshIsSingleFlight(t *testing.T) {
	a := newCatalogueApp(t, cataloguePayload)

	if started, message := a.beginCatalogueRefresh(); !started || message != "" {
		t.Fatalf("the first refresh was refused: %q", message)
	}

	started, message := a.beginCatalogueRefresh()
	if started {
		t.Fatal("a second refresh must not start while one is running")
	}
	if !strings.Contains(message, "already running") {
		t.Errorf("the busy message is %q", message)
	}

	a.endCatalogueRefresh()

	if started, message := a.beginCatalogueRefresh(); started || !strings.Contains(message, "fresh") {
		t.Errorf("a refresh right after another must wait: started=%v message=%q", started, message)
	}
}

// TestCatalogueSummaryReportsFailures covers the compact result, including a
// provider that could not be reached.
func TestCatalogueSummaryReportsFailures(t *testing.T) {
	a := newCatalogueApp(t, cataloguePayload)

	summary := a.catalogueSummaryText(2, []string{"broken-provider"})

	for _, want := range []string{
		"✅ <b>Model catalogue refreshed</b>",
		"Providers: <b>2</b> · 1 failed",
		"Models: <b>3</b>",
		"🆓 <b>1</b>",
		"broken-provider unreachable",
	} {
		if !strings.Contains(summary, want) {
			t.Errorf("the summary is missing %q:\n%s", want, summary)
		}
	}

	if strings.Contains(summary, "test-key") {
		t.Errorf("the summary leaks a credential:\n%s", summary)
	}
}

// TestSearchIsPerChat covers the search state the filter buttons rely on.
func TestSearchIsPerChat(t *testing.T) {
	a := newCatalogueApp(t, cataloguePayload)

	a.setSearch(1, " qwen ")
	if got := a.searchQuery(1); got != "qwen" {
		t.Errorf("search = %q", got)
	}
	if got := a.searchQuery(2); got != "" {
		t.Errorf("another chat inherited the search: %q", got)
	}

	a.setSearch(1, "")
	if got := a.searchQuery(1); got != "" {
		t.Errorf("the search was not cleared: %q", got)
	}
}

// TestRefreshAgeRendersTheLastFetch covers the timestamp shown by /models.
func TestRefreshAgeRendersTheLastFetch(t *testing.T) {
	a := newCatalogueApp(t, cataloguePayload)

	fetchedAt, ok := a.catalogueFetchedAt()
	if !ok || fetchedAt.IsZero() {
		t.Fatal("a discovered catalogue must carry a fetch time")
	}
	if got := refreshAge(fetchedAt, ok); got != "just now" {
		t.Errorf("age = %q", got)
	}
	if got := refreshAge(fetchedAt, false); got != "never refreshed" {
		t.Errorf("missing age = %q", got)
	}
}

// TestRefreshCatalogueReadsEveryProvider drives /refresh_models end to end:
// the immediate ack, the compact summary, and a failing provider that must
// not take the working catalogue down with it.
func TestRefreshCatalogueReadsEveryProvider(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(cataloguePayload))
	}))
	defer good.Close()

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()

	chain, err := provider.NewChain([]provider.Config{
		{Name: "testllm", BaseURL: good.URL, APIKey: "test-key", Models: []string{"deepseek/deepseek-chat"}},
		{Name: "broken", BaseURL: bad.URL, Models: []string{"fallback-model"}},
	})
	if err != nil {
		t.Fatalf("NewChain: %v", err)
	}

	a := &app{ctx: context.Background(), chain: chain, modelCatalog: models.New()}

	var (
		mu       sync.Mutex
		messages []string
	)
	a.notify = func(_ int64, text, _ string) {
		mu.Lock()
		messages = append(messages, text)
		mu.Unlock()
	}

	if !a.refreshCatalogue(7) {
		t.Fatal("the refresh was refused")
	}

	summary := ""
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		count := len(messages)
		if count > 1 {
			summary = messages[count-1]
		}
		first := ""
		if count > 0 {
			first = messages[0]
		}
		mu.Unlock()

		if summary != "" {
			if first != "🔄 Refreshing model catalogue…" {
				t.Errorf("the first message is %q", first)
			}

			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if summary == "" {
		t.Fatal("the refresh never reported a summary")
	}

	for _, want := range []string{
		"✅ <b>Model catalogue refreshed</b>",
		"Providers: <b>2</b> · 1 failed",
		"Models: <b>4</b>",
		"🆓 <b>1</b>",
		"broken unreachable",
	} {
		if !strings.Contains(summary, want) {
			t.Errorf("the summary is missing %q:\n%s", want, summary)
		}
	}

	if _, ok := findEntry(a.catalogueEntries(), "qwen/qwen3-coder:free"); !ok {
		t.Error("the refreshed provider's models are missing from the catalogue")
	}
	if _, ok := findEntry(a.catalogueEntries(), "fallback-model"); !ok {
		t.Error("the failing provider lost its last known models")
	}

	mu.Lock()
	acked := len(messages)
	mu.Unlock()

	if a.refreshCatalogue(7) {
		t.Error("a second refresh must wait for the minimum interval")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(messages) != acked+1 || !strings.Contains(messages[len(messages)-1], "fresh") {
		t.Errorf("the refused refresh did not explain itself: %v", messages[acked:])
	}
}
