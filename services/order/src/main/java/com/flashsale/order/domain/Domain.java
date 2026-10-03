package com.flashsale.order.domain;

import java.math.BigDecimal;
import java.time.Instant;
import java.util.List;

public final class Domain {

	private Domain() {
	}

	public enum OrderStatus {
		PENDING, CONFIRMED, FAILED, CANCELLED
	}

	/** An item as requested by the client. */
	public record ItemRequest(String sku, long qty) {
	}

	public record Product(long id, String sku, BigDecimal price) {
	}

	public record OrderItem(String sku, long productId, long qty, BigDecimal price) {
	}

	public record Order(String id, String userId, OrderStatus status, BigDecimal total, String idempotencyKey,
			Instant createdAt, String failureReason, List<OrderItem> items) {

		public Order withStatus(OrderStatus newStatus, String reason) {
			return new Order(id, userId, newStatus, total, idempotencyKey, createdAt, reason, items);
		}
	}
}
