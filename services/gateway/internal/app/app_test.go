package app_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nhancaon/flashsale/services/gateway/internal/app"
	"github.com/nhancaon/flashsale/services/gateway/internal/auth"
	"github.com/nhancaon/flashsale/services/gateway/internal/health"
	"github.com/nhancaon/flashsale/services/gateway/internal/proxy"
	"github.com/nhancaon/flashsale/services/ratelimiter-go/pkg/limiter"
)

const (
	secret   = "0123456789abcdef0123456789abcdef"
	password = "demo-password"
)

// memLimiter is a fixed counter per key: the first `limit` calls pass, the rest are rejected. No windows, no Redis.
type memLimiter struct {
	mu     sync.Mutex
	counts map[string]int64
}

func (m *memLimiter) Name() string { return "mem" }
func (m *memLimiter) Allow(_ context.Context, key string, limit int64, _ time.Duration) (limiter.Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.counts == nil {
		m.counts = map[string]int64{}
	}
	m.counts[key]++
	if m.counts[key] > limit {
		return limiter.Result{Allowed: false, RetryAfterMs: 1200}, nil
	}
	return limiter.Result{Allowed: true, Remaining: limit - m.counts[key]}, nil
}

type fake struct {
	srv    *httptest.Server
	hits   atomic.Int64
	status atomic.Int64
	last   atomic.Pointer[http.Request]
}

