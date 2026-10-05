package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Result is one measured repetition. Go and Java print exactly the same shape (one JSON object per line), so
// report.mjs aggregates both without knowing which language produced them.
type Result struct {
	Exp     int                `json:"exp"`
	Lang    string             `json:"lang"`
	Variant string             `json:"variant"`
	Param   string             `json:"param"`
	Rep     int                `json:"rep"`
	Metrics map[string]float64 `json:"metrics"`
	Notes   string             `json:"notes,omitempty"`
}

// measure runs fn `warmup` times without recording, then `reps` times, printing one Result per repetition.
// Every repetition also records wall time, process CPU time and the CPU utilisation over the available CPUs.
func measure(exp int, variant, param string, warmup, reps int, fn func() map[string]float64) {
	for i := 0; i < warmup; i++ {
		fn()
	}
	for rep := 1; rep <= reps; rep++ {
		runtime.GC()
		cpu0 := cpuTime()
		start := time.Now()
		m := fn()
		wall := time.Since(start)
		cpu := cpuTime() - cpu0
		if m == nil {
			m = map[string]float64{}
		}
		m["wallMs"] = ms(wall)
		m["cpuMs"] = ms(cpu)
		m["cpuUtil"] = cpu.Seconds() / wall.Seconds() / float64(runtime.GOMAXPROCS(0))
		m["peakRssMb"] = peakRSSMB()
		emit(Result{Exp: exp, Lang: "go", Variant: variant, Param: param, Rep: rep, Metrics: m})
	}
}

func emit(r Result) {
	b, err := json.Marshal(r)
	if err != nil {
		panic(err)
	}
	fmt.Println(string(b))
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// cpuTime is user + system time of the whole process.
func cpuTime() time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// peakRSSMB is the high-water mark of the resident set (VmHWM), the same source Java reads, so both are comparable.
func peakRSSMB() float64 {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return -1
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		if v, ok := strings.CutPrefix(s.Text(), "VmHWM:"); ok {
			kb, _ := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(v), "kB")), 64)
			return kb / 1024
		}
	}
	return -1
}

// percentile of an already sorted slice (nearest rank).
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(p/100*float64(len(sorted))+0.5) - 1
	return sorted[max(0, min(i, len(sorted)-1))]
}
