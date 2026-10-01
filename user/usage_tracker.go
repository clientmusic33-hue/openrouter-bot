package user

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"openrouter-bot/config"
	"openrouter-bot/internal/atomicfile"
	"openrouter-bot/provider"
)

// NewUsageTracker creates a new UsageTracker.
func NewUsageTracker(userID, userName, logsDir string, conf *config.Config, history *History) *UsageTracker {
	if history == nil {
		history = NewHistory()
	}

	systemPrompt := ""
	if conf != nil {
		systemPrompt = conf.SystemPrompt
	}

	usageTracker := &UsageTracker{
		UserID:          userID,
		UserName:        userName,
		LogsDir:         logsDir,
		systemPrompt:    systemPrompt,
		Usage:           newUserUsage(userName),
		History:         history,
		lastMessageTime: time.Now(),
	}

	if err := usageTracker.loadUsage(); err != nil {
		log.Printf("Error loading usage for user %s: %v", userID, err)
	}

	return usageTracker
}

func newUserUsage(userName string) *UserUsage {
	settings := DefaultSettings()
	settings.Initialised = true

	return &UserUsage{
		UserName: userName,
		UsageHistory: UsageHist{
			ChatCost: make(map[string]float64),
			Requests: make(map[string]int),
		},
		Settings: settings,
	}
}

// updateUsage mutates the usage record under its lock and persists the result.
func (ut *UsageTracker) updateUsage(mutate func(*UserUsage)) {
	ut.UsageMu.Lock()
	if ut.Usage == nil {
		ut.Usage = newUserUsage(ut.UserName)
	}
	if ut.Usage.UsageHistory.ChatCost == nil {
		ut.Usage.UsageHistory.ChatCost = make(map[string]float64)
	}
	if ut.Usage.UsageHistory.Requests == nil {
		ut.Usage.UsageHistory.Requests = make(map[string]int)
	}

	mutate(ut.Usage)
	ut.UsageMu.Unlock()

	if err := ut.saveUsage(); err != nil {
		logSaveFailure("usage", ut.UserID, err)
	}
}

// logSaveFailure reports a failed persist without flooding the log: a broken
// log directory would otherwise print a warning on every single message.
func logSaveFailure(kind, userID string, err error) {
	key := userID + "/" + kind
	if last, ok := lastSaveFailure.Load(key); ok {
		if lastTime, ok := last.(time.Time); ok && time.Since(lastTime) < time.Minute {
			return
		}
	}
	lastSaveFailure.Store(key, time.Now())

	log.Printf("Failed to save %s for user %s: %v", kind, userID, err)
}

// lastSaveFailure rate limits persistence warnings per user and kind.
var lastSaveFailure sync.Map

// -----------------------------------------------------------------------------
// ACCESS CONTROL
// -----------------------------------------------------------------------------

// GetUserRole returns the user's configured role.
//
// When ALLOWED_USER_IDS is empty the bot is open to everyone and every user is
// treated as USER; otherwise only listed users are USERs and everybody else is
// a GUEST.
func (ut *UsageTracker) GetUserRole(conf *config.Config) string {
	if conf == nil {
		return RoleGuest
	}

	for _, id := range conf.AdminChatIDs {
		if ut.UserID == fmt.Sprintf("%d", id) {
			return RoleAdmin
		}
	}

	for _, id := range conf.AllowedUserChatIDs {
		if ut.UserID == fmt.Sprintf("%d", id) {
			return RoleUser
		}
	}

	if len(conf.AllowedUserChatIDs) == 0 {
		return RoleUser
	}

	return RoleGuest
}

// budgetFor returns the spending allowance for this user in the current
// budget period. A negative allowance means unlimited.
func (ut *UsageTracker) budgetFor(conf *config.Config) float64 {
	if conf == nil {
		return 0
	}

	switch ut.GetUserRole(conf) {
	case RoleAdmin:
		// Operators are not limited by the budget; they can always use the
		// bot and can always see the statistics.
		return math.Inf(1)
	case RoleUser:
		return conf.UserBudget
	default:
		return conf.GuestBudget
	}
}

