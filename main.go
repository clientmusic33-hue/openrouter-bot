// Command openrouter-bot runs a Telegram bot that answers with AI models
// served through OpenRouter, Groq, Gemini or any OpenAI-compatible endpoint.
//
// The bot is designed to be usable without commands: every action has a
// button, and the AI answer itself carries stop / regenerate / feedback
// controls.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"openrouter-bot/agent"
	"openrouter-bot/api"
	"openrouter-bot/config"
	"openrouter-bot/features/memory"
	"openrouter-bot/features/reminders"
	"openrouter-bot/groups"
	"openrouter-bot/internal/cache"
	"openrouter-bot/internal/models"
	"openrouter-bot/internal/ratelimit"
	"openrouter-bot/internal/security"
	"openrouter-bot/internal/telemetry"
	"openrouter-bot/lang"
	"openrouter-bot/provider"
	"openrouter-bot/storage"
	"openrouter-bot/tools"
	"openrouter-bot/translator"
	"openrouter-bot/ui"
	"openrouter-bot/user"
	"openrouter-bot/userproviders"
	"openrouter-bot/workers"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const (
	shutdownTimeout    = 30 * time.Second
	translationTimeout = 60 * time.Second

	// pendingTTL is how long the buttons under an answer keep working.
	pendingTTL = 3 * time.Hour
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ---------------------------------------------------------------------
	// TRANSLATIONS
	// ---------------------------------------------------------------------
	//
	// Loaded exactly once. Reloading per message rewrote a package level map
	// that other goroutines were reading concurrently.

	if err := lang.LoadTranslations("./lang/"); err != nil {
		log.Fatalf("Error loading translations: %v", err)
	}

	// ---------------------------------------------------------------------
	// CONFIGURATION
	// ---------------------------------------------------------------------

	manager, err := config.NewManager("./config.yaml")
	if err != nil {
		log.Fatalf("Error initializing config manager: %v", err)
	}

	conf := manager.GetConfig()

	// ---------------------------------------------------------------------
	// TELEGRAM BOT
	// ---------------------------------------------------------------------

	if conf.TelegramBotToken == "" {
		log.Fatal("TELEGRAM_BOT_TOKEN is not set")
	}

	bot, err := newBotAPI(conf)
	if err != nil {
		log.Fatalf("Failed to create Telegram bot: %v", err)
	}

	bot.Debug = false
	log.Printf("Authorized as @%s", bot.Self.UserName)

	// ---------------------------------------------------------------------
	// PROVIDER CHAIN
	// ---------------------------------------------------------------------

	providers := conf.Providers
	if len(providers) == 0 {
		// No providers block in config.yaml, so fall back to the flat
		// environment variables plus every preset with a key.
		providers = config.FallbackProviders(conf)
	}

	chain, err := provider.NewChain(providers)
	if err != nil {
		log.Fatalf("Failed to build provider chain: %v", err)
	}

	for _, status := range chain.Status() {
		switch {
		case !status.Configured:
			log.Printf("Provider %s listed without an API key, requests will skip it", status.Name)
		default:
			log.Printf("Provider %s ready: %d model(s)", status.Name, len(status.Models))
		}
	}

	// ---------------------------------------------------------------------
	// APPLICATION
	// ---------------------------------------------------------------------

	redisClient := cache.NewRedisFromEnv()
	store := storage.Open(conf.StorageType, "data", conf.PostgresDSN)
	respCache := cache.NewResponseCache(512, 15*time.Minute, redisClient)
	limiter := ratelimit.New(redisClient)
	memMgr := memory.NewManager(store)
	optimizer := memory.NewOptimizer()
	remMgr := reminders.NewManager(store)
	toolReg := tools.DefaultRegistry(chain, remMgr)
	aiAgent := agent.New(chain, toolReg)
	metrics := telemetry.NewCollector()

	// The master key that seals user API keys lives in the environment only.
	// Without it the "bring your own key" feature stays off rather than
	// storing anything in plaintext.
	userProviderStore := userproviders.NewStore(store, security.MasterKeyFromEnv())
	if !userProviderStore.Enabled() {
		log.Printf(
			"User providers disabled: set %s to let users add their own API keys",
			security.EncryptionKeyEnv,
		)
	}

	app := &app{
		ctx:           ctx,
		bot:           bot,
		manager:       manager,
		chain:         chain,
		users:         user.NewUserManager(logsDir()),
		groups:        groups.NewManager("data/groups.json"),
		store:         store,
		cache:         respCache,
		limiter:       limiter,
		memory:        memMgr,
		optimizer:     optimizer,
		reminders:     remMgr,
		agent:         aiAgent,
		metrics:       metrics,
		userProviders: userProviderStore,
		modelCatalog:  models.New(),
		semaphore:     make(chan struct{}, conf.MaxConcurrentRequests),
		pending:       make(map[int64]map[int]pendingAnswer),
		warned:        make(map[warnKey]time.Time),
		flows:         make(map[int64]*providerFlow),
		fetched:       make(map[int64][]string),
		started:       time.Now(),
	}

	workers.StartReminderScheduler(ctx, remMgr, 5*time.Second, app.deliverReminder)

	app.setCommands(manager.GetConfig())

	// ---------------------------------------------------------------------
	// HEALTH SERVER & KEEP-ALIVE
	// ---------------------------------------------------------------------

	server := app.startHealthServer()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	app.startKeepAlive()

	// ---------------------------------------------------------------------
	// UPDATE LOOP
	// ---------------------------------------------------------------------

	app.ready.Store(true)
	log.Printf("Bot is running, waiting for updates | user data=%s", logsDir())

	app.run()

	// ---------------------------------------------------------------------
	// GRACEFUL SHUTDOWN
	// ---------------------------------------------------------------------

	log.Println("Shutting down, waiting for in-flight requests")

	bot.StopReceivingUpdates()

	done := make(chan struct{})
	go func() {
		app.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		log.Println("All requests finished")
	case <-time.After(shutdownTimeout):
		log.Printf("Timed out after %s waiting for requests to finish", shutdownTimeout)
	}

	log.Println("Goodbye")
}

