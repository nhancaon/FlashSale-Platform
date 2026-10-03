package middleware_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nhancaon/flashsale/services/gateway/internal/middleware"
	"github.com/nhancaon/flashsale/services/ratelimiter-go/pkg/limiter"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

var ok = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })

func serve(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// ---- request id ----

func TestRequestIDKeepsAWellFormedIdAndReplacesAnythingElse(t *testing.T) {
	var seen string
	h := middleware.RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = middleware.InfoFrom(r.Context()).RequestID }))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Request-Id", "trace-123.abc")
	rec := serve(h, req)
	assert.Equal(t, "trace-123.abc", rec.Header().Get("X-Request-Id"))
	assert.Equal(t, "trace-123.abc", seen)

	for _, bad := range []string{"", "has space", "new\nline", "<script>", string(make([]byte, 100))} {
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("X-Request-Id", bad)
		rec := serve(h, req)
		assert.Len(t, rec.Header().Get("X-Request-Id"), 16, "bad id %q replaced", bad)
	}
}

// ---- authenticate ----

func verifier(token string) (string, error) {
	if token == "good" {
		return "alice", nil
	}
	return "", errors.New("bad token")
}

func TestAuthenticate(t *testing.T) {
	var user string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user = middleware.InfoFrom(r.Context()).User
		w.WriteHeader(http.StatusNoContent)
	})
	h := middleware.RequestID(middleware.Authenticate(verifier)(next))

	cases := map[string]struct {
		header string
		status int
	}{
		"missing":          {"", 401},
		"wrong scheme":     {"Basic abc", 401},
		"empty bearer":     {"Bearer ", 401},
		"lowercase scheme": {"bearer good", 401},
		"bad token":        {"Bearer nope", 401},
		"good token":       {"Bearer good", 204},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			user = ""
			req := httptest.NewRequest("GET", "/", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := serve(h, req)
			assert.Equal(t, tc.status, rec.Code)
			if tc.status == 401 {
				assert.Contains(t, rec.Header().Get("WWW-Authenticate"), "Bearer")
				assert.Contains(t, rec.Body.String(), `"code":"UNAUTHORIZED"`)
				assert.Empty(t, user, "the handler must not run")
			} else {
				assert.Equal(t, "alice", user)
			}
		})
	}
}

// ---- rate limit ----

type fakeLimiter struct {
	mu     sync.Mutex
	calls  []string
	result limiter.Result
	err    error
	deny   map[string]limiter.Result // per key
}

func (f *fakeLimiter) Name() string { return "fake" }
func (f *fakeLimiter) Allow(_ context.Context, key string, _ int64, _ time.Duration) (limiter.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, key)
	if f.err != nil {
		return limiter.Result{}, f.err
	}
	if r, ok := f.deny[key]; ok {
		return r, nil
	}
	return f.result, nil
}

func rateLimited(l limiter.Limiter, failOpen bool, scopes ...middleware.Scope) http.Handler {
	rl := middleware.NewRateLimiter(l, failOpen, time.Second, quiet, prometheus.NewRegistry())
	return middleware.RequestID(rl.Limit(scopes...)(ok))
}

func scope(name string, key func(*http.Request) string) middleware.Scope {
	return middleware.Scope{Name: name, Limit: 10, Window: time.Second, Key: key}
}

func TestRateLimitAllowsAndReportsHeaders(t *testing.T) {
	f := &fakeLimiter{result: limiter.Result{Allowed: true, Remaining: 7}}
	h := rateLimited(f, true, scope("ip", func(*http.Request) string { return "1.2.3.4" }))
	rec := serve(h, httptest.NewRequest("GET", "/", nil))
	assert.Equal(t, 204, rec.Code)
	assert.Equal(t, "10", rec.Header().Get("X-RateLimit-Limit"))
	assert.Equal(t, "7", rec.Header().Get("X-RateLimit-Remaining"))
	assert.Equal(t, []string{"ip:1.2.3.4"}, f.calls)
}

func TestRateLimitRejectsWith429AndRetryAfterRoundedUp(t *testing.T) {
	cases := map[int64]string{1: "1", 999: "1", 1000: "1", 1001: "2", 2500: "3", 0: "1"}
	for ms, want := range cases {
		f := &fakeLimiter{result: limiter.Result{Allowed: false, RetryAfterMs: ms}}
		rec := serve(rateLimited(f, true, scope("user", func(*http.Request) string { return "alice" })), httptest.NewRequest("GET", "/", nil))
		assert.Equal(t, 429, rec.Code)
		assert.Equal(t, want, rec.Header().Get("Retry-After"), "retryAfterMs=%d", ms)
		assert.Contains(t, rec.Body.String(), `"code":"RATE_LIMITED"`)
	}
}

func TestFirstRejectingScopeWinsAndLaterScopesAreNotCalled(t *testing.T) {
	f := &fakeLimiter{result: limiter.Result{Allowed: true}, deny: map[string]limiter.Result{"ip:9.9.9.9": {Allowed: false, RetryAfterMs: 500}}}
	h := rateLimited(f, true,
		scope("ip", func(*http.Request) string { return "9.9.9.9" }),
		scope("user", func(*http.Request) string { return "alice" }))
	rec := serve(h, httptest.NewRequest("GET", "/", nil))
	assert.Equal(t, 429, rec.Code)
	assert.Equal(t, []string{"ip:9.9.9.9"}, f.calls, "the user scope is not even consulted")
}

func TestScopeWithEmptyKeyIsSkipped(t *testing.T) {
	f := &fakeLimiter{result: limiter.Result{Allowed: true}}
	h := rateLimited(f, true, scope("user", func(*http.Request) string { return "" }))
	assert.Equal(t, 204, serve(h, httptest.NewRequest("GET", "/", nil)).Code)
	assert.Empty(t, f.calls)
}

func TestLimiterFailureFailOpenOrClosed(t *testing.T) {
	boom := &fakeLimiter{err: errors.New("redis down")}
	key := func(*http.Request) string { return "k" }

	open := serve(rateLimited(boom, true, scope("ip", key)), httptest.NewRequest("GET", "/", nil))
	assert.Equal(t, 204, open.Code, "fail open: the request goes through")

	closed := serve(rateLimited(boom, false, scope("ip", key)), httptest.NewRequest("GET", "/", nil))
	assert.Equal(t, 503, closed.Code, "fail closed: refuse")
	assert.Contains(t, closed.Body.String(), "RATE_LIMITER_UNAVAILABLE")
	assert.Equal(t, "1", closed.Header().Get("Retry-After"))
}

func TestClientIP(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.5:5555"
	r.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")

	assert.Equal(t, "10.0.0.5", middleware.ClientIP(r, false), "XFF is ignored unless the proxy is trusted")
	assert.Equal(t, "203.0.113.7", middleware.ClientIP(r, true))

	r.Header.Set("X-Forwarded-For", "not-an-ip")
	assert.Equal(t, "10.0.0.5", middleware.ClientIP(r, true), "garbage falls back to the socket address")
}

// ---- logging ----

func TestLoggingRecordsMetricsPerStatus(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := middleware.NewMetrics(reg)
	h := middleware.RequestID(middleware.Logging(quiet, m)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})))
	serve(h, httptest.NewRequest("GET", "/x", nil))

	families, err := reg.Gather()
	require.NoError(t, err)
	var found bool
	for _, f := range families {
		if f.GetName() == "http_requests_total" {
			found = true
			assert.Equal(t, "418", f.GetMetric()[0].GetLabel()[2].GetValue())
		}
	}
	assert.True(t, found)
}
