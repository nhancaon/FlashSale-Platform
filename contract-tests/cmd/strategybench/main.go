// strategybench measures reserve latency and throughput of one inventory service for two scenarios and
// prints one JSON line per run (see loadtest/run-inventory-bench.sh).
//
//	sellout: 100 units, 2000 buyers  -> mostly OUT_OF_STOCK, heavy contention on one row
//	plenty : 100000 units, 2000 buyers -> every reserve succeeds, pure write cost under contention
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/nhancaon/flashsale/contract-tests/internal/fixture"
)

type result struct {
	Service    string  `json:"service"`
	Strategy   string  `json:"strategy"`
	Scenario   string  `json:"scenario"`
	Run        int     `json:"run"`
	Requests   int     `json:"requests"`
	Stock      int     `json:"stock"`
	Workers    int     `json:"workers"`
	OK         int     `json:"ok"`
	OutOfStock int     `json:"outOfStock"`
	Contention int     `json:"contention"`
	Errors     int     `json:"errors"`
	DurationMs float64 `json:"durationMs"`
	RPS        float64 `json:"rps"`
	P50Ms      float64 `json:"p50Ms"`
	P95Ms      float64 `json:"p95Ms"`
	P99Ms      float64 `json:"p99Ms"`
	MaxMs      float64 `json:"maxMs"`
	Oversold   bool    `json:"oversold"`
	Consistent bool    `json:"consistent"`
}

func main() {
	service := flag.String("service", "", "label: go or java")
	scenario := flag.String("scenario", "sellout", "sellout | plenty")
	requests := flag.Int("requests", 2000, "number of reserve requests")
	workers := flag.Int("workers", 64, "concurrent in-flight requests")
	runs := flag.Int("runs", 3, "repetitions")
	flag.Parse()

	env, err := fixture.Open()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	stock := 100
	if *scenario == "plenty" {
		stock = 100000
	}
	for run := 1; run <= *runs; run++ {
		r, err := once(env, *service, *scenario, run, stock, *requests, *workers)
		if err != nil {
			fmt.Fprintln(os.Stderr, "run failed:", err)
			os.Exit(1)
		}
		b, _ := json.Marshal(r)
		fmt.Println(string(b))
	}
}

func once(env *fixture.Env, service, scenario string, run, stock, requests, workers int) (result, error) {
	ctx := context.Background()
	sku, productID, err := env.NewSKU(ctx, stock)
	if err != nil {
		return result{}, err
	}
	c := env.Client()
	jobs := make(chan int)
	lat := make([]float64, requests)
	status := make([]string, requests)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				t := time.Now()
				resp, err := c.Reserve(fmt.Sprintf("bench-%s-%d", sku, i), sku, 1)
				lat[i] = float64(time.Since(t).Microseconds()) / 1000
				switch {
				case err != nil:
					status[i] = "error"
				case resp.Status == 200:
					status[i] = "ok"
				case resp.Status == 409 && resp.Str("code") == "OUT_OF_STOCK":
					status[i] = "oos"
				case resp.Status == 503 && resp.Str("code") == "CONTENTION":
					status[i] = "contention"
				default:
					status[i] = "error"
				}
			}
		}()
	}
	start := time.Now()
	for i := 0; i < requests; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	elapsed := time.Since(start)

	r := result{Service: service, Strategy: env.Strategy, Scenario: scenario, Run: run, Requests: requests, Stock: stock, Workers: workers,
		DurationMs: float64(elapsed.Microseconds()) / 1000, RPS: float64(requests) / elapsed.Seconds()}
	for _, s := range status {
		switch s {
		case "ok":
			r.OK++
		case "oos":
			r.OutOfStock++
		case "contention":
			r.Contention++
		default:
			r.Errors++
		}
	}
	sorted := append([]float64(nil), lat...)
	sort.Float64s(sorted)
	pct := func(p float64) float64 { return sorted[min(len(sorted)-1, int(float64(len(sorted))*p))] }
	r.P50Ms, r.P95Ms, r.P99Ms, r.MaxMs = pct(.50), pct(.95), pct(.99), sorted[len(sorted)-1]

	db, err := env.DBStock(ctx, productID)
	if err != nil {
		return r, err
	}
	reserved, _, err := env.DBReservations(ctx, productID, "RESERVED")
	if err != nil {
		return r, err
	}
	r.Oversold = int64(r.OK) > int64(stock) || db.Available < 0
	r.Consistent = db.Available+db.Reserved == int64(stock) && reserved == int64(r.OK)
	return r, nil
}