// logsDir is where per-user preferences, favourites and spend live. It is
// configurable so a container can point it at a mounted volume; the default
// keeps the original layout next to the binary.
func logsDir() string {
	dir := strings.TrimSpace(os.Getenv("LOGS_DIR"))
	if dir == "" {
		dir = "logs"
	}

	return dir
}

// newBotAPI connects to api.telegram.org, or to a self-hosted Bot API server
// when telegram_api_url is configured.
func newBotAPI(conf *config.Config) (*tgbotapi.BotAPI, error) {
	if conf.TelegramAPIURL != "" {
		log.Printf("Using custom Telegram API endpoint %s", conf.TelegramAPIURL)
		bot, err := tgbotapi.NewBotAPIWithAPIEndpoint(conf.TelegramBotToken, conf.TelegramAPIURL)
		if err != nil {
			return nil, err
		}

		return bot, nil
	}

	return tgbotapi.NewBotAPI(conf.TelegramBotToken)
}

// -----------------------------------------------------------------------------
// APPLICATION
// -----------------------------------------------------------------------------

type app struct {
	ctx     context.Context
	bot     *tgbotapi.BotAPI
	manager *config.Manager

	// chain is the ordered list of AI providers tried on every request.
	chain *provider.Chain

	users     *user.Manager
	groups    *groups.Manager
	store     storage.Store
	cache     *cache.ResponseCache
	limiter   *ratelimit.Limiter
	memory    *memory.Manager
	optimizer *memory.Optimizer
	reminders *reminders.Manager
	agent     *agent.Agent
	metrics   *telemetry.Collector

	// userProviders holds the providers users bring themselves, with their
	// API keys sealed under the server master key.
	userProviders *userproviders.Store
	// modelCatalog caches the model lists discovered from the providers.
	modelCatalog *models.Catalog

	// refreshMu guards the global catalogue refresh: one refresh at a time,
	// and never more often than the minimum interval.
	refreshMu   sync.Mutex
	refreshing  bool
	lastRefresh time.Time

	// searchMu guards the model search text a chat is filtering on.
	searchMu sync.Mutex
	search   map[int64]string

	// notify replaces send when it is set. The catalogue refresh reports
	// through it so the whole flow can be exercised without a bot.
	notify func(chatID int64, text, mode string)

	// flowMu guards the in progress /addprovider conversations.
	flowMu sync.Mutex
	flows  map[int64]*providerFlow

	// fetchedMu guards the numbered model lists a user is choosing from
	// while adding a provider. Only the update loop touches the flows
	// themselves, so the two are kept apart on purpose.
	fetchedMu sync.Mutex
	fetched   map[int64][]string

	// modelViews remembers which provider screen a message shows, so a
	// background model refresh never overwrites a different menu.
	modelViews sync.Map

	// semaphore caps concurrent AI requests so that a burst of group
	// messages cannot exhaust the upstream rate limit.
	semaphore chan struct{}

	// pending links the buttons under an answer back to the question that
	// produced it, which is what makes regenerate and feedback possible.
	pendingMu sync.Mutex
	pending   map[int64]map[int]pendingAnswer

	// warned remembers when a restricted user was last told why the bot
	// stayed silent, so the explanation does not repeat on every message.
	warnMu sync.Mutex
	warned map[warnKey]time.Time

	started time.Time

	wg    sync.WaitGroup
	ready atomic.Bool
}

