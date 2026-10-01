// Package config loads bot configuration from an optional .env file, the
// process environment and config.yaml (via viper).
package config

import (
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"github.com/spf13/viper"

	"openrouter-bot/provider"
)

// TelegramMaxMessageLength is the hard limit Telegram enforces on a single
// message. Anything longer is rejected with a 400, so every outgoing message
// has to be split below this value.
const TelegramMaxMessageLength = 4096

// ChunkLimit is the maximum size used for a single outgoing chunk. It stays
// comfortably below TelegramMaxMessageLength so that formatting added by
// parse modes can never push a message over the limit.
const ChunkLimit = 3800

// Group chat answering modes.
const (
	// GroupModeOff never answers in groups.
	GroupModeOff = "off"
	// GroupModeMention answers only when the bot is mentioned or replied to.
	GroupModeMention = "mention"
	// GroupModeAll answers every group message.
	GroupModeAll = "all"
)

// Config is an immutable snapshot of the configuration. A pointer handed out
// by the manager must never be modified by the caller.
type Config struct {
	TelegramBotToken string
	OpenAIApiKey     string
	GeminiAPIKey     string

	Model     ModelParameters
	MaxTokens int

	OpenAIBaseURL string
	SystemPrompt  string

	BudgetPeriod string
	UserBudget   float64
	GuestBudget  float64

	AdminChatIDs       []int64
	AllowedUserChatIDs []int64

	MaxHistorySize int
	MaxHistoryTime int

	// Token optimization & short-term memory summarization settings.
	MaxContextMessages int
	MaxContextTokens   int
	SummaryThreshold   int

	// Agent, file intelligence, and storage settings.
	MaxAgentSteps int
	MaxFileSize   int64
	StorageType   string
	PostgresDSN   string
	RedisURL      string

	Vision        bool
	VisionPrompt  string
	VisionDetails string

	StatsMinRole string
	Lang         string

	// GroupChatMode controls whether the bot answers in group chats.
	// One of GroupModeOff, GroupModeMention (default) or GroupModeAll.
	GroupChatMode string

	// RequestTimeout bounds the total duration of a single AI request,
	// including streaming.
	RequestTimeout time.Duration
	// StreamIdleTimeout bounds the gap between two streamed chunks. A stream
	// that stalls for longer than this is aborted.
	StreamIdleTimeout time.Duration
	// MaxConcurrentRequests caps how many AI requests run at once. It keeps a
	// burst of group messages from exhausting the OpenRouter rate limit or
	// the process memory.
	MaxConcurrentRequests int
	// RateLimitPerMinute caps the number of AI requests a single user may
	// start per minute. Zero disables rate limiting.
	RateLimitPerMinute int

	// Providers is the failover chain, best first. When empty it is
	// synthesised from BASE_URL/MODEL/API_KEY plus every preset whose API
	// key is present in the environment.
	Providers []provider.Config

	// MaxReplyChars caps one outgoing answer. Telegram allows 4096
	// characters per message; the default stays comfortably below that so
	// adding formatting cannot push a chunk over the limit.
	MaxReplyChars int
	// RenderMarkdown makes the final message try MarkdownV2 before falling
	// back to plain text. Off by default: the built-in persona answers in
	// plain text, and Telegram rejects unclosed markup outright.
	RenderMarkdown bool
	// SuggestModels enables the occasional "this model may suit you better"
	// hint based on the user's usage.
	SuggestModels bool
	// GroupAccess is the default access mode for a new group:
	// "everyone", "admins" or "owner". A group can override it with /group.
	GroupAccess string
	// PrivateAccess is who may use the bot in a direct chat: "everyone",
	// "admins" (the owner) or "owner". PUBLIC_MODE forces it to everyone,
	// because a public bot has to answer anybody who finds it.
	PrivateAccess string
	// PersonaPrompt holds the response style rules appended to every system
	// prompt.
	PersonaPrompt string

	// TelegramAPIURL points at a custom Bot API server. Empty means
	// api.telegram.org. The value keeps the two %s placeholders the Telegram
	// library fills with the token and the method name.
	TelegramAPIURL string

	// PublicMode removes every usage restriction: unlimited budget for all
	// roles, no rate limiting and answers to every group message. It exists
	// because those three settings always have to be changed together.
	PublicMode bool
}

