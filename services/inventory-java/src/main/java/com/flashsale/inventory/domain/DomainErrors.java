package com.flashsale.inventory.domain;

import org.springframework.http.HttpStatus;

/**
 * Business errors with the stable error code of the API contract
 * ({"code": "...", "message": "..."}). Both language implementations use the same codes.
 */
public class DomainErrors {

	private DomainErrors() {
	}

	public abstract static class DomainException extends RuntimeException {

		private final HttpStatus status;

		private final String code;

		DomainException(HttpStatus status, String code, String message) {
			super(message);
			this.status = status;
			this.code = code;
		}

		public HttpStatus status() {
			return status;
		}

		public String code() {
			return code;
		}
	}

	public static final class InvalidRequest extends DomainException {

		public InvalidRequest(String message) {
			super(HttpStatus.BAD_REQUEST, "INVALID_REQUEST", message);
		}
	}

	public static final class SkuNotFound extends DomainException {

		public SkuNotFound(String sku) {
			super(HttpStatus.NOT_FOUND, "SKU_NOT_FOUND", "unknown sku " + sku);
		}
	}

	public static final class ReservationNotFound extends DomainException {

		public ReservationNotFound(String orderId, String sku) {
			super(HttpStatus.NOT_FOUND, "RESERVATION_NOT_FOUND", "no reservation for order %s sku %s".formatted(orderId, sku));
		}
	}

	public static final class OutOfStock extends DomainException {

		public OutOfStock(String sku) {
			super(HttpStatus.CONFLICT, "OUT_OF_STOCK", "not enough stock for " + sku);
		}
	}

	/** Same (orderId, sku) reused with a different quantity. */
	public static final class IdempotencyConflict extends DomainException {

		public IdempotencyConflict(String message) {
			super(HttpStatus.CONFLICT, "IDEMPOTENCY_CONFLICT", message);
		}
	}

	/** The reservation is in a state that does not allow the requested transition. */
	public static final class InvalidState extends DomainException {

		public InvalidState(String message) {
			super(HttpStatus.CONFLICT, "INVALID_STATE", message);
		}
	}

	/** Optimistic locking kept losing; the caller may retry. */
	public static final class Contention extends DomainException {

		public Contention(String sku) {
			super(HttpStatus.SERVICE_UNAVAILABLE, "CONTENTION", "too much contention on " + sku + ", retry");
		}
	}
}
