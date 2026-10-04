package com.flashsale.order.saga;

import com.flashsale.order.domain.Domain.OrderStatus;
import com.flashsale.order.repository.OrderStore;

import io.micrometer.core.instrument.MeterRegistry;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

import org.springframework.beans.factory.annotation.Value;
import org.springframework.boot.autoconfigure.condition.ConditionalOnProperty;
import org.springframework.scheduling.annotation.Scheduled;
import org.springframework.stereotype.Component;

/**
 * Finishes abandoned sagas. A PENDING order used to move on only when its client retried the same Idempotency-Key;
 * a client that gave up (timeout under overload, crash) left it PENDING for ever, with its ORDER_CREATED event but no
 * final event and no notification (found by the 2000-VU load test). This job resumes such orders in the background.
 * It claims each order like a client retry does (ADR 0005), so replicas and retries never resume the same order at
 * the same time; every saga step is idempotent anyway.
 */
@Component
@ConditionalOnProperty(name = "order.recovery.enabled", havingValue = "true", matchIfMissing = true)
public class PendingOrderRecovery {

	private static final Logger log = LoggerFactory.getLogger(PendingOrderRecovery.class);

	private final OrderStore store;
	private final OrderSaga saga;
	private final MeterRegistry metrics;
	private final long staleAfterSeconds;
	private final int batchSize;

	PendingOrderRecovery(OrderStore store, OrderSaga saga, MeterRegistry metrics,
			@Value("${order.stale-after-ms:30000}") long staleAfterMs,
			@Value("${order.recovery.batch-size:50}") int batchSize) {
		this.store = store;
		this.saga = saga;
		this.metrics = metrics;
		this.staleAfterSeconds = Math.max(1, staleAfterMs / 1000);
		this.batchSize = batchSize;
	}

	/** Runs on Spring's scheduler, which stops with the application context (graceful shutdown). */
	@Scheduled(fixedDelayString = "${order.recovery.interval-ms:10000}", initialDelayString = "${order.recovery.interval-ms:10000}")
	public void scheduled() {
		try {
			recoverOnce();
		}
		catch (RuntimeException ex) { // database unavailable: try again on the next tick
			log.warn("pending order recovery failed: {}", ex.toString());
		}
	}

	/** Resumes up to one batch of stale PENDING orders; returns how many reached a final state. */
	public int recoverOnce() {
		int finished = 0;
		for (String id : store.findStalePending(staleAfterSeconds, batchSize)) {
			if (!store.claimStale(id, staleAfterSeconds)) {
				continue; // a client retry or another replica took it
			}
			var order = store.find(id);
			if (order.isEmpty()) {
				continue;
			}
			OrderStatus result = saga.run(order.get()).status();
			metrics.counter("order.recovery.resumed", "result", result.name()).increment();
			if (result != OrderStatus.PENDING) {
				finished++;
			}
			log.info("resumed abandoned order {}: {}", id, result);
		}
		return finished;
	}
}
