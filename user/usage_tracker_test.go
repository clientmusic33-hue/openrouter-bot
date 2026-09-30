package user

import (
	"testing"
	"time"

	"openrouter-bot/config"
)

func testConfig() *config.Config {
	return &config.Config{
		BudgetPeriod:       "monthly",
		UserBudget:         1,
		GuestBudget:        0,
		MaxHistorySize:     3,
		MaxHistoryTime:     60,
		RateLimitPerMinute: 2,
		SystemPrompt:       "You are a test assistant.",
		StatsMinRole:       "ADMIN",
		AdminChatIDs:       []int64{100},
		AllowedUserChatIDs: []int64{200},
	}
}

func newTracker(t *testing.T, userID string, conf *config.Config) *UsageTracker {
	t.Helper()

	return NewUsageTracker(userID, "tester", t.TempDir(), conf, nil)
}

// ---------------------------------------------------------------------------
// Access control - this is what protects the OpenRouter budget.
// ---------------------------------------------------------------------------

func TestHaveAccessDeniesGuestsWithoutBudget(t *testing.T) {
	conf := testConfig()
	tracker := newTracker(t, "999", conf)

	if tracker.HaveAccess(conf) {
		t.Error("a guest with a zero budget must be denied")
	}
}

func TestHaveAccessAllowsUsersWithinBudget(t *testing.T) {
	conf := testConfig()
	tracker := newTracker(t, "200", conf)

	if !tracker.HaveAccess(conf) {
		t.Error("an allowed user under budget must have access")
	}
}

func TestHaveAccessDeniesWhenBudgetIsSpent(t *testing.T) {
	conf := testConfig()
	tracker := newTracker(t, "200", conf)

	tracker.AddCost(1.5)

	if tracker.HaveAccess(conf) {
		t.Error("a user who spent their budget must be denied")
	}
}

func TestHaveAccessAdminsAreUnlimited(t *testing.T) {
	conf := testConfig()
	tracker := newTracker(t, "100", conf)

	tracker.AddCost(5000)

	if !tracker.HaveAccess(conf) {
		t.Error("admins must not be blocked by the budget")
	}
}

func TestOpenBotTreatsEveryoneAsUser(t *testing.T) {
	conf := testConfig()
	conf.AllowedUserChatIDs = nil

	tracker := newTracker(t, "424242", conf)

	if role := tracker.GetUserRole(conf); role != RoleUser {
		t.Errorf("with no allowlist the role should be USER, got %s", role)
	}
	if !tracker.HaveAccess(conf) {
		t.Error("with no allowlist and a user budget, everyone should have access")
	}
}

// ---------------------------------------------------------------------------
// Rate limiting
// ---------------------------------------------------------------------------

func TestRateLimit(t *testing.T) {
	conf := testConfig()
	conf.RateLimitPerMinute = 2
	tracker := newTracker(t, "1", conf)

	if err := tracker.AllowRequest(conf); err != nil {
		t.Fatalf("first request should pass: %v", err)
	}
	if err := tracker.AllowRequest(conf); err != nil {
		t.Fatalf("second request should pass: %v", err)
	}
	if err := tracker.AllowRequest(conf); err == nil {
		t.Fatal("third request must be rejected")
	}

	// Moving the window forward lets requests through again.
	tracker.mu.Lock()
	tracker.rateWindowStart = time.Now().Add(-2 * time.Minute)
	tracker.mu.Unlock()

	if err := tracker.AllowRequest(conf); err != nil {
		t.Fatalf("request after the window should pass: %v", err)
	}
}

func TestRateLimitDisabled(t *testing.T) {
	conf := testConfig()
	conf.RateLimitPerMinute = 0
	tracker := newTracker(t, "1", conf)

	for i := 0; i < 100; i++ {
		if err := tracker.AllowRequest(conf); err != nil {
			t.Fatalf("rate limiting should be disabled: %v", err)
		}
	}
}

// ---------------------------------------------------------------------------
// History
// ---------------------------------------------------------------------------

func TestHistoryIsCappedByMessageCount(t *testing.T) {
	conf := testConfig()
	tracker := newTracker(t, "1", conf)

	for i := 0; i < 10; i++ {
		tracker.AddMessage("user", "message")
	}

	tracker.CheckHistory(conf.MaxHistorySize, conf.MaxHistoryTime)

	if got := len(tracker.GetMessages()); got != 3 {
		t.Errorf("history has %d messages, want 3", got)
	}
}