// pendingAnswer is the state a button press needs.
type pendingAnswer struct {
	ChatID    int64
	MessageID int
	UserID    int64
	Prompt    string
	Model     string
	CreatedAt time.Time
}

// run consumes Telegram updates in a resilient loop, automatically restarting
// polling on crash or channel closure until the context is cancelled.
func (a *app) run() {
	for {
		select {
		case <-a.ctx.Done():
			return
		default:
		}

		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("Polling recovered from panic: %v", r)
				}
			}()

			if _, err := a.bot.Request(tgbotapi.DeleteWebhookConfig{}); err != nil {
				log.Printf("Warning: failed to delete webhook: %v", err)
			}

			u := tgbotapi.NewUpdate(0)
			u.Timeout = 60
			updates := a.bot.GetUpdatesChan(u)

			for {
				select {
				case <-a.ctx.Done():
					return
				case update, ok := <-updates:
					if !ok {
						log.Println("Updates channel closed, restarting polling...")
						return
					}
					a.processUpdate(update)
				}
			}
		}()

		select {
		case <-a.ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
}

func (a *app) processUpdate(update tgbotapi.Update) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("Recovered from panic in processUpdate: %v", r)
			if update.Message != nil && update.Message.Chat != nil {
				a.send(update.Message.Chat.ID, "❌ Something went wrong. Please try again.", "")
			}
		}
	}()

	// Read a fresh snapshot: the configuration may have been reloaded.
	conf := a.manager.GetConfig()

	if update.CallbackQuery != nil {
		a.handleCallback(update.CallbackQuery, conf)
		return
	}

	// The bot joining, leaving or being promoted in a chat.
	if update.MyChatMember != nil {
		a.handleChatMember(update.MyChatMember, conf)
		return
	}

	if update.Message == nil {
		return
	}

	// Ignore messages sent by other bots.
	if update.Message.From != nil && update.Message.From.IsBot {
		return
	}

	senderID, senderUsername := senderInfo(update.Message)
	if senderID == 0 {
		log.Printf("Ignoring message without a resolvable sender in chat %d", update.Message.Chat.ID)
		return
	}

	tracker := a.users.GetUser(update.Message.Chat.ID, senderID, senderUsername, conf)

	logText := update.Message.Text
	if a.flowAwaitingSecret(senderID) {
		// A user typing their API key must never have it land in the log.
		logText = "[REDACTED_API_KEY]"
	}

	log.Printf(
		"INCOMING MESSAGE: chat=%d type=%s sender_id=%d username=%q text=%q",
		update.Message.Chat.ID,
		update.Message.Chat.Type,
		senderID,
		senderUsername,
		logText,
	)

	if isGroupChat(update.Message.Chat) {
		a.trackChat(update.Message.Chat, conf)
		settings := a.groups.Get(update.Message.Chat.ID)
		if !settings.MemoryDisabled && !update.Message.IsCommand() {
			senderLabel := senderUsername
			if senderLabel != "" {
				senderLabel = "@" + senderLabel
			} else {
				senderLabel = strconv.FormatInt(senderID, 10)
			}
			a.groups.RecordMessage(update.Message.Chat.ID, senderLabel, messageText(update.Message))
		}
	}

	if update.Message.IsCommand() {
		a.handleCommand(update.Message, conf, tracker)
		return
	}

	// The persistent keyboard sends plain text, not commands, so the labels
	// are routed here. Restricted groups only get the menus, never an answer.
	if action, ok := quickAction(update.Message.Text); ok {
		if a.mayUse(update.Message.Chat, senderID, conf) {
			a.handleQuickAction(update.Message, action, conf, tracker)
		}
		return
	}

	// -----------------------------------------------------------------
	// ACCESS CONTROL
	// -----------------------------------------------------------------

	if !a.mayUse(update.Message.Chat, senderID, conf) {
		if isGroupChat(update.Message.Chat) {
			if a.isMentioned(update.Message) || a.isReplyToBot(update.Message) {
				a.notifyRestricted(update.Message, conf)
			}
		} else {
			a.warnRestrictedPrivate(update.Message.Chat.ID, senderID, conf)
		}
		return
	}

	// -----------------------------------------------------------------
	// AUTOMATIC GROUP TRANSLATION
	// -----------------------------------------------------------------

	// A started /addprovider conversation owns the next message: the answer
	// is a name, a URL, a key or a model, never a prompt for the AI.
	if a.consumeProviderFlow(update.Message, conf, tracker) {
		return
	}

	if isGroupChat(update.Message.Chat) {
		settings := a.groups.Get(update.Message.Chat.ID)
		text := strings.TrimSpace(messageText(update.Message))

		if settings.TranslateEnabled && text != "" {
			a.startTranslation(update.Message, settings.TargetLanguage, conf)

			// Translation mode replaces the chat mode for this group.
			// Without this return every message would also trigger a full
			// AI completion.
			return
		}
	}

	// -----------------------------------------------------------------
	// NORMAL AI CHAT
	// -----------------------------------------------------------------

	if !a.shouldAnswer(update.Message, conf) {
		return
	}

	if update.Message.Voice != nil || update.Message.Audio != nil {
		a.handleVoice(update.Message, conf, tracker)
		return
	}

	if update.Message.Document != nil && !strings.HasPrefix(strings.ToLower(update.Message.Document.MimeType), "image/") {
		a.handleDocument(update.Message, conf, tracker)
		return
	}

	a.wg.Add(1)
	go a.handleChat(update.Message, conf, tracker)
}