// HaveAccess reports whether the user may start a new request.
//
// This enforces the budget: a user whose spend in the current budget period
// has reached their allowance is refused until the period rolls over.
func (ut *UsageTracker) HaveAccess(conf *config.Config) bool {
	budget := ut.budgetFor(conf)
	if budget < 0 {
		return true
	}
	if budget <= 0 {
		log.Printf("Access denied for user %s: no budget for role %s", ut.UserID, ut.GetUserRole(conf))
		return false
	}

	spent := ut.GetCurrentCost(conf.BudgetPeriod)
	if spent >= budget {
		log.Printf(
			"Access denied for user %s: spent %.6f of %.6f budget (%s)",
			ut.UserID, spent, budget, conf.BudgetPeriod,
		)
		return false
	}

	return true
}

// CanViewStats determines whether the user can view statistics.
func (ut *UsageTracker) CanViewStats(conf *config.Config) bool {
	if conf == nil {
		return false
	}

	role := ut.GetUserRole(conf)

	switch conf.StatsMinRole {
	case RoleGuest:
		return true
	case RoleUser:
		return role == RoleAdmin || role == RoleUser
	default: // ADMIN
		return role == RoleAdmin
	}
}

// AllowRequest enforces the per-user rate limit. It returns an error when the
// user has to wait before starting another request.
func (ut *UsageTracker) AllowRequest(conf *config.Config) error {
	if conf == nil || conf.RateLimitPerMinute <= 0 {
		return nil
	}

	ut.mu.Lock()
	defer ut.mu.Unlock()

	now := time.Now()
	if ut.rateWindowStart.IsZero() || now.Sub(ut.rateWindowStart) >= time.Minute {
		ut.rateWindowStart = now
		ut.rateCount = 0
	}

	if ut.rateCount >= conf.RateLimitPerMinute {
		retryAfter := time.Minute - now.Sub(ut.rateWindowStart)
		return fmt.Errorf(
			"rate limit of %d requests per minute exceeded, retry in %s",
			conf.RateLimitPerMinute, retryAfter.Round(time.Second),
		)
	}

	ut.rateCount++

	return nil
}

// -----------------------------------------------------------------------------
// PERSISTENCE
// -----------------------------------------------------------------------------

// saveUsage saves the user's usage to a JSON file.
func (ut *UsageTracker) saveUsage() error {
	ut.FileMu.Lock()
	defer ut.FileMu.Unlock()

	ut.UsageMu.Lock()
	if ut.Usage == nil {
		ut.Usage = newUserUsage(ut.UserName)
	}
	if ut.Usage.UsageHistory.ChatCost == nil {
		ut.Usage.UsageHistory.ChatCost = make(map[string]float64)
	}
	data, err := json.MarshalIndent(ut.Usage, "", "  ")
	ut.UsageMu.Unlock()

	if err != nil {
		log.Printf("Error marshalling usage data for user %s: %v", ut.UserID, err)
		return fmt.Errorf("error marshalling usage data: %w", err)
	}

	if err := os.MkdirAll(ut.LogsDir, 0o755); err != nil {
		log.Printf("Error creating logs directory for user %s: %v", ut.UserID, err)
		return fmt.Errorf("error creating logs directory: %w", err)
	}

	if err := atomicfile.Write(filepath.Join(ut.LogsDir, ut.UserID+".json"), data, 0o644); err != nil {
		log.Printf("Error writing usage data to file for user %s: %v", ut.UserID, err)
		return fmt.Errorf("error writing usage data to file: %w", err)
	}

	return nil
}

// loadUsage loads the user's usage from a JSON file, creating it if needed.
func (ut *UsageTracker) loadUsage() error {
	filePath := filepath.Join(ut.LogsDir, ut.UserID+".json")

	data, err := os.ReadFile(filePath)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("Error reading usage data from file for user %s: %v", ut.UserID, err)
			return fmt.Errorf("error reading usage data from file: %w", err)
		}

		ut.UsageMu.Lock()
		ut.Usage = newUserUsage(ut.UserName)
		ut.UsageMu.Unlock()

		return ut.saveUsage()
	}

	var usage UserUsage
	if err := json.Unmarshal(data, &usage); err != nil {
		log.Printf("Error unmarshalling usage data for user %s: %v", ut.UserID, err)
		return fmt.Errorf("error unmarshalling usage data: %w", err)
	}

	if usage.UsageHistory.ChatCost == nil {
		usage.UsageHistory.ChatCost = make(map[string]float64)
	}
	if usage.UsageHistory.Requests == nil {
		usage.UsageHistory.Requests = make(map[string]int)
	}

	ut.UsageMu.Lock()
	ut.Usage = &usage
	ut.UsageMu.Unlock()

	return nil
}

