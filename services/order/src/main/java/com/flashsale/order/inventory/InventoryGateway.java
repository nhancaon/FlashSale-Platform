package com.flashsale.order.inventory;

/**
 * Port to the inventory service. reserve, release and confirm are all idempotent on the inventory side,
 * so every call here can be retried safely.
 */
public interface InventoryGateway {

	void reserve(String orderId, String sku, long qty);

	void release(String orderId, String sku);

	void confirm(String orderId, String sku);

	/** The inventory answered 409 OUT_OF_STOCK. A business outcome: never retried, does not trip the breaker. */
	class OutOfStockException extends RuntimeException {

		public OutOfStockException(String sku) {
			super("out of stock: " + sku);
		}
	}

	/** The inventory rejected the request (other 4xx). Not retried, does not trip the breaker. */
	class RejectedException extends RuntimeException {

		private final int status;
		private final String code;

		public RejectedException(int status, String code) {
			super("inventory rejected the request: " + status + " " + code);
			this.status = status;
			this.code = code;
		}

		public int status() {
			return status;
		}

		public String code() {
			return code;
		}
	}

	/** Timeouts, connection errors, 5xx and 409 CONFLICT_RETRY: worth retrying and counted by the breaker. */
	class TransientException extends RuntimeException {

		public TransientException(String message, Throwable cause) {
			super(message, cause);
		}
	}

	/** The call was not made or gave up: breaker open, bulkhead full or retries exhausted. */
	class UnavailableException extends RuntimeException {

		public UnavailableException(String message, Throwable cause) {
			super(message, cause);
		}
	}
}
