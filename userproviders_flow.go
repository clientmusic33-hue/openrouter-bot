package main

import (
	"fmt"
	"log"
	"net/url"
	"strconv"
	"strings"
	"time"

	"openrouter-bot/config"
	"openrouter-bot/internal/models"
	"openrouter-bot/internal/security"
	"openrouter-bot/provider"
	"openrouter-bot/ui"
	"openrouter-bot/user"
	"openrouter-bot/userproviders"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// userProvidersDisabledText is shown when the operator never set the master
// key. User owned providers are refused instead of stored in plaintext.
const userProvidersDisabledText = "🔐 <b>Your providers</b>\n\n" +
	"This bot cannot store personal API keys safely: the operator has not set " +
	"<code>USER_PROVIDER_ENCRYPTION_KEY</code>.\n\n" +
	"Nothing was changed."

// modelsPageLimit caps how many discovered models one message lists, so a
// provider with hundreds of models never floods the chat.
const modelsPageLimit = 30

// Steps of the /addprovider conversation.
const (
	flowStepName    = "name"
	flowStepBaseURL = "base_url"
	flowStepKey     = "key"
	flowStepModel   = "model"
)

// flowTTL is how long a started flow waits for the next answer.
const flowTTL = 15 * time.Minute

// providerFlow is one user's half finished /addprovider conversation. It holds
// no secrets: the API key is sealed into storage the moment it arrives, so the
// flow only remembers which record it is filling in.
type providerFlow struct {
	Step    string
	Name    string
	BaseURL string
	ID      string
	Updated time.Time
}

// modelsView remembers which provider screen a message currently shows, so a
// background refresh never overwrites a menu the user has already left.
type modelsView struct {
	index int
	page  int
	user  bool
}

type viewKey struct {
	chatID    int64
	messageID int
}

// -----------------------------------------------------------------------------
// COMMANDS
// -----------------------------------------------------------------------------

// handleAddProvider starts the guided flow that stores a user's own provider.
func (a *app) handleAddProvider(message *tgbotapi.Message, conf *config.Config, tracker *user.UsageTracker) {
	if !a.userProvidersReady() {
		a.send(message.Chat.ID, userProvidersDisabledText, "HTML")

		return
	}

	a.setFlow(senderID(message), &providerFlow{Step: flowStepName})
	a.send(message.Chat.ID,
		"🔐 <b>Add your provider</b>\n\n"+
			"Enter a name for it, for example <code>Mistral</code> or <code>My Groq</code>.\n\n"+
			"Send /cancel to stop.",
		"HTML",
	)
}

// handleMyProviders lists the providers a user added themselves.
func (a *app) handleMyProviders(message *tgbotapi.Message, conf *config.Config, tracker *user.UsageTracker) {
	if !a.userProvidersReady() {
		a.send(message.Chat.ID, userProvidersDisabledText, "HTML")

		return
	}

	a.sendScreen(message.Chat.ID, a.userProvidersScreen(senderID(message), 0))
}

// handleUseProvider switches the user to one of their own providers.
func (a *app) handleUseProvider(message *tgbotapi.Message, conf *config.Config, tracker *user.UsageTracker) {
	if !a.userProvidersReady() {
		a.send(message.Chat.ID, userProvidersDisabledText, "HTML")

		return
	}

	userID := senderID(message)

	list, err := a.userProviderList(userID)
	if err != nil {
		log.Printf("Could not load user providers for user %d: %v", userID, err)
		a.send(message.Chat.ID, "⚠️ Could not load your providers right now.", "")

		return
	}

	args := strings.TrimSpace(message.CommandArguments())
	if args == "" {
		a.sendScreen(message.Chat.ID, a.userProvidersScreen(userID, 0))

		return
	}

	record, ok := resolveUserProvider(list, args)
	if !ok {
		a.send(message.Chat.ID, "❌ Unknown provider. Use /myproviders to see yours.", "")

		return
	}

	if strings.TrimSpace(record.Model) == "" {
		// No model chosen yet: open the picker instead of pinning nothing.
		a.sendScreen(message.Chat.ID, a.userProviderModelsScreen(userID, tracker, indexOfUserProvider(list, record.ID), 0))

		return
	}

	tracker.SetModel(record.Model, record.Name)

	a.send(message.Chat.ID, fmt.Sprintf(
		"📌 Switched to <b>%s</b> · <code>%s</code>",
		escapeHTML(record.Name),
		escapeHTML(record.Model),
	), "HTML")
	a.sendScreen(message.Chat.ID, a.homeScreen(conf, tracker))
}

// handleRemoveProvider deletes one of the user's own providers.
func (a *app) handleRemoveProvider(message *tgbotapi.Message, conf *config.Config, tracker *user.UsageTracker) {
	if !a.userProvidersReady() {
		a.send(message.Chat.ID, userProvidersDisabledText, "HTML")

		return
	}

	userID := senderID(message)

	args := strings.TrimSpace(message.CommandArguments())
	if args == "" {
		a.send(message.Chat.ID, "🗑 Usage: /removeprovider &lt;name&gt;\n\nUse /myproviders to see yours.", "HTML")

		return
	}

	list, err := a.userProviderList(userID)
	if err != nil {
		a.send(message.Chat.ID, "⚠️ Could not load your providers right now.", "")

		return
	}

	record, ok := resolveUserProvider(list, args)
	if !ok {
		a.send(message.Chat.ID, "❌ Unknown provider. Use /myproviders to see yours.", "")

		return
	}

	removed, err := a.userProviders.Remove(a.ctx, userID, record.ID)
	if err != nil || !removed {
		log.Printf("Could not remove user provider %s of user %d: %v", record.ID, userID, err)
		a.send(message.Chat.ID, "⚠️ Could not remove that provider.", "")

		return
	}

	// A provider that no longer exists must not stay pinned.
	if strings.EqualFold(tracker.Preference().Provider, record.Name) {
		tracker.ResetPreference()
	}

	a.send(message.Chat.ID, fmt.Sprintf("🗑 Removed <b>%s</b>.", escapeHTML(record.Name)), "HTML")
}

// -----------------------------------------------------------------------------
// THE GUIDED FLOW
// -----------------------------------------------------------------------------

// consumeProviderFlow serves the next answer of a started /addprovider
// conversation. It returns true when the message was consumed, so the caller
// must not treat it as a prompt for the AI.
func (a *app) consumeProviderFlow(message *tgbotapi.Message, conf *config.Config, tracker *user.UsageTracker) bool {
	userID := senderID(message)

	flow, ok := a.flow(userID)
	if !ok {
		return false
	}

	text := strings.TrimSpace(messageText(message))
	if text == "" {
		return false
	}

	chatID := message.Chat.ID
	flow.Updated = time.Now()

	switch flow.Step {

	case flowStepName:
		if len([]rune(text)) > 40 {
			a.send(chatID, "❌ That name is too long (40 characters max). Try a shorter one, or /cancel.", "")

			return true
		}

		// A name that matches a built-in preset carries its official
		// endpoint, so the user is not asked for a URL.
		if preset, known := provider.LookupPreset(text); known {
			flow.Name = preset.Name
			flow.BaseURL = preset.BaseURL
			flow.Step = flowStepKey

			a.send(chatID, fmt.Sprintf(
				"🔑 Send your <b>%s</b> API key.\n\nIt is encrypted before it is stored, and it is never shown again.",
				escapeHTML(preset.Name),
			), "HTML")

			return true
		}

		flow.Name = text
		flow.Step = flowStepBaseURL

		a.send(chatID,
			"🌐 Enter the base URL of the API. It must be OpenAI-compatible, for example <code>https://api.example.com/v1</code>.",
			"HTML",
		)

		return true

	case flowStepBaseURL:
		parsed, err := security.ValidateExternalURL(text)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			a.send(chatID, "❌ That is not a public http(s) endpoint. Try again, or /cancel.", "")

			return true
		}

		flow.BaseURL = strings.TrimRight(text, "/")
		flow.Step = flowStepKey

		a.send(chatID,
			"🔑 Send your API key.\n\nIt is encrypted before it is stored, and it is never shown again.",
			"HTML",
		)

		return true

	case flowStepKey:
		record, err := a.userProviders.Add(a.ctx, userID, flow.Name, "", flow.BaseURL, text)
		if err != nil {
			// The reason is logged without the key, and the user gets a
			// generic answer: no credential ever reaches a message.
			log.Printf("Could not add a user provider for user %d: %v", userID, err)
			a.send(chatID, "⚠️ Could not save that provider. Start again with /addprovider.", "")
			a.clearFlow(userID)

			return true
		}

		flow.ID = record.ID
		flow.Step = flowStepModel

		a.sendKeyboard(chatID,
			"✅ <b>Provider added securely.</b>\n\n"+
				"🤖 Send a model id, or tap 🔎 <b>Fetch models</b> to list what your key can use.",
			ui.FetchModelsButton(record.ID),
			false,
		)

		return true

	case flowStepModel:
		if flow.ID == "" {
			a.clearFlow(userID)

			return false
		}

		model := text
		ids := a.fetchedModels(userID)

		// A number is only treated as a list position while it points at a
		// fetched model; anything else is taken as a model id.
		if index, err := strconv.Atoi(text); err == nil && len(ids) > 0 && index >= 1 && index <= len(ids) {
			model = ids[index-1]
		} else if strings.ContainsAny(text, " \t") {
			a.send(chatID, "❌ A model id has no spaces. Try again, or /cancel.", "")

			return true
		}

		name := flow.Name

		if err := a.userProviders.SetModel(a.ctx, userID, flow.ID, model); err != nil {
			log.Printf("Could not store the model of a user provider for user %d: %v", userID, err)
			a.send(chatID, "⚠️ Could not save that model. Start again with /addprovider.", "")
			a.clearFlow(userID)

			return true
		}

		a.clearFlow(userID)
		a.send(chatID, fmt.Sprintf(
			"✅ <b>Provider ready.</b>\n\nModel: <code>%s</code>\n\nSwitch to it with /useprovider <b>%s</b>.",
			escapeHTML(model),
			escapeHTML(name),
		), "HTML")

		return true
	}

	a.clearFlow(userID)

	return false
}

