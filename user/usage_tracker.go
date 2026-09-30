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
	"time"

	"openrouter-bot/config"
	"openrouter-bot/internal/atomicfile"
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
	return &UserUsage{
		UserName: userName,
		UsageHistory: UsageHist{
			ChatCost: make(map[string]float64),
		},
	}
}

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
// OPENROUTER GENERATION STATISTICS
// -----------------------------------------------------------------------------

// GetUsageFromApi retrieves the cost of a generation from OpenRouter and adds
// it to the user's usage.
func (ut *UsageTracker) GetUsageFromApi(id string, conf *config.Config) error {
	id = strings.TrimSpace(id)
	if id == "" {
		// Nothing was generated (the request failed before the first chunk),
		// so there is no cost to look up.
		return nil
	}

	if conf == nil || conf.OpenAIApiKey == "" {
		return nil
	}

	endpoint := "https://openrouter.ai/api/v1/generation?id=" + url.QueryEscape(id)

	totalCost, err := fetchGenerationCost(endpoint, conf.OpenAIApiKey)
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
	client := &http.Client{Timeout: 20 * time.Second}

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
		return 0, fmt.Errorf("OpenRouter generation API returned status %s", resp.Status)
	}

	var generation GenerationResponse
	if err := json.NewDecoder(resp.Body).Decode(&generation); err != nil {
		return 0, fmt.Errorf("error decoding response: %w", err)
	}

	return generation.Data.TotalCost, nil
}
