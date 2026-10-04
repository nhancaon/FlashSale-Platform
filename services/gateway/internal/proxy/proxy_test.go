package proxy_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nhancaon/flashsale/services/gateway/internal/middleware"
	"github.com/nhancaon/flashsale/services/gateway/internal/proxy"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type upstream struct {
	srv    *httptest.Server
	hits   atomic.Int64
	status atomic.Int64
	delay  atomic.Int64 // ms
	last   atomic.Pointer[http.Request]
}

func newUpstream(t *testing.T) *upstream {
	u := &upstream{}
	u.status.Store(200)
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hits.Add(1)
		u.last.Store(r.Clone(r.Context()))
		if d := u.delay.Load(); d > 0 {
			time.Sleep(time.Duration(d) * time.Millisecond)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(int(u.status.Load()))
		_, _ = io.WriteString(w, `{"from":"upstream"}`)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func handler(t *testing.T, u *upstream, mutate func(*proxy.Upstream)) http.Handler {
	t.Helper()
	target, err := url.Parse(u.srv.URL)
	require.NoError(t, err)
	cfg := proxy.Upstream{Name: "order", Target: target, StripPrefix: "/api/orders", AddPrefix: "/v1/orders",
		Methods: []string{"GET", "POST"}, Timeout: 2 * time.Second, BreakerOpen: 300 * time.Millisecond, BreakerTrips: 5}
	if mutate != nil {
		mutate(&cfg)
	}
	state := proxy.BreakerStateGauge(prometheus.NewRegistry())
	return middleware.RequestID(proxy.Handler(cfg, quiet, state))
}

func call(h http.Handler, method, path string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestRewritesThePathAndRelaysTheAnswer(t *testing.T) {
	u := newUpstream(t)
	h := handler(t, u, nil)

	for in, want := range map[string]string{
		"/api/orders":         "/v1/orders",
		"/api/orders/abc":     "/v1/orders/abc",
		"/api/orders/a/b?x=1": "/v1/orders/a/b",
	} {
		rec := call(h, "GET", in, nil)
		assert.Equal(t, 200, rec.Code)
		assert.JSONEq(t, `{"from":"upstream"}`, rec.Body.String())
		assert.Equal(t, want, u.last.Load().URL.Path, in)
	}
}

func TestUpstreamNeverSeesClientControlledIdentity(t *testing.T) {
	u := newUpstream(t)
	// An inner middleware in the real chain sets the verified user; simulate it.
	h := middleware.RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		middleware.InfoFrom(r.Context()).User = "alice"
		proxy.Handler(proxy.Upstream{Name: "order", Target: mustURL(u.srv.URL), StripPrefix: "/api/orders", AddPrefix: "/v1/orders",
			Methods: []string{"GET"}, Timeout: time.Second, BreakerOpen: time.Second}, quiet, nil).ServeHTTP(w, r)
	}))

	rec := call(h, "GET", "/api/orders/1", map[string]string{
		"X-User-Id":     "mallory",       // spoofing attempt
		"Authorization": "Bearer secret", // must not leak upstream
		"X-Request-Id":  "trace-1",
	})
	require.Equal(t, 200, rec.Code)
	got := u.last.Load()
	assert.Equal(t, "alice", got.Header.Get("X-User-Id"), "identity comes from the verified token only")
	assert.Empty(t, got.Header.Get("Authorization"))
	assert.Equal(t, "trace-1", got.Header.Get("X-Request-Id"), "the trace id follows the request")
	assert.NotEmpty(t, got.Header.Get("X-Forwarded-For"))
}

func mustURL(s string) *url.URL { u, _ := url.Parse(s); return u }

func TestResponseCarriesExactlyOneRequestID(t *testing.T) {
	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", r.Header.Get("X-Request-Id")) // an upstream that echoes the id, like ours
		w.WriteHeader(200)
	}))
	defer echo.Close()
	u := &upstream{srv: echo}
	rec := call(handler(t, u, nil), "GET", "/api/orders/1", map[string]string{"X-Request-Id": "trace-9"})
	assert.Equal(t, []string{"trace-9"}, rec.Header().Values("X-Request-Id"))
}