// ModelParameters holds the sampling parameters sent to the model.
type ModelParameters struct {
	Type             string
	ModelName        string
	ModelNameDefault string
	FrequencyPenalty float64
	PresencePenalty  float64
	Temperature      float64
	TopP             float64
}

// Load reads the configuration. It never terminates the process: every
// failure is reported to the caller so that the bot can degrade gracefully
// instead of dying on a transient error.
func Load() (*Config, error) {
	if err := loadDotEnv(); err != nil {
		return nil, err
	}

	setDefaults()

	config := &Config{
		TelegramBotToken: os.Getenv("TELEGRAM_BOT_TOKEN"),
		OpenAIApiKey:     os.Getenv("API_KEY"),
		GeminiAPIKey:     os.Getenv("GEMINI_API_KEY"),
		Model: ModelParameters{
			Type:             viper.GetString("TYPE"),
			ModelName:        viper.GetString("MODEL"),
			ModelNameDefault: viper.GetString("MODEL"),
			FrequencyPenalty: viper.GetFloat64("FREQUENCY_PENALTY"),
			PresencePenalty:  viper.GetFloat64("PRESENCE_PENALTY"),
			Temperature:      viper.GetFloat64("TEMPERATURE"),
			TopP:             viper.GetFloat64("TOP_P"),
		},
		MaxTokens:     viper.GetInt("MAX_TOKENS"),
		OpenAIBaseURL: viper.GetString("BASE_URL"),

		SystemPrompt: viper.GetString("ASSISTANT_PROMPT"),

		BudgetPeriod: viper.GetString("BUDGET_PERIOD"),
		UserBudget:   viper.GetFloat64("USER_BUDGET"),
		GuestBudget:  viper.GetFloat64("GUEST_BUDGET"),

		AdminChatIDs:       getStrAsIntList("ADMIN_IDS"),
		AllowedUserChatIDs: getStrAsIntList("ALLOWED_USER_IDS"),

		MaxHistorySize: viper.GetInt("MAX_HISTORY_SIZE"),
		MaxHistoryTime: viper.GetInt("MAX_HISTORY_TIME"),

		MaxContextMessages: viper.GetInt("MAX_CONTEXT_MESSAGES"),
		MaxContextTokens:   viper.GetInt("MAX_CONTEXT_TOKENS"),
		SummaryThreshold:   viper.GetInt("SUMMARY_THRESHOLD"),

		MaxAgentSteps: viper.GetInt("MAX_AGENT_STEPS"),
		MaxFileSize:   viper.GetInt64("MAX_FILE_SIZE"),
		StorageType:   strings.ToLower(strings.TrimSpace(viper.GetString("STORAGE_TYPE"))),
		PostgresDSN:   strings.TrimSpace(viper.GetString("POSTGRES_DSN")),
		RedisURL:      strings.TrimSpace(viper.GetString("REDIS_URL")),

		Vision:        viper.GetBool("VISION"),
		VisionPrompt:  viper.GetString("VISION_PROMPT"),
		VisionDetails: viper.GetString("VISION_DETAIL"),

		StatsMinRole: strings.ToUpper(strings.TrimSpace(viper.GetString("STATS_MIN_ROLE"))),
		Lang:         strings.ToUpper(strings.TrimSpace(viper.GetString("LANG"))),

		GroupChatMode: strings.ToLower(strings.TrimSpace(viper.GetString("GROUP_CHAT_MODE"))),

		RequestTimeout:        time.Duration(viper.GetInt("REQUEST_TIMEOUT_SECONDS")) * time.Second,
		StreamIdleTimeout:     time.Duration(viper.GetInt("STREAM_IDLE_TIMEOUT_SECONDS")) * time.Second,
		MaxConcurrentRequests: viper.GetInt("MAX_CONCURRENT_REQUESTS"),
		RateLimitPerMinute:    viper.GetInt("RATE_LIMIT_PER_MINUTE"),

		Providers:      loadProviders(),
		PublicMode:     viper.GetBool("PUBLIC_MODE"),
		MaxReplyChars:  viper.GetInt("MAX_REPLY_CHARS"),
		RenderMarkdown: viper.GetBool("RENDER_MARKDOWN"),
		SuggestModels:  viper.GetBool("SUGGEST_MODELS"),
		GroupAccess:    strings.ToLower(strings.TrimSpace(viper.GetString("GROUP_ACCESS"))),
		PrivateAccess:  strings.ToLower(strings.TrimSpace(viper.GetString("PRIVATE_ACCESS"))),
		PersonaPrompt:  viper.GetString("PERSONA_PROMPT"),
		TelegramAPIURL: strings.TrimSpace(viper.GetString("TELEGRAM_API_URL")),
	}

	if config.Model.ModelName == "" {
		return nil, errors.New("no model configured: set MODEL in the environment or config.yaml")
	}

	if err := config.Validate(); err != nil {
		return nil, err
	}

	config.SystemPrompt = buildSystemPrompt(config)

	printConfig(config)

	return config, nil
}

