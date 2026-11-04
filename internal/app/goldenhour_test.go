package app

import (
	"testing"
	"time"
)

func TestIsGoldenHour(t *testing.T) {
	// London, 21 June: sunset ~21:21 local solar-ish; midday is not golden, evening is.
	noon := time.Date(2024, 6, 21, 12, 0, 0, 0, time.UTC)
	evening := time.Date(2024, 6, 21, 20, 45, 0, 0, time.UTC)
	if isGoldenHour(noon, 51.5, 0) {
		t.Error("noon should not be golden hour")
	}
	if !isGoldenHour(evening, 51.5, 0) {
		t.Error("evening should be golden hour")
	}
}
