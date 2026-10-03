package com.flashsale.order.payment;

import java.math.BigDecimal;
import java.util.concurrent.locks.LockSupport;
import java.time.Duration;

/**
 * Stand-in for a payment provider: waits a configurable time and declines a configurable share of orders.
 * The decision is a pure function of the order id, so retries (and a resumed saga) get the same answer,
 * like a real provider keyed by an idempotency key.
 */
public class SimulatedPaymentGateway implements PaymentGateway {

	private final Duration latency;
	private final double failRate;

	public SimulatedPaymentGateway(long latencyMs, double failRatePercent) {
		this.latency = Duration.ofMillis(latencyMs);
		this.failRate = failRatePercent / 100.0;
	}

	@Override
	public void charge(String orderId, BigDecimal amount) {
		if (!latency.isZero()) {
			LockSupport.parkNanos(latency.toNanos());
		}
		// Map the id to [0, 1) deterministically.
		double bucket = (Math.floorMod(orderId.hashCode() * 2654435761L, 10_000L)) / 10_000.0;
		if (bucket < failRate) {
			throw new PaymentDeclinedException(orderId);
		}
	}
}