// Validate rejects obviously broken configuration early and fills in
// remaining defaults.
func (c *Config) Validate() error {
	if c.Lang == "" {
		c.Lang = "EN"
	}

	switch c.GroupChatMode {
	case GroupModeOff, GroupModeMention, GroupModeAll:
	default:
		if c.GroupChatMode != "" {
			log.Printf("Unknown GROUP_CHAT_MODE %q, falling back to %q", c.GroupChatMode, GroupModeMention)
		}
		c.GroupChatMode = GroupModeMention
	}

	if c.BudgetPeriod == "" {
		return errors.New("set budget_period in the environment or config.yaml")
	}

	switch strings.ToLower(c.BudgetPeriod) {
	case "daily", "monthly", "total":
	default:
		return fmt.Errorf("invalid budget_period %q: use daily, monthly or total", c.BudgetPeriod)
	}
	c.BudgetPeriod = strings.ToLower(c.BudgetPeriod)

	if c.RequestTimeout <= 0 {
		c.RequestTimeout = 300 * time.Second
	}
	if c.StreamIdleTimeout <= 0 {
		c.StreamIdleTimeout = 120 * time.Second
	}
	if c.MaxConcurrentRequests <= 0 {
		c.MaxConcurrentRequests = 8
	}
	if c.RateLimitPerMinute < 0 {
		c.RateLimitPerMinute = 0
	}
	if c.MaxTokens <= 0 {
		c.MaxTokens = 2000
	}
	if c.MaxHistorySize <= 0 {
		c.MaxHistorySize = 10
	}
	if c.MaxHistoryTime <= 0 {
		c.MaxHistoryTime = 60
	}
	if c.MaxContextMessages <= 0 {
		c.MaxContextMessages = c.MaxHistorySize
	}
	if c.MaxContextTokens <= 0 {
		c.MaxContextTokens = 3000
	}
	if c.SummaryThreshold <= 0 {
		c.SummaryThreshold = 10
	}
	if c.MaxAgentSteps <= 0 {
		c.MaxAgentSteps = 5
	}
	if c.MaxFileSize <= 0 {
		c.MaxFileSize = 10 << 20 // 10 MiB
	}
	if c.StorageType == "" {
		c.StorageType = "json"
	}
	if c.OpenAIBaseURL == "" {
		c.OpenAIBaseURL = "https://openrouter.ai/api/v1"
	}
	if c.VisionPrompt == "" {
		c.VisionPrompt = "Describe the image"
	}
	if c.VisionDetails == "" {
		c.VisionDetails = "low"
	}
	if c.StatsMinRole == "" {
		c.StatsMinRole = "ADMIN"
	}
	if c.MaxReplyChars <= 0 {
		c.MaxReplyChars = DefaultMaxReplyChars
	}
	// Never hand Telegram more than it accepts, whatever the config says.
	if c.MaxReplyChars > TelegramMaxMessageLength-96 {
		c.MaxReplyChars = TelegramMaxMessageLength - 96
	}
	if !ValidGroupAccess(c.GroupAccess) {
		if c.GroupAccess != "" {
			log.Printf("Unknown GROUP_ACCESS %q, falling back to %q", c.GroupAccess, AccessEveryone)
		}
		c.GroupAccess = AccessEveryone
	}
	if !ValidPrivateAccess(c.PrivateAccess) {
		if c.PrivateAccess != "" {
			log.Printf("Unknown PRIVATE_ACCESS %q, falling back to %q", c.PrivateAccess, AccessEveryone)
		}
		c.PrivateAccess = AccessEveryone
	}
	if strings.TrimSpace(c.PersonaPrompt) == "" {
		c.PersonaPrompt = DefaultPersonaPrompt
	}

	if c.PublicMode {
		// A negative budget means unlimited in HaveAccess.
		c.UserBudget = -1
		c.GuestBudget = -1
		c.RateLimitPerMinute = 0
		c.GroupChatMode = GroupModeAll
		// A public bot answers anybody, in groups and in direct messages.
		// Restricting direct messages is the one thing PUBLIC_MODE overrules.
		if c.PrivateAccess != AccessEveryone {
			log.Printf("PUBLIC_MODE overrides PRIVATE_ACCESS=%s; direct messages are open", c.PrivateAccess)
		}
		c.PrivateAccess = AccessEveryone

		log.Printf(
			"PUBLIC MODE ENABLED: budgets are unlimited, rate limiting is off " +
				"and the bot answers every group message. Anyone who finds the " +
				"bot can spend your provider credits - set a hard limit on the " +
				"provider side.",
		)
	}

	return nil
}

