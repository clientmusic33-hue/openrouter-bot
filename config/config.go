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
	}

	if config.Model.ModelName == "" {
		return nil, errors.New("no model configured: set MODEL in the environment or config.yaml")
	}

	if err := config.Validate(); err != nil {
		return nil, err
	}

	// The assistant is asked to answer in the configured UI language unless a
	// custom prompt already says otherwise.
	config.SystemPrompt = "Always answer in " +
		languageName(config.Lang) + " language." + config.SystemPrompt

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
	// Must match the file names in lang/, which are upper case.
	viper.SetDefault("LANG", "EN")
	viper.SetDefault("STATS_MIN_ROLE", "ADMIN")
	viper.SetDefault("GROUP_CHAT_MODE", GroupModeMention)
	viper.SetDefault("REQUEST_TIMEOUT_SECONDS", 300)
	viper.SetDefault("STREAM_IDLE_TIMEOUT_SECONDS", 120)
	viper.SetDefault("MAX_CONCURRENT_REQUESTS", 8)
	viper.SetDefault("RATE_LIMIT_PER_MINUTE", 10)
	viper.SetDefault("TYPE", "openrouter")
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
