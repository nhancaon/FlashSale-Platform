package com.flashsale.inventory.strategy;

import java.util.concurrent.ThreadLocalRandom;
import java.util.concurrent.locks.LockSupport;

import com.flashsale.inventory.domain.DomainErrors;
import com.flashsale.inventory.domain.Records.StockRow;
import com.flashsale.inventory.repository.InventoryRepository;

/** Factory plus the three implementations. */
public final class StockStrategies {

	public static final String ATOMIC = "atomic";
	public static final String PESSIMISTIC = "pessimistic";
	public static final String OPTIMISTIC = "optimistic";

	private StockStrategies() {
	}

	public static StockStrategy create(String name, InventoryRepository repo, int optimisticMaxRetries) {
		return switch (name) {
			case ATOMIC -> new Atomic(repo);
			case PESSIMISTIC -> new Pessimistic(repo);
			case OPTIMISTIC -> new Optimistic(repo, optimisticMaxRetries);
			default -> throw new IllegalArgumentException(
					"unknown STOCK_STRATEGY \"%s\" (want %s, %s or %s)".formatted(name, ATOMIC, PESSIMISTIC, OPTIMISTIC));
		};
	}

	/** One conditional UPDATE: check and decrement in a single statement, rows affected tells the outcome. */
	record Atomic(InventoryRepository repo) implements StockStrategy {

		@Override
		public String name() {
			return ATOMIC;
		}

		@Override
		public boolean tryReserve(long productId, long qty, String sku) {
			return repo.decrementIfAvailable(productId, qty);
		}
	}

	/** SELECT ... FOR UPDATE, check in the application, then UPDATE while holding the lock. */
	record Pessimistic(InventoryRepository repo) implements StockStrategy {

		@Override
		public String name() {
			return PESSIMISTIC;
		}

		@Override
		public boolean tryReserve(long productId, long qty, String sku) {
			StockRow row = repo.lockStock(productId);
			if (row.available() < qty) {
				return false;
			}
			repo.decrement(productId, qty);
			return true;
		}
	}

	/** Read version, update only if the version is unchanged, retry (bounded, with jitter) when it lost. */
	record Optimistic(InventoryRepository repo, int maxRetries) implements StockStrategy {

		@Override
		public String name() {
			return OPTIMISTIC;
		}

		@Override
		public boolean tryReserve(long productId, long qty, String sku) {
			for (int attempt = 0; attempt <= maxRetries; attempt++) {
				StockRow row = repo.readStock(productId);
				if (row.available() < qty) {
					return false;
				}
				if (repo.decrementIfVersion(productId, qty, row.version())) {
					return true;
				}
				// Lost the race: back off a little so the winners can finish.
				LockSupport.parkNanos(ThreadLocalRandom.current().nextLong(100_000, 2_000_000L * Math.min(attempt + 1, 10)));
			}
			throw new DomainErrors.Contention(sku);
		}
	}
}
