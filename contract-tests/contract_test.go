// Contract tests for the inventory API. The same suite runs against inventory-go and inventory-java:
//
//	BASE_URL=http://localhost:8083 STRATEGY=atomic DB_PASSWORD=... go test ./...
package contract_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nhancaon/flashsale/contract-tests/internal/fixture"
)

var env *fixture.Env

func TestMain(m *testing.M) {
	var err error
	env, err = fixture.Open()
	if err != nil {
		fmt.Fprintln(os.Stderr, "contract tests need a running service and Oracle:", err)
		os.Exit(2)
	}
	code := m.Run()
	_ = env.DB.Close()
	os.Exit(code)
}

func ctx() context.Context { return context.Background() }

func newSKU(t *testing.T, stock int) (string, int64) {
	t.Helper()
	sku, id, err := env.NewSKU(ctx(), stock)
	require.NoError(t, err)
	return sku, id
}

func orderID(t *testing.T, suffix string) string {
	return fmt.Sprintf("o-%d-%s", time.Now().UnixNano(), suffix)
}

func mustResp(t *testing.T) func(fixture.Resp, error) fixture.Resp {
	return func(r fixture.Resp, err error) fixture.Resp {
		t.Helper()
		require.NoError(t, err)
		return r
	}
}

func TestGetStock(t *testing.T) {
	c, ok := env.Client(), mustResp(t)
	sku, _ := newSKU(t, 10)

	r := ok(c.Get(sku))
	assert.Equal(t, 200, r.Status)
	assert.Equal(t, sku, r.Str("sku"))
	assert.EqualValues(t, 10, r.Num("available"))
	assert.EqualValues(t, 0, r.Num("reserved"))

	r = ok(c.Get("NO-SUCH-SKU"))
	assert.Equal(t, 404, r.Status)
	assert.Equal(t, "SKU_NOT_FOUND", r.Str("code"))
}

func TestReserveIsIdempotent(t *testing.T) {
	c, ok := env.Client(), mustResp(t)
	sku, id := newSKU(t, 10)
	order := orderID(t, "a")

	first := ok(c.Reserve(order, sku, 3))
	require.Equal(t, 200, first.Status, first.Raw)
	require.NotEmpty(t, first.Str("reservationId"))

	again := ok(c.Reserve(order, sku, 3))
	assert.Equal(t, 200, again.Status)
	assert.Equal(t, first.Str("reservationId"), again.Str("reservationId"), "replay returns the original reservation")

	db, err := env.DBStock(ctx(), id)
	require.NoError(t, err)
	assert.EqualValues(t, 7, db.Available, "stock is decremented once")
	assert.EqualValues(t, 3, db.Reserved)

	r := ok(c.Get(sku))
	assert.EqualValues(t, 7, r.Num("available"), "GET must reflect the change (cache invalidated)")
	assert.EqualValues(t, 3, r.Num("reserved"))

	diff := ok(c.Reserve(order, sku, 4))
	assert.Equal(t, 409, diff.Status)
	assert.Equal(t, "IDEMPOTENCY_CONFLICT", diff.Str("code"))
}

func TestConcurrentReplaysReserveOnce(t *testing.T) {
	c := env.Client()
	sku, id := newSKU(t, 10)
	order := orderID(t, "same")

	const n = 50
	ids := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := c.Reserve(order, sku, 2)
			if assert.NoError(t, err) && assert.Equal(t, 200, r.Status, r.Raw) {
				ids[i] = r.Str("reservationId")
			}
		}()
	}
	wg.Wait()
	for _, got := range ids {
		assert.Equal(t, ids[0], got, "every replay sees the same reservation")
	}
	db, err := env.DBStock(ctx(), id)
	require.NoError(t, err)
	assert.EqualValues(t, 8, db.Available, "50 identical requests must decrement exactly once")
	assert.EqualValues(t, 2, db.Reserved)
}