func (a *app) setFlow(userID int64, flow *providerFlow) {
	a.flowMu.Lock()
	defer a.flowMu.Unlock()

	if a.flows == nil {
		a.flows = make(map[int64]*providerFlow)
	}

	flow.Updated = time.Now()
	a.flows[userID] = flow
}

func (a *app) clearFlow(userID int64) {
	a.flowMu.Lock()
	delete(a.flows, userID)
	a.flowMu.Unlock()

	a.fetchedMu.Lock()
	delete(a.fetched, userID)
	a.fetchedMu.Unlock()
}

// flow returns the live conversation of a user, dropping it once it expired.
func (a *app) flow(userID int64) (*providerFlow, bool) {
	a.flowMu.Lock()
	defer a.flowMu.Unlock()

	flow, ok := a.flows[userID]
	if !ok || flow == nil {
		return nil, false
	}

	if time.Since(flow.Updated) > flowTTL {
		delete(a.flows, userID)

		return nil, false
	}

	return flow, true
}

// flowAwaitingSecret reports whether the user is about to type an API key, so
// the generic message log skips the text instead of writing a credential.
func (a *app) flowAwaitingSecret(userID int64) bool {
	flow, ok := a.flow(userID)

	return ok && flow.Step == flowStepKey
}

// rememberFetchedModels keeps the numbered list a user is choosing from. It
// holds model ids, never credentials, and it is written from the discovery
// goroutine, so it carries its own lock.
func (a *app) rememberFetchedModels(userID int64, ids []string) {
	a.fetchedMu.Lock()
	defer a.fetchedMu.Unlock()

	if a.fetched == nil {
		a.fetched = make(map[int64][]string)
	}

	a.fetched[userID] = ids
}

