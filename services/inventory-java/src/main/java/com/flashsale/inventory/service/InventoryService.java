package com.flashsale.inventory.service;

import java.util.Optional;
import java.util.UUID;

import com.flashsale.inventory.cache.StockCache;
import com.flashsale.inventory.domain.DomainErrors;
import com.flashsale.inventory.domain.Records.Reservation;
import com.flashsale.inventory.domain.Records.StockView;
import com.flashsale.inventory.repository.InventoryRepository;
import com.flashsale.inventory.strategy.StockStrategy;

import org.springframework.dao.DuplicateKeyException;
import org.springframework.stereotype.Service;
import org.springframework.transaction.support.TransactionTemplate;

/**
 * Use cases. Each public method is one short Oracle transaction; the cache entry is deleted
 * only after the transaction committed.
 */
@Service
public class InventoryService {

	public static final String RESERVED = "RESERVED";
	public static final String RELEASED = "RELEASED";
	public static final String CONFIRMED = "CONFIRMED";

	private final InventoryRepository repo;
	private final StockStrategy strategy;
	private final StockCache cache;
	private final TransactionTemplate tx;

	public InventoryService(InventoryRepository repo, StockStrategy strategy, StockCache cache, TransactionTemplate tx) {
		this.repo = repo;
		this.strategy = strategy;
		this.cache = cache;
		this.tx = tx;
	}

	public StockView getStock(String sku) {
		Optional<StockView> cached = cache.get(sku);
		if (cached.isPresent()) {
			return cached.get();
		}
		var snapshot = repo.findStockBySku(sku).orElseThrow(() -> new DomainErrors.SkuNotFound(sku));
		StockView view = new StockView(snapshot.sku(), snapshot.available(), snapshot.reserved());
		cache.put(view);
		return view;
	}

	/** Idempotent per (orderId, sku): a replay returns the original reservation id. */
	public String reserve(String orderId, String sku, long qty) {
		long productId = repo.findProductId(sku).orElseThrow(() -> new DomainErrors.SkuNotFound(sku));

		Optional<Reservation> existing = repo.findReservation(orderId, productId);
		if (existing.isPresent()) {
			return replay(existing.get(), qty);
		}
		try {
			String id = tx.execute(status -> {
				String reservationId = UUID.randomUUID().toString();
				repo.insertReservation(reservationId, orderId, productId, qty);
				if (!strategy.tryReserve(productId, qty, sku)) {
					// Rolls back the reservation row too, so a failed attempt leaves no trace.
					throw new DomainErrors.OutOfStock(sku);
				}
				return reservationId;
			});
			cache.invalidate(sku);
			return id;
		}
		catch (DuplicateKeyException race) {
			// Another request with the same (orderId, sku) won the unique constraint between our check and insert.
			return replay(repo.findReservation(orderId, productId).orElseThrow(() -> race), qty);
		}
	}

	private String replay(Reservation existing, long qty) {
		if (existing.qty() != qty) {
			throw new DomainErrors.IdempotencyConflict(
					"reservation already exists with qty " + existing.qty() + ", request has qty " + qty);
		}
		if (RELEASED.equals(existing.status())) {
			throw new DomainErrors.InvalidState("reservation " + existing.id() + " was already released");
		}
		return existing.id();
	}

	/** Idempotent: releasing an unknown or already released reservation is a successful no-op. */
	public String release(String orderId, String sku) {
		long productId = repo.findProductId(sku).orElseThrow(() -> new DomainErrors.SkuNotFound(sku));
		String result = tx.execute(status -> {
			Optional<Reservation> r = repo.findReservation(orderId, productId);
			if (r.isEmpty()) {
				return "NOT_FOUND";
			}
			Reservation res = r.get();
			if (CONFIRMED.equals(res.status())) {
				throw new DomainErrors.InvalidState("reservation " + res.id() + " is already confirmed");
			}
			if (repo.transitionFromReserved(res.id(), RELEASED)) {
				repo.restock(productId, res.qty());
			}
			else if (CONFIRMED.equals(currentStatus(orderId, productId))) {
				// Lost a race against a concurrent confirm.
				throw new DomainErrors.InvalidState("reservation " + res.id() + " is already confirmed");
			}
			return RELEASED;
		});
		cache.invalidate(sku);
		return result;
	}

	/** Idempotent: confirming twice is fine; confirming a released reservation is not. */
	public String confirm(String orderId, String sku) {
		long productId = repo.findProductId(sku).orElseThrow(() -> new DomainErrors.SkuNotFound(sku));
		String result = tx.execute(status -> {
			Reservation res = repo.findReservation(orderId, productId)
					.orElseThrow(() -> new DomainErrors.ReservationNotFound(orderId, sku));
			if (RELEASED.equals(res.status())) {
				throw new DomainErrors.InvalidState("reservation " + res.id() + " was released");
			}
			if (repo.transitionFromReserved(res.id(), CONFIRMED)) {
				repo.markSold(productId, res.qty());
			}
			else if (RELEASED.equals(currentStatus(orderId, productId))) {
				// Lost a race against a concurrent release.
				throw new DomainErrors.InvalidState("reservation " + res.id() + " was released");
			}
			return CONFIRMED;
		});
		cache.invalidate(sku);
		return result;
	}

	private String currentStatus(String orderId, long productId) {
		return repo.findReservation(orderId, productId).map(Reservation::status).orElse("");
	}
}