// -----------------------------------------------------------------------------
// COST ACCOUNTING
// -----------------------------------------------------------------------------

// AddCost adds the generation cost to the user's usage history.
func (ut *UsageTracker) AddCost(cost float64) {
	if cost == 0 {
		return
	}

	ut.UsageMu.Lock()
	if ut.Usage == nil {
		ut.Usage = newUserUsage(ut.UserName)
	}
	if ut.Usage.UsageHistory.ChatCost == nil {
		ut.Usage.UsageHistory.ChatCost = make(map[string]float64)
	}
	if ut.Usage.UsageHistory.Requests == nil {
		ut.Usage.UsageHistory.Requests = make(map[string]int)
	}
	ut.Usage.UsageHistory.ChatCost[time.Now().Format("2006-01-02")] += cost
	ut.UsageMu.Unlock()

	if err := ut.saveUsage(); err != nil {
		log.Printf("Failed to save usage after adding cost for user %s: %v", ut.UserID, err)
	}
}

// GetCurrentCost returns the current cost based on the specified period.
func (ut *UsageTracker) GetCurrentCost(period string) float64 {
	ut.UsageMu.Lock()
	defer ut.UsageMu.Unlock()

	if ut.Usage == nil || ut.Usage.UsageHistory.ChatCost == nil {
		return 0
	}

	today := time.Now().Format("2006-01-02")
	chatCost := ut.Usage.UsageHistory.ChatCost

	switch strings.ToLower(period) {
	case "daily":
		return calculateCostForDay(chatCost, today)
	case "monthly":
		return calculateCostForMonth(chatCost, today)
	case "total":
		return calculateTotalCost(chatCost)
	default:
		log.Printf("Invalid period: %s. Valid periods are 'daily', 'monthly', 'total'.", period)
		return 0
	}
}

// calculateCostForDay calculates the cost for a specific day.
func calculateCostForDay(chatCost map[string]float64, day string) float64 {
	return chatCost[day]
}

// calculateCostForMonth calculates the cost for the current month.
func calculateCostForMonth(chatCost map[string]float64, today string) float64 {
	if len(today) < 7 {
		return 0
	}

	month := today[:7]

	var cost float64
	for date, dailyCost := range chatCost {
		if strings.HasPrefix(date, month) {
			cost += dailyCost
		}
	}

	return cost
}

// calculateTotalCost calculates the total cost from usage history.
func calculateTotalCost(chatCost map[string]float64) float64 {
	var total float64
	for _, cost := range chatCost {
		total += cost
	}

	return total
}

// -----------------------------------------------------------------------------
// USAGE PROFILE
// -----------------------------------------------------------------------------

// RecordRequest notes that the user started a request and folds the prompt
// length into the running average. Cost is tracked separately because a
// request can fail before it is billed.
func (ut *UsageTracker) RecordRequest(promptChars int) {
	ut.UsageMu.Lock()

	if ut.Usage == nil {
		ut.Usage = newUserUsage(ut.UserName)
	}
	if ut.Usage.UsageHistory.Requests == nil {
		ut.Usage.UsageHistory.Requests = make(map[string]int)
	}

	ut.Usage.UsageHistory.Requests[time.Now().Format("2006-01-02")]++

	if promptChars > 0 {
		count := ut.Usage.PromptSamples
		ut.Usage.AvgPromptChars =
			(ut.Usage.AvgPromptChars*float64(count) + float64(promptChars)) / float64(count+1)
		ut.Usage.PromptSamples = count + 1
	}

	ut.UsageMu.Unlock()

	// Persisting on every request would be wasteful, so the counters are
	// written when cost is added and at shutdown. Failures here are not
	// critical.
	_ = ut.saveUsage()
}

