// Package store is the Repository layer: all SQL lives here, every statement uses bind variables.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/sijms/go-ora/v2/network"

	"github.com/nhancaon/flashsale/services/inventory-go/internal/apperr"
)

const (
	StatusReserved  = "RESERVED"
	StatusReleased  = "RELEASED"
	StatusConfirmed = "CONFIRMED"
)

type StockSnapshot struct {
	SKU       string
	Available int64
	Reserved  int64
}

type StockRow struct {
	Available int64
	Version   int64
}

type Reservation struct {
	ID        string
	ProductID int64
	Qty       int64
	Status    string
}

// ErrNotFound is returned by lookups that find no row.
var ErrNotFound = errors.New("not found")

// ErrDuplicate is Oracle ORA-00001 (unique constraint violated).
var ErrDuplicate = errors.New("duplicate key")

// Oracle error numbers we translate.
const (
	oraUniqueViolation = 1
	oraDeadlock        = 60
	oraResourceBusy    = 54    // NOWAIT / resource busy
	oraLockTimeout     = 30006 // resource busy; WAIT timeout expired
	oraCheckViolated   = 2290
)

// Tx is the set of operations available inside one Oracle transaction.
type Tx interface {
	FindReservation(ctx context.Context, orderID string, productID int64) (*Reservation, error)
	InsertReservation(ctx context.Context, id, orderID string, productID, qty int64) error
	TransitionFromReserved(ctx context.Context, reservationID, newStatus string) (bool, error)
	Restock(ctx context.Context, productID, qty int64) error
	MarkSold(ctx context.Context, productID, qty int64) error
	StockOps
}

// StockOps are the building blocks the stock strategies combine.
type StockOps interface {
	DecrementIfAvailable(ctx context.Context, productID, qty int64) (bool, error)
	LockStock(ctx context.Context, productID int64) (StockRow, error)
	ReadStock(ctx context.Context, productID int64) (StockRow, error)
	Decrement(ctx context.Context, productID, qty int64) error
	DecrementIfVersion(ctx context.Context, productID, qty, version int64) (bool, error)
}

// Store opens transactions and serves non-transactional reads.
type Store interface {
	FindProductID(ctx context.Context, sku string) (int64, error)
	FindStockBySKU(ctx context.Context, sku string) (StockSnapshot, error)
	FindReservation(ctx context.Context, orderID string, productID int64) (*Reservation, error)
	InTx(ctx context.Context, fn func(Tx) error) error
}

type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Oracle is the Store backed by an Oracle database/sql pool.
type Oracle struct{ db *sql.DB }

func NewOracle(db *sql.DB) *Oracle { return &Oracle{db: db} }

func (o *Oracle) FindProductID(ctx context.Context, sku string) (int64, error) {
	return findProductID(ctx, o.db, sku)
}

func (o *Oracle) FindStockBySKU(ctx context.Context, sku string) (StockSnapshot, error) {
	var s StockSnapshot
	err := o.db.QueryRowContext(ctx, `
		SELECT p.sku, s.available, s.reserved
		FROM product p JOIN stock s ON s.product_id = p.id
		WHERE p.sku = :1`, sku).Scan(&s.SKU, &s.Available, &s.Reserved)
	return s, mapErr(err)
}

func (o *Oracle) FindReservation(ctx context.Context, orderID string, productID int64) (*Reservation, error) {
	return findReservation(ctx, o.db, orderID, productID)
}

// InTx runs fn in a READ COMMITTED transaction (Oracle's default): commit on nil, rollback on error.
func (o *Oracle) InTx(ctx context.Context, fn func(Tx) error) error {
	tx, err := o.db.BeginTx(ctx, nil)
	if err != nil {
		return mapErr(err)
	}
	if err := fn(&oracleTx{q: tx}); err != nil {
		_ = tx.Rollback()
		return err
	}
	return mapErr(tx.Commit())
}

type oracleTx struct{ q querier }

func findProductID(ctx context.Context, q querier, sku string) (int64, error) {
	var id int64
	err := q.QueryRowContext(ctx, "SELECT id FROM product WHERE sku = :1", sku).Scan(&id)
	return id, mapErr(err)
}

