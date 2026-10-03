package com.flashsale.order.inventory;

import java.time.Duration;

import io.github.resilience4j.bulkhead.BulkheadConfig;
import io.github.resilience4j.circuitbreaker.CircuitBreakerConfig;
import io.github.resilience4j.core.IntervalFunction;
import io.github.resilience4j.retry.RetryConfig;

/** The Resilience4j policy for calls to inventory (see ADR 0005). */
public final class ResilienceFactory {

	private ResilienceFactory() {
	}

	public record Settings(int retryMaxAttempts, long retryWaitMs, int breakerWindow, float breakerFailureRate,
			long breakerOpenMs, int bulkheadMaxConcurrent, long bulkheadMaxWaitMs) {
	}

	/** Opens when at least half of the last N calls failed technically; business answers are ignored. */
	public static CircuitBreakerConfig circuitBreaker(Settings s) {
		return CircuitBreakerConfig.custom()
				.slidingWindowType(CircuitBreakerConfig.SlidingWindowType.COUNT_BASED)
				.slidingWindowSize(s.breakerWindow())
				.minimumNumberOfCalls(Math.max(5, s.breakerWindow() / 2))
				.failureRateThreshold(s.breakerFailureRate())
				.waitDurationInOpenState(Duration.ofMillis(s.breakerOpenMs()))
				.permittedNumberOfCallsInHalfOpenState(5)
				.automaticTransitionFromOpenToHalfOpenEnabled(true)
				.recordExceptions(InventoryGateway.TransientException.class)
				.ignoreExceptions(InventoryGateway.OutOfStockException.class, InventoryGateway.RejectedException.class)
				.build();
	}

	/** Retries only transient failures, with exponential backoff and jitter so clients do not retry in lockstep. */
	public static RetryConfig retry(Settings s) {
		return RetryConfig.custom()
				.maxAttempts(s.retryMaxAttempts())
				.intervalFunction(IntervalFunction.ofExponentialRandomBackoff(Duration.ofMillis(s.retryWaitMs()), 2.0, 0.5))
				.retryExceptions(InventoryGateway.TransientException.class)
				.build();
	}

	/** Caps concurrent calls to inventory; callers beyond the cap fail fast instead of piling up. */
	public static BulkheadConfig bulkhead(Settings s) {
		return BulkheadConfig.custom()
				.maxConcurrentCalls(s.bulkheadMaxConcurrent())
				.maxWaitDuration(Duration.ofMillis(s.bulkheadMaxWaitMs()))
				.build();
	}
}
