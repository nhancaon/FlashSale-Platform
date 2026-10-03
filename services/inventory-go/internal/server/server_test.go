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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nhancaon/flashsale/services/inventory-go/internal/apperr"
	"github.com/nhancaon/flashsale/services/inventory-go/internal/cache"
	"github.com/nhancaon/flashsale/services/inventory-go/internal/server"
	"github.com/nhancaon/flashsale/services/inventory-go/internal/service"
	"github.com/nhancaon/flashsale/services/inventory-go/internal/storetest"
	"github.com/nhancaon/flashsale/services/inventory-go/internal/strategy"
)

type pinger struct{ err error }

func (p pinger) Ping(context.Context) error { return p.err }

func newHandler(t *testing.T, ready server.Pinger) (http.Handler, *storetest.Fake) {
	t.Helper()
	fake := storetest.New()
	fake.AddProduct("SKU", 5)
	st, err := strategy.New(strategy.Atomic, 3)
	require.NoError(t, err)
	srv := server.New(server.Config{
		Service: service.New(fake, st, cache.Noop{}),
		Ready:   ready,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return srv.Handler(), fake
}

func do(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestReserveAndGet(t *testing.T) {
	h, _ := newHandler(t, nil)
	rec := do(h, "POST", "/v1/inventory/reserve", `{"orderId":"o1","sku":"SKU","qty":2}`)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"reservationId"`)

	rec = do(h, "GET", "/v1/inventory/SKU", "")
	assert.Equal(t, 200, rec.Code)
	assert.JSONEq(t, `{"sku":"SKU","available":3,"reserved":2}`, rec.Body.String())
}

func TestErrorMapping(t *testing.T) {
	h, _ := newHandler(t, nil)
	cases := []struct {
		name, method, path, body string
		status                   int
		code                     string
	}{
		{"unknown sku", "GET", "/v1/inventory/NOPE", "", 404, "SKU_NOT_FOUND"},
		{"out of stock", "POST", "/v1/inventory/reserve", `{"orderId":"o","sku":"SKU","qty":99}`, 409, "OUT_OF_STOCK"},
		{"not json", "POST", "/v1/inventory/reserve", `nope`, 400, "INVALID_REQUEST"},
		{"unknown field", "POST", "/v1/inventory/reserve", `{"orderId":"o","sku":"SKU","qty":1,"x":1}`, 400, "INVALID_REQUEST"},
		{"missing order", "POST", "/v1/inventory/reserve", `{"sku":"SKU","qty":1}`, 400, "INVALID_REQUEST"},
		{"missing qty", "POST", "/v1/inventory/reserve", `{"orderId":"o","sku":"SKU"}`, 400, "INVALID_REQUEST"},
		{"zero qty", "POST", "/v1/inventory/reserve", `{"orderId":"o","sku":"SKU","qty":0}`, 400, "INVALID_REQUEST"},
		{"qty too big", "POST", "/v1/inventory/reserve", `{"orderId":"o","sku":"SKU","qty":1001}`, 400, "INVALID_REQUEST"},
		{"long order id", "POST", "/v1/inventory/release", `{"orderId":"` + strings.Repeat("x", 65) + `","sku":"SKU"}`, 400, "INVALID_REQUEST"},
		{"confirm unknown", "POST", "/v1/inventory/confirm", `{"orderId":"ghost","sku":"SKU"}`, 404, "RESERVATION_NOT_FOUND"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(h, tc.method, tc.path, tc.body)
			assert.Equal(t, tc.status, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), `"code":"`+tc.code+`"`)
		})
	}
}

func TestInfrastructureErrorsAreMapped(t *testing.T) {
	h, fake := newHandler(t, nil)
	fake.FailNextTx = apperr.DBUnavailable()
	rec := do(h, "POST", "/v1/inventory/reserve", `{"orderId":"o","sku":"SKU","qty":1}`)
	assert.Equal(t, 503, rec.Code)
	assert.Contains(t, rec.Body.String(), "DB_UNAVAILABLE")

	fake.FailNextTx = apperr.ConflictRetry()
	rec = do(h, "POST", "/v1/inventory/reserve", `{"orderId":"o2","sku":"SKU","qty":1}`)
	assert.Equal(t, 409, rec.Code)
	assert.Contains(t, rec.Body.String(), "CONFLICT_RETRY")

	fake.FailNextTx = errors.New("ORA-12345: secret internals")
	rec = do(h, "POST", "/v1/inventory/reserve", `{"orderId":"o3","sku":"SKU","qty":1}`)
	assert.Equal(t, 500, rec.Code)
	assert.NotContains(t, rec.Body.String(), "ORA-12345", "internal errors must not leak")
}

func TestHealthReadyMetrics(t *testing.T) {
	h, _ := newHandler(t, pinger{})
	assert.Equal(t, 200, do(h, "GET", "/healthz", "").Code)
	assert.Equal(t, 200, do(h, "GET", "/readyz", "").Code)

	do(h, "POST", "/v1/inventory/reserve", `{"orderId":"o1","sku":"SKU","qty":1}`)
	do(h, "POST", "/v1/inventory/reserve", `{"orderId":"o2","sku":"SKU","qty":99}`)
	metrics := do(h, "GET", "/metrics", "").Body.String()
	assert.Contains(t, metrics, `inventory_reserve_total{result="ok"} 1`)
	assert.Contains(t, metrics, `inventory_reserve_total{result="out_of_stock"} 1`)
	assert.Contains(t, metrics, `http_requests_total{method="POST",route="/v1/inventory/reserve",status="409"} 1`)

	down, _ := newHandler(t, pinger{err: errors.New("down")})
	assert.Equal(t, 503, do(down, "GET", "/readyz", "").Code)
}

func TestRequestIDEchoedOrGenerated(t *testing.T) {
	h, _ := newHandler(t, nil)
	req := httptest.NewRequest("GET", "/v1/inventory/SKU", nil)
	req.Header.Set("X-Request-Id", "abc-123")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, "abc-123", rec.Header().Get("X-Request-Id"))
	assert.Len(t, do(h, "GET", "/v1/inventory/SKU", "").Header().Get("X-Request-Id"), 16)
}