func findReservation(ctx context.Context, q querier, orderID string, productID int64) (*Reservation, error) {
	var r Reservation
	err := q.QueryRowContext(ctx,
		"SELECT id, product_id, qty, status FROM reservation WHERE order_id = :1 AND product_id = :2",
		orderID, productID).Scan(&r.ID, &r.ProductID, &r.Qty, &r.Status)
	if err != nil {
		return nil, mapErr(err)
	}
	return &r, nil
}

func (t *oracleTx) FindReservation(ctx context.Context, orderID string, productID int64) (*Reservation, error) {
	return findReservation(ctx, t.q, orderID, productID)
}

func (t *oracleTx) InsertReservation(ctx context.Context, id, orderID string, productID, qty int64) error {
	_, err := t.q.ExecContext(ctx,
		"INSERT INTO reservation (id, order_id, product_id, qty, status) VALUES (:1, :2, :3, :4, 'RESERVED')",
		id, orderID, productID, qty)
	return mapErr(err)
}

func (t *oracleTx) TransitionFromReserved(ctx context.Context, reservationID, newStatus string) (bool, error) {
	return t.exec1(ctx, "UPDATE reservation SET status = :1 WHERE id = :2 AND status = 'RESERVED'", newStatus, reservationID)
}

func (t *oracleTx) Restock(ctx context.Context, productID, qty int64) error {
	_, err := t.q.ExecContext(ctx,
		"UPDATE stock SET available = available + :1, reserved = reserved - :2, version = version + 1 WHERE product_id = :3",
		qty, qty, productID)
	return mapErr(err)
}

func (t *oracleTx) MarkSold(ctx context.Context, productID, qty int64) error {
	_, err := t.q.ExecContext(ctx,
		"UPDATE stock SET reserved = reserved - :1, version = version + 1 WHERE product_id = :2", qty, productID)
	return mapErr(err)
}

func (t *oracleTx) DecrementIfAvailable(ctx context.Context, productID, qty int64) (bool, error) {
	return t.exec1(ctx, `UPDATE stock SET available = available - :1, reserved = reserved + :2, version = version + 1
		WHERE product_id = :3 AND available >= :4`, qty, qty, productID, qty)
}

func (t *oracleTx) LockStock(ctx context.Context, productID int64) (StockRow, error) {
	var r StockRow
	err := t.q.QueryRowContext(ctx, "SELECT available, version FROM stock WHERE product_id = :1 FOR UPDATE", productID).
		Scan(&r.Available, &r.Version)
	return r, mapErr(err)
}

func (t *oracleTx) ReadStock(ctx context.Context, productID int64) (StockRow, error) {
	var r StockRow
	err := t.q.QueryRowContext(ctx, "SELECT available, version FROM stock WHERE product_id = :1", productID).
		Scan(&r.Available, &r.Version)
	return r, mapErr(err)
}

func (t *oracleTx) Decrement(ctx context.Context, productID, qty int64) error {
	_, err := t.q.ExecContext(ctx,
		"UPDATE stock SET available = available - :1, reserved = reserved + :2, version = version + 1 WHERE product_id = :3",
		qty, qty, productID)
	return mapErr(err)
}

func (t *oracleTx) DecrementIfVersion(ctx context.Context, productID, qty, version int64) (bool, error) {
	return t.exec1(ctx, `UPDATE stock SET available = available - :1, reserved = reserved + :2, version = version + 1
		WHERE product_id = :3 AND version = :4 AND available >= :5`, qty, qty, productID, version, qty)
}

// exec1 runs a statement and reports whether exactly one row changed.
func (t *oracleTx) exec1(ctx context.Context, query string, args ...any) (bool, error) {
	res, err := t.q.ExecContext(ctx, query, args...)
	if err != nil {
		return false, mapErr(err)
	}
	n, err := res.RowsAffected()
	return n == 1, mapErr(err)
}

// mapErr translates driver errors: ORA-00001 becomes ErrDuplicate, no rows becomes ErrNotFound,
// ORA-00060 and lock timeouts become retryable conflicts, timeouts become DB_UNAVAILABLE.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return apperr.DBUnavailable()
	}
	var oe *network.OracleError
	if errors.As(err, &oe) {
		switch oe.ErrCode {
		case oraUniqueViolation:
			return ErrDuplicate
		case oraDeadlock, oraResourceBusy, oraLockTimeout:
			return apperr.ConflictRetry()
		case oraCheckViolated:
			return fmt.Errorf("check constraint violated (stock would go negative): %w", err)
		}
	}
	return err
}
