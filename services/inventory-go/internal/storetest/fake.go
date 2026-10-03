// Package storetest is an in-memory store.Store for unit tests (no Oracle needed). Transactions are
// serialized by a global lock and rolled back by restoring a snapshot, which is enough to test the
// service logic; real concurrency behaviour is covered by the contract tests against Oracle.
package storetest

import (
	"context"
	"sync"

	"github.com/nhancaon/flashsale/services/inventory-go/internal/store"
)

type resKey struct {
	order   string
	product int64
}

type Fake struct {
	mu       sync.Mutex
	products map[string]int64
	stock    map[int64]*store.StockRow
	reserved map[int64]int64
	res      map[resKey]store.Reservation

	// DuplicateOnce makes the next InsertReservation fail with ErrDuplicate after storing the row,
	// simulating a concurrent request that won the unique constraint.
	DuplicateOnce bool
	// FailNextTx makes the next InTx return this error before running the callback.
	FailNextTx error

	winner *winnerRow // committed by "the other request"; survives our rollback
}

type winnerRow struct {
	key resKey
	res store.Reservation
}

func New() *Fake {
	return &Fake{products: map[string]int64{}, stock: map[int64]*store.StockRow{}, reserved: map[int64]int64{}, res: map[resKey]store.Reservation{}}
}

// AddProduct registers a sku with initial available stock and returns its product id.
func (f *Fake) AddProduct(sku string, available int64) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := int64(len(f.products) + 1)
	f.products[sku] = id
	f.stock[id] = &store.StockRow{Available: available}
	return id
}

func (f *Fake) Stock(productID int64) (available, reserved int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stock[productID].Available, f.reserved[productID]
}

func (f *Fake) Reservation(orderID string, productID int64) (store.Reservation, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.res[resKey{orderID, productID}]
	return r, ok
}

func (f *Fake) FindProductID(_ context.Context, sku string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id, ok := f.products[sku]; ok {
		return id, nil
	}
	return 0, store.ErrNotFound
}

func (f *Fake) FindStockBySKU(_ context.Context, sku string) (store.StockSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.products[sku]
	if !ok {
		return store.StockSnapshot{}, store.ErrNotFound
	}
	return store.StockSnapshot{SKU: sku, Available: f.stock[id].Available, Reserved: f.reserved[id]}, nil
}

func (f *Fake) FindReservation(_ context.Context, orderID string, productID int64) (*store.Reservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.find(orderID, productID)
}

func (f *Fake) find(orderID string, productID int64) (*store.Reservation, error) {
	r, ok := f.res[resKey{orderID, productID}]
	if !ok {
		return nil, store.ErrNotFound
	}
	return &r, nil
}

func (f *Fake) InTx(_ context.Context, fn func(store.Tx) error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.FailNextTx != nil {
		err := f.FailNextTx
		f.FailNextTx = nil
		return err
	}
	snapStock := map[int64]store.StockRow{}
	for k, v := range f.stock {
		snapStock[k] = *v
	}
	snapReserved := map[int64]int64{}
	for k, v := range f.reserved {
		snapReserved[k] = v
	}
	snapRes := map[resKey]store.Reservation{}
	for k, v := range f.res {
		snapRes[k] = v
	}
	if err := fn(&tx{f: f}); err != nil {
		for k, v := range snapStock {
			row := v
			f.stock[k] = &row
		}
		f.reserved, f.res = snapReserved, snapRes
		if f.winner != nil {
			f.res[f.winner.key] = f.winner.res
			f.winner = nil
		}
		return err
	}
	return nil
}

type tx struct{ f *Fake }

func (t *tx) FindReservation(_ context.Context, orderID string, productID int64) (*store.Reservation, error) {
	return t.f.find(orderID, productID)
}

func (t *tx) InsertReservation(_ context.Context, id, orderID string, productID, qty int64) error {
	k := resKey{orderID, productID}
	if _, exists := t.f.res[k]; exists {
		return store.ErrDuplicate
	}
	if t.f.DuplicateOnce {
		t.f.DuplicateOnce = false
		// The "other request" committed its own row for this key first; our insert violates the constraint.
		t.f.winner = &winnerRow{k, store.Reservation{ID: "winner-" + id, ProductID: productID, Qty: qty, Status: store.StatusReserved}}
		return store.ErrDuplicate
	}
	t.f.res[k] = store.Reservation{ID: id, ProductID: productID, Qty: qty, Status: store.StatusReserved}
	return nil
}

func (t *tx) TransitionFromReserved(_ context.Context, reservationID, newStatus string) (bool, error) {
	for k, r := range t.f.res {
		if r.ID == reservationID && r.Status == store.StatusReserved {
			r.Status = newStatus
			t.f.res[k] = r
			return true, nil
		}
	}
	return false, nil
}

func (t *tx) Restock(_ context.Context, productID, qty int64) error {
	t.f.stock[productID].Available += qty
	t.f.reserved[productID] -= qty
	return nil
}

func (t *tx) MarkSold(_ context.Context, productID, qty int64) error {
	t.f.reserved[productID] -= qty
	return nil
}

func (t *tx) DecrementIfAvailable(_ context.Context, productID, qty int64) (bool, error) {
	s := t.f.stock[productID]
	if s.Available < qty {
		return false, nil
	}
	s.Available -= qty
	t.f.reserved[productID] += qty
	s.Version++
	return true, nil
}

func (t *tx) LockStock(_ context.Context, productID int64) (store.StockRow, error) {
	return *t.f.stock[productID], nil
}

func (t *tx) ReadStock(_ context.Context, productID int64) (store.StockRow, error) {
	return *t.f.stock[productID], nil
}

func (t *tx) Decrement(_ context.Context, productID, qty int64) error {
	s := t.f.stock[productID]
	s.Available -= qty
	t.f.reserved[productID] += qty
	s.Version++
	return nil
}

func (t *tx) DecrementIfVersion(_ context.Context, productID, qty, version int64) (bool, error) {
	s := t.f.stock[productID]
	if s.Version != version || s.Available < qty {
		return false, nil
	}
	s.Available -= qty
	t.f.reserved[productID] += qty
	s.Version++
	return true, nil
}