// Clone returns a deep copy so that callers can hold a stable snapshot while
// the manager swaps in a reloaded configuration.
func (c *Config) Clone() *Config {
	if c == nil {
		return nil
	}

	copied := *c
	copied.AdminChatIDs = append([]int64(nil), c.AdminChatIDs...)
	copied.AllowedUserChatIDs = append([]int64(nil), c.AllowedUserChatIDs...)

	if c.Providers != nil {
		copied.Providers = make([]provider.Config, len(c.Providers))
		for i, p := range c.Providers {
			copied.Providers[i] = p
			copied.Providers[i].Models = append([]string(nil), p.Models...)
		}
	}

	return &copied
}

// loadDotEnv loads an optional .env file. Missing files are fine: in
// containers configuration normally arrives through the environment.
func loadDotEnv() error {
	path := os.Getenv("ENV_FILE")
	if path == "" {
		path = ".env"
	}

	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			log.Printf("No %s file found, relying on environment variables", path)
			return nil
		}
		return fmt.Errorf("checking %s: %w", path, err)
	}

	// Do not override variables that are already set explicitly.
	if err := godotenv.Load(path); err != nil {
		return fmt.Errorf("loading %s: %w", path, err)
	}

	return nil
}

func setDefaults() {
	viper.SetDefault("MAX_TOKENS", 2000)
	viper.SetDefault("TEMPERATURE", 0.7)
	viper.SetDefault("TOP_P", 0.7)
	viper.SetDefault("FREQUENCY_PENALTY", 0)
	viper.SetDefault("PRESENCE_PENALTY", 0)
	viper.SetDefault("BASE_URL", "https://openrouter.ai/api/v1")
	viper.SetDefault("BUDGET_PERIOD", "monthly")
	viper.SetDefault("USER_BUDGET", 1)
	viper.SetDefault("GUEST_BUDGET", 0)
	viper.SetDefault("MAX_HISTORY_SIZE", 10)
	viper.SetDefault("MAX_HISTORY_TIME", 60)
	viper.SetDefault("MAX_CONTEXT_MESSAGES", 8)
	viper.SetDefault("MAX_CONTEXT_TOKENS", 3000)
	viper.SetDefault("SUMMARY_THRESHOLD", 10)
	viper.SetDefault("MAX_AGENT_STEPS", 5)
	viper.SetDefault("MAX_FILE_SIZE", 10485760)
	viper.SetDefault("STORAGE_TYPE", "json")
	// Must match the file names in lang/, which are upper case.
	viper.SetDefault("LANG", "EN")
	viper.SetDefault("STATS_MIN_ROLE", "ADMIN")
	viper.SetDefault("GROUP_CHAT_MODE", GroupModeMention)
	viper.SetDefault("REQUEST_TIMEOUT_SECONDS", 300)
	viper.SetDefault("STREAM_IDLE_TIMEOUT_SECONDS", 120)
	viper.SetDefault("MAX_CONCURRENT_REQUESTS", 8)
	viper.SetDefault("RATE_LIMIT_PER_MINUTE", 10)
	viper.SetDefault("TYPE", "openrouter")
	viper.SetDefault("MAX_REPLY_CHARS", DefaultMaxReplyChars)
	viper.SetDefault("RENDER_MARKDOWN", false)
	viper.SetDefault("SUGGEST_MODELS", true)
	viper.SetDefault("GROUP_ACCESS", AccessEveryone)
	viper.SetDefault("PRIVATE_ACCESS", AccessEveryone)
}