// fetchedModels returns the numbered list a user is choosing from.
func (a *app) fetchedModels(userID int64) []string {
	a.fetchedMu.Lock()
	defer a.fetchedMu.Unlock()

	return a.fetched[userID]
}

// -----------------------------------------------------------------------------
// USER PROVIDER ACCESS
// -----------------------------------------------------------------------------

// userProvidersReady reports whether the "bring your own key" feature can be
// used on this deployment.
func (a *app) userProvidersReady() bool {
	return a.userProviders != nil && a.userProviders.Enabled()
}

// userProviderList loads the providers of one user. The list is always scoped
// to that Telegram id, which is what keeps one user's credentials out of
// another user's hands.
func (a *app) userProviderList(userID int64) ([]userproviders.Provider, error) {
	if !a.userProvidersReady() {
		return nil, nil
	}

	return a.userProviders.List(a.ctx, userID)
}

// userProviderModelIDs returns the models to show for one of a user's
// providers: the live catalogue when it is cached, the stored model otherwise.
func (a *app) userProviderModelIDs(record userproviders.Provider) []string {
	if a.modelCatalog != nil {
		if discovered, ok := a.modelCatalog.Cached(models.CacheKey("", record.ID)); ok && len(discovered) > 0 {
			ids := make([]string, 0, len(discovered))
			for _, model := range discovered {
				ids = append(ids, model.ModelID)
			}

			return ids
		}
	}

	if model := strings.TrimSpace(record.Model); model != "" {
		return []string{model}
	}

	return nil
}

