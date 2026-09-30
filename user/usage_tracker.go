package user

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"openrouter-bot/config"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// NewUsageTracker creates a new UsageTracker.
func NewUsageTracker(userID, userName, logsDir string, conf *config.Config) *UsageTracker {
	usageTracker := &UsageTracker{
		UserID:   userID,
		UserName: userName,
		LogsDir:  logsDir,
		Usage: &UserUsage{
			UsageHistory: UsageHist{
				ChatCost: make(map[string]float64),
			},
		},
		History: History{
			messages: make([]Message, 0),
		},
		SystemPrompt: conf.SystemPrompt,
	}

	err := usageTracker.loadUsage()
	if err != nil {
		log.Printf("Error loading usage for user %s: %v", userID, err)
	}

	return usageTracker
}

// HaveAccess determines whether a user can use the bot.
//
// The bot is public and has no application-level usage limit.
// Every Telegram user is allowed to use the bot.
func (ut *UsageTracker) HaveAccess(conf *config.Config) bool {
	return true
}

// GetUserRole returns the user's configured role.
func (ut *UsageTracker) GetUserRole(conf *config.Config) string {
	for _, id := range conf.AdminChatIDs {
		idStr := fmt.Sprintf("%d", id)

		if ut.UserID == idStr {
			return "ADMIN"
		}
	}

	for _, id := range conf.AllowedUserChatIDs {
		idStr := fmt.Sprintf("%d", id)

		if ut.UserID == idStr {
			return "USER"
		}
	}

	return "GUEST"
}

// CanViewStats determines whether the user can view statistics.
func (ut *UsageTracker) CanViewStats(conf *config.Config) bool {
	userRole := ut.GetUserRole(conf)

	return userRole == "ADMIN" ||
		(conf.StatsMinRole == "USER" && userRole != "GUEST")
}

// loadOrCreateUsage loads or creates the usage file for a user.
func (ut *UsageTracker) loadOrCreateUsage() error {
	userFile := filepath.Join(ut.LogsDir, ut.UserID+".json")

	if _, err := os.Stat(userFile); os.IsNotExist(err) {
		ut.UsageMu.Lock()

		ut.Usage = &UserUsage{
			UserName: ut.UserName,
			UsageHistory: UsageHist{
				ChatCost: make(map[string]float64),
			},
		}

		ut.UsageMu.Unlock()

		if err := ut.saveUsage(); err != nil {
			return err
		}
	} else {
		data, err := os.ReadFile(userFile)
		if err != nil {
			log.Println(err)
			return err
		}

		ut.UsageMu.Lock()

		err = json.Unmarshal(data, ut.Usage)

		ut.UsageMu.Unlock()

		if err != nil {
			log.Println(err)
			return err
		}
	}

	return nil
}

// saveUsage saves the user's usage to a JSON file.
func (ut *UsageTracker) saveUsage() error {
	ut.FileMu.Lock()
	defer ut.FileMu.Unlock()

	ut.UsageMu.Lock()

	data, err := json.MarshalIndent(
		ut.Usage,
		"",
		"  ",
	)

	ut.UsageMu.Unlock()

	if err != nil {
		log.Printf(
			"Error marshalling usage data for user %s: %v",
			ut.UserID,
			err,
		)

		return fmt.Errorf(
			"error marshalling usage data: %w",
			err,
		)
	}

	if err := os.MkdirAll(ut.LogsDir, 0755); err != nil {
		log.Printf(
			"Error creating logs directory for user %s: %v",
			ut.UserID,
			err,
		)

		return fmt.Errorf(
			"error creating logs directory: %w",
			err,
		)
	}

	filename := filepath.Join(
		ut.LogsDir,
		ut.UserID+".json",
	)

	err = os.WriteFile(
		filename,
		data,
		0644,
	)

	if err != nil {
		log.Printf(
			"Error writing usage data to file for user %s: %v",
			ut.UserID,
			err,
		)

		return fmt.Errorf(
			"error writing usage data to file: %w",
			err,
		)
	}

	return nil
}

// loadUsage loads the user's usage from a JSON file.
func (ut *UsageTracker) loadUsage() error {
	filePath := filepath.Join(
		ut.LogsDir,
		ut.UserID+".json",
	)

	data, err := os.ReadFile(filePath)

	if err != nil {
		if os.IsNotExist(err) {
			log.Printf(
				"File not found for user %s, creating new usage data.",
				ut.UserID,
			)

			ut.UsageMu.Lock()

			ut.Usage = &UserUsage{
				UserName: ut.UserName,
				UsageHistory: UsageHist{
					ChatCost: make(map[string]float64),
				},
			}

			ut.UsageMu.Unlock()

			return ut.saveUsage()
		}

		log.Printf(
			"Error reading usage data from file for user %s: %v",
			ut.UserID,
			err,
		)

		return fmt.Errorf(
			"error reading usage data from file: %w",
			err,
		)
	}

	ut.UsageMu.Lock()

	var usage UserUsage

	err = json.Unmarshal(
		data,
		&usage,
	)

	if err != nil {
		ut.UsageMu.Unlock()

		log.Printf(
			"Error unmarshalling usage data for user %s: %v",
			ut.UserID,
			err,
		)

		return fmt.Errorf(
			"error unmarshalling usage data: %w",
			err,
		)
	}

	if usage.UsageHistory.ChatCost == nil {
		usage.UsageHistory.ChatCost = make(map[string]float64)
	}

	ut.Usage = &usage

	ut.UsageMu.Unlock()

	return nil
}