// languageName is intentionally dependency free so that the config package
// does not import the translation bundle it helps configure.
func languageName(code string) string {
	switch strings.ToUpper(strings.TrimSpace(code)) {
	case "RU":
		return "Russian"
	case "EN":
		return "English"
	default:
		return strings.ToUpper(strings.TrimSpace(code))
	}
}

// loadProviders reads the provider chain from config.yaml and resolves each
// API key from the environment named by api_key_env.
func loadProviders() []provider.Config {
	var list []provider.Config

	if err := viper.UnmarshalKey("providers", &list); err != nil {
		log.Printf("Could not parse providers from config: %v", err)
		return nil
	}

	resolved := make([]provider.Config, 0, len(list))

	for _, entry := range list {
		entry.Name = strings.TrimSpace(entry.Name)
		if entry.Name == "" {
			continue
		}

		// A provider may be listed by name only: the preset supplies the
		// endpoint, the models and the name of the key variable.
		entry = provider.ApplyPreset(entry)

		if entry.APIKeyEnv != "" {
			entry.APIKey = os.Getenv(entry.APIKeyEnv)
		}
		resolved = append(resolved, entry)
	}

	if len(resolved) == 0 {
		return nil
	}

	return resolved
}

// extraProviderOrder is the order in which environment-configured providers
// join a chain that config.yaml did not define itself.
var extraProviderOrder = []string{"groq", "gemini", "cerebras", "nvidia", "mistral", "deepseek", "together"}

// PrimaryProvider describes the endpoint of the flat BASE_URL/MODEL/API_KEY
// variables. It is the first entry of the fallback chain.
func PrimaryProvider(c *Config) provider.Config {
	return provider.Config{
		Name:    "openrouter",
		BaseURL: c.OpenAIBaseURL,
		APIKey:  c.OpenAIApiKey,
		Models:  []string{c.Model.ModelName},
	}
}

// ProvidersFromEnv returns the preset providers whose API key is present in
// the environment, in a stable order. This is what makes "just set
// GROQ_API_KEY" (or GEMINI_API_KEY, or CEREBRAS_API_KEY) enough to get a
// failover chain.
func ProvidersFromEnv() []provider.Config {
	var out []provider.Config

	for _, name := range extraProviderOrder {
		preset, ok := provider.LookupPreset(name)
		if !ok || preset.APIKeyEnv == "" {
			continue
		}

		key := strings.TrimSpace(os.Getenv(preset.APIKeyEnv))
		if key == "" {
			continue
		}

		out = append(out, provider.Config{
			Name:        preset.Name,
			BaseURL:     preset.BaseURL,
			APIKeyEnv:   preset.APIKeyEnv,
			APIKey:      key,
			Models:      append([]string(nil), preset.Models...),
			RequiresKey: true,
		})
	}

	return out
}

// hasProvider reports whether a list already contains a provider by name.
func hasProvider(list []provider.Config, name string) bool {
	for _, entry := range list {
		if strings.EqualFold(entry.Name, name) {
			return true
		}
	}

	return false
}

