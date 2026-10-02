package main

import (
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"openrouter-bot/config"
	"openrouter-bot/internal/models"
	"openrouter-bot/provider"
	"openrouter-bot/ui"
	"openrouter-bot/user"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const (
	// catalogueTopFree is how many free models the /models header shows.
	catalogueTopFree = 8
	// catalogueListPageSize is one page of the filtered global list.
	catalogueListPageSize = 8
	// catalogueProvidersShown caps the provider block of the header.
	catalogueProvidersShown = 8

	// catalogueMinRefreshInterval keeps /refresh_models from being used to
	// hammer the providers, while still letting an admin force an update.
	catalogueMinRefreshInterval = 20 * time.Second
	// catalogueRefreshTimeout bounds how long a global refresh may stay
	// open before the summary is reported with whatever came back.
	catalogueRefreshTimeout = 45 * time.Second
)

// catalogueEntries returns every model the bot knows about, in catalogue
// order, together with the (provider, model) indices its buttons carry. The
// order depends on the catalogue snapshot only, never on the user, so an
// index stays valid between rendering and the tap.
func (a *app) catalogueEntries() []ui.CatalogueEntry {
	names := a.chain.Names()
	entries := make([]ui.CatalogueEntry, 0, 64)

	for index, name := range names {
		for modelIndex, ref := range a.providerModels(name) {
			entries = append(entries, ui.CatalogueEntry{
				Ref:           ref,
				ProviderIndex: index,
				ModelIndex:    modelIndex,
			})
		}
	}

	return provider.SortBy(entries, func(entry ui.CatalogueEntry) provider.ModelRef {
		return entry.Ref
	}, a.chain.Model(), nil)
}

// catalogueRefs flattens the entries, which is what the counters and the
// ranking helpers want.
func (a *app) catalogueRefs() []provider.ModelRef {
	entries := a.catalogueEntries()
	refs := make([]provider.ModelRef, len(entries))
	for i, entry := range entries {
		refs[i] = entry.Ref
	}

	return refs
}

// catalogueFetchedAt is the most recent successful model list fetch.
func (a *app) catalogueFetchedAt() (time.Time, bool) {
	if a.modelCatalog == nil {
		return time.Time{}, false
	}

	var latest time.Time
	found := false

	for _, name := range a.chain.Names() {
		fetchedAt, ok := a.modelCatalog.FetchedAt(models.CacheKey(name, ""))
		if !ok {
			continue
		}
		found = true
		if fetchedAt.After(latest) {
			latest = fetchedAt
		}
	}

	return latest, found
}

// -----------------------------------------------------------------------------
// SCREENS
// -----------------------------------------------------------------------------

// catalogueScreen is the global /models view: the live status of everything
// the configured providers serve, the free models worth using right now, and
// the compact filters.
func (a *app) catalogueScreen(conf *config.Config, tracker *user.UsageTracker, filter provider.ModelFilter, page int) screen {
	entries := a.catalogueEntries()
	refs := make([]provider.ModelRef, len(entries))
	for i, entry := range entries {
		refs[i] = entry.Ref
	}

	if len(refs) == 0 {
		return screen{
			Text: "❌ No models available. Try 🔄 Refresh.",
			Keyboard: tgbotapi.NewInlineKeyboardMarkup(
				tgbotapi.NewInlineKeyboardRow(
					tgbotapi.NewInlineKeyboardButtonData("🔄 Refresh", ui.Encode(ui.ActionCatalogRefresh)),
					tgbotapi.NewInlineKeyboardButtonData("🏠 Menu", ui.Encode(ui.ActionMenu)),
				),
			),
		}
	}

	counts := provider.Count(refs)
	matching := len(provider.Filter(refs, filter))

	var builder strings.Builder

	builder.WriteString(fmt.Sprintf("📚 <b>Model catalogue</b> · %d models · %d providers\n", counts.Total, a.chain.Count()))
	builder.WriteString(fmt.Sprintf(
		"🟢 %d working · 🟡 %d limited · 🔴 %d unavailable · ⚪ %d unknown\n",
		counts.Working, counts.Limited, counts.Unavailable, counts.Unknown,
	))
	builder.WriteString(fmt.Sprintf("🆓 %d free · 🔄 %s\n", counts.Free, refreshAge(a.catalogueFetchedAt())))

	if query := strings.TrimSpace(filter.Query); query != "" {
		builder.WriteString(fmt.Sprintf("🔎 <b>%s</b> · %d matches\n", escapeHTML(query), matching))
	} else if filter.Active() {
		builder.WriteString(fmt.Sprintf("🎛 Filters on · %d matches\n", matching))
	}

	if top := provider.TopFree(refs, catalogueTopFree); len(top) > 0 {
		builder.WriteString("\n🆓 <b>TOP FREE &amp; AVAILABLE</b>\n")
		for i, ref := range top {
			builder.WriteString(fmt.Sprintf("%d. %s\n", i+1, modelLine(ref)))
		}
	}

	builder.WriteString("\n<b>Providers</b>\n")
	for i, info := range a.providerSummaries() {
		if i >= catalogueProvidersShown {
			builder.WriteString(fmt.Sprintf("• … %d more\n", len(a.chain.Names())-catalogueProvidersShown))
			break
		}
		builder.WriteString(info + "\n")
	}

	builder.WriteString("\nCurrent: <b>" + escapeHTML(tracker.SettingsSummary()) + "</b>")

	return screen{
		Text:     strings.TrimRight(builder.String(), "\n"),
		Keyboard: ui.Catalogue(filter, matching),
	}
}

// catalogueListScreen is one page of the filtered global list.
func (a *app) catalogueListScreen(conf *config.Config, tracker *user.UsageTracker, filter provider.ModelFilter, page int) screen {
	entries := a.catalogueEntries()

	matching := make([]ui.CatalogueEntry, 0, len(entries))
	for _, entry := range entries {
		if filter.Match(entry.Ref) {
			matching = append(matching, entry)
		}
	}

	if len(matching) == 0 {
		return screen{
			Text:     "🔎 No model matches those filters.\n\nClear a filter, or use /models &lt;name&gt; to search.",
			Keyboard: ui.Catalogue(filter, 0),
		}
	}

	if page < 0 {
		page = 0
	}
	pages := (len(matching) + catalogueListPageSize - 1) / catalogueListPageSize
	if page >= pages {
		page = pages - 1
	}

	offset := page * catalogueListPageSize
	end := min(offset+catalogueListPageSize, len(matching))

	var lines []string
	for _, entry := range matching[offset:end] {
		lines = append(lines, modelLine(entry.Ref))
	}

	header := "📚 <b>Models</b>"
	if filter.Active() {
		header += " · filtered"
	}
	if query := strings.TrimSpace(filter.Query); query != "" {
		header += fmt.Sprintf(" · 🔎 %s", escapeHTML(query))
	}

	text := fmt.Sprintf(
		"%s\n%d match(es) · page %d/%d\n\n%s\n\n🔄 %s",
		header, len(matching), page+1, pages, strings.Join(lines, "\n"), refreshAge(a.catalogueFetchedAt()),
	)

	current := tracker.Preference().Model
	if current == "" {
		current = a.chain.Model()
	}

	return screen{
		Text: text,
		Keyboard: ui.CatalogueList(
			filter, matching, current, offset, catalogueListPageSize, tracker.IsFavourite,
		),
	}
}

// modelDetailsScreen shows only what is known about one model.
func (a *app) modelDetailsScreen(conf *config.Config, tracker *user.UsageTracker, providerIndex, modelIndex int) screen {
	name, refs := a.modelsForProvider(providerIndex)
	if name == "" || modelIndex < 0 || modelIndex >= len(refs) {
		return a.catalogueScreen(conf, tracker, provider.ModelFilter{}, 0)
	}

	ref := refs[modelIndex]

	var builder strings.Builder

	title := ref.DisplayName
	if strings.TrimSpace(title) == "" {
		title = ref.Model
	}

	builder.WriteString(fmt.Sprintf("🤖 <b>%s</b>\n<code>%s</code>\n\n", escapeHTML(title), escapeHTML(ref.Model)))
	builder.WriteString(fmt.Sprintf("Provider: <b>%s</b>\n", escapeHTML(name)))
	if ref.SourceProvider != "" {
		builder.WriteString(fmt.Sprintf("Publisher: %s\n", escapeHTML(ref.SourceProvider)))
	}
	builder.WriteString("Status: " + modelAvailabilityLabel(ref.Availability) + "\n")
	builder.WriteString("Price: " + modelPriceLabel(ref) + priceDetail(ref) + "\n")

	if ref.ContextLength > 0 {
		builder.WriteString(fmt.Sprintf("Context: <b>%s</b> tokens\n", formatTokens(ref.ContextLength)))
	}

	known := provider.LookupModel(ref.Model).Known
	if ref.Vision {
		builder.WriteString("Vision: yes\n")
	}
	if ref.Coding || (known && provider.LookupModel(ref.Model).Coding) {
		builder.WriteString("Coding: yes\n")
	}
	if ref.Reasoning {
		builder.WriteString("Reasoning: yes\n")
	}
	if ref.Fast {
		builder.WriteString("Fast: yes\n")
	}

	if ref.Latency > 0 {
		builder.WriteString(fmt.Sprintf("Latency: %dms\n", ref.Latency.Milliseconds()))
	}
	if ref.Failures > 0 {
		builder.WriteString(fmt.Sprintf("Recent failures: %d\n", ref.Failures))
	}
	if !ref.LastSeen.IsZero() {
		builder.WriteString("Last seen: " + time.Since(ref.LastSeen).Round(time.Second).String() + " ago\n")
	}

	builder.WriteString("\nCurrent: <b>" + escapeHTML(tracker.SettingsSummary()) + "</b>")

	return screen{
		Text:     strings.TrimRight(builder.String(), "\n"),
		Keyboard: ui.ModelDetails(ref.Model, providerIndex, modelIndex, tracker.IsFavourite(ref.Model)),
	}
}

// -----------------------------------------------------------------------------
// RENDERING HELPERS
// -----------------------------------------------------------------------------

// modelLine renders one catalogue row compactly.
func modelLine(ref provider.ModelRef) string {
	name := strings.TrimSpace(ref.DisplayName)
	if name == "" {
		name = ref.Model
	}

	line := fmt.Sprintf("%s %s <b>%s</b> · <code>%s</code>",
		provider.AvailabilityIcon(ref.Availability), priceIcon(ref), escapeHTML(name), escapeHTML(ref.Model))

	var badges []string
	if ref.Fast {
		badges = append(badges, "⚡")
	}
	if ref.Reasoning {
		badges = append(badges, "🧠")
	}
	if ref.Coding {
		badges = append(badges, "💻")
	}
	if ref.Vision {
		badges = append(badges, "👁")
	}
	if len(badges) > 0 {
		line += " · " + strings.Join(badges, "")
	}
	if ref.ContextLength > 0 {
		line += " · " + formatTokens(ref.ContextLength)
	}

	return line
}

func priceIcon(ref provider.ModelRef) string {
	if !ref.PriceKnown {
		return "❔"
	}
	if ref.Free {
		return "🆓"
	}

	return "💰"
}

// priceDetail renders the per-token prices the provider actually reported.
func priceDetail(ref provider.ModelRef) string {
	if !ref.PriceKnown || ref.PromptPrice == "" || ref.CompletionPrice == "" {
		return ""
	}

	return fmt.Sprintf(" ($%s / $%s per 1M tok)", perMillion(ref.PromptPrice), perMillion(ref.CompletionPrice))
}

// perMillion turns the per-token price a provider reports into a readable
// per-million figure. An unparseable value is dropped, never guessed.
func perMillion(raw string) string {
	value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return ""
	}

	return strconv.FormatFloat(value*1_000_000, 'f', -1, 64)
}