// AddCost adds the generation cost to the user's usage history.
func (ut *UsageTracker) AddCost(cost float64) {
	ut.UsageMu.Lock()

	if ut.Usage == nil {
		ut.Usage = &UserUsage{
			UserName: ut.UserName,
			UsageHistory: UsageHist{
				ChatCost: make(map[string]float64),
			},
		}
	}

	if ut.Usage.UsageHistory.ChatCost == nil {
		ut.Usage.UsageHistory.ChatCost = make(map[string]float64)
	}

	today := time.Now().Format("2006-01-02")

	ut.Usage.UsageHistory.ChatCost[today] += cost

	ut.UsageMu.Unlock()

	if err := ut.saveUsage(); err != nil {
		log.Printf(
			"Failed to save usage after adding cost for user %s: %v",
			ut.UserID,
			err,
		)
	}
}

// GetCurrentCost returns the current cost based on the specified period.
func (ut *UsageTracker) GetCurrentCost(period string) float64 {
	ut.UsageMu.Lock()
	defer ut.UsageMu.Unlock()

	if ut.Usage == nil {
		return 0
	}

	if ut.Usage.UsageHistory.ChatCost == nil {
		return 0
	}

	today := time.Now().Format("2006-01-02")

	var cost float64

	var err error

	switch period {

	case "daily":
		cost = calculateCostForDay(
			ut.Usage.UsageHistory.ChatCost,
			today,
		)

	case "monthly":
		cost, err = calculateCostForMonth(
			ut.Usage.UsageHistory.ChatCost,
			today,
		)

		if err != nil {
			log.Printf(
				"Error calculating monthly cost for user %s: %v",
				ut.UserID,
				err,
			)

			return 0
		}

	case "total":
		cost = calculateTotalCost(
			ut.Usage.UsageHistory.ChatCost,
		)

	default:
		log.Printf(
			"Invalid period: %s. Valid periods are 'daily', 'monthly', 'total'.",
			period,
		)

		return 0
	}

	return cost
}

// calculateCostForDay calculates the cost for a specific day.
func calculateCostForDay(
	chatCost map[string]float64,
	day string,
) float64 {
	if cost, ok := chatCost[day]; ok {
		return cost
	}

	return 0
}

// calculateCostForMonth calculates the cost for the current month.
func calculateCostForMonth(
	chatCost map[string]float64,
	today string,
) (float64, error) {
	if len(today) < 7 {
		return 0, fmt.Errorf("invalid date: %s", today)
	}

	cost := 0.0

	month := today[:7]

	for date, dailyCost := range chatCost {
		if strings.HasPrefix(date, month) {
			cost += dailyCost
		}
	}

	return cost, nil
}

// calculateTotalCost calculates the total cost from usage history.
func calculateTotalCost(
	chatCost map[string]float64,
) float64 {
	totalCost := 0.0

	for _, cost := range chatCost {
		totalCost += cost
	}

	return totalCost
}

// GetUsageFromApi gets the cost of a generation from OpenRouter.
func (ut *UsageTracker) GetUsageFromApi(
	id string,
	conf *config.Config,
) error {
	url := fmt.Sprintf(
		"https://openrouter.ai/api/v1/generation?id=%s",
		id,
	)

	req, err := http.NewRequest(
		"GET",
		url,
		nil,
	)

	if err != nil {
		log.Printf(
			"Error creating request for user %s: %v",
			ut.UserID,
			err,
		)

		return fmt.Errorf(
			"error creating request: %w",
			err,
		)
	}

	bearer := fmt.Sprintf(
		"Bearer %s",
		conf.OpenAIApiKey,
	)

	req.Header.Set(
		"Authorization",
		bearer,
	)

	client := &http.Client{}

	resp, err := client.Do(req)

	if err != nil {
		log.Printf(
			"Error sending request for user %s: %v",
			ut.UserID,
			err,
		)

		return fmt.Errorf(
			"error sending request: %w",
			err,
		)
	}

	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf(
			"OpenRouter generation API returned status %s",
			resp.Status,
		)
	}

	var generationResponse GenerationResponse

	err = json.NewDecoder(resp.Body).Decode(
		&generationResponse,
	)

	if err != nil {
		log.Printf(
			"Error decoding response for user %s: %v",
			ut.UserID,
			err,
		)

		return fmt.Errorf(
			"error decoding response: %w",
			err,
		)
	}

	fmt.Printf(
		"Total Cost for user %s: %.6f\n",
		ut.UserID,
		generationResponse.Data.TotalCost,
	)

	ut.AddCost(
		generationResponse.Data.TotalCost,
	)

	return nil
}