func TestOutOfStockLeavesNoTrace(t *testing.T) {
	c, ok := env.Client(), mustResp(t)
	sku, id := newSKU(t, 2)
	order := orderID(t, "big")

	r := ok(c.Reserve(order, sku, 3))
	assert.Equal(t, 409, r.Status)
	assert.Equal(t, "OUT_OF_STOCK", r.Str("code"))

	db, err := env.DBStock(ctx(), id)
	require.NoError(t, err)
	assert.EqualValues(t, 2, db.Available)
	assert.EqualValues(t, 0, db.Reserved)
	n, _, err := env.DBReservations(ctx(), id, "RESERVED")
	require.NoError(t, err)
	assert.EqualValues(t, 0, n, "a failed reserve must not leave a reservation row")

	// Exact fit works.
	assert.Equal(t, 200, ok(c.Reserve(order, sku, 2)).Status)
}

func TestReleaseIsIdempotentAndRestoresStock(t *testing.T) {
	c, ok := env.Client(), mustResp(t)
	sku, id := newSKU(t, 5)
	order := orderID(t, "rel")
	require.Equal(t, 200, ok(c.Reserve(order, sku, 4)).Status)

	for i := 0; i < 2; i++ {
		r := ok(c.Release(order, sku))
		assert.Equal(t, 200, r.Status, r.Raw)
		assert.Equal(t, "RELEASED", r.Str("status"))
	}
	db, err := env.DBStock(ctx(), id)
	require.NoError(t, err)
	assert.EqualValues(t, 5, db.Available, "released once, restored once")
	assert.EqualValues(t, 0, db.Reserved)

	unknown := ok(c.Release(orderID(t, "never"), sku))
	assert.Equal(t, 200, unknown.Status, "compensation for a reserve that never happened must succeed")
	assert.Equal(t, "NOT_FOUND", unknown.Str("status"))

	replay := ok(c.Reserve(order, sku, 4))
	assert.Equal(t, 409, replay.Status, "a released reservation cannot be reserved again with the same key")
	assert.Equal(t, "INVALID_STATE", replay.Str("code"))
}

func TestConfirmLifecycle(t *testing.T) {
	c, ok := env.Client(), mustResp(t)
	sku, id := newSKU(t, 5)
	order := orderID(t, "conf")
	require.Equal(t, 200, ok(c.Reserve(order, sku, 2)).Status)

	for i := 0; i < 2; i++ {
		r := ok(c.Confirm(order, sku))
		assert.Equal(t, 200, r.Status, r.Raw)
		assert.Equal(t, "CONFIRMED", r.Str("status"))
	}
	db, err := env.DBStock(ctx(), id)
	require.NoError(t, err)
	assert.EqualValues(t, 3, db.Available)
	assert.EqualValues(t, 0, db.Reserved, "confirmed units are sold, no longer reserved")
	_, sold, err := env.DBReservations(ctx(), id, "CONFIRMED")
	require.NoError(t, err)
	assert.EqualValues(t, 2, sold)

	r := ok(c.Release(order, sku))
	assert.Equal(t, 409, r.Status, "cannot release a confirmed reservation")
	assert.Equal(t, "INVALID_STATE", r.Str("code"))

	r = ok(c.Confirm(orderID(t, "ghost"), sku))
	assert.Equal(t, 404, r.Status)
	assert.Equal(t, "RESERVATION_NOT_FOUND", r.Str("code"))

	other := orderID(t, "conf2")
	require.Equal(t, 200, ok(c.Reserve(other, sku, 1)).Status)
	require.Equal(t, 200, ok(c.Release(other, sku)).Status)
	r = ok(c.Confirm(other, sku))
	assert.Equal(t, 409, r.Status, "cannot confirm a released reservation")
	assert.Equal(t, "INVALID_STATE", r.Str("code"))
}

