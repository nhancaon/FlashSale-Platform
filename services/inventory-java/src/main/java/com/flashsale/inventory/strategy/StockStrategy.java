package com.flashsale.inventory.strategy;

/**
 * Strategy: how stock is decremented safely under concurrency.
 * Always called inside a transaction opened by the service.
 */
public interface StockStrategy {

	String name();

	/**
	 * @return true if {@code qty} units were moved from available to reserved, false if there is not enough stock
	 * @throws com.flashsale.inventory.domain.DomainErrors.Contention if an optimistic strategy gives up
	 */
	boolean tryReserve(long productId, long qty, String sku);
}
