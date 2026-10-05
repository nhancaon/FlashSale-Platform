package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"runtime"
	"runtime/pprof"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---- 1. N concurrent tasks, each sleeps 100 ms ----

func exp1(n int) map[string]float64 {
	var wg sync.WaitGroup
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			time.Sleep(100 * time.Millisecond)
		}()
	}
	wg.Wait()
	return map[string]float64{"tasks": float64(n)}
}

// ---- 2. worker pool, M CPU-bound jobs ----

// cpuJob hashes a small buffer `rounds` times: pure CPU, no allocation in the loop, identical work in Java.
func cpuJob(seed, rounds int) byte {
	var buf [32]byte
	buf[0] = byte(seed)
	for range rounds {
		buf = sha256.Sum256(buf[:])
	}
	return buf[0]
}

func exp2(jobs, rounds int) map[string]float64 {
	workers := runtime.GOMAXPROCS(0)
	in := make(chan int, workers*2)
	var wg sync.WaitGroup
	var sink atomic.Uint64
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range in {
				sink.Add(uint64(cpuJob(j, rounds)))
			}
		}()
	}
	start := time.Now()
	for j := range jobs {
		in <- j
	}
	close(in)
	wg.Wait()
	return map[string]float64{"jobsPerSec": float64(jobs) / time.Since(start).Seconds(), "workers": float64(workers)}
}

// ---- 3. fan-out of 10k simulated I/O calls (50-200 ms) ----

// latencies is deterministic and identical in Java: 50 + (i*7919 mod 151) ms, spread over 50..200 ms.
func latencies(n int) []time.Duration {
	d := make([]time.Duration, n)
	for i := range d {
		d[i] = time.Duration(50+(i*7919)%151) * time.Millisecond
	}
	return d
}

func exp3(n int) map[string]float64 {
	lat := latencies(n)
	done := make([]float64, n)
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select { // a "call" that honours cancellation, like an HTTP client with a context
			case <-time.After(lat[i]):
			case <-ctx.Done():
				return
			}
			done[i] = ms(time.Since(start)) - ms(lat[i]) // scheduling overhead on top of the simulated latency
		}()
	}
	wg.Wait()
	slices.Sort(done)
	return map[string]float64{"calls": float64(n), "overheadP50Ms": percentile(done, 50), "overheadP99Ms": percentile(done, 99)}
}

// ---- 4. shared counter, T threads x K increments ----

func exp4(variant string, threads, perThread int) map[string]float64 {
	var total int64
	var wg sync.WaitGroup
	start := time.Now()
	switch variant {
	case "mutex":
		var mu sync.Mutex
		var c int64
		for range threads {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range perThread {
					mu.Lock()
					c++
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		total = c
	case "atomic":
		var c atomic.Int64
		for range threads {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range perThread {
					c.Add(1)
				}
			}()
		}
		wg.Wait()
		total = c.Load()
	case "channel": // one owner goroutine, everybody else sends increments
		ch := make(chan int64, 1024)
		res := make(chan int64)
		go func() {
			var c int64
			for d := range ch {
				c += d
			}
			res <- c
		}()
		for range threads {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range perThread {
					ch <- 1
				}
			}()
		}
		wg.Wait()
		close(ch)
		total = <-res
	default:
		panic("unknown variant " + variant)
	}
	el := time.Since(start)
	expected := int64(threads) * int64(perThread)
	return map[string]float64{"nsPerOp": float64(el.Nanoseconds()) / float64(expected), "correct": b2f(total == expected)}
}

// ---- 6. bounded producer-consumer ----

func exp6(variant string, items, capacity, producers, consumers int) map[string]float64 {
	q := make(chan int, capacity)
	var produced, dropped, consumed atomic.Int64
	var blockedNs atomic.Int64
	var pw, cw sync.WaitGroup
	for range consumers {
		cw.Add(1)
		go func() {
			defer cw.Done()
			for v := range q {
				_ = cpuJob(v, 2) // a little work per item, so the queue fills up
				consumed.Add(1)
			}
		}()
	}
	start := time.Now()
	for p := range producers {
		pw.Add(1)
		go func() {
			defer pw.Done()
			for i := p; i < items; i += producers {
				switch variant {
				case "block": // backpressure: the producer waits while the queue is full
					t := time.Now()
					q <- i
					blockedNs.Add(time.Since(t).Nanoseconds())
				case "drop": // load shedding: never wait, count what did not fit
					select {
					case q <- i:
					default:
						dropped.Add(1)
						continue
					}
				}
				produced.Add(1)
			}
		}()
	}
	pw.Wait()
	close(q)
	cw.Wait()
	el := time.Since(start)
	return map[string]float64{
		"itemsPerSec":       float64(consumed.Load()) / el.Seconds(),
		"dropped":           float64(dropped.Load()),
		"producerBlockedMs": float64(blockedNs.Load()) / 1e6 / float64(producers),
		"correct":           b2f(consumed.Load() == produced.Load()),
	}
}