func TestOnlyConfiguredMethodsAreForwarded(t *testing.T) {
	u := newUpstream(t)
	h := handler(t, u, func(c *proxy.Upstream) { c.Methods = []string{"GET"} })

	rec := call(h, "POST", "/api/orders", nil)
	assert.Equal(t, 405, rec.Code)
	assert.Equal(t, "GET", rec.Header().Get("Allow"))
	assert.Contains(t, rec.Body.String(), "METHOD_NOT_ALLOWED")
	assert.Zero(t, u.hits.Load())
}

func TestBreakerOpensOnUpstreamFailuresAndFailsFast(t *testing.T) {
	u := newUpstream(t)
	u.status.Store(500)
	h := handler(t, u, nil)

	// The 5xx answer is relayed to the client while the breaker counts it.
	for i := 0; i < 5; i++ {
		rec := call(h, "GET", "/api/orders/1", nil)
		assert.Equal(t, 500, rec.Code)
		assert.JSONEq(t, `{"from":"upstream"}`, rec.Body.String())
	}
	hits := u.hits.Load()

	rec := call(h, "GET", "/api/orders/1", nil)
	assert.Equal(t, 503, rec.Code)
	assert.Contains(t, rec.Body.String(), "UPSTREAM_UNAVAILABLE")
	assert.NotEmpty(t, rec.Header().Get("Retry-After"))
	assert.Equal(t, hits, u.hits.Load(), "an open breaker does not call the upstream")
}

func TestBreakerRecoversThroughHalfOpen(t *testing.T) {
	u := newUpstream(t)
	u.status.Store(500)
	h := handler(t, u, nil)
	for i := 0; i < 6; i++ {
		call(h, "GET", "/api/orders/1", nil)
	}
	require.Equal(t, 503, call(h, "GET", "/api/orders/1", nil).Code)

	u.status.Store(200)
	time.Sleep(400 * time.Millisecond) // longer than BreakerOpen: the next calls are probes
	assert.Equal(t, 200, call(h, "GET", "/api/orders/1", nil).Code)
	for i := 0; i < 10; i++ {
		assert.Equal(t, 200, call(h, "GET", "/api/orders/1", nil).Code, "closed again")
	}
}

func TestBusinessErrorsDoNotTripTheBreaker(t *testing.T) {
	u := newUpstream(t)
	u.status.Store(409) // for example OUT_OF_STOCK: a healthy, expected answer
	h := handler(t, u, nil)
	for i := 0; i < 50; i++ {
		assert.Equal(t, 409, call(h, "GET", "/api/orders/1", nil).Code)
	}
	assert.EqualValues(t, 50, u.hits.Load(), "every request reached the upstream")
}

func TestUpstream503DoesNotTripTheBreaker(t *testing.T) {
	u := newUpstream(t)
	u.status.Store(503) // the order service shedding load, or its inventory being down
	h := handler(t, u, nil)
	for i := 0; i < 30; i++ {
		assert.Equal(t, 503, call(h, "GET", "/api/orders/1", nil).Code)
	}
	assert.EqualValues(t, 30, u.hits.Load(), "every request still reaches the upstream: no cascade")
}

func TestSlowUpstreamTimesOutWith504(t *testing.T) {
	u := newUpstream(t)
	u.delay.Store(500)
	h := handler(t, u, func(c *proxy.Upstream) { c.Timeout = 100 * time.Millisecond })

	start := time.Now()
	rec := call(h, "GET", "/api/orders/1", nil)
	assert.Equal(t, 504, rec.Code)
	assert.Contains(t, rec.Body.String(), "UPSTREAM_TIMEOUT")
	assert.Less(t, time.Since(start), 400*time.Millisecond, "the client is not kept waiting for the slow service")
}

func TestUnreachableUpstreamIs502ThenBreakerOpens(t *testing.T) {
	u := newUpstream(t)
	h := handler(t, u, nil)
	u.srv.Close() // connection refused from now on

	for i := 0; i < 5; i++ {
		assert.Equal(t, 502, call(h, "GET", "/api/orders/1", nil).Code)
	}
	assert.Equal(t, 503, call(h, "GET", "/api/orders/1", nil).Code, "after repeated failures the breaker answers instead")
}
