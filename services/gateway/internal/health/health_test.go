package health_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/nhancaon/flashsale/services/gateway/internal/health"
)

func ready(a *health.Aggregator) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	a.ReadyHandler().ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
	return rec
}

func TestAllHealthy(t *testing.T) {
	a := &health.Aggregator{Timeout: time.Second, Checks: map[string]health.Check{
		"order": func(context.Context) error { return nil }, "redis": func(context.Context) error { return nil }}}
	rec := ready(a)
	assert.Equal(t, 200, rec.Code)
	assert.JSONEq(t, `{"status":"ok","components":{"order":"ok","redis":"ok"}}`, rec.Body.String())
}

func TestOneDependencyDownDegradesTheGateway(t *testing.T) {
	a := &health.Aggregator{Timeout: time.Second, Checks: map[string]health.Check{
		"order": func(context.Context) error { return errors.New("refused") }, "redis": func(context.Context) error { return nil }}}
	rec := ready(a)
	assert.Equal(t, 503, rec.Code)
	assert.JSONEq(t, `{"status":"degraded","components":{"order":"down","redis":"ok"}}`, rec.Body.String())
}

func TestSlowCheckIsCutOffByTheTimeout(t *testing.T) {
	a := &health.Aggregator{Timeout: 100 * time.Millisecond, Checks: map[string]health.Check{
		"slow": func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }}}
	start := time.Now()
	rec := ready(a)
	assert.Equal(t, 503, rec.Code)
	assert.Less(t, time.Since(start), time.Second)
}

func TestChecksRunConcurrently(t *testing.T) {
	slow := func(context.Context) error { time.Sleep(150 * time.Millisecond); return nil }
	a := &health.Aggregator{Timeout: time.Second, Checks: map[string]health.Check{"a": slow, "b": slow, "c": slow, "d": slow}}
	start := time.Now()
	assert.Equal(t, 200, ready(a).Code)
	assert.Less(t, time.Since(start), 400*time.Millisecond, "4 x 150 ms checks must overlap")
}

func TestHTTPCheck(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer good.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer bad.Close()

	c := &http.Client{Timeout: time.Second}
	assert.NoError(t, health.HTTPCheck(c, good.URL)(context.Background()))
	assert.Error(t, health.HTTPCheck(c, bad.URL)(context.Background()))
	assert.Error(t, health.HTTPCheck(c, "http://127.0.0.1:1")(context.Background()))
}