// ---- 7. cancellation and timeout propagation ----

func exp7(tasks int) map[string]float64 {
	before := runtime.NumGoroutine()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	var stopped sync.WaitGroup
	for range tasks {
		stopped.Add(1)
		go func() {
			defer stopped.Done()
			for { // a long job made of short steps, checking the context between steps
				select {
				case <-ctx.Done():
					return
				case <-time.After(10 * time.Millisecond):
				}
			}
		}()
	}
	<-ctx.Done()
	t := time.Now()
	stopped.Wait()
	stopMs := ms(time.Since(t))
	time.Sleep(50 * time.Millisecond)
	return map[string]float64{"stopMs": stopMs, "leaked": float64(max(0, runtime.NumGoroutine()-before))}
}

// ---- 8. deliberate deadlock and goroutine leak, then diagnosis ----

func exp8(variant string) map[string]float64 {
	switch variant {
	case "deadlock":
		// Two goroutines take two mutexes in opposite order. Go has no lock-order detector for a partial deadlock
		// (the runtime only reports "all goroutines are asleep"), so we diagnose it the way an operator would:
		// a goroutine dump (pprof, debug=2) after a watchdog timeout, then look for goroutines stuck in Mutex.Lock.
		var a, b sync.Mutex
		go func() { a.Lock(); time.Sleep(10 * time.Millisecond); b.Lock() }()
		go func() { b.Lock(); time.Sleep(10 * time.Millisecond); a.Lock() }()
		start := time.Now()
		time.Sleep(200 * time.Millisecond) // watchdog: no progress for 200 ms
		var sb strings.Builder
		_ = pprof.Lookup("goroutine").WriteTo(&sb, 2)
		stuck := strings.Count(sb.String(), "sync.(*Mutex).Lock")
		if os.Getenv("LAB_DUMP_DIR") != "" {
			_ = os.WriteFile(os.Getenv("LAB_DUMP_DIR")+"/go-deadlock-goroutines.txt", []byte(sb.String()), 0o644)
		}
		return map[string]float64{"detected": b2f(stuck >= 2), "detectMs": ms(time.Since(start)), "stuckGoroutines": float64(stuck)}
	case "leak":
		// Each "request" starts a goroutine that sends its result on an unbuffered channel nobody reads after a timeout.
		before := runtime.NumGoroutine()
		for range 1000 {
			ch := make(chan int)
			go func() { ch <- 1 }() // leaks: the receiver below gave up
			select {
			case <-ch:
			case <-time.After(time.Microsecond):
			}
		}
		time.Sleep(20 * time.Millisecond)
		leaked := runtime.NumGoroutine() - before
		return map[string]float64{"detected": b2f(leaked > 0), "leaked": float64(leaked)}
	}
	panic("unknown variant " + variant)
}

// ---- 9. pipeline: parse -> transform -> write ----

func lines(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = strconv.Itoa(i) + ",item-" + strconv.Itoa(i%1000) + "," + strconv.Itoa(i%97)
	}
	return out
}

type record struct {
	id    int
	sku   string
	qty   int
	total int
}

func parse(l string) (record, error) {
	p := strings.Split(l, ",")
	if len(p) != 3 {
		return record{}, errors.New("bad line")
	}
	id, err1 := strconv.Atoi(p[0])
	qty, err2 := strconv.Atoi(p[2])
	return record{id: id, sku: p[1], qty: qty}, errors.Join(err1, err2)
}

func exp9(in []string) map[string]float64 {
	parsed := make(chan record, 1024)
	transformed := make(chan record, 1024)
	var written atomic.Int64
	var checksum atomic.Int64
	go func() { // stage 1
		defer close(parsed)
		for _, l := range in {
			if r, err := parse(l); err == nil {
				parsed <- r
			}
		}
	}()
	var tw sync.WaitGroup
	for range runtime.GOMAXPROCS(0) { // stage 2, parallel
		tw.Add(1)
		go func() {
			defer tw.Done()
			for r := range parsed {
				r.total = r.qty * 490
				transformed <- r
			}
		}()
	}
	go func() { tw.Wait(); close(transformed) }()
	for r := range transformed { // stage 3, single writer
		written.Add(1)
		checksum.Add(int64(r.total))
	}
	return map[string]float64{"written": float64(written.Load()), "checksum": float64(checksum.Load())} // lines/s = written / wallMs
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