// handleChat runs one AI request. It is started in its own goroutine and
// bounded by the concurrency semaphore.
func (a *app) handleChat(message *tgbotapi.Message, conf *config.Config, tracker *user.UsageTracker) {
	defer a.wg.Done()
	defer func() {
		if r := recover(); r != nil {
			log.Printf("Recovered from panic in handleChat: %v", r)
			a.send(message.Chat.ID, "❌ Could not answer right now, please try again.", "")
		}
	}()

	select {
	case a.semaphore <- struct{}{}:
		defer func() { <-a.semaphore }()
	case <-a.ctx.Done():
		return
	}

	if !tracker.HaveAccess(conf) {
		a.send(message.Chat.ID, lang.Translate("budget_out", conf.Lang), "")
		return
	}

	if err := tracker.AllowRequest(conf); err != nil {
		log.Printf("Rate limiting user %s: %v", tracker.UserID, err)
		a.send(message.Chat.ID, lang.Translate("rate_limit", conf.Lang), "")
		return
	}
	if a.limiter != nil {
		cat := ratelimit.CategoryChat
		if len(message.Photo) > 0 || (message.Document != nil && strings.HasPrefix(strings.ToLower(message.Document.MimeType), "image/")) {
			cat = ratelimit.CategoryImage
		}
		if err := a.limiter.Allow(tracker.UserID, tracker.GetUserRole(conf), cat, conf.RateLimitPerMinute); err != nil {
			log.Printf("Tiered rate limit user %s: %v", tracker.UserID, err)
			a.send(message.Chat.ID, lang.Translate("rate_limit", conf.Lang), "")
			return
		}
	}

	prompt := messageText(message)

	log.Printf(
		"AI REQUEST: chat=%d sender_id=%s username=%q text=%q",
		message.Chat.ID,
		tracker.UserID,
		tracker.UserName,
		prompt,
	)

	tracker.RecordRequest(len(strings.TrimSpace(prompt)))
	if len(message.Photo) > 0 || (message.Document != nil && strings.HasPrefix(strings.ToLower(message.Document.MimeType), "image/")) {
		tracker.MarkVision()
	}

	done := a.metrics.BeginRequest()
	opts := a.chatOptions(conf, tracker)
	opts.SystemSupplement = a.buildContextSupplement(message.Chat.ID, prompt, conf, tracker)

	result, err := api.HandleChatGPTStreamResponse(
		a.ctx,
		a.bot,
		a.chain,
		message,
		conf,
		tracker,
		opts,
	)
	done(result.Model, memory.EstimateTokens(prompt+result.Text), err)
	if err != nil {
		log.Printf("AI request failed for user %s: %v", tracker.UserID, err)
		if len(result.Messages) == 0 {
			errMsg := lang.Translate("errorText", conf.Lang)
			if errMsg == "" {
				errMsg = "❌ Could not answer right now, please try again."
			}
			a.send(message.Chat.ID, errMsg, "")
		}
	}

	// Buttons under the answer need to find the question again.
	if len(result.Messages) > 0 && result.Text != "" && !result.Stopped {
		a.rememberAnswer(message.Chat.ID, result.Messages[0], senderID(message), prompt, result.Model)
	}

	// Cost statistics: only the provider that answered knows the price, and
	// only OpenRouter-style endpoints expose a generation lookup. Anything
	// else simply has no cost to add.
	if baseURL, apiKey, ok := a.chain.CostEndpoint(result.Provider); ok {
		if err := tracker.GetUsageFromApi(result.ResponseID, baseURL, apiKey); err != nil {
			log.Printf("Failed to fetch usage for user %s: %v", tracker.UserID, err)
		}
	}

	a.maybeSuggestModel(message.Chat.ID, conf, tracker)
}

