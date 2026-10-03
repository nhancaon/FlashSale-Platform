package service_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nhancaon/flashsale/services/inventory-go/internal/apperr"
	"github.com/nhancaon/flashsale/services/inventory-go/internal/cache"
	"github.com/nhancaon/flashsale/services/inventory-go/internal/service"
	"github.com/nhancaon/flashsale/services/inventory-go/internal/storetest"
	"github.com/nhancaon/flashsale/services/inventory-go/internal/strategy"
)

type spyCache struct {
	mu          sync.Mutex
	entries     map[string]cache.Stock
	invalidated []string
}

func newSpyCache() *spyCache { return &spyCache{entries: map[string]cache.Stock{}} }

func (c *spyCache) Get(_ context.Context, sku string) (cache.Stock, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.entries[sku]
	return s, ok
}
func (c *spyCache) Put(_ context.Context, s cache.Stock) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[s.SKU] = s
}
func (c *spyCache) Invalidate(_ context.Context, sku string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, sku)
	c.invalidated = append(c.invalidated, sku)
}

func setup(t *testing.T, stock int64) (*service.Service, *storetest.Fake, *spyCache, int64) {
	t.Helper()
	fake := storetest.New()
	id := fake.AddProduct("SKU", stock)
	st, err := strategy.New(strategy.Atomic, 3)
	require.NoError(t, err)
	c := newSpyCache()
	return service.New(fake, st, c), fake, c, id
}

func code(t *testing.T, err error) string {
	t.Helper()
	e, ok := apperr.As(err)
	require.True(t, ok, "expected an API error, got %v", err)
	return e.Code
}

var bg = context.Background()

func TestReserveMovesStockAndIsIdempotent(t *testing.T) {
	svc, fake, _, id := setup(t, 10)
	first, err := svc.Reserve(bg, "o1", "SKU", 3)
	require.NoError(t, err)
	again, err := svc.Reserve(bg, "o1", "SKU", 3)
	require.NoError(t, err)
	assert.Equal(t, first, again)
	avail, rsv := fake.Stock(id)
	assert.EqualValues(t, 7, avail)
	assert.EqualValues(t, 3, rsv)

	_, err = svc.Reserve(bg, "o1", "SKU", 4)
	assert.Equal(t, "IDEMPOTENCY_CONFLICT", code(t, err))
}

func TestReserveOutOfStockRollsBackTheReservationRow(t *testing.T) {
	svc, fake, _, id := setup(t, 2)
	_, err := svc.Reserve(bg, "o1", "SKU", 3)
	assert.Equal(t, "OUT_OF_STOCK", code(t, err))
	_, found := fake.Reservation("o1", id)
	assert.False(t, found, "no trace of a failed reserve")
	avail, rsv := fake.Stock(id)
	assert.EqualValues(t, 2, avail)
	assert.EqualValues(t, 0, rsv)
}

func TestReserveUnknownSku(t *testing.T) {
	svc, _, _, _ := setup(t, 1)
	_, err := svc.Reserve(bg, "o1", "NOPE", 1)
	assert.Equal(t, "SKU_NOT_FOUND", code(t, err))
}

func TestReserveLosingTheUniqueRaceReturnsTheWinnersReservation(t *testing.T) {
	svc, fake, _, id := setup(t, 10)
	fake.DuplicateOnce = true
	got, err := svc.Reserve(bg, "o1", "SKU", 2)
	require.NoError(t, err)
	winner, found := fake.Reservation("o1", id)
	require.True(t, found)
	assert.Equal(t, winner.ID, got, "the replay path must return the id that is actually stored")
}

func TestReserveInvalidatesCacheOnlyAfterCommit(t *testing.T) {
	svc, _, c, _ := setup(t, 1)
	_, err := svc.GetStock(bg, "SKU") // fills the cache
	require.NoError(t, err)
	require.Contains(t, c.entries, "SKU")

	_, err = svc.Reserve(bg, "o1", "SKU", 5) // fails: out of stock, rolled back
	require.Error(t, err)
	assert.Empty(t, c.invalidated, "a rolled back transaction changed nothing, so the cache stays")

	_, err = svc.Reserve(bg, "o2", "SKU", 1)
	require.NoError(t, err)
	assert.Equal(t, []string{"SKU"}, c.invalidated)
	assert.NotContains(t, c.entries, "SKU")
}