func formatTokens(tokens int) string {
	switch {
	case tokens >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(tokens)/1_000_000)
	case tokens >= 1000:
		return fmt.Sprintf("%dk", tokens/1000)
	default:
		return strconv.Itoa(tokens)
	}
}

// providerSummaries renders one line per provider with its model status.
func (a *app) providerSummaries() []string {
	lines := make([]string, 0, a.chain.Count())

	for _, name := range a.chain.Names() {
		counts := provider.Count(a.providerModels(name))
		latency := ""
		for _, info := range a.chain.Status() {
			if strings.EqualFold(info.Name, name) && info.AvgLatency > 0 {
				latency = fmt.Sprintf(" · ⚡ %dms", info.AvgLatency.Milliseconds())
				break
			}
		}

		lines = append(lines, fmt.Sprintf(
			"• <b>%s</b> · %d · 🟢 %d 🟡 %d 🔴 %d ⚪ %d · 🆓 %d%s",
			escapeHTML(name), counts.Total, counts.Working, counts.Limited,
			counts.Unavailable, counts.Unknown, counts.Free, latency,
		))
	}

	return lines
}

// statusSummary is the compact health line shared by /providers and /auto.
func (a *app) statusSummary() string {
	counts := provider.Count(a.catalogueRefs())

	return fmt.Sprintf("🟢 %d · 🟡 %d · 🔴 %d · ⚪ %d · 🆓 %d",
		counts.Working, counts.Limited, counts.Unavailable, counts.Unknown, counts.Free)
}