// chatOptions assembles the per-request rendering options from the user's
// stored settings.
func (a *app) chatOptions(conf *config.Config, tracker *user.UsageTracker) api.Options {
	settings := tracker.Settings()
	profile := tracker.UsageProfile()

	return api.Options{
		Preference: tracker.Preference(),
		Profile:    &profile,
		ShowFooter: settings.ShowFooter,
		Streaming:  settings.Streaming,
		Markdown:   conf.RenderMarkdown,
		MaxChars:   conf.MaxReplyChars,
		Buttons:    true,
		// A provider the user brought themselves leads the walk. Its key is
		// decrypted for this request only.
		UserCandidates: a.userCandidate(tracker),
	}
}

// -----------------------------------------------------------------------------
// GROUP BEHAVIOUR
// -----------------------------------------------------------------------------

// shouldAnswer decides whether the bot answers a non-command message.
func (a *app) shouldAnswer(message *tgbotapi.Message, conf *config.Config) bool {
	if !isGroupChat(message.Chat) {
		return true
	}

	switch conf.GroupChatMode {
	case config.GroupModeOff:
		return false
	case config.GroupModeAll:
		return true
	default: // config.GroupModeMention
		return a.isReplyToBot(message) || a.isMentioned(message)
	}
}

func (a *app) isReplyToBot(message *tgbotapi.Message) bool {
	return message.ReplyToMessage != nil &&
		message.ReplyToMessage.From != nil &&
		message.ReplyToMessage.From.ID == a.bot.Self.ID
}

func (a *app) isMentioned(message *tgbotapi.Message) bool {
	username := strings.ToLower(a.bot.Self.UserName)
	if username == "" {
		return false
	}

	if strings.Contains(strings.ToLower(messageText(message)), "@"+username) {
		return true
	}

	// A reply to any message of the bot counts, including replies to an
	// answer that was rendered as several messages.
	return a.isReplyToBot(message)
}

// trackChat records the chat title and the bot's own membership.
func (a *app) trackChat(chat *tgbotapi.Chat, conf *config.Config) {
	if chat == nil {
		return
	}

	if strings.TrimSpace(chat.Title) != "" {
		a.groups.SetTitle(chat.ID, chat.Title)
	}
}

// memberRole resolves a member's role, caching the Telegram lookup so the API
// is not called for every message.
func (a *app) memberRole(chatID, userID int64) string {
	if role, ok := a.groups.MemberRole(chatID, userID); ok {
		return role
	}

	member, err := a.bot.GetChatMember(tgbotapi.GetChatMemberConfig{
		ChatConfigWithUser: tgbotapi.ChatConfigWithUser{
			ChatID: chatID,
			UserID: userID,
		},
	})
	if err != nil {
		log.Printf("Could not resolve member %d in chat %d: %v", userID, chatID, err)
		return ""
	}

	a.groups.SetMemberRole(chatID, userID, member.Status)

	return member.Status
}

// accessMode returns the effective access mode for a chat.
func (a *app) accessMode(chatID int64, conf *config.Config) string {
	mode := strings.ToLower(strings.TrimSpace(a.groups.Get(chatID).Access))
	if !config.ValidGroupAccess(mode) {
		mode = conf.GroupAccess
	}
	if !config.ValidGroupAccess(mode) {
		mode = config.AccessEveryone
	}

	return mode
}