func TestValidation(t *testing.T) {
	c, ok := env.Client(), mustResp(t)
	sku, _ := newSKU(t, 5)
	cases := map[string]string{
		"not json":      `nope`,
		"missing order": fmt.Sprintf(`{"sku":%q,"qty":1}`, sku),
		"missing sku":   `{"orderId":"o1","qty":1}`,
		"zero qty":      fmt.Sprintf(`{"orderId":"o1","sku":%q,"qty":0}`, sku),
		"negative qty":  fmt.Sprintf(`{"orderId":"o1","sku":%q,"qty":-2}`, sku),
		"missing qty":   fmt.Sprintf(`{"orderId":"o1","sku":%q}`, sku),
		"unknown field": fmt.Sprintf(`{"orderId":"o1","sku":%q,"qty":1,"extra":true}`, sku),
		"huge order id": fmt.Sprintf(`{"orderId":%q,"sku":%q,"qty":1}`, strings.Repeat("x", 65), sku),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			r := ok(c.Raw("POST", "/v1/inventory/reserve", body))
			assert.Equal(t, 400, r.Status, r.Raw)
			assert.Equal(t, "INVALID_REQUEST", r.Str("code"))
		})
	}
	r := ok(c.Reserve("o1", "NO-SUCH-SKU", 1))
	assert.Equal(t, 404, r.Status)
	assert.Equal(t, "SKU_NOT_FOUND", r.Str("code"))
}

func TestHealthEndpoints(t *testing.T) {
	c, ok := env.Client(), mustResp(t)
	assert.Equal(t, 200, ok(c.Raw("GET", "/healthz", "")).Status)
	assert.Equal(t, 200, ok(c.Raw("GET", "/readyz", "")).Status)
	m := ok(c.Raw("GET", "/metrics", ""))
	assert.Equal(t, 200, m.Status)
	assert.Contains(t, m.Raw, "inventory_reserve_total")
}

// The headline guarantee: 100 units, 2000 simultaneous buyers, exactly 100 succeed and nothing goes negative.
func TestNoOversell(t *testing.T) {
	const (
		stock   = 100
		buyers  = 2000
		timeout = 120 * time.Second
	)
	c := env.Client()
	sku, id := newSKU(t, stock)

	var ok, oos, contention, other atomic.Int64
	var mu sync.Mutex
	var unexpected []string
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < buyers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r, err := c.Reserve(fmt.Sprintf("oversell-%s-%d", sku, i), sku, 1)
			switch {
			case err != nil:
				other.Add(1)
				mu.Lock()
				unexpected = append(unexpected, err.Error())
				mu.Unlock()
			case r.Status == 200:
				ok.Add(1)
			case r.Status == 409 && r.Str("code") == "OUT_OF_STOCK":
				oos.Add(1)
			case r.Status == 503 && r.Str("code") == "CONTENTION":
				contention.Add(1)
			default:
				other.Add(1)
				mu.Lock()
				unexpected = append(unexpected, fmt.Sprintf("%d %s", r.Status, r.Raw))
				mu.Unlock()
			}
		}()
	}
	t0 := time.Now()
	close(start)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatal("buyers did not finish in time")
	}
	t.Logf("strategy=%s: ok=%d out_of_stock=%d contention=%d other=%d in %s",
		env.Strategy, ok.Load(), oos.Load(), contention.Load(), other.Load(), time.Since(t0).Round(time.Millisecond))

	if len(unexpected) > 0 {
		t.Errorf("unexpected responses (first 3 of %d): %v", len(unexpected), unexpected[:min(3, len(unexpected))])
	}
	assert.EqualValues(t, buyers, ok.Load()+oos.Load()+contention.Load()+other.Load())

	// Truth is in Oracle.
	db, err := env.DBStock(ctx(), id)
	require.NoError(t, err)
	reserved, qty, err := env.DBReservations(ctx(), id, "RESERVED")
	require.NoError(t, err)
	assert.GreaterOrEqual(t, db.Available, int64(0), "stock must never go negative")
	assert.LessOrEqual(t, ok.Load(), int64(stock), "never sell more than the stock")
	assert.EqualValues(t, ok.Load(), reserved, "one reservation row per successful reply")
	assert.EqualValues(t, ok.Load(), qty)
	assert.EqualValues(t, stock, db.Available+db.Reserved, "available + reserved must equal the initial stock")

	if env.Strategy == "optimistic" {
		// Optimistic locking may give up under heavy contention (503 CONTENTION); that is allowed, overselling is not.
		t.Logf("optimistic: %d request(s) gave up with CONTENTION", contention.Load())
		return
	}
	assert.EqualValues(t, stock, ok.Load(), "exactly the stock must be sold")
	assert.EqualValues(t, buyers-stock, oos.Load())
	assert.EqualValues(t, 0, db.Available)
	assert.EqualValues(t, 0, contention.Load())
}