// userCandidate builds the single (provider, model) pair a user pinned on one
// of their own providers. The key is decrypted here, used for that request and
// then dropped with the candidate: it never enters a cache, a log, a prompt or
// a message.
func (a *app) userCandidate(tracker *user.UsageTracker) []provider.Candidate {
	if !a.userProvidersReady() || tracker == nil {
		return nil
	}

	// The tracker carries the Telegram id the providers are keyed on.
	userID, err := strconv.ParseInt(strings.TrimSpace(tracker.UserID), 10, 64)
	if err != nil {
		return nil
	}

	preference := tracker.Preference()
	if strings.TrimSpace(preference.Model) == "" {
		return nil
	}

	cfg, found, cfgErr := a.userProviders.ProviderConfig(a.ctx, userID, preference.Provider)
	if cfgErr != nil {
		log.Printf("User provider lookup failed for user %d: %v", userID, cfgErr)

		return nil
	}
	if !found {
		return nil
	}

	cfg.Models = []string{preference.Model}

	backend, err := provider.NewProvider(cfg)
	if err != nil {
		log.Printf("User provider %q could not be built for user %d: %v", cfg.Name, userID, err)

		return nil
	}

	return []provider.Candidate{provider.NewCandidate(backend, preference.Model)}
}

// resolveUserProvider finds one of a user's providers by number or by name.
func resolveUserProvider(list []userproviders.Provider, choice string) (userproviders.Provider, bool) {
	if index, err := strconv.Atoi(choice); err == nil {
		if index < 1 || index > len(list) {
			return userproviders.Provider{}, false
		}

		return list[index-1], true
	}

	for _, record := range list {
		if strings.EqualFold(record.Name, choice) {
			return record, true
		}
	}

	return userproviders.Provider{}, false
}

func indexOfUserProvider(list []userproviders.Provider, id string) int {
	for i, record := range list {
		if strings.EqualFold(record.ID, id) {
			return i
		}
	}

	return 0
}

// hostOf renders a base URL without scheme or path. It never touches the key.
func hostOf(rawURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Host == "" {
		return strings.TrimSpace(rawURL)
	}

	return parsed.Host
}

// -----------------------------------------------------------------------------
// BUTTONS
// -----------------------------------------------------------------------------