// permitted decides an access mode against what the bot knows about a user.
// It is the single place where the everyone/admins/owner rule lives, so a
// private chat and a group can never drift apart.
//
// The chat-admin check is a function so that it only runs when the mode asks
// for it: a public group must not spend a Telegram lookup on every member.
func permitted(mode string, isOwner bool, isChatAdmin func() bool) bool {
	switch mode {
	case config.AccessOwner:
		return isOwner
	case config.AccessAdmins:
		return isOwner || (isChatAdmin != nil && isChatAdmin())
	default:
		return true
	}
}

// mayUse answers "is this user allowed to talk to the bot here?".
//
// A private chat follows PRIVATE_ACCESS (everyone by default, the owner when
// the bot is meant as a personal assistant; PUBLIC_MODE always opens it). A
// group follows its own setting, which any chat admin can change with /group.
func (a *app) mayUse(chat *tgbotapi.Chat, userID int64, conf *config.Config) bool {
	owner := conf.IsAdmin(userID)

	if !isGroupChat(chat) {
		return permitted(conf.PrivateAccess, owner, nil)
	}

	return permitted(a.accessMode(chat.ID, conf), owner, func() bool {
		return groups.IsAdminRole(a.memberRole(chat.ID, userID))
	})
}

// warnKey identifies one kind of restriction notice for one user, so the
// group notice and the direct-message notice do not silence each other.
type warnKey struct {
	UserID int64
	Kind   string
}

// warnOnce reports whether a notice should be sent. Repeated mentions in a
// group, or a stream of messages from a stranger, would otherwise turn the
// explanation into spam and into Telegram rate limits; one per hour is enough
// to make the situation clear.
func (a *app) warnOnce(userID int64, kind string) bool {
	a.warnMu.Lock()
	defer a.warnMu.Unlock()

	// The map is created lazily so that a warning never depends on the
	// caller having initialised it.
	if a.warned == nil {
		a.warned = make(map[warnKey]time.Time)
	}

	key := warnKey{UserID: userID, Kind: kind}
	now := time.Now()

	if last, ok := a.warned[key]; ok && now.Sub(last) < time.Hour {
		return false
	}
	a.warned[key] = now

	return true
}

// warnRestrictedPrivate tells a stranger once why the bot stayed silent in a
// direct chat. One reply per hour is enough to explain the situation without
// turning the chat into a fight with the messages the bot is refusing.
func (a *app) warnRestrictedPrivate(chatID, userID int64, conf *config.Config) {
	if !a.warnOnce(userID, "private") {
		return
	}

	hint := "🔒 This bot only answers its owner."
	if strings.EqualFold(conf.PrivateAccess, config.AccessAdmins) {
		hint = "🔒 This bot only answers its owner in direct messages."
	}

	a.send(chatID, hint+"\nAsk the owner for access, or use the bot in a group.", "")
}

// isChatAdmin reports whether a user administers the chat (or owns the bot).
func (a *app) isChatAdmin(chat *tgbotapi.Chat, userID int64, conf *config.Config) bool {
	if conf.IsAdmin(userID) {
		return true
	}
	if !isGroupChat(chat) {
		return false
	}

	return groups.IsAdminRole(a.memberRole(chat.ID, userID))
}

// notifyRestricted explains why the bot stayed silent. It is throttled per
// user, so mentioning the bot repeatedly does not turn into a fight with a
// notice.
func (a *app) notifyRestricted(message *tgbotapi.Message, conf *config.Config) {
	if sender := senderID(message); sender == 0 || !a.warnOnce(sender, "group") {
		return
	}

	mode := a.accessMode(message.Chat.ID, conf)

	hint := "🛡 In this group only administrators can use the bot."
	if mode == config.AccessOwner {
		hint = "🔒 In this group only the bot owner can use the bot."
	}

	a.send(message.Chat.ID, hint+"\nAn admin can change this with /group.", "")
}

