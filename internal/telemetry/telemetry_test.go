package telemetry

import (
	"errors"
	"testing"
)

func TestCollectorTracksRequestsErrorsModelsAndCost(t *testing.T) {
	c := NewCollector()

	done1 := c.BeginRequest()
	done1("groq/llama-3.3-70b-versatile", 450, nil)

	done2 := c.BeginRequest()
	done2("gemini-2.5-flash", 100, errors.New("boom"))

	c.AddCost(0.0025)

	snap := c.Snapshot()
	if snap.TotalRequests != 2 {
		t.Errorf("TotalRequests = %d, want 2", snap.TotalRequests)
	}
	if snap.ActiveRequests != 0 {
		t.Errorf("ActiveRequests = %d, want 0", snap.ActiveRequests)
	}
	if snap.ErrorCount != 1 || snap.ErrorRatePct != 50 {
		t.Errorf("ErrorCount=%d ErrorRatePct=%v, want 1 and 50%%", snap.ErrorCount, snap.ErrorRatePct)
	}
	if snap.TokensUsed != 550 {
		t.Errorf("TokensUsed = %d, want 550", snap.TokensUsed)
	}
	if snap.TotalCostUSD != 0.0025 {
		t.Errorf("TotalCostUSD = %v, want 0.0025", snap.TotalCostUSD)
	}
	if len(snap.TopModels) != 2 {
		t.Errorf("TopModels = %+v, want 2 entries", snap.TopModels)
	}
}
