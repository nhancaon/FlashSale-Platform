package strategy_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nhancaon/flashsale/services/inventory-go/internal/apperr"
	"github.com/nhancaon/flashsale/services/inventory-go/internal/store"
	"github.com/nhancaon/flashsale/services/inventory-go/internal/strategy"
)

// recorder counts calls and lets a test script the answers.
type recorder struct {
	row            store.StockRow
	calls          []string
	versionResults []bool // answers for successive DecrementIfVersion calls
}

func (r *recorder) DecrementIfAvailable(context.Context, int64, int64) (bool, error) {
	r.calls = append(r.calls, "DecrementIfAvailable")
	return r.row.Available >= 1, nil
}
func (r *recorder) LockStock(context.Context, int64) (store.StockRow, error) {
	r.calls = append(r.calls, "LockStock")
	return r.row, nil
}
func (r *recorder) ReadStock(context.Context, int64) (store.StockRow, error) {
	r.calls = append(r.calls, "ReadStock")
	return r.row, nil
}
func (r *recorder) Decrement(context.Context, int64, int64) error {
	r.calls = append(r.calls, "Decrement")
	return nil
}
func (r *recorder) DecrementIfVersion(context.Context, int64, int64, int64) (bool, error) {
	r.calls = append(r.calls, "DecrementIfVersion")
	if len(r.versionResults) == 0 {
		return false, nil
	}
	v := r.versionResults[0]
	r.versionResults = r.versionResults[1:]
	return v, nil
}

func TestFactory(t *testing.T) {
	for _, name := range []string{strategy.Atomic, strategy.Pessimistic, strategy.Optimistic} {
		s, err := strategy.New(name, 3)
		require.NoError(t, err)
		assert.Equal(t, name, s.Name())
	}
	_, err := strategy.New("magic", 3)
	assert.ErrorContains(t, err, "magic")
}

func TestAtomicUsesASingleConditionalUpdate(t *testing.T) {
	s, _ := strategy.New(strategy.Atomic, 3)
	r := &recorder{row: store.StockRow{Available: 5}}
	ok, err := s.TryReserve(context.Background(), r, 1, 1, "sku")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, []string{"DecrementIfAvailable"}, r.calls)

	r = &recorder{row: store.StockRow{Available: 0}}
	ok, err = s.TryReserve(context.Background(), r, 1, 1, "sku")
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestPessimisticLocksBeforeUpdating(t *testing.T) {
	s, _ := strategy.New(strategy.Pessimistic, 3)
	r := &recorder{row: store.StockRow{Available: 5}}
	ok, err := s.TryReserve(context.Background(), r, 1, 2, "sku")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, []string{"LockStock", "Decrement"}, r.calls, "lock first, then update")

	r = &recorder{row: store.StockRow{Available: 1}}
	ok, err = s.TryReserve(context.Background(), r, 1, 2, "sku")
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Equal(t, []string{"LockStock"}, r.calls, "no update when stock is insufficient")
}

func TestOptimisticRetriesThenSucceeds(t *testing.T) {
	s, _ := strategy.New(strategy.Optimistic, 5)
	r := &recorder{row: store.StockRow{Available: 5, Version: 7}, versionResults: []bool{false, false, true}}
	ok, err := s.TryReserve(context.Background(), r, 1, 1, "sku")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, 3, count(r.calls, "DecrementIfVersion"), "two lost races, then a win")
	assert.Equal(t, 3, count(r.calls, "ReadStock"), "each attempt re-reads the row")
}

func TestOptimisticGivesUpWithContention(t *testing.T) {
	s, _ := strategy.New(strategy.Optimistic, 2)
	r := &recorder{row: store.StockRow{Available: 5}} // never wins
	ok, err := s.TryReserve(context.Background(), r, 1, 1, "sku-x")
	assert.False(t, ok)
	e, isAPI := apperr.As(err)
	require.True(t, isAPI)
	assert.Equal(t, "CONTENTION", e.Code)
	assert.Equal(t, 3, count(r.calls, "DecrementIfVersion"), "initial attempt + 2 retries")
}

func TestOptimisticReportsOutOfStockWithoutUpdating(t *testing.T) {
	s, _ := strategy.New(strategy.Optimistic, 2)
	r := &recorder{row: store.StockRow{Available: 0}}
	ok, err := s.TryReserve(context.Background(), r, 1, 1, "sku")
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Equal(t, 0, count(r.calls, "DecrementIfVersion"))
}

func TestOptimisticStopsWhenContextIsCancelled(t *testing.T) {
	s, _ := strategy.New(strategy.Optimistic, 1000)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := &recorder{row: store.StockRow{Available: 5}}
	_, err := s.TryReserve(ctx, r, 1, 1, "sku")
	assert.ErrorIs(t, err, context.Canceled)
}

func count(calls []string, name string) int {
	n := 0
	for _, c := range calls {
		if c == name {
			n++
		}
	}
	return n
}
