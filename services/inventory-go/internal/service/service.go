// Package service holds the use cases. Each public method is one short Oracle transaction; the cache
// entry is deleted only after the transaction committed.
package service

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/nhancaon/flashsale/services/inventory-go/internal/apperr"
	"github.com/nhancaon/flashsale/services/inventory-go/internal/cache"
	"github.com/nhancaon/flashsale/services/inventory-go/internal/store"
	"github.com/nhancaon/flashsale/services/inventory-go/internal/strategy"
)

type Service struct {
	store    store.Store
	strategy strategy.Strategy
	cache    cache.Cache
}

func New(s store.Store, st strategy.Strategy, c cache.Cache) *Service {
	return &Service{store: s, strategy: st, cache: c}
}

type StockView struct {
	SKU       string
	Available int64
	Reserved  int64
}

func (s *Service) GetStock(ctx context.Context, sku string) (StockView, error) {
	if c, ok := s.cache.Get(ctx, sku); ok {
		return StockView(c), nil
	}
	snap, err := s.store.FindStockBySKU(ctx, sku)
	if errors.Is(err, store.ErrNotFound) {
		return StockView{}, apperr.SkuNotFound(sku)
	}
	if err != nil {
		return StockView{}, err
	}
	v := StockView(snap)
	s.cache.Put(ctx, cache.Stock(v))
	return v, nil
}

func (s *Service) productID(ctx context.Context, sku string) (int64, error) {
	id, err := s.store.FindProductID(ctx, sku)
	if errors.Is(err, store.ErrNotFound) {
		return 0, apperr.SkuNotFound(sku)
	}
	return id, err
}

// Reserve is idempotent per (orderID, sku): a replay returns the original reservation id.
func (s *Service) Reserve(ctx context.Context, orderID, sku string, qty int64) (string, error) {
	productID, err := s.productID(ctx, sku)
	if err != nil {
		return "", err
	}
	existing, err := s.store.FindReservation(ctx, orderID, productID)
	switch {
	case err == nil:
		return replay(existing, qty)
	case !errors.Is(err, store.ErrNotFound):
		return "", err
	}

	reservationID := uuid.NewString()
	err = s.store.InTx(ctx, func(tx store.Tx) error {
		if err := tx.InsertReservation(ctx, reservationID, orderID, productID, qty); err != nil {
			return err
		}
		ok, err := s.strategy.TryReserve(ctx, tx, productID, qty, sku)
		if err != nil {
			return err
		}
		if !ok {
			// Rolls back the reservation row too, so a failed attempt leaves no trace.
			return apperr.OutOfStock(sku)
		}
		return nil
	})
	if errors.Is(err, store.ErrDuplicate) {
		// Another request with the same (orderID, sku) won the unique constraint between our check and insert.
		existing, ferr := s.store.FindReservation(ctx, orderID, productID)
		if ferr != nil {
			return "", ferr
		}
		return replay(existing, qty)
	}
	if err != nil {
		return "", err
	}
	s.cache.Invalidate(ctx, sku)
	return reservationID, nil
}

func replay(existing *store.Reservation, qty int64) (string, error) {
	if existing.Qty != qty {
		return "", apperr.IdempotencyConflict(
			"reservation already exists with qty " + itoa(existing.Qty) + ", request has qty " + itoa(qty))
	}
	if existing.Status == store.StatusReleased {
		return "", apperr.InvalidState("reservation " + existing.ID + " was already released")
	}
	return existing.ID, nil
}

// Release is idempotent: releasing an unknown or already released reservation is a successful no-op.
func (s *Service) Release(ctx context.Context, orderID, sku string) (string, error) {
	productID, err := s.productID(ctx, sku)
	if err != nil {
		return "", err
	}
	result := store.StatusReleased
	err = s.store.InTx(ctx, func(tx store.Tx) error {
		res, err := tx.FindReservation(ctx, orderID, productID)
		if errors.Is(err, store.ErrNotFound) {
			result = "NOT_FOUND"
			return nil
		}
		if err != nil {
			return err
		}
		if res.Status == store.StatusConfirmed {
			return apperr.InvalidState("reservation " + res.ID + " is already confirmed")
		}
		moved, err := tx.TransitionFromReserved(ctx, res.ID, store.StatusReleased)
		if err != nil {
			return err
		}
		if moved {
			return tx.Restock(ctx, productID, res.Qty)
		}
		// Lost a race against a concurrent confirm?
		now, err := tx.FindReservation(ctx, orderID, productID)
		if err != nil {
			return err
		}
		if now.Status == store.StatusConfirmed {
			return apperr.InvalidState("reservation " + res.ID + " is already confirmed")
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	s.cache.Invalidate(ctx, sku)
	return result, nil
}

// Confirm is idempotent: confirming twice is fine; confirming a released reservation is not.
func (s *Service) Confirm(ctx context.Context, orderID, sku string) (string, error) {
	productID, err := s.productID(ctx, sku)
	if err != nil {
		return "", err
	}
	err = s.store.InTx(ctx, func(tx store.Tx) error {
		res, err := tx.FindReservation(ctx, orderID, productID)
		if errors.Is(err, store.ErrNotFound) {
			return apperr.ReservationNotFound(orderID, sku)
		}
		if err != nil {
			return err
		}
		if res.Status == store.StatusReleased {
			return apperr.InvalidState("reservation " + res.ID + " was released")
		}
		moved, err := tx.TransitionFromReserved(ctx, res.ID, store.StatusConfirmed)
		if err != nil {
			return err
		}
		if moved {
			return tx.MarkSold(ctx, productID, res.Qty)
		}
		// Lost a race against a concurrent release?
		now, err := tx.FindReservation(ctx, orderID, productID)
		if err != nil {
			return err
		}
		if now.Status == store.StatusReleased {
			return apperr.InvalidState("reservation " + res.ID + " was released")
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	s.cache.Invalidate(ctx, sku)
	return store.StatusConfirmed, nil
}