// refreshAge renders when the catalogue was last read from the providers.
func refreshAge(fetchedAt time.Time, ok bool) string {
	if !ok {
		return "never refreshed"
	}

	age := time.Since(fetchedAt).Round(time.Second)
	if age < time.Minute {
		return "just now"
	}

	return age.String() + " ago"
}

// -----------------------------------------------------------------------------
// GLOBAL REFRESH
// -----------------------------------------------------------------------------

// refreshCatalogue re-reads the model list of every configured provider,
// updates the chain catalogue and reports one compact result. A provider that
// fails keeps its last known list, and only one refresh runs at a time.
func (a *app) refreshCatalogue(chatID int64) bool {
	if a.modelCatalog == nil {
		return false
	}

	type target struct {
		name    string
		baseURL string
		apiKey  string
	}

	var targets []target
	for _, name := range a.chain.Names() {
		baseURL, apiKey, ok := a.chain.EndpointOf(name)
		if !ok || strings.TrimSpace(baseURL) == "" {
			continue
		}
		targets = append(targets, target{name: name, baseURL: baseURL, apiKey: apiKey})
	}
	if len(targets) == 0 {
		return false
	}

	started, busy := a.beginCatalogueRefresh()
	if !started {
		a.report(chatID, busy, "")

		return false
	}

	a.report(chatID, "🔄 Refreshing model catalogue…", "")

	var (
		mu      sync.Mutex
		pending = len(targets)
		failed  []string
		done    atomic.Bool
	)

	finish := func() {
		if !done.CompareAndSwap(false, true) {
			return
		}

		a.endCatalogueRefresh()

		mu.Lock()
		failedProviders := append([]string(nil), failed...)
		mu.Unlock()

		a.report(chatID, a.catalogueSummaryText(len(targets), failedProviders), "HTML")
	}

	record := func(name string, err error) {
		mu.Lock()
		if err != nil {
			failed = append(failed, name)
		}
		pending--
		last := pending == 0
		mu.Unlock()

		if last {
			finish()
		}
	}

	for _, item := range targets {
		provider := item
		key := models.CacheKey(provider.name, "")

		started := a.modelCatalog.RefreshNow(key, provider.baseURL, func() string { return provider.apiKey },
			func(fetched []models.Model, err error) {
				if err == nil && len(fetched) > 0 {
					a.setChainModelCatalog(provider.name, fetched)
					record(provider.name, nil)

					return
				}

				// The upstream error never carries the key, and only the
				// provider name reaches the log.
				log.Printf("Model catalogue refresh failed for provider %s", provider.name)
				record(provider.name, fmt.Errorf("model list unavailable"))
			},
		)

		if !started {
			record(provider.name, nil)
		}
	}

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()

		select {
		case <-time.After(catalogueRefreshTimeout):
			finish()
		case <-a.ctx.Done():
		}
	}()

	return true
}

