package main

import (
	"testing"

	"openrouter-bot/config"
)

// TestPermitted covers the one rule that decides who may talk to the bot, in
// private chats and in groups alike.
func TestPermitted(t *testing.T) {
	cases := []struct {
		mode      string
		owner     bool
		chatAdmin bool
		want      bool
	}{
		{config.AccessEveryone, false, false, true},
		{config.AccessEveryone, true, false, true},
		{config.AccessAdmins, false, true, true},
		{config.AccessAdmins, false, false, false},
		{config.AccessAdmins, true, false, true},
		{config.AccessOwner, true, false, true},
		{config.AccessOwner, false, true, false},
		{config.AccessOwner, false, false, false},
		{"", false, false, true},
	}

	for _, tc := range cases {
		chatAdmin := func() bool { return tc.chatAdmin }

		if got := permitted(tc.mode, tc.owner, chatAdmin); got != tc.want {
			t.Errorf("permitted(%q, owner=%v, chatAdmin=%v) = %v, want %v",
				tc.mode, tc.owner, tc.chatAdmin, got, tc.want)
		}
	}
}

// TestPrivateAccessDefaultsToEveryone guards the public bot behaviour: a
// configuration without PRIVATE_ACCESS must answer anybody.
func TestPrivateAccessDefaultsToEveryone(t *testing.T) {
	conf := &config.Config{BudgetPeriod: "monthly"}

	if err := conf.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	if conf.PrivateAccess != config.AccessEveryone {
		t.Errorf("PrivateAccess = %q, want %q", conf.PrivateAccess, config.AccessEveryone)
	}
	if !permitted(conf.PrivateAccess, false, nil) {
		t.Error("a stranger must be able to use a public bot in a direct chat")
	}
}

// TestPublicModeOpensPrivateChats pins that PUBLIC_MODE wins over
// PRIVATE_ACCESS, since it is documented as removing every restriction.
func TestPublicModeOpensPrivateChats(t *testing.T) {
	conf := &config.Config{BudgetPeriod: "monthly", PrivateAccess: config.AccessOwner, PublicMode: true}

	if err := conf.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	if conf.PrivateAccess != config.AccessEveryone {
		t.Errorf("PrivateAccess = %q, want PUBLIC_MODE to open it", conf.PrivateAccess)
	}
	if !permitted(conf.PrivateAccess, false, nil) {
		t.Error("a stranger must be able to use a public bot")
	}
}

// TestOwnerOnlyPrivateChats covers the assistant setup: the bot answers the
// owner and stays quiet for everybody else, without crashing on an empty
// owner list.
func TestOwnerOnlyPrivateChats(t *testing.T) {
	conf := &config.Config{BudgetPeriod: "monthly", PrivateAccess: config.AccessOwner, AdminChatIDs: []int64{777}}

	if err := conf.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	if !permitted(conf.PrivateAccess, conf.IsAdmin(777), nil) {
		t.Error("the owner must be answered")
	}
	if permitted(conf.PrivateAccess, conf.IsAdmin(999), nil) {
		t.Error("a stranger must not be answered")
	}
}

// TestUnknownPrivateAccessFallsBack keeps a typo in PRIVATE_ACCESS from
// locking the owner out or opening a restricted bot.
func TestUnknownPrivateAccessFallsBack(t *testing.T) {
	conf := &config.Config{BudgetPeriod: "monthly", PrivateAccess: "owner-only"}

	if err := conf.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	if conf.PrivateAccess != config.AccessEveryone {
		t.Errorf("PrivateAccess = %q, want the safe default", conf.PrivateAccess)
	}
}

// TestPermittedSkipsTheAdminLookupWhenEveryoneIsAllowed pins the cost rule:
// in a public group the bot must not call the Telegram API for every member
// just to throw the answer away.
func TestPermittedSkipsTheAdminLookupWhenEveryoneIsAllowed(t *testing.T) {
	lookups := 0
	lookup := func() bool {
		lookups++

		return false
	}

	if !permitted(config.AccessEveryone, false, lookup) {
		t.Fatal("everyone mode must admit the user")
	}
	if lookups != 0 {
		t.Errorf("the chat-admin lookup ran %d time(s) in everyone mode", lookups)
	}

	for _, mode := range []string{config.AccessAdmins, config.AccessOwner} {
		lookups = 0
		if permitted(mode, false, lookup) {
			t.Errorf("%s mode admitted a stranger", mode)
		}
		if mode == config.AccessAdmins && lookups == 0 {
			t.Error("admins mode must look the member up")
		}
	}

	// An owner bypasses the lookup entirely, whatever the mode.
	lookups = 0
	if !permitted(config.AccessAdmins, true, lookup) {
		t.Error("the owner must always pass")
	}
	if lookups != 0 {
		t.Errorf("the owner triggered %d lookup(s)", lookups)
	}
}

// TestWarnOnceThrottlesPerNotice pins the anti-spam rule: a restricted user
// is told once, not once per message, while a different notice (or a different
// user) is still delivered.
func TestWarnOnceThrottlesPerNotice(t *testing.T) {
	app := &app{}

	if !app.warnOnce(5, "group") {
		t.Error("the first group notice must be sent")
	}
	if app.warnOnce(5, "group") {
		t.Error("a repeated group notice within an hour must be suppressed")
	}
	if !app.warnOnce(5, "private") {
		t.Error("the direct-message notice must not be silenced by the group one")
	}
	if !app.warnOnce(6, "group") {
		t.Error("another user must still be told")
	}
}
