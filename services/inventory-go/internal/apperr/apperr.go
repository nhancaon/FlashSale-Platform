// Package apperr holds the business errors of the inventory API. Each carries the HTTP status and the
// stable error code of the contract; the Java service uses the very same codes.
package apperr

import (
	"errors"
	"fmt"
	"net/http"
)

type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func InvalidRequest(msg string) *Error {
	return &Error{http.StatusBadRequest, "INVALID_REQUEST", msg}
}
func SkuNotFound(sku string) *Error {
	return &Error{http.StatusNotFound, "SKU_NOT_FOUND", "unknown sku " + sku}
}
func ReservationNotFound(orderID, sku string) *Error {
	return &Error{http.StatusNotFound, "RESERVATION_NOT_FOUND", fmt.Sprintf("no reservation for order %s sku %s", orderID, sku)}
}
func OutOfStock(sku string) *Error {
	return &Error{http.StatusConflict, "OUT_OF_STOCK", "not enough stock for " + sku}
}

// IdempotencyConflict: the same (orderId, sku) was reused with a different quantity.
func IdempotencyConflict(msg string) *Error {
	return &Error{http.StatusConflict, "IDEMPOTENCY_CONFLICT", msg}
}

// InvalidState: the reservation's state does not allow the requested transition.
func InvalidState(msg string) *Error {
	return &Error{http.StatusConflict, "INVALID_STATE", msg}
}

// Contention: optimistic locking kept losing; the caller may retry.
func Contention(sku string) *Error {
	return &Error{http.StatusServiceUnavailable, "CONTENTION", "too much contention on " + sku + ", retry"}
}

// ConflictRetry: ORA-00060 deadlock or a lock wait timeout; retrying is safe.
func ConflictRetry() *Error {
	return &Error{http.StatusConflict, "CONFLICT_RETRY", "concurrent update, retry the request"}
}

// DBUnavailable: pool exhausted, query timeout or Oracle unreachable.
func DBUnavailable() *Error {
	return &Error{http.StatusServiceUnavailable, "DB_UNAVAILABLE", "database unavailable or timed out"}
}

func Internal() *Error {
	return &Error{http.StatusInternalServerError, "INTERNAL", "unexpected error"}
}

// As reports whether err is (or wraps) an *Error.
func As(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}