// startProviderFlow opens the name step of /addprovider from a button.
func (a *app) startProviderFlow(query *tgbotapi.CallbackQuery, conf *config.Config, tracker *user.UsageTracker) {
	if !a.userProvidersReady() {
		a.editScreen(query.Message.Chat.ID, query.Message.MessageID, screen{
			Text:     userProvidersDisabledText,
			Keyboard: ui.UserProviderList(nil, 0, providersPageSize),
		})

		return
	}

	a.setFlow(query.From.ID, &providerFlow{Step: flowStepName})

	a.editScreen(query.Message.Chat.ID, query.Message.MessageID, screen{
		Text: "🔐 <b>Add your provider</b>\n\n" +
			"Enter a name for it, for example <code>Mistral</code> or <code>My Groq</code>.\n\n" +
			"Send /cancel to stop.",
		Keyboard: tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("⬅️ Your providers", ui.Encode(ui.ActionUserProviders)),
		)),
	})
}

// showChainModels renders one chain provider's models and remembers the view,
// so a background refresh knows the message still shows that provider.
func (a *app) showChainModels(chatID, messageID int64, conf *config.Config, tracker *user.UsageTracker, index, page int) {
	a.rememberModelsView(chatID, messageID, modelsView{index: index, page: page})
	a.editScreen(chatID, messageID, a.modelsScreen(conf, tracker, index, page))
	a.refreshChainModels(index, chatID, messageID, conf, tracker)
}

// showUserProviderModels renders one of the user's own providers and remembers
// the view the same way.
func (a *app) showUserProviderModels(chatID, messageID int64, userID int64, tracker *user.UsageTracker, index, page int) {
	a.rememberModelsView(chatID, messageID, modelsView{index: index, page: page, user: true})
	a.editScreen(chatID, messageID, a.userProviderModelsScreen(userID, tracker, index, page))
	a.refreshUserProviderModels(userID, chatID, messageID, tracker, index, page, false)
}

// selectUserProviderModel pins a model from one of the user's own providers.
func (a *app) selectUserProviderModel(query *tgbotapi.CallbackQuery, tracker *user.UsageTracker, providerIndex, modelIndex int) {
	userID := query.From.ID

	list, err := a.userProviderList(userID)
	if err != nil || providerIndex < 0 || providerIndex >= len(list) {
		a.toast(query, "That provider is gone, try again")

		return
	}

	record := list[providerIndex]
	modelIDs := a.userProviderModelIDs(record)

	if modelIndex < 0 || modelIndex >= len(modelIDs) {
		a.toast(query, "That model is gone, try again")

		return
	}

	model := modelIDs[modelIndex]
	tracker.SetModel(model, record.Name)

	a.toast(query, "Pinned "+truncateCallbackText(model)+" ✅")
	a.showUserProviderModels(query.Message.Chat.ID, query.Message.MessageID, userID, tracker, providerIndex, 0)
}

// fetchUserProviderModels lists what a user's own key can use, while they are
// adding that provider. The key is decrypted for this one request only.
func (a *app) fetchUserProviderModels(query *tgbotapi.CallbackQuery, conf *config.Config, tracker *user.UsageTracker, id string) {
	if a.modelCatalog == nil || !a.userProvidersReady() {
		return
	}

	chatID := query.Message.Chat.ID
	userID := query.From.ID

	record, ok, err := a.userProviders.Get(a.ctx, userID, id)
	if err != nil || !ok {
		a.toast(query, "Provider not found")

		return
	}

	apiKey, ok, err := a.userProviders.APIKey(a.ctx, userID, id)
	if err != nil || !ok {
		a.toast(query, "Could not read the stored key")

		return
	}

	a.toast(query, "Fetching models 🔎")

	// The lookup runs off the update loop: a slow provider must not stall
	// everybody else's messages.
	go func() {
		list, err := a.modelCatalog.Discover(a.ctx, models.CacheKey("", record.ID), record.BaseURL, apiKey)
		if err != nil || len(list) == 0 {
			// The provider error is never shown, and it never carried the key.
			a.send(chatID, "⚠️ Could not fetch models right now.\nYou can still type a model id manually.", "")

			return
		}

		ids := make([]string, 0, len(list))
		lines := make([]string, 0, modelsPageLimit)

		for i, model := range list {
			ids = append(ids, model.ModelID)

			if i < modelsPageLimit {
				lines = append(lines, fmt.Sprintf("%d. <code>%s</code>", i+1, escapeHTML(model.ModelID)))
			}
		}

		a.rememberFetchedModels(userID, ids)

		a.send(chatID, fmt.Sprintf(
			"🤖 <b>%s</b> models\n\n%s\n\nReply with a number, or type a model id.",
			escapeHTML(record.Name),
			strings.Join(lines, "\n"),
		), "HTML")
	}()
}