func newFake(t *testing.T) *fake {
	f := &fake{}
	f.status.Store(200)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		f.last.Store(r.Clone(r.Context()))
		if r.URL.Path == "/readyz" {
			w.WriteHeader(int(f.status.Load()))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(int(f.status.Load()))
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

type rig struct {
	h         http.Handler
	order     *fake
	inventory *fake
}

func build(t *testing.T, l limiter.Limiter, mutate func(*app.Config)) rig {
	t.Helper()
	order, inventory := newFake(t), newFake(t)
	issuer, err := auth.NewIssuer(secret, 15*time.Minute)
	require.NoError(t, err)
	client := &http.Client{Timeout: time.Second}
	cfg := app.Config{
		Issuer: issuer, Credentials: auth.NewCredentials(password),
		Limiter: l, FailOpen: true, LimiterTimeout: time.Second,
		IPLimit: 1000, IPWindow: time.Second, UserLimit: 1000, UserWindow: time.Second,
		Order: proxy.Upstream{Name: "order", Target: must(order.srv.URL), StripPrefix: "/api/orders", AddPrefix: "/v1/orders",
			Methods: []string{"GET", "POST"}, Timeout: 2 * time.Second, BreakerOpen: 300 * time.Millisecond, BreakerTrips: 5},
		Inventory: proxy.Upstream{Name: "inventory", Target: must(inventory.srv.URL), StripPrefix: "/api/inventory", AddPrefix: "/v1/inventory",
			Methods: []string{"GET"}, Timeout: 2 * time.Second, BreakerOpen: 300 * time.Millisecond, BreakerTrips: 5},
		Checks: map[string]health.Check{
			"order":     health.HTTPCheck(client, order.srv.URL+"/readyz"),
			"inventory": health.HTTPCheck(client, inventory.srv.URL+"/readyz"),
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	return rig{h: app.New(cfg), order: order, inventory: inventory}
}

func must(s string) *url.URL { u, _ := url.Parse(s); return u }

func (r rig) do(method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = "198.51.100.10:4000"
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)
	return rec
}

func (r rig) login(t *testing.T, user string) string {
	t.Helper()
	rec := r.do("POST", "/auth/login", `{"username":"`+user+`","password":"`+password+`"}`, nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var out struct {
		AccessToken string `json:"accessToken"`
		TokenType   string `json:"tokenType"`
		ExpiresIn   int    `json:"expiresIn"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Equal(t, "Bearer", out.TokenType)
	assert.InDelta(t, 900, out.ExpiresIn, 5)
	return out.AccessToken
}

func bearer(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

func TestLoginThenCallOrderThroughTheGateway(t *testing.T) {
	r := build(t, &memLimiter{}, nil)
	token := r.login(t, "alice")

	rec := r.do("POST", "/api/orders", `{"items":[]}`, map[string]string{
		"Authorization": "Bearer " + token, "Idempotency-Key": "k-1", "X-User-Id": "mallory"})
	assert.Equal(t, 200, rec.Code)
	got := r.order.last.Load()
	assert.Equal(t, "/v1/orders", got.URL.Path)
	assert.Equal(t, "alice", got.Header.Get("X-User-Id"), "the verified identity, not the spoofed header")
	assert.Equal(t, "k-1", got.Header.Get("Idempotency-Key"), "client headers the upstream needs are forwarded")
	assert.Empty(t, got.Header.Get("Authorization"))
	assert.Len(t, rec.Header().Get("X-Request-Id"), 16)
}

func TestLoginFailures(t *testing.T) {
	r := build(t, &memLimiter{}, nil)
	for name, body := range map[string]string{
		"wrong password": `{"username":"alice","password":"nope"}`,
		"bad username":   `{"username":"a b","password":"` + password + `"}`,
		"empty":          `{}`,
	} {
		rec := r.do("POST", "/auth/login", body, nil)
		assert.Equal(t, 401, rec.Code, name)
		assert.Contains(t, rec.Body.String(), `"code":"INVALID_CREDENTIALS"`)
	}
	for name, body := range map[string]string{"not json": `nope`, "unknown field": `{"username":"a","password":"b","x":1}`} {
		assert.Equal(t, 400, r.do("POST", "/auth/login", body, nil).Code, name)
	}
}

func TestApiRequiresAValidToken(t *testing.T) {
	r := build(t, &memLimiter{}, nil)
	assert.Equal(t, 401, r.do("GET", "/api/orders/1", "", nil).Code)
	assert.Equal(t, 401, r.do("GET", "/api/orders/1", "", bearer("garbage")).Code)
	assert.Equal(t, 401, r.do("GET", "/api/inventory/SKU-1", "", nil).Code)
	assert.Zero(t, r.order.hits.Load()+r.inventory.hits.Load(), "nothing reaches the services without a token")
}

func TestInventoryIsReadOnlyThroughTheGateway(t *testing.T) {
	r := build(t, &memLimiter{}, nil)
	token := r.login(t, "alice")

	assert.Equal(t, 200, r.do("GET", "/api/inventory/SKU-1", "", bearer(token)).Code)
	assert.Equal(t, "/v1/inventory/SKU-1", r.inventory.last.Load().URL.Path)

	for _, path := range []string{"/api/inventory/reserve", "/api/inventory/release", "/api/inventory/confirm"} {
		assert.Equal(t, 405, r.do("POST", path, `{}`, bearer(token)).Code, path+": stock changes are internal")
	}
	assert.Equal(t, 404, r.do("GET", "/internal/anything", "", bearer(token)).Code)
}

func TestIPRateLimitProtectsLoginFromBruteForce(t *testing.T) {
	r := build(t, &memLimiter{}, func(c *app.Config) { c.IPLimit = 3 })
	bad := `{"username":"alice","password":"wrong"}`
	for i := 0; i < 3; i++ {
		assert.Equal(t, 401, r.do("POST", "/auth/login", bad, nil).Code)
	}
	rec := r.do("POST", "/auth/login", bad, nil)
	assert.Equal(t, 429, rec.Code, "the 4th attempt from the same IP is throttled")
	assert.Equal(t, "2", rec.Header().Get("Retry-After"))
	assert.Contains(t, rec.Body.String(), "ip limit")
}

func TestUserRateLimitIsPerUser(t *testing.T) {
	r := build(t, &memLimiter{}, func(c *app.Config) { c.UserLimit = 2 })
	alice, bob := r.login(t, "alice"), r.login(t, "bob")

	assert.Equal(t, 200, r.do("GET", "/api/orders/1", "", bearer(alice)).Code)
	assert.Equal(t, 200, r.do("GET", "/api/orders/1", "", bearer(alice)).Code)
	rec := r.do("GET", "/api/orders/1", "", bearer(alice))
	assert.Equal(t, 429, rec.Code)
	assert.Contains(t, rec.Body.String(), "user limit")
	assert.Equal(t, "2", rec.Header().Get("Retry-After"))

	assert.Equal(t, 200, r.do("GET", "/api/orders/1", "", bearer(bob)).Code, "bob is not affected by alice's burst")
}

func TestRejectedByTheLimiterNeverReachesTheService(t *testing.T) {
	r := build(t, &memLimiter{}, func(c *app.Config) { c.UserLimit = 1 })
	token := r.login(t, "alice")
	r.do("GET", "/api/orders/1", "", bearer(token))
	before := r.order.hits.Load()
	for i := 0; i < 10; i++ {
		assert.Equal(t, 429, r.do("GET", "/api/orders/1", "", bearer(token)).Code)
	}
	assert.Equal(t, before, r.order.hits.Load())
}

func TestUpstreamOutageOpensTheBreakerAndTheGatewayStaysUp(t *testing.T) {
	r := build(t, &memLimiter{}, nil)
	token := r.login(t, "alice")
	r.order.status.Store(500)

	for i := 0; i < 5; i++ {
		assert.Equal(t, 500, r.do("GET", "/api/orders/1", "", bearer(token)).Code)
	}
	rec := r.do("GET", "/api/orders/1", "", bearer(token))
	assert.Equal(t, 503, rec.Code)
	assert.Contains(t, rec.Body.String(), "UPSTREAM_UNAVAILABLE")

	// The inventory has its own breaker: it keeps working.
	assert.Equal(t, 200, r.do("GET", "/api/inventory/SKU-1", "", bearer(token)).Code)
	assert.Equal(t, 200, r.do("GET", "/healthz", "", nil).Code, "the gateway itself is alive")

	r.order.status.Store(200)
	time.Sleep(400 * time.Millisecond)
	assert.Equal(t, 200, r.do("GET", "/api/orders/1", "", bearer(token)).Code, "recovers by itself")
}

func TestReadinessAggregatesDependencies(t *testing.T) {
	r := build(t, &memLimiter{}, nil)
	rec := r.do("GET", "/readyz", "", nil)
	assert.Equal(t, 200, rec.Code)
	assert.JSONEq(t, `{"status":"ok","components":{"order":"ok","inventory":"ok"}}`, rec.Body.String())

	r.inventory.status.Store(503)
	rec = r.do("GET", "/readyz", "", nil)
	assert.Equal(t, 503, rec.Code)
	assert.JSONEq(t, `{"status":"degraded","components":{"order":"ok","inventory":"down"}}`, rec.Body.String())
}

func TestMetricsEndpoint(t *testing.T) {
	r := build(t, &memLimiter{}, func(c *app.Config) { c.UserLimit = 1 })
	token := r.login(t, "alice")
	r.do("GET", "/api/orders/1", "", bearer(token))
	r.do("GET", "/api/orders/1", "", bearer(token)) // rejected

	body := r.do("GET", "/metrics", "", nil).Body.String()
	assert.Contains(t, body, `gateway_ratelimit_rejected_total{scope="user"} 1`)
	assert.Contains(t, body, `gateway_upstream_breaker_state{upstream="order"} 0`)
	// A request rejected by a middleware never reaches the inner route, so it is counted under the group pattern.
	assert.Contains(t, body, `http_requests_total{method="GET",route="/api/*",status="429"} 1`)
	assert.Contains(t, body, `http_requests_total{method="GET",route="/api/orders/*",status="200"} 1`)
}

func TestFailClosedWhenTheLimiterIsDown(t *testing.T) {
	down := brokenLimiter{}
	open := build(t, down, nil) // fail-open (default in build)
	token := open.login(t, "alice")
	assert.Equal(t, 200, open.do("GET", "/api/orders/1", "", bearer(token)).Code)

	closed := build(t, down, func(c *app.Config) { c.FailOpen = false })
	assert.Equal(t, 503, closed.do("POST", "/auth/login", `{"username":"a","password":"b"}`, nil).Code)
}

type brokenLimiter struct{}

func (brokenLimiter) Name() string { return "broken" }
func (brokenLimiter) Allow(context.Context, string, int64, time.Duration) (limiter.Result, error) {
	return limiter.Result{}, context.DeadlineExceeded
}

// With the real Redis-backed library the limits hold across requests (needs `make up`; skipped otherwise).
func TestEmbeddedLimiterOnRealRedis(t *testing.T) {
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("TEST_REDIS_ADDR not set: run `make up` and use make test-go")
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	defer rdb.Close()
	lim, err := limiter.New(limiter.SlidingWindow, rdb, limiter.SystemClock())
	require.NoError(t, err)
	r := build(t, lim, func(c *app.Config) { c.UserLimit = 5; c.UserWindow = 10 * time.Second })

	user := "redis-user-" + time.Now().Format("150405.000000")
	token := r.login(t, user)
	var ok, limited int
	for i := 0; i < 9; i++ {
		switch r.do("GET", "/api/orders/1", "", bearer(token)).Code {
		case 200:
			ok++
		case 429:
			limited++
		}
	}
	assert.Equal(t, 5, ok, "exactly the configured limit passes")
	assert.Equal(t, 4, limited)
}
