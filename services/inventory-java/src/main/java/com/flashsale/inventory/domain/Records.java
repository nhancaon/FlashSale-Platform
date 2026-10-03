package com.flashsale.inventory.domain;

public final class Records {

	private Records() {
	}

	public record StockView(String sku, long available, long reserved) {
	}

	public record Reservation(String id, long productId, long qty, String status) {
	}

	public record StockRow(long available, long version) {
	}
}
