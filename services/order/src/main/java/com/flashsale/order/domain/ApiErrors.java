package com.flashsale.order.domain;

import org.springframework.http.HttpStatus;

/** Business errors with the stable codes of the order API: {"code": "...", "message": "..."}. */
public final class ApiErrors {

	private ApiErrors() {
	}

	public static class ApiException extends RuntimeException {

		private final HttpStatus status;
		private final String code;
		private final String orderId;

		ApiException(HttpStatus status, String code, String message, String orderId) {
			super(message);
			this.status = status;
			this.code = code;
			this.orderId = orderId;
		}

		public HttpStatus status() {
			return status;
		}

		public String code() {
			return code;
		}

		/** Present when the failure belongs to an order that was persisted (so the client can look it up). */
		public String orderId() {
			return orderId;
		}
	}

	public static ApiException invalid(String message) {
		return new ApiException(HttpStatus.BAD_REQUEST, "INVALID_REQUEST", message, null);
	}

	public static ApiException unknownSku(String sku) {
		return new ApiException(HttpStatus.BAD_REQUEST, "SKU_NOT_FOUND", "unknown sku " + sku, null);
	}

	public static ApiException orderNotFound(String id) {
		return new ApiException(HttpStatus.NOT_FOUND, "ORDER_NOT_FOUND", "no order " + id, null);
	}

	/** Same Idempotency-Key reused with a different body. */
	public static ApiException keyReused() {
		return new ApiException(HttpStatus.CONFLICT, "IDEMPOTENCY_KEY_REUSED",
				"this Idempotency-Key was already used with a different request body", null);
	}

	/** The first request with this key is still running. */
	public static ApiException inProgress(String orderId) {
		return new ApiException(HttpStatus.CONFLICT, "REQUEST_IN_PROGRESS",
				"a request with this Idempotency-Key is still being processed, retry shortly", orderId);
	}

	public static ApiException outOfStock(String orderId) {
		return new ApiException(HttpStatus.CONFLICT, "OUT_OF_STOCK", "not enough stock, the order was cancelled", orderId);
	}

	public static ApiException paymentDeclined(String orderId) {
		return new ApiException(HttpStatus.CONFLICT, "PAYMENT_DECLINED", "payment declined, the order was cancelled", orderId);
	}

	public static ApiException inventoryUnavailable(String orderId) {
		return new ApiException(HttpStatus.SERVICE_UNAVAILABLE, "INVENTORY_UNAVAILABLE",
				"inventory is unavailable, the order was not placed", orderId);
	}

	public static ApiException failed(String reason, String orderId) {
		return switch (reason == null ? "" : reason) {
			case "OUT_OF_STOCK" -> outOfStock(orderId);
			case "PAYMENT_DECLINED" -> paymentDeclined(orderId);
			case "INVENTORY_UNAVAILABLE" -> inventoryUnavailable(orderId);
			default -> new ApiException(HttpStatus.CONFLICT, "ORDER_FAILED", "the order failed: " + reason, orderId);
		};
	}
}