// report sends one line of the refresh progress.
func (a *app) report(chatID int64, text, mode string) {
	if a.notify != nil {
		a.notify(chatID, text, mode)

		return
	}

	a.send(chatID, text, mode)
}

// beginCatalogueRefresh claims the one global refresh slot, so a second user
// tapping 🔄 joins the run in progress instead of starting another one.
func (a *app) beginCatalogueRefresh() (bool, string) {
	a.refreshMu.Lock()
	defer a.refreshMu.Unlock()

	if a.refreshing {
		return false, "🔄 A catalogue refresh is already running — one moment."
	}
	if since := time.Since(a.lastRefresh); since < catalogueMinRefreshInterval {
		return false, "🔄 Refreshed " + since.Round(time.Second).String() + " ago — the catalogue is fresh."
	}

	a.refreshing = true

	return true, ""
}

// endCatalogueRefresh releases the slot and stamps the refresh time.
func (a *app) endCatalogueRefresh() {
	a.refreshMu.Lock()
	defer a.refreshMu.Unlock()

	a.refreshing = false
	a.lastRefresh = time.Now()
}

// catalogueSummaryText is the compact result of a global refresh.
func (a *app) catalogueSummaryText(requested int, failed []string) string {
	counts := provider.Count(a.catalogueRefs())
	fetchedAt, ok := a.catalogueFetchedAt()

	var builder strings.Builder

	builder.WriteString("✅ <b>Model catalogue refreshed</b>\n\n")
	builder.WriteString(fmt.Sprintf("Providers: <b>%d</b>", requested))
	if len(failed) > 0 {
		builder.WriteString(fmt.Sprintf(" · %d failed", len(failed)))
	}
	builder.WriteString(fmt.Sprintf("\nModels: <b>%d</b> · 🆓 <b>%d</b>\n", counts.Total, counts.Free))
	builder.WriteString(fmt.Sprintf("🟢 %d working · 🟡 %d limited · 🔴 %d unavailable · ⚪ %d unknown\n",
		counts.Working, counts.Limited, counts.Unavailable, counts.Unknown))
	builder.WriteString("🔄 " + refreshAge(fetchedAt, ok))

	if len(failed) > 0 {
		builder.WriteString(fmt.Sprintf(
			"\n\n⚠️ %s unreachable — kept the last known list.",
			escapeHTML(strings.Join(failed, ", ")),
		))
	}

	return builder.String()
}

