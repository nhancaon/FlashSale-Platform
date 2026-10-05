// Command lab runs one experiment of the concurrency lab and prints one JSON line per repetition.
//
//	lab -exp 1 -variant goroutine -n 100000 [-reps 5] [-warmup 1] [-cpuprofile file]
//
// The Java side (../java) accepts the same flags and prints the same JSON, see ../README.md.
// Linux only: memory and CPU are read from /proc and getrusage (the lab always runs in a Linux container).
package main

import (
	"flag"
	"fmt"
	"os"
	"runtime/pprof"
	"strconv"
)

func main() {
	exp := flag.Int("exp", 0, "experiment 1..9 (10 is the LOC count done by report.mjs)")
	variant := flag.String("variant", "", "variant of the experiment")
	n := flag.Int("n", 0, "size parameter (tasks, jobs, calls, items...)")
	reps := flag.Int("reps", 5, "measured repetitions")
	warmup := flag.Int("warmup", 1, "unmeasured repetitions first")
	cpuprofile := flag.String("cpuprofile", "", "write a pprof CPU profile of the whole run (warm-up included)")
	flag.Parse()

	if *cpuprofile != "" {
		f, err := os.Create(*cpuprofile)
		if err != nil {
			fail(err)
		}
		defer f.Close()
		if err := pprof.StartCPUProfile(f); err != nil {
			fail(err)
		}
		defer pprof.StopCPUProfile()
	}

	p := strconv.Itoa(*n)
	switch *exp {
	case 1:
		measure(1, "goroutine", p, *warmup, *reps, func() map[string]float64 { return exp1(*n) })
	case 2:
		measure(2, "goroutine-channel-pool", p, *warmup, *reps, func() map[string]float64 { return exp2(*n, 2000) })
	case 3:
		measure(3, "goroutine-waitgroup", p, *warmup, *reps, func() map[string]float64 { return exp3(*n) })
	case 4:
		measure(4, *variant, "100x"+p, *warmup, *reps, func() map[string]float64 { return exp4(*variant, 100, *n) })
	case 6:
		measure(6, "channel-"+*variant, p, *warmup, *reps, func() map[string]float64 { return exp6(*variant, *n, 100, 4, 4) })
	case 7:
		measure(7, "context", p, *warmup, *reps, func() map[string]float64 { return exp7(*n) })
	case 8:
		measure(8, *variant, "", 0, *reps, func() map[string]float64 { return exp8(*variant) })
	case 9:
		in := lines(*n) // input built once, outside the measured time
		measure(9, "channel-pipeline", p, *warmup, *reps, func() map[string]float64 { return exp9(in) })
	default:
		fail(fmt.Errorf("unknown experiment %d (5 is `go test -race ./race`, 10 is counted by report.mjs)", *exp))
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "lab:", err)
	os.Exit(2)
}
