package main

import "testing"

// The experiments measure speed; these tests check they compute the right thing, so a fast wrong variant cannot win.

func TestCountersAreExact(t *testing.T) {
	for _, v := range []string{"mutex", "atomic", "channel"} {
		if m := exp4(v, 8, 1000); m["correct"] != 1 {
			t.Errorf("%s lost increments", v)
		}
	}
}

func TestQueueLosesNothingWhenBlocking(t *testing.T) {
	m := exp6("block", 10_000, 10, 2, 2)
	if m["correct"] != 1 || m["dropped"] != 0 {
		t.Fatalf("block variant: %v", m)
	}
	if m := exp6("drop", 10_000, 10, 2, 2); m["correct"] != 1 {
		t.Fatalf("drop variant: consumed != produced: %v", m)
	}
}

func TestPipelineChecksumMatchesJava(t *testing.T) {
	// The Java side prints the same checksum for the same input (2351845650 for 100k lines).
	m := exp9(lines(100_000))
	if m["written"] != 100_000 || m["checksum"] != 2351845650 {
		t.Fatalf("pipeline: %v", m)
	}
}

func TestCancellationStopsEverything(t *testing.T) {
	if m := exp7(1000); m["leaked"] != 0 {
		t.Fatalf("leaked goroutines after cancel: %v", m)
	}
}

func TestDiagnosesFindTheBugs(t *testing.T) {
	if m := exp8("deadlock"); m["detected"] != 1 {
		t.Fatalf("deadlock not found in the goroutine dump: %v", m)
	}
	if m := exp8("leak"); m["detected"] != 1 {
		t.Fatalf("leak not seen: %v", m)
	}
}

func TestLatenciesStayInRange(t *testing.T) {
	for i, d := range latencies(10_000) {
		if ms := d.Milliseconds(); ms < 50 || ms > 200 {
			t.Fatalf("latency %d = %d ms", i, ms)
		}
	}
}
