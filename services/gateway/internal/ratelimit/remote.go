// Package ratelimit adapts the standalone ratelimiter service to the limiter.Limiter interface, so the gateway
// can use either the embedded library or the remote service without changing the middleware (Strategy pattern).
package ratelimit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/nhancaon/flashsale/services/ratelimiter-go/pkg/limiter"
)

// Remote calls POST /v1/check of a ratelimiter service (Go or Java: same contract).
type Remote struct {
	url    string
	client *http.Client
}

func NewRemote(baseURL string, timeout time.Duration) *Remote {
	return &Remote{url: strings.TrimRight(baseURL, "/") + "/v1/check", client: &http.Client{Timeout: timeout}}
}

func (r *Remote) Name() string { return "remote" }

type checkRequest struct {
	Key       string `json:"key"`
	Limit     int64  `json:"limit"`
	WindowSec int64  `json:"windowSec"`
}

type checkResponse struct {
	Allowed      bool  `json:"allowed"`
	Remaining    int64 `json:"remaining"`
	RetryAfterMs int64 `json:"retryAfterMs"`
}

func (r *Remote) Allow(ctx context.Context, key string, limit int64, window time.Duration) (limiter.Result, error) {
	body, _ := json.Marshal(checkRequest{Key: key, Limit: limit, WindowSec: int64(window / time.Second)})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.url, bytes.NewReader(body))
	if err != nil {
		return limiter.Result{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return limiter.Result{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return limiter.Result{}, fmt.Errorf("ratelimiter answered %d", resp.StatusCode)
	}
	var out checkResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out); err != nil {
		return limiter.Result{}, fmt.Errorf("ratelimiter response: %w", err)
	}
	return limiter.Result{Allowed: out.Allowed, Remaining: out.Remaining, RetryAfterMs: out.RetryAfterMs}, nil
}