func TestHistoryExpiresByTime(t *testing.T) {
	conf := testConfig()
	tracker := newTracker(t, "1", conf)

	tracker.History.add(Message{Role: "user", Content: "old", CreatedAt: time.Now().Add(-3 * time.Hour)})
	tracker.History.add(Message{Role: "user", Content: "new", CreatedAt: time.Now()})

	tracker.CheckHistory(10, 60)

	messages := tracker.GetMessages()
	if len(messages) != 1 || messages[0].Content != "new" {
		t.Errorf("expired message was not pruned: %#v", messages)
	}
}

func TestAddMessageIgnoresBlankContent(t *testing.T) {
	conf := testConfig()
	tracker := newTracker(t, "1", conf)

	tracker.AddMessage("user", "   ")
	tracker.AddMessage("assistant", "")

	if got := len(tracker.GetMessages()); got != 0 {
		t.Errorf("blank messages should be ignored, got %d", got)
	}
}

func TestHistorySnapshotIsACopy(t *testing.T) {
	conf := testConfig()
	tracker := newTracker(t, "1", conf)

	tracker.AddMessage("user", "hello")

	snapshot := tracker.GetMessages()
	snapshot[0].Content = "tampered"

	if tracker.GetMessages()[0].Content != "hello" {
		t.Error("GetMessages must return a copy")
	}
}

// ---------------------------------------------------------------------------
// Cost accounting
// ---------------------------------------------------------------------------

func TestAddCostAccumulatesPerDay(t *testing.T) {
	conf := testConfig()
	tracker := newTracker(t, "1", conf)

	tracker.AddCost(0.5)
	tracker.AddCost(0.25)

	if got := tracker.GetCurrentCost("total"); got != 0.75 {
		t.Errorf("total cost = %v, want 0.75", got)
	}
}

func TestGetUsageFromApiIgnoresEmptyID(t *testing.T) {
	conf := testConfig()
	tracker := newTracker(t, "1", conf)

	// An empty id must be a no-op, not an HTTP request that 400s.
	if err := tracker.GetUsageFromApi("", "", ""); err != nil {
		t.Errorf("empty id should be ignored, got %v", err)
	}
	if err := tracker.GetUsageFromApi("   ", "", ""); err != nil {
		t.Errorf("blank id should be ignored, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Stats visibility
// ---------------------------------------------------------------------------

func TestCanViewStats(t *testing.T) {
	conf := testConfig()

	admin := newTracker(t, "100", conf)
	user := newTracker(t, "200", conf)
	guest := newTracker(t, "999", conf)

	if !admin.CanViewStats(conf) {
		t.Error("admins should see stats")
	}
	if user.CanViewStats(conf) {
		t.Error("users should not see stats when stats_min_role is ADMIN")
	}

	conf.StatsMinRole = "USER"
	if !user.CanViewStats(conf) {
		t.Error("users should see stats when stats_min_role is USER")
	}
	if guest.CanViewStats(conf) {
		t.Error("guests should not see stats when stats_min_role is USER")
	}

	// GUEST used to be ignored entirely.
	conf.StatsMinRole = "GUEST"
	if !guest.CanViewStats(conf) {
		t.Error("guests should see stats when stats_min_role is GUEST")
	}
}

// ---------------------------------------------------------------------------
// Stream lifecycle
// ---------------------------------------------------------------------------

func TestStopStreamWithoutStream(t *testing.T) {
	conf := testConfig()
	tracker := newTracker(t, "1", conf)

	if tracker.StopStream() {
		t.Error("StopStream should report false when nothing is running")
	}
	if tracker.HasStream() {
		t.Error("HasStream should be false")
	}
}

func TestSystemPromptIsSynchronised(t *testing.T) {
	conf := testConfig()
	tracker := newTracker(t, "1", conf)

	if got := tracker.GetSystemPrompt(); got != conf.SystemPrompt {
		t.Errorf("system prompt = %q, want %q", got, conf.SystemPrompt)
	}

	tracker.SetSystemPrompt("be brief")

	if got := tracker.GetSystemPrompt(); got != "be brief" {
		t.Errorf("system prompt = %q, want %q", got, "be brief")
	}
}
