package ratelimit

import (
	"testing"
)

func TestEffectiveLimitByRoleAndCategory(t *testing.T) {
	if got := EffectiveLimit(0, RoleUser, CategoryChat); got != 0 {
		t.Fatalf("base=0 must disable rate limiting, got %d", got)
	}
	if got := EffectiveLimit(10, RoleAdmin, CategoryChat); got != 0 {
		t.Fatalf("admins must have unlimited requests, got %d", got)
	}
	if got := EffectiveLimit(10, RoleGuest, CategoryChat); got != 5 {
		t.Errorf("guest chat limit = %d, want 5", got)
	}
	if got := EffectiveLimit(10, RoleUser, CategoryChat); got != 10 {
		t.Errorf("user chat limit = %d, want 10", got)
	}
	if got := EffectiveLimit(10, RolePremium, CategoryChat); got != 20 {
		t.Errorf("premium chat limit = %d, want 20", got)
	}
	if got := EffectiveLimit(10, RoleUser, CategoryResearch); got != 4 {
		t.Errorf("user research limit = %d, want 4", got)
	}
}

func TestLimiterIsolatesCategories(t *testing.T) {
	lim := New(nil)

	// Base 4 -> research limit is 1, chat limit is 4.
	if err := lim.Allow("u1", RoleUser, CategoryResearch, 4); err != nil {
		t.Fatalf("first research request should pass: %v", err)
	}
	if err := lim.Allow("u1", RoleUser, CategoryResearch, 4); err == nil {
		t.Fatal("second research request should be rate limited")
	}

	// Chat category should still have quota!
	if err := lim.Allow("u1", RoleUser, CategoryChat, 4); err != nil {
		t.Fatalf("chat category should not be blocked by research limit: %v", err)
	}
}