// handleChatMember reacts to the bot joining, leaving or being promoted.
func (a *app) handleChatMember(update *tgbotapi.ChatMemberUpdated, conf *config.Config) {
	chatID := update.Chat.ID

	if strings.TrimSpace(update.Chat.Title) != "" {
		a.groups.SetTitle(chatID, update.Chat.Title)
	}

	status := string(update.NewChatMember.Status)
	a.groups.SetBotAdmin(chatID, groups.IsAdminRole(status))

	switch status {
	case "member", "administrator":
		if update.OldChatMember.Status == "left" || update.OldChatMember.Status == "kicked" {
			// Freshly added: introduce the bot and its buttons.
			text := "👋 Thanks for adding me!\n\n" +
				"Mention me or reply to my messages and I will answer.\n" +
				"Admins can open the group panel with /group.\n\n" +
				"Your free AI assistant, no limits, no signup. 🤖✨"

			a.sendKeyboard(chatID, text, ui.GroupPanel(a.accessMode(chatID, conf), false, groups.IsAdminRole(status)), false)
		}
	case "left", "kicked":
		log.Printf("Removed from chat %d", chatID)
	}
}

// startTranslation translates one message into the group's language.
func (a *app) startTranslation(message *tgbotapi.Message, target string, conf *config.Config) {
	chatID := message.Chat.ID
	messageID := message.MessageID
	text := strings.TrimSpace(messageText(message))

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		defer func() {
			if r := recover(); r != nil {
				log.Printf("Recovered from panic in startTranslation: %v", r)
			}
		}()

		select {
		case a.semaphore <- struct{}{}:
			defer func() { <-a.semaphore }()
		case <-a.ctx.Done():
			return
		}

		ctx, cancel := context.WithTimeout(a.ctx, translationTimeout)
		defer cancel()

		log.Printf("Starting automatic translation: chat=%d message=%d target=%q", chatID, messageID, target)

		translated, err := translator.Translate(ctx, a.chain, text, target)
		if err != nil {
			log.Printf("Automatic translation FAILED: chat=%d message=%d error=%v", chatID, messageID, err)
			a.send(chatID, "❌ Automatic translation failed. Please try again.", "")
			return
		}

		log.Printf("Automatic translation SUCCESS: chat=%d message=%d", chatID, messageID)

		msg := tgbotapi.NewMessage(chatID, fmt.Sprintf("🌐 <b>%s</b>\n\n%s", escapeHTML(target), escapeHTML(translated)))
		msg.ParseMode = "HTML"
		msg.ReplyToMessageID = messageID

		if _, err := a.bot.Send(msg); err != nil {
			log.Printf("Failed to send automatic translation: chat=%d message=%d error=%v", chatID, messageID, err)
		}
	}()
}

// -----------------------------------------------------------------------------
// PENDING ANSWERS
// -----------------------------------------------------------------------------

// rememberAnswer stores the question behind an answer so its buttons work.
func (a *app) rememberAnswer(chatID int64, messageID int, userID int64, prompt, model string) {
	a.pendingMu.Lock()
	defer a.pendingMu.Unlock()

	a.sweepLocked()

	if a.pending[chatID] == nil {
		a.pending[chatID] = make(map[int]pendingAnswer)
	}
	a.pending[chatID][messageID] = pendingAnswer{
		ChatID:    chatID,
		MessageID: messageID,
		UserID:    userID,
		Prompt:    prompt,
		Model:     model,
		CreatedAt: time.Now(),
	}
}

// pendingAnswer returns the state for a message, if it is still fresh.
func (a *app) pendingAnswer(chatID int64, messageID int) (pendingAnswer, bool) {
	a.pendingMu.Lock()
	defer a.pendingMu.Unlock()

	a.sweepLocked()

	byMessage, ok := a.pending[chatID]
	if !ok {
		return pendingAnswer{}, false
	}

	answer, ok := byMessage[messageID]

	return answer, ok
}

func (a *app) forgetAnswer(chatID int64, messageID int) {
	a.pendingMu.Lock()
	defer a.pendingMu.Unlock()

	if byMessage, ok := a.pending[chatID]; ok {
		delete(byMessage, messageID)
	}
}

// sweepLocked drops expired entries. Called on every access, which is cheap
// because the map only holds recent answers.
func (a *app) sweepLocked() {
	cutoff := time.Now().Add(-pendingTTL)

	for chatID, byMessage := range a.pending {
		for messageID, answer := range byMessage {
			if answer.CreatedAt.Before(cutoff) {
				delete(byMessage, messageID)
			}
		}
		if len(byMessage) == 0 {
			delete(a.pending, chatID)
		}
	}
}

// -----------------------------------------------------------------------------
// HEALTH SERVER & KEEP-ALIVE
// -----------------------------------------------------------------------------

