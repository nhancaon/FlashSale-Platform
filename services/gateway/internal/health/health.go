// Package health aggregates the readiness of the gateway's dependencies.
package health

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"time"
)

// Check reports whether one dependency is usable.
type Check func(ctx context.Context) error

// Aggregator runs all checks concurrently, each with the same timeout.
type Aggregator struct {
	Checks  map[string]Check
	Timeout time.Duration
}

type report struct {
	Status     string            `json:"status"` // ok or degraded
	Components map[string]string `json:"components"`
}

// ReadyHandler answers 200 when every dependency is healthy and 503 when any is not, with one line per component.
func (a *Aggregator) ReadyHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		names := make([]string, 0, len(a.Checks))
		for n := range a.Checks {
			names = append(names, n)
		}
		sort.Strings(names)

		results := make(map[string]string, len(names))
		var mu sync.Mutex
		var wg sync.WaitGroup
		for _, n := range names {
			wg.Add(1)
			go func(name string, check Check) {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(r.Context(), a.Timeout)
				defer cancel()
				status := "ok"
				if err := check(ctx); err != nil {
					status = "down"
				}
				mu.Lock()
				results[name] = status
				mu.Unlock()
			}(n, a.Checks[n])
		}
		wg.Wait()

		rep := report{Status: "ok", Components: results}
		code := http.StatusOK
		for _, s := range results {
			if s != "ok" {
				rep.Status, code = "degraded", http.StatusServiceUnavailable
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(rep)
	})
}

// HTTPCheck is a Check that expects 200 from GET url.
func HTTPCheck(client *http.Client, url string) Check {
	return func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return &statusError{resp.StatusCode}
		}
		return nil
	}
}

type statusError struct{ code int }

func (e *statusError) Error() string { return "status " + http.StatusText(e.code) }
