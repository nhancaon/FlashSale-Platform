package com.flashsale.order.repository;

import java.math.BigDecimal;
import java.time.Instant;
import java.util.List;
import java.util.Optional;

import com.flashsale.order.domain.Domain.Order;
import com.flashsale.order.domain.Domain.OrderItem;
import com.flashsale.order.domain.Domain.OrderStatus;
import com.flashsale.order.domain.Domain.Product;

import org.springframework.jdbc.core.simple.JdbcClient;
import org.springframework.stereotype.Repository;

/** All SQL for orders and the outbox. Bind variables everywhere; methods join the caller's transaction. */
@Repository
public class OrderRepository {

	private final JdbcClient jdbc;

	OrderRepository(JdbcClient jdbc) {
		this.jdbc = jdbc;
	}

	public Optional<Product> findProduct(String sku) {
		return jdbc.sql("SELECT id, sku, price FROM product WHERE sku = :sku").param("sku", sku)
				.query((rs, i) -> new Product(rs.getLong("id"), rs.getString("sku"), rs.getBigDecimal("price"))).optional();
	}

	public Optional<Order> findByUserAndKey(String userId, String idempotencyKey) {
		return loadWithItems(jdbc.sql("""
				SELECT id, user_id, status, total, idempotency_key, created_at, failure_reason FROM orders
				WHERE user_id = :userId AND idempotency_key = :key""")
				.param("userId", userId).param("key", idempotencyKey));
	}

	public Optional<Order> findById(String id) {
		return loadWithItems(jdbc.sql("""
				SELECT id, user_id, status, total, idempotency_key, created_at, failure_reason FROM orders
				WHERE id = :id""").param("id", id));
	}

	private Optional<Order> loadWithItems(JdbcClient.StatementSpec query) {
		Optional<Order> header = query.query((rs, i) -> new Order(rs.getString("id"), rs.getString("user_id"),
				OrderStatus.valueOf(rs.getString("status")), rs.getBigDecimal("total"), rs.getString("idempotency_key"),
				utcInstant(rs.getObject("created_at", java.time.LocalDateTime.class)), rs.getString("failure_reason"), List.of())).optional();
		return header.map(o -> new Order(o.id(), o.userId(), o.status(), o.total(), o.idempotencyKey(), o.createdAt(),
				o.failureReason(), items(o.id())));
	}

	private List<OrderItem> items(String orderId) {
		return jdbc.sql("""
				SELECT p.sku, i.product_id, i.qty, i.price FROM order_item i JOIN product p ON p.id = i.product_id
				WHERE i.order_id = :orderId ORDER BY p.sku""").param("orderId", orderId)
				.query((rs, i) -> new OrderItem(rs.getString("sku"), rs.getLong("product_id"), rs.getLong("qty"),
						rs.getBigDecimal("price")))
				.list();
	}

	/** Unique (user_id, idempotency_key): a duplicate raises DuplicateKeyException (ORA-00001). */
	public void insertOrder(String id, String userId, BigDecimal total, String idempotencyKey) {
		jdbc.sql("""
				INSERT INTO orders (id, user_id, status, total, idempotency_key, updated_at)
				VALUES (:id, :userId, 'PENDING', :total, :key, SYSTIMESTAMP)""")
				.param("id", id).param("userId", userId).param("total", total).param("key", idempotencyKey).update();
	}

	public void insertItem(String orderId, long productId, long qty, BigDecimal price) {
		jdbc.sql("INSERT INTO order_item (order_id, product_id, qty, price) VALUES (:orderId, :productId, :qty, :price)")
				.param("orderId", orderId).param("productId", productId).param("qty", qty).param("price", price).update();
	}

	/** Finalizes a PENDING order exactly once; false means another request already finalized it. */
	public boolean finalizeOrder(String orderId, OrderStatus status, String failureReason) {
		return jdbc.sql("""
				UPDATE orders SET status = :status, failure_reason = :reason, updated_at = SYSTIMESTAMP
				WHERE id = :id AND status = 'PENDING'""")
				.param("status", status.name()).param("reason", failureReason).param("id", orderId).update() == 1;
	}

	/** Part of the order transaction (Transactional Outbox): the event commits or rolls back with the order. */
	public void insertOutboxEvent(String aggregateId, String eventType, String payloadJson) {
		jdbc.sql("INSERT INTO outbox_events (aggregate_id, event_type, payload) VALUES (:aggregateId, :type, :payload)")
				.param("aggregateId", aggregateId).param("type", eventType).param("payload", payloadJson).update();
	}

	/**
	 * TIMESTAMP columns hold the database server wall clock (DEFAULT SYSTIMESTAMP, UTC in our containers). Read them as
	 * LocalDateTime, never as java.sql.Timestamp, which would apply the JVM time zone and shift the value.
	 */
	private static Instant utcInstant(java.time.LocalDateTime wallClock) {
		return wallClock.toInstant(java.time.ZoneOffset.UTC);
	}

	/** Ids of PENDING orders untouched for more than {@code seconds} (oldest first), for the recovery job. */
	public List<String> findStalePending(long seconds, int limit) {
		return jdbc.sql("""
				SELECT id FROM orders
				WHERE status = 'PENDING' AND updated_at < CAST(SYSTIMESTAMP AS TIMESTAMP) - NUMTODSINTERVAL(:secs, 'SECOND')
				ORDER BY updated_at OFFSET 0 ROWS FETCH NEXT :lim ROWS ONLY""")
				.param("secs", seconds).param("lim", limit).query(String.class).list();
	}

	/**
	 * Takes over an abandoned PENDING order: true when it was untouched for more than {@code seconds} (database clock)
	 * and this caller refreshed updated_at first. Concurrent callers (a client retry, the recovery job of any replica)
	 * race on the same row and only one wins per stale period, so a saga is not resumed twice at once.
	 */
	public boolean claimStale(String orderId, long seconds) {
		return jdbc.sql("""
				UPDATE orders SET updated_at = SYSTIMESTAMP
				WHERE id = :id AND status = 'PENDING'
				AND updated_at < CAST(SYSTIMESTAMP AS TIMESTAMP) - NUMTODSINTERVAL(:secs, 'SECOND')""")
				.param("id", orderId).param("secs", seconds).update() == 1;
	}
}
