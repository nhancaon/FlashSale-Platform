package race

import (
	"os"
	"testing"
)

// TestRacy is meant to FAIL under `go test -race` ("WARNING: DATA RACE"): run.sh runs it on purpose, records whether the
// detector caught the race and how long it took. It only runs when LAB_RACE=1, so `go test ./...` stays green.
func TestRacy(t *testing.T) {
	if os.Getenv("LAB_RACE") != "1" {
		t.Skip("set LAB_RACE=1: this test demonstrates a race and fails under -race")
	}
	got := RacyCount(8, 10_000)
	t.Logf("racy count: %d of %d (lost updates: %d)", got, 80_000, 80_000-got)
}

// TestSafe passes with -race: the fix.
func TestSafe(t *testing.T) {
	if got := SafeCount(8, 10_000); got != 80_000 {
		t.Fatalf("got %d, want 80000", got)
	}
}
