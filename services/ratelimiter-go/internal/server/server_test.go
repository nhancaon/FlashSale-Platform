package server_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nhancaon/flashsale/services/ratelimiter-go/internal/server"
	"github.com/nhancaon/flashsale/services/ratelimiter-go/pkg/limiter"
)

type stubLimiter struct {
	result limiter.Result
	err    error
	gotKey string
	gotWin time.Duration
}

func (s *stubLimiter) Name() string { return "stub" }

func (s *stubLimiter) Allow(_ context.Context, key string, _ int64, window time.Duration) (limiter.Result, error) {
	s.gotKey, s.gotWin = key, window
	return s.result, s.err
}

type stubPinger struct{ err error }

func (p stubPinger) Ping(context.Context) error { return p.err }

func newServer(l limiter.Limiter, ready server.Pinger) http.Handler {
	return server.New(server.Config{
		Limiter: l, Ready: ready,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}).Handler()
}

func post(h http.Handler, body string, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/check", strings.NewReader(body))
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestCheckAllowed(t *testing.T) {
	stub := &stubLimiter{result: limiter.Result{Allowed: true, Remaining: 4}}
	rec := post(newServer(stub, nil), `{"key":"user:1","limit":5,"windowSec":60}`)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"allowed":true,"remaining":4,"retryAfterMs":0}`, rec.Body.String())
	assert.Equal(t, "user:1", stub.gotKey)
	assert.Equal(t, 60*time.Second, stub.gotWin)
}

func TestCheckRejected(t *testing.T) {
	stub := &stubLimiter{result: limiter.Result{Allowed: false, RetryAfterMs: 1500}}
	rec := post(newServer(stub, nil), `{"key":"k","limit":1,"windowSec":1}`)

	assert.Equal(t, http.StatusOK, rec.Code, "a rejection is a normal answer, not an HTTP error")
	assert.JSONEq(t, `{"allowed":false,"remaining":0,"retryAfterMs":1500}`, rec.Body.String())
}

func TestCheckValidation(t *testing.T) {
	cases := map[string]string{
		"not json":       `nope`,
		"missing key":    `{"limit":5,"windowSec":60}`,
		"zero limit":     `{"key":"k","limit":0,"windowSec":60}`,
		"negative limit": `{"key":"k","limit":-1,"windowSec":60}`,
		"zero window":    `{"key":"k","limit":5,"windowSec":0}`,
		"unknown field":  `{"key":"k","limit":5,"windowSec":60,"extra":1}`,
		"key too long":   `{"key":"` + strings.Repeat("a", 257) + `","limit":5,"windowSec":60}`,
	}
	h := newServer(&stubLimiter{}, nil)
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			rec := post(h, body)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Contains(t, rec.Body.String(), `"code":"INVALID_REQUEST"`)
		})
	}
}

func TestBackendErrorMapsTo503(t *testing.T) {
	rec := post(newServer(&stubLimiter{err: errors.New("redis down")}, nil), `{"key":"k","limit":5,"windowSec":60}`)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), `"code":"BACKEND_UNAVAILABLE"`)
	assert.NotContains(t, rec.Body.String(), "redis down", "internal errors must not leak")
}

func TestRequestIDIsEchoedOrGenerated(t *testing.T) {
	h := newServer(&stubLimiter{}, nil)
	rec := post(h, `{"key":"k","limit":5,"windowSec":60}`, "X-Request-Id", "abc-123")
	assert.Equal(t, "abc-123", rec.Header().Get("X-Request-Id"))

	rec = post(h, `{"key":"k","limit":5,"windowSec":60}`)
	assert.Len(t, rec.Header().Get("X-Request-Id"), 16)
}

func TestHealthAndReadiness(t *testing.T) {
	get := func(h http.Handler, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}
	assert.Equal(t, http.StatusOK, get(newServer(&stubLimiter{}, stubPinger{}), "/healthz").Code)
	assert.Equal(t, http.StatusOK, get(newServer(&stubLimiter{}, stubPinger{}), "/readyz").Code)
	assert.Equal(t, http.StatusServiceUnavailable,
		get(newServer(&stubLimiter{}, stubPinger{err: errors.New("down")}), "/readyz").Code)
}

func TestMetricsCountDecisions(t *testing.T) {
	stub := &stubLimiter{result: limiter.Result{Allowed: true}}
	h := newServer(stub, nil)
	post(h, `{"key":"k","limit":5,"windowSec":60}`)
	post(h, `{"key":"k","limit":5,"windowSec":60}`)
	stub.result = limiter.Result{Allowed: false, RetryAfterMs: 10}
	post(h, `{"key":"k","limit":5,"windowSec":60}`)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, body, `ratelimit_allowed_total{algorithm="stub"} 2`)
	assert.Contains(t, body, `ratelimit_rejected_total{algorithm="stub"} 1`)
	assert.Contains(t, body, `ratelimit_check_duration_seconds_count{algorithm="stub"} 3`)
}
