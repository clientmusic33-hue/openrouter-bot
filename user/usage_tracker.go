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
		Usage: &UserUsage{ // Initialize as pointer
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
func (ut *UsageTracker) HaveAccess(conf *config.Config) bool {
	// Admins always have unlimited access.
	for _, id := range conf.AdminChatIDs {
		idStr := fmt.Sprintf("%d", id)

		if ut.UserID == idStr {
			log.Printf("Admin access: %s", ut.UserID)
			return true
		}
	}

	// Allowed users always have unlimited access.
	for _, id := range conf.AllowedUserChatIDs {
		idStr := fmt.Sprintf("%d", id)

		if ut.UserID == idStr {
			log.Printf("Allowed user access: %s", ut.UserID)
			return true
		}
	}

	// Public access:
	// All other Telegram users are allowed to use the bot
	// without a guest budget limit.
	log.Printf("Public guest access: %s", ut.UserID)
	return true
}
	currentCost := ut.GetCurrentCost(conf.BudgetPeriod)

	if float64(conf.GuestBudget) > currentCost {
		log.Println("ID:", ut.UserID, " GuestBudget:", conf.GuestBudget, " CurrentCost:", currentCost)
		return true
	}
	log.Printf("UserID: %s, AdminChatIDs: %v, AllowedUserChatIDs: %v", ut.UserID, conf.AdminChatIDs, conf.AllowedUserChatIDs)
	log.Printf("UserBudget: %f, GuestBudget: %f, CurrentCost: %f", float64(conf.UserBudget), float64(conf.GuestBudget), currentCost)
	return false

}

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

func (ut *UsageTracker) CanViewStats(conf *config.Config) bool {
	userRole := ut.GetUserRole(conf)
	return userRole == "ADMIN" || (conf.StatsMinRole == "USER" && userRole != "GUEST")
}

// loadOrCreateUsage loads or creates the usage file for a user
func (ut *UsageTracker) loadOrCreateUsage() error {
	userFile := filepath.Join(ut.LogsDir, ut.UserID+".json")
	if _, err := os.Stat(userFile); os.IsNotExist(err) {
		ut.UsageMu.Lock()      // Added lock
		ut.Usage = &UserUsage{ // Initialize as a pointer
			UserName: ut.UserName,
			UsageHistory: UsageHist{
				ChatCost: make(map[string]float64),
			},
		}
		ut.UsageMu.Unlock() // Added unlock
		err := ut.saveUsage()
		if err != nil {
			return err
		}
	} else {
		data, err := os.ReadFile(userFile)
		if err != nil {
			log.Println(err)
			return err
		}
		ut.UsageMu.Lock() // Added lock
		err = json.Unmarshal(data, ut.Usage)
		ut.UsageMu.Unlock() // Added unlock
		if err != nil {
			log.Println(err)
			return err
		}
	}
	return nil
}

// saveUsage saves the user usage to a JSON file.
func (ut *UsageTracker) saveUsage() error {
	ut.FileMu.Lock()
	defer ut.FileMu.Unlock()

	ut.UsageMu.Lock()
	data, err := json.MarshalIndent(ut.Usage, "", "  ")
	ut.UsageMu.Unlock()

	if err != nil {
		log.Printf("Error marshalling usage data for user %s: %v", ut.UserID, err)
		return fmt.Errorf("error marshalling usage data: %w", err)
	}

	filename := fmt.Sprintf("%s/%s.json", ut.LogsDir, ut.UserID)
	err = os.WriteFile(filename, data, 0644) // Use os.WriteFile instead of ioutil.WriteFile
	if err != nil {
		log.Printf("Error writing usage data to file for user %s: %v", ut.UserID, err)
		return fmt.Errorf("error writing usage data to file: %w", err)
	}

	return nil
}