func (a *app) startHealthServer() *http.Server {
	port := strings.TrimSpace(os.Getenv("PORT"))
	if port == "" {
		port = "10000"
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OpenRouter Telegram Bot is running"))
	})

	healthHandler := func(w http.ResponseWriter, r *http.Request) {
		if !a.ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("starting"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	}

	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/healthz", healthHandler)

	server := &http.Server{
		Addr:              "0.0.0.0:" + port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("HTTP server listening on %s", server.Addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.ready.Store(false)
			log.Printf("HTTP server failed: %v", err)
		}
	}()

	return server
}

// startKeepAlive periodically pings the bot's public Render URL (or local
// health endpoint) every 10 minutes so Render free tier does not spin down
// after 15 minutes of inactivity.
func (a *app) startKeepAlive() {
	port := strings.TrimSpace(os.Getenv("PORT"))
	if port == "" {
		port = "10000"
	}

	target := strings.TrimSpace(os.Getenv("KEEP_ALIVE_URL"))
	if target == "" {
		target = strings.TrimSpace(os.Getenv("RENDER_EXTERNAL_URL"))
	}
	if target != "" {
		target = strings.TrimRight(target, "/") + "/health"
	} else {
		target = "http://127.0.0.1:" + port + "/health"
	}

	go func() {
		client := &http.Client{Timeout: 10 * time.Second}
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()

		for {
			select {
			case <-a.ctx.Done():
				return
			case <-ticker.C:
				req, err := http.NewRequestWithContext(a.ctx, http.MethodGet, target, nil)
				if err != nil {
					continue
				}
				resp, err := client.Do(req)
				if err != nil {
					log.Printf("Keep-alive ping failed (%s): %v", target, err)
					continue
				}
				_ = resp.Body.Close()
			}
		}
	}()
}

// -----------------------------------------------------------------------------
// HELPERS
// -----------------------------------------------------------------------------

// send writes a message, splitting it into Telegram sized chunks and retrying
// without formatting if the parse mode is rejected.
func (a *app) send(chatID int64, text string, parseMode string) {
	sendMessage(a.bot, chatID, text, parseMode)
}

// sendKeyboard writes a message with an inline keyboard.
func (a *app) sendKeyboard(chatID int64, text string, keyboard tgbotapi.InlineKeyboardMarkup, quick bool) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = tgbotapi.ModeHTML
	msg.ReplyMarkup = keyboard
	if quick {
		msg.ReplyMarkup = ui.QuickKeyboard()
	}

	if _, err := a.bot.Send(msg); err != nil {
		log.Printf("Failed to send message to chat %d: %v", chatID, err)
	}
}

func sendMessage(bot *tgbotapi.BotAPI, chatID int64, text string, parseMode string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}

	for _, chunk := range api.SplitMessage(text, config.ChunkLimit, 0) {
		if parseMode != "" {
			msg := tgbotapi.NewMessage(chatID, chunk)
			msg.ParseMode = parseMode

			_, err := bot.Send(msg)
			if err == nil {
				continue
			}

			// Telegram rejects the whole message when it cannot parse the
			// markup, so retry this chunk without formatting rather than
			// losing it.
			log.Printf("Sending chunk with parse mode %q failed, retrying as plain text: %v", parseMode, err)
		}

		msg := tgbotapi.NewMessage(chatID, chunk)
		if _, err := bot.Send(msg); err != nil {
			log.Printf("Failed to send message to chat %d: %v", chatID, err)
			return err
		}
	}

	return nil
}

func senderInfo(message *tgbotapi.Message) (int64, string) {
	if message.From != nil {
		return message.From.ID, message.From.UserName
	}
	if message.SenderChat != nil {
		return message.SenderChat.ID, message.SenderChat.UserName
	}

	return 0, ""
}

func senderID(message *tgbotapi.Message) int64 {
	id, _ := senderInfo(message)

	return id
}

// messageText returns the text or, for media messages, the caption.
func messageText(message *tgbotapi.Message) string {
	if message == nil {
		return ""
	}
	if message.Text != "" {
		return message.Text
	}

	return message.Caption
}

func isGroupChat(chat *tgbotapi.Chat) bool {
	return chat != nil && (chat.Type == "group" || chat.Type == "supergroup")
}

func formatMoney(value float64) string {
	return strconv.FormatFloat(value, 'f', 6, 64)
}

// escapeHTML escapes user and model supplied text before it is embedded in an
// HTML formatted Telegram message.
func escapeHTML(text string) string {
	replacer := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
	)

	return replacer.Replace(text)
}
