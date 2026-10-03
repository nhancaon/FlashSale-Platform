package com.flashsale.order.repository;

import java.util.List;

import com.flashsale.order.domain.Domain.Order;
import com.flashsale.order.domain.Domain.OrderItem;
import com.flashsale.order.domain.Domain.OrderStatus;

import tools.jackson.databind.json.JsonMapper;

import org.springframework.stereotype.Component;
import org.springframework.transaction.support.TransactionTemplate;

/**
 * The two short database transactions of the order flow. Each writes the order change and its outbox event
 * together, so an event exists if and only if the change committed (Transactional Outbox).
 */
@Component
public class OrderStore {

	private final OrderRepository repo;
	private final TransactionTemplate tx;
	private final JsonMapper json;

	OrderStore(OrderRepository repo, TransactionTemplate tx, JsonMapper json) {
		this.repo = repo;
		this.tx = tx;
		this.json = json;
	}

	/** Transaction 1: order (PENDING) + items + ORDER_CREATED event. Throws DuplicateKeyException on a reused key. */
	public void createPending(Order order) {
		tx.executeWithoutResult(status -> {
			repo.insertOrder(order.id(), order.userId(), order.total(), order.idempotencyKey());
			for (OrderItem item : order.items()) {
				repo.insertItem(order.id(), item.productId(), item.qty(), item.price());
			}
			repo.insertOutboxEvent(order.id(), "ORDER_CREATED", payload(order));
		});
	}

	/**
	 * Transaction 2: set the final status and write ORDER_CONFIRMED / ORDER_FAILED. Only the request that
	 * actually moves the order out of PENDING writes the event, so concurrent resumptions never duplicate it.
	 */
	public Order finalizeOrder(Order order, OrderStatus status, String reason) {
		tx.executeWithoutResult(s -> {
			if (repo.finalizeOrder(order.id(), status, reason)) {
				String type = status == OrderStatus.CONFIRMED ? "ORDER_CONFIRMED" : "ORDER_FAILED";
				repo.insertOutboxEvent(order.id(), type, payload(order.withStatus(status, reason)));
			}
		});
		return repo.findById(order.id()).orElseThrow();
	}

	public boolean isStale(String orderId, long seconds) {
		return repo.isStale(orderId, seconds);
	}

	private String payload(Order o) {
		record Item(String sku, long qty, java.math.BigDecimal price) {
		}
		record Payload(String orderId, String userId, String status, java.math.BigDecimal total, String failureReason,
				List<Item> items) {
		}
		return json.writeValueAsString(new Payload(o.id(), o.userId(), o.status().name(), o.total(), o.failureReason(),
				o.items().stream().map(i -> new Item(i.sku(), i.qty(), i.price())).toList()));
	}
}