// loadUsage loads the user usage from a JSON file.
func (ut *UsageTracker) loadUsage() error {
	filePath := filepath.Join(ut.LogsDir, ut.UserID+".json")
	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("File not found for user %s, creating new usage data.", ut.UserID)
			ut.UsageMu.Lock()
			ut.Usage = &UserUsage{ // Initialize as pointer
				UsageHistory: UsageHist{
					ChatCost: make(map[string]float64),
				},
			}
			ut.UsageMu.Unlock()
			return ut.saveUsage()
		}
		log.Printf("Error reading usage data from file for user %s: %v", ut.UserID, err)
		return fmt.Errorf("error reading usage data from file: %w", err)
	}

	ut.UsageMu.Lock()
	var usage UserUsage
	err = json.Unmarshal(data, &usage) // Unmarshal into temporary variable
	if err != nil {
		ut.UsageMu.Unlock()
		log.Printf("Error unmarshalling usage data for user %s: %v", ut.UserID, err)
		return fmt.Errorf("error unmarshalling usage data: %w", err)
	}
	ut.Usage = &usage // Assign pointer to unmarshaled data
	ut.UsageMu.Unlock()

	return nil
}

// AddCost Добавляет стоимость к текущему использованию и сохраняет данные
func (ut *UsageTracker) AddCost(cost float64) {
	ut.UsageMu.Lock()

	today := time.Now().Format("2006-01-02")
	if ut.Usage.UsageHistory.ChatCost == nil { // Добавлена проверка на nil
		ut.Usage.UsageHistory.ChatCost = make(map[string]float64)
	}
	ut.Usage.UsageHistory.ChatCost[today] += cost

	ut.UsageMu.Unlock() // Переместил Unlock после вызова saveUsage()

	if err := ut.saveUsage(); err != nil {
		log.Printf("Failed to save usage after adding cost for user %s: %v", ut.UserID, err)
	}
}

// GetCurrentCost returns the current cost based on the specified period.
func (ut *UsageTracker) GetCurrentCost(period string) float64 {
	ut.UsageMu.Lock()
	defer ut.UsageMu.Unlock()

	today := time.Now().Format("2006-01-02")
	var cost float64
	var err error

	switch period {
	case "daily":
		cost = calculateCostForDay(ut.Usage.UsageHistory.ChatCost, today)
	case "monthly":
		cost, err = calculateCostForMonth(ut.Usage.UsageHistory.ChatCost, today)
		if err != nil {
			log.Printf("Error calculating monthly cost for user %s: %v", ut.UserID, err)
			return 0.0 // Или другое значение по умолчанию
		}
	case "total":
		cost = calculateTotalCost(ut.Usage.UsageHistory.ChatCost)
	default:
		log.Printf("Invalid period: %s. Valid periods are 'daily', 'monthly', 'total'.", period)
		return 0.0
	}

	return cost
}

// calculateCostForDay calculates the cost for a specific day from usage history
func calculateCostForDay(chatCost map[string]float64, day string) float64 {
	if cost, ok := chatCost[day]; ok {
		return cost
	}
	return 0.0
}

// calculateCostForMonth calculates the cost for the current month from usage history
func calculateCostForMonth(chatCost map[string]float64, today string) (float64, error) {
	cost := 0.0
	month := today[:7] // Получаем год и месяц в формате "YYYY-MM"

	for date, dailyCost := range chatCost {
		if strings.HasPrefix(date, month) {
			cost += dailyCost
		}
	}

	return cost, nil
}

// calculateTotalCost calculates the total cost from usage history
func calculateTotalCost(chatCost map[string]float64) float64 {
	totalCost := 0.0
	for _, cost := range chatCost {
		totalCost += cost
	}
	return totalCost
}

// GetUsageFromApi Get cost of current generation
func (ut *UsageTracker) GetUsageFromApi(id string, conf *config.Config) error {
	url := fmt.Sprintf("https://openrouter.ai/api/v1/generation?id=%s", id)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		log.Printf("Error creating request for user %s: %v", ut.UserID, err)
		return fmt.Errorf("error creating request: %w", err)
	}

	bearer := fmt.Sprintf("Bearer %s", conf.OpenAIApiKey)
	req.Header.Add("Authorization", bearer)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("Error sending request for user %s: %v", ut.UserID, err)
		return fmt.Errorf("error sending request: %w", err)
	}
	defer resp.Body.Close()

	var generationResponse GenerationResponse
	err = json.NewDecoder(resp.Body).Decode(&generationResponse)
	if err != nil {
		log.Printf("Error decoding response for user %s: %v", ut.UserID, err)
		return fmt.Errorf("error decoding response: %w", err)
	}

	fmt.Printf("Total Cost for user %s: %.6f\n", ut.UserID, generationResponse.Data.TotalCost)
	ut.AddCost(generationResponse.Data.TotalCost)
	return nil
}
