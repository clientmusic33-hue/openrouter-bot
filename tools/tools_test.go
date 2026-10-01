package tools

import (
	"context"
	"math"
	"testing"

	"openrouter-bot/features/reminders"
	jsonstore "openrouter-bot/storage/json"
)

func TestEvalMath(t *testing.T) {
	cases := map[string]float64{
		"2 + 3 * 4":       14,
		"(2 + 3) * 4":     20,
		"2^10":            1024,
		"sqrt(144) + 8":   20,
		"abs(-42)":        42,
		"log(1000)":       3,
		"sin(0) + cos(0)": 1,
	}

	for expr, want := range cases {
		got, err := EvalMath(expr)
		if err != nil {
			t.Fatalf("EvalMath(%q): %v", expr, err)
		}
		if math.Abs(got-want) > 1e-9 {
			t.Errorf("EvalMath(%q) = %v, want %v", expr, got, want)
		}
	}

	if _, err := EvalMath("1 / 0"); err == nil {
		t.Error("expected division by zero error")
	}
}

func TestToolPermissionsAndExecution(t *testing.T) {
	rem := reminders.NewManager(jsonstore.New(t.TempDir()))
	reg := DefaultRegistry(nil, rem)

	// Calculator is open to everyone.
	out, err := reg.Run(context.Background(), "calculator", "u1", "GUEST", "6 * 7")
	if err != nil || out != "42" {
		t.Fatalf("calculator Run = %q, %v; want 42", out, err)
	}

	// File tool is disabled by default even for ADMIN.
	if _, err := reg.Run(context.Background(), "file", "u1", "ADMIN", "/etc/passwd"); err == nil {
		t.Fatal("disabled file tool must not run even for ADMIN")
	}

	// URL tool requires USER or ADMIN, not GUEST.
	if _, err := reg.Run(context.Background(), "url", "u1", "GUEST", "https://example.com"); err == nil {
		t.Fatal("url tool should be denied for GUEST")
	}

	// Notes tool works for user.
	if _, err := reg.Run(context.Background(), "notes", "u1", "USER", "add: buy milk"); err != nil {
		t.Fatalf("notes add failed: %v", err)
	}
	listOut, err := reg.Run(context.Background(), "notes", "u1", "USER", "list")
	if err != nil || listOut == "" {
		t.Fatalf("notes list failed: %q, %v", listOut, err)
	}
}