// -----------------------------------------------------------------------------
// BACKGROUND DISCOVERY
// -----------------------------------------------------------------------------

// refreshChainModels keeps the picker honest: when the cached catalogue of a
// chain provider is stale, the live list is fetched in the background and the
// screen is updated in place. The user is never blocked by the request, and a
// screen they already left is never overwritten.
func (a *app) refreshChainModels(index int, chatID, messageID int64, conf *config.Config, tracker *user.UsageTracker) {
	if a.modelCatalog == nil || index < 0 {
		return
	}

	names := a.chain.Names()
	if index >= len(names) {
		return
	}

	name := names[index]

	baseURL, apiKey, ok := a.chain.EndpointOf(name)
	if !ok || strings.TrimSpace(baseURL) == "" {
		return
	}

	a.modelCatalog.RefreshAsync(models.CacheKey(name, ""), baseURL, func() string { return apiKey },
		func(fetched []models.Model, err error) {
			if err != nil || len(fetched) == 0 {
				return
			}

			view, showing := a.modelsView(chatID, messageID)
			if !showing || view.user || view.index != index {
				return
			}

			a.editScreen(chatID, messageID, a.modelsScreen(conf, tracker, index, view.page))
		})
}

// refreshUserProviderModels does the same for one of the user's own
// providers, using that user's key.
func (a *app) refreshUserProviderModels(
	userID int64,
	chatID, messageID int64,
	tracker *user.UsageTracker,
	index, page int,
	force bool,
) {
	if a.modelCatalog == nil || !a.userProvidersReady() {
		return
	}

	list, err := a.userProviderList(userID)
	if err != nil || index < 0 || index >= len(list) {
		return
	}

	record := list[index]
	view := modelsView{index: index, page: page, user: true}
	key := models.CacheKey("", record.ID)

	if force {
		// The refresh button drops the cached list on purpose.
		a.modelCatalog.Invalidate(key)
	}

	a.modelCatalog.RefreshAsync(key, record.BaseURL, func() string {
		// The key is opened here, used for this one request and dropped.
		apiKey, _, err := a.userProviders.APIKey(a.ctx, userID, record.ID)
		if err != nil {
			return ""
		}

		return apiKey
	}, func(fetched []models.Model, err error) {
		if !a.showingModels(chatID, messageID, view) {
			return
		}

		if err != nil || len(fetched) == 0 {
			a.send(chatID, "⚠️ Could not fetch models right now.", "")

			return
		}

		a.editScreen(chatID, messageID, a.userProviderModelsScreen(userID, tracker, index, page))
	})
}

func (a *app) rememberModelsView(chatID, messageID int64, view modelsView) {
	a.modelViews.Store(viewKey{chatID: chatID, messageID: messageID}, view)
}

// modelsView returns the provider screen a message currently shows.
func (a *app) modelsView(chatID, messageID int64) (modelsView, bool) {
	value, ok := a.modelViews.Load(viewKey{chatID: chatID, messageID: messageID})
	if !ok {
		return modelsView{}, false
	}

	view, ok := value.(modelsView)

	return view, ok
}

// showingModels reports whether a message still displays the given view.
func (a *app) showingModels(chatID, messageID int64, want modelsView) bool {
	view, ok := a.modelsView(chatID, messageID)

	return ok && view == want
}
