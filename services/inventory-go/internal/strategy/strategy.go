// Package strategy implements the three ways of decrementing stock safely under concurrency.
// A Strategy is always called inside a transaction opened by the service.
package strategy

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/nhancaon/flashsale/services/inventory-go/internal/apperr"
	"github.com/nhancaon/flashsale/services/inventory-go/internal/store"
)

const (
	Atomic      = "atomic"
	Pessimistic = "pessimistic"
	Optimistic  = "optimistic"
)

// Strategy moves qty units from available to reserved. It returns false when there is not enough stock.
type Strategy interface {
	Name() string
	TryReserve(ctx context.Context, ops store.StockOps, productID, qty int64, sku string) (bool, error)
}

// New is the factory that builds a Strategy from its configured name.
func New(name string, optimisticMaxRetries int) (Strategy, error) {
	switch name {
	case Atomic:
		return atomic{}, nil
	case Pessimistic:
		return pessimistic{}, nil
	case Optimistic:
		return optimistic{maxRetries: optimisticMaxRetries}, nil
	default:
		return nil, fmt.Errorf("unknown STOCK_STRATEGY %q (want %s, %s or %s)", name, Atomic, Pessimistic, Optimistic)
	}
}

// atomic: one conditional UPDATE checks and decrements; rows affected tells the outcome.
type atomic struct{}

func (atomic) Name() string { return Atomic }

func (atomic) TryReserve(ctx context.Context, ops store.StockOps, productID, qty int64, _ string) (bool, error) {
	return ops.DecrementIfAvailable(ctx, productID, qty)
}

// pessimistic: SELECT ... FOR UPDATE, check in the application, then UPDATE while holding the lock.
type pessimistic struct{}

func (pessimistic) Name() string { return Pessimistic }

func (pessimistic) TryReserve(ctx context.Context, ops store.StockOps, productID, qty int64, _ string) (bool, error) {
	row, err := ops.LockStock(ctx, productID)
	if err != nil {
		return false, err
	}
	if row.Available < qty {
		return false, nil
	}
	return true, ops.Decrement(ctx, productID, qty)
}

// optimistic: read the version, update only if unchanged, retry (bounded, with jitter) when it lost.
type optimistic struct{ maxRetries int }

func (optimistic) Name() string { return Optimistic }

func (o optimistic) TryReserve(ctx context.Context, ops store.StockOps, productID, qty int64, sku string) (bool, error) {
	for attempt := 0; attempt <= o.maxRetries; attempt++ {
		row, err := ops.ReadStock(ctx, productID)
		if err != nil {
			return false, err
		}
		if row.Available < qty {
			return false, nil
		}
		won, err := ops.DecrementIfVersion(ctx, productID, qty, row.Version)
		if err != nil {
			return false, err
		}
		if won {
			return true, nil
		}
		// Lost the race: back off a little so the winners can finish.
		backoff := time.Duration(100_000 + rand.Int64N(2_000_000*int64(min(attempt+1, 10)))) //nolint:gosec // jitter, not security
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	return false, apperr.Contention(sku)
}