// MarkVision records that the user sends images, which makes vision support a
// hard requirement in recommendations.
func (ut *UsageTracker) MarkVision() {
	ut.UsageMu.Lock()
	if ut.Usage != nil {
		ut.Usage.UsedVision = true
	}
	ut.UsageMu.Unlock()
}

// UsageProfile summarises how this user talks to the bot.
func (ut *UsageTracker) UsageProfile() provider.UsageProfile {
	ut.UsageMu.Lock()

	profile := provider.UsageProfile{}

	if ut.Usage != nil {
		profile.AvgPromptChars = ut.Usage.AvgPromptChars
		profile.UsesVision = ut.Usage.UsedVision
		profile.DislikedModels = ut.Usage.Settings.Downvotes

		total, days := 0, 0
		for _, count := range ut.Usage.UsageHistory.Requests {
			total += count
			days++
		}
		if days > 0 {
			profile.RequestsPerDay = float64(total) / float64(days)
		}

		// Recent days matter more than all-time history.
		if days > 7 {
			recent := 0
			for i := 0; i < 7; i++ {
				day := time.Now().AddDate(0, 0, -i).Format("2006-01-02")
				recent += ut.Usage.UsageHistory.Requests[day]
			}
			profile.RequestsPerDay = float64(recent) / 7
		}
	}

	ut.UsageMu.Unlock()

	// Heavy users care about latency, essay writers about depth.
	profile.PrefersFast = profile.RequestsPerDay > 50
	profile.NeedsReasoning = profile.AvgPromptChars > 800

	// History size and the biggest single message decide the context need.
	history := ut.GetMessages()
	profile.HistoryMessages = len(history)

	peak := 0
	for _, message := range history {
		if len(message.Content) > peak {
			peak = len(message.Content)
		}
	}
	profile.PeakHistoryChars = peak

	return profile
}

// -----------------------------------------------------------------------------
// OPENROUTER GENERATION STATISTICS
// -----------------------------------------------------------------------------

// GetUsageFromApi retrieves the cost of a generation from the provider that
// produced it and adds it to the user's usage.
//
// baseURL and apiKey identify the answering backend, so the lookup follows the
// provider that actually served the request instead of assuming OpenRouter.
// Providers without a generation statistics endpoint return an error from the
// request, which callers treat as "no cost data available".
func (ut *UsageTracker) GetUsageFromApi(id, baseURL, apiKey string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		// Nothing was generated (the request failed before the first chunk),
		// so there is no cost to look up.
		return nil
	}

	if strings.TrimSpace(baseURL) == "" || strings.TrimSpace(apiKey) == "" {
		return nil
	}

	endpoint := strings.TrimRight(baseURL, "/") + "/generation?id=" + url.QueryEscape(id)

	totalCost, err := fetchGenerationCost(endpoint, apiKey)
	if err != nil {
		return err
	}

	log.Printf("Total Cost for user %s: %.6f", ut.UserID, totalCost)

	ut.AddCost(totalCost)

	return nil
}

// fetchGenerationCost performs the request, retrying once because OpenRouter
// needs a moment to finalise the statistics for a generation.
func fetchGenerationCost(endpoint, apiKey string) (float64, error) {
	client := provider.SharedHTTPClient(20 * time.Second)

	var lastErr error

	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			time.Sleep(2 * time.Second)
		}

		cost, err := requestGenerationCost(client, endpoint, apiKey)
		if err == nil {
			return cost, nil
		}
		lastErr = err

		// A 404 immediately after a stream ends usually means the statistics
		// are not ready yet; other failures will not improve on a retry.
		if attempt == 0 && !strings.Contains(err.Error(), "status 404") {
			return 0, err
		}
	}

	return 0, lastErr
}

func requestGenerationCost(client *http.Client, endpoint, apiKey string) (float64, error) {
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, fmt.Errorf("error creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("error sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, fmt.Errorf("generation API returned status %s", resp.Status)
	}

	var generation GenerationResponse
	if err := json.NewDecoder(resp.Body).Decode(&generation); err != nil {
		return 0, fmt.Errorf("error decoding response: %w", err)
	}

	return generation.Data.TotalCost, nil
}