// FallbackProviders builds a chain from the flat environment variables, used
// when config.yaml does not define a providers list. The primary provider
// comes first, then every preset with a key, then a local Ollama endpoint if
// one was configured.
func FallbackProviders(c *Config) []provider.Config {
	list := []provider.Config{PrimaryProvider(c)}

	list = append(list, ProvidersFromEnv()...)

	// An explicit GEMINI_API_KEY on the config struct counts even when the
	// variable was set through config.yaml rather than the environment.
	if c.GeminiAPIKey != "" && !hasProvider(list, "gemini") {
		if preset, ok := provider.LookupPreset("gemini"); ok {
			list = append(list, provider.Config{
				Name:        preset.Name,
				BaseURL:     preset.BaseURL,
				APIKeyEnv:   preset.APIKeyEnv,
				APIKey:      c.GeminiAPIKey,
				Models:      append([]string(nil), preset.Models...),
				RequiresKey: true,
			})
		}
	}

	if host := strings.TrimSpace(os.Getenv("OLLAMA_BASE_URL")); host != "" {
		if preset, ok := provider.LookupPreset("ollama"); ok {
			list = append(list, provider.Config{
				Name:    preset.Name,
				BaseURL: strings.TrimRight(host, "/") + "/v1",
				Models:  append([]string(nil), preset.Models...),
			})
		}
	}

	return list
}

func getStrAsIntList(name string) []int64 {
	valueStr := strings.TrimSpace(viper.GetString(name))
	if valueStr == "" {
		return nil
	}

	var values []int64
	for _, str := range strings.Split(valueStr, ",") {
		str = strings.TrimSpace(str)
		if str == "" {
			continue
		}
		value, err := strconv.ParseInt(str, 10, 64)
		if err != nil {
			log.Printf("Invalid value %q in %s: %v", str, name, err)
			continue
		}
		values = append(values, value)
	}

	return values
}

// printConfig logs the effective configuration. Secrets are masked - the
// previous implementation reflected over the whole struct and wrote the bot
// token and the API key straight into the logs.
func printConfig(c *Config) {
	if c == nil {
		log.Println("Config is nil")
		return
	}

	admins := make([]string, 0, len(c.AdminChatIDs))
	for _, id := range c.AdminChatIDs {
		admins = append(admins, strconv.FormatInt(id, 10))
	}
	allowed := make([]string, 0, len(c.AllowedUserChatIDs))
	for _, id := range c.AllowedUserChatIDs {
		allowed = append(allowed, strconv.FormatInt(id, 10))
	}
	sort.Strings(admins)
	sort.Strings(allowed)

	log.Printf(
		"Configuration: model=%s type=%s base_url=%s max_tokens=%d "+
			"temperature=%.2f top_p=%.2f lang=%s group_chat_mode=%s "+
			"budget_period=%s user_budget=%.4f guest_budget=%.4f "+
			"max_history=%d/%dmin vision=%t stats_min_role=%s "+
			"request_timeout=%s stream_idle_timeout=%s "+
			"max_concurrent_requests=%d rate_limit_per_minute=%d "+
			"telegram_token=%s api_key=%s gemini=%t admin_ids=[%s] allowed_user_ids=[%s]",
		c.Model.ModelName,
		c.Model.Type,
		c.OpenAIBaseURL,
		c.MaxTokens,
		c.Model.Temperature,
		c.Model.TopP,
		c.Lang,
		c.GroupChatMode,
		c.BudgetPeriod,
		c.UserBudget,
		c.GuestBudget,
		c.MaxHistorySize,
		c.MaxHistoryTime,
		c.Vision,
		c.StatsMinRole,
		c.RequestTimeout,
		c.StreamIdleTimeout,
		c.MaxConcurrentRequests,
		c.RateLimitPerMinute,
		mask(c.TelegramBotToken),
		mask(c.OpenAIApiKey),
		c.GeminiAPIKey != "",
		strings.Join(admins, ","),
		strings.Join(allowed, ","),
	)
}

// mask renders a secret safe for logs: empty, or a short prefix plus a length
// hint so that a misconfigured value is still recognisable.
func mask(secret string) string {
	if secret == "" {
		return "<unset>"
	}
	if len(secret) <= 8 {
		return "<set:" + strconv.Itoa(len(secret)) + " chars>"
	}
	return secret[:4] + "…" + strconv.Itoa(len(secret)) + " chars"
}
