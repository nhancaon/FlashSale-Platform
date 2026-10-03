package com.flashsale.inventory.repository;

import java.util.Optional;

import com.flashsale.inventory.domain.Records.Reservation;
import com.flashsale.inventory.domain.Records.StockRow;

import org.springframework.jdbc.core.simple.JdbcClient;
import org.springframework.stereotype.Repository;

/**
 * All SQL lives here (Repository pattern). Every statement uses bind variables.
 * Methods run inside the caller's transaction.
 */
@Repository
public class InventoryRepository {

	private final JdbcClient jdbc;

	InventoryRepository(JdbcClient jdbc) {
		this.jdbc = jdbc;
	}

	public Optional<Long> findProductId(String sku) {
		return jdbc.sql("SELECT id FROM product WHERE sku = :sku").param("sku", sku).query(Long.class).optional();
	}

	public Optional<StockSnapshot> findStockBySku(String sku) {
		return jdbc.sql("""
				SELECT p.sku, s.available, s.reserved
				FROM product p JOIN stock s ON s.product_id = p.id
				WHERE p.sku = :sku""")
				.param("sku", sku)
				.query((rs, i) -> new StockSnapshot(rs.getString("sku"), rs.getLong("available"), rs.getLong("reserved")))
				.optional();
	}

	public record StockSnapshot(String sku, long available, long reserved) {
	}

	public Optional<Reservation> findReservation(String orderId, long productId) {
		return jdbc.sql("""
				SELECT id, product_id, qty, status FROM reservation
				WHERE order_id = :orderId AND product_id = :productId""")
				.param("orderId", orderId).param("productId", productId)
				.query((rs, i) -> new Reservation(rs.getString("id"), rs.getLong("product_id"), rs.getLong("qty"), rs.getString("status")))
				.optional();
	}

	/** Unique (order_id, product_id): a duplicate raises DuplicateKeyException (ORA-00001). */
	public void insertReservation(String id, String orderId, long productId, long qty) {
		jdbc.sql("""
				INSERT INTO reservation (id, order_id, product_id, qty, status)
				VALUES (:id, :orderId, :productId, :qty, 'RESERVED')""")
				.param("id", id).param("orderId", orderId).param("productId", productId).param("qty", qty)
				.update();
	}

	// ---- stock decrement building blocks (used by the strategies) ----

	/** Single statement that checks and decrements; the row lock makes it atomic. */
	public boolean decrementIfAvailable(long productId, long qty) {
		return jdbc.sql("""
				UPDATE stock SET available = available - :qty, reserved = reserved + :qty, version = version + 1
				WHERE product_id = :productId AND available >= :qty""")
				.param("qty", qty).param("productId", productId).update() == 1;
	}

	public StockRow lockStock(long productId) {
		return jdbc.sql("SELECT available, version FROM stock WHERE product_id = :productId FOR UPDATE")
				.param("productId", productId)
				.query((rs, i) -> new StockRow(rs.getLong("available"), rs.getLong("version"))).single();
	}

	public StockRow readStock(long productId) {
		return jdbc.sql("SELECT available, version FROM stock WHERE product_id = :productId")
				.param("productId", productId)
				.query((rs, i) -> new StockRow(rs.getLong("available"), rs.getLong("version"))).single();
	}

	/** Unconditional decrement; the caller already holds the row lock. */
	public void decrement(long productId, long qty) {
		jdbc.sql("""
				UPDATE stock SET available = available - :qty, reserved = reserved + :qty, version = version + 1
				WHERE product_id = :productId""")
				.param("qty", qty).param("productId", productId).update();
	}

	/** Decrement only if nobody changed the row since it was read. */
	public boolean decrementIfVersion(long productId, long qty, long expectedVersion) {
		return jdbc.sql("""
				UPDATE stock SET available = available - :qty, reserved = reserved + :qty, version = version + 1
				WHERE product_id = :productId AND version = :version AND available >= :qty""")
				.param("qty", qty).param("productId", productId).param("version", expectedVersion).update() == 1;
	}

	// ---- release / confirm ----

	/** Moves RESERVED to the new status; false means someone else already changed it. */
	public boolean transitionFromReserved(String reservationId, String newStatus) {
		return jdbc.sql("UPDATE reservation SET status = :status WHERE id = :id AND status = 'RESERVED'")
				.param("status", newStatus).param("id", reservationId).update() == 1;
	}

	public void restock(long productId, long qty) {
		jdbc.sql("""
				UPDATE stock SET available = available + :qty, reserved = reserved - :qty, version = version + 1
				WHERE product_id = :productId""")
				.param("qty", qty).param("productId", productId).update();
	}

	public void markSold(long productId, long qty) {
		jdbc.sql("UPDATE stock SET reserved = reserved - :qty, version = version + 1 WHERE product_id = :productId")
				.param("qty", qty).param("productId", productId).update();
	}
}