// -----------------------------------------------------------------------------
// SEARCH
// -----------------------------------------------------------------------------

// searchHelpText explains how to search, because a Telegram button cannot
// collect free text.
const searchHelpText = "🔎 <b>Search the catalogue</b>\n\n" +
	"/models qwen\n" +
	"/models free · /models vision · /models coding\n" +
	"/models openrouter reasoning\n" +
	"/models &lt;provider&gt; — one provider's models\n\n" +
	"Send /models to clear the search."

// setSearch remembers the model search text of one chat, so the filter
// buttons keep working while it is active.
func (a *app) setSearch(chatID int64, query string) {
	a.searchMu.Lock()
	defer a.searchMu.Unlock()

	if a.search == nil {
		a.search = make(map[int64]string)
	}
	// The map is bounded by chats; a runaway deployment simply drops the
	// remembered searches instead of growing without limit.
	if len(a.search) > 5000 {
		a.search = make(map[int64]string)
	}

	query = strings.TrimSpace(query)
	if query == "" {
		delete(a.search, chatID)

		return
	}

	a.search[chatID] = query
}

func (a *app) searchQuery(chatID int64) string {
	a.searchMu.Lock()
	defer a.searchMu.Unlock()

	return a.search[chatID]
}

// -----------------------------------------------------------------------------
// FREE MODELS
// -----------------------------------------------------------------------------

// freeModelsText lists the models the providers priced at zero for both
// prompt and completion tokens, best first.
func (a *app) freeModelsText() string {
	refs := provider.Filter(a.catalogueRefs(), provider.ModelFilter{Free: true})
	if len(refs) == 0 {
		return ""
	}

	listed := min(len(refs), modelsPageLimit)
	lines := make([]string, 0, listed)
	for _, ref := range refs[:listed] {
		lines = append(lines, modelLine(ref))
	}

	text := fmt.Sprintf("🆓 <b>Free models</b> · %d of %d shown\n\n%s",
		listed, len(refs), strings.Join(lines, "\n"))

	if len(refs) > listed {
		text += "\n\n… /models 🆓 to browse them all."
	}

	return text
}