func TestGetStockUsesCacheThenOracle(t *testing.T) {
	svc, _, c, _ := setup(t, 4)
	v, err := svc.GetStock(bg, "SKU")
	require.NoError(t, err)
	assert.EqualValues(t, 4, v.Available)

	c.entries["SKU"] = cache.Stock{SKU: "SKU", Available: 99, Reserved: 1}
	v, err = svc.GetStock(bg, "SKU")
	require.NoError(t, err)
	assert.EqualValues(t, 99, v.Available, "served from the cache")

	_, err = svc.GetStock(bg, "NOPE")
	assert.Equal(t, "SKU_NOT_FOUND", code(t, err))
}

func TestReleaseLifecycle(t *testing.T) {
	svc, fake, _, id := setup(t, 5)
	_, err := svc.Reserve(bg, "o1", "SKU", 4)
	require.NoError(t, err)

	for i := 0; i < 2; i++ {
		status, err := svc.Release(bg, "o1", "SKU")
		require.NoError(t, err)
		assert.Equal(t, "RELEASED", status)
	}
	avail, rsv := fake.Stock(id)
	assert.EqualValues(t, 5, avail, "restocked exactly once")
	assert.EqualValues(t, 0, rsv)

	status, err := svc.Release(bg, "never", "SKU")
	require.NoError(t, err)
	assert.Equal(t, "NOT_FOUND", status)

	_, err = svc.Reserve(bg, "o1", "SKU", 4)
	assert.Equal(t, "INVALID_STATE", code(t, err), "released reservations cannot be revived")
}

func TestConfirmLifecycle(t *testing.T) {
	svc, fake, _, id := setup(t, 5)
	_, err := svc.Reserve(bg, "o1", "SKU", 2)
	require.NoError(t, err)
	for i := 0; i < 2; i++ {
		status, err := svc.Confirm(bg, "o1", "SKU")
		require.NoError(t, err)
		assert.Equal(t, "CONFIRMED", status)
	}
	avail, rsv := fake.Stock(id)
	assert.EqualValues(t, 3, avail)
	assert.EqualValues(t, 0, rsv, "sold units leave the reserved bucket exactly once")

	_, err = svc.Release(bg, "o1", "SKU")
	assert.Equal(t, "INVALID_STATE", code(t, err))
	_, err = svc.Confirm(bg, "ghost", "SKU")
	assert.Equal(t, "RESERVATION_NOT_FOUND", code(t, err))

	_, err = svc.Reserve(bg, "o2", "SKU", 1)
	require.NoError(t, err)
	_, err = svc.Release(bg, "o2", "SKU")
	require.NoError(t, err)
	_, err = svc.Confirm(bg, "o2", "SKU")
	assert.Equal(t, "INVALID_STATE", code(t, err))
}

func TestStoreErrorsPropagateAndSkipCacheInvalidation(t *testing.T) {
	svc, fake, c, _ := setup(t, 5)
	fake.FailNextTx = apperr.DBUnavailable()
	_, err := svc.Reserve(bg, "o1", "SKU", 1)
	assert.Equal(t, "DB_UNAVAILABLE", code(t, err))
	assert.Empty(t, c.invalidated)
}

// Many buyers, small stock: the in-memory store serializes transactions, so this checks that the
// service logic itself never sells more than the stock (the Oracle behaviour is in the contract tests).
func TestConcurrentBuyersNeverOversell(t *testing.T) {
	svc, fake, _, id := setup(t, 25)
	var ok atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 400; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := svc.Reserve(bg, fmt.Sprintf("order-%d", i), "SKU", 1); err == nil {
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	avail, rsv := fake.Stock(id)
	assert.EqualValues(t, 25, ok.Load())
	assert.EqualValues(t, 0, avail)
	assert.EqualValues(t, 25, rsv)
}
