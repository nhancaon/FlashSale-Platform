package com.flashsale.order;

import static org.assertj.core.api.Assertions.assertThat;
import static org.assertj.core.api.Assertions.assertThatThrownBy;

import java.time.Duration;
import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.Executors;
import java.util.concurrent.Future;
import java.util.concurrent.atomic.AtomicInteger;

import com.flashsale.order.inventory.HttpInventoryGateway;
import com.flashsale.order.inventory.InventoryGateway.OutOfStockException;
import com.flashsale.order.inventory.InventoryGateway.UnavailableException;
import com.flashsale.order.inventory.ResilienceFactory;
import com.flashsale.order.inventory.ResilienceFactory.Settings;

import io.github.resilience4j.bulkhead.Bulkhead;
import io.github.resilience4j.circuitbreaker.CircuitBreaker;
import io.github.resilience4j.retry.Retry;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;
import tools.jackson.databind.json.JsonMapper;

/** The resilience policy against a fake inventory that can be slow or broken. */
class InventoryGatewayTests {

	private FakeInventoryServer inventory;
	private CircuitBreaker breaker;

	@BeforeEach
	void start() throws Exception {
		inventory = new FakeInventoryServer();
	}

	@AfterEach
	void stop() {
		inventory.close();
	}

	private HttpInventoryGateway gateway(Settings s, Duration readTimeout) {
		breaker = CircuitBreaker.of("t", ResilienceFactory.circuitBreaker(s));
		return new HttpInventoryGateway(inventory.url(), Duration.ofMillis(300), readTimeout, JsonMapper.builder().build(),
				breaker, Retry.of("t", ResilienceFactory.retry(s)), Bulkhead.of("t", ResilienceFactory.bulkhead(s)));
	}

	private static Settings settings(int attempts, int window, int bulkhead, long bulkheadWaitMs) {
		return new Settings(attempts, 10, window, 50f, 60_000, bulkhead, bulkheadWaitMs);
	}

	@Test
	void successfulCallsReachInventory() {
		HttpInventoryGateway gw = gateway(settings(3, 10, 10, 100), Duration.ofSeconds(2));
		gw.reserve("o1", "SKU", 2);
		gw.confirm("o1", "SKU");
		gw.release("o1", "SKU");
		assertThat(inventory.calls).extracting(FakeInventoryServer.Call::path)
				.containsExactly("/v1/inventory/reserve", "/v1/inventory/confirm", "/v1/inventory/release");
	}

	@Test
	void outOfStockIsABusinessAnswerNotRetriedAndNotCountedByTheBreaker() {
		inventory.outOfStock.add("SKU");
		HttpInventoryGateway gw = gateway(settings(3, 10, 10, 100), Duration.ofSeconds(2));

		for (int i = 0; i < 20; i++) {
			assertThatThrownBy(() -> gw.reserve("o", "SKU", 1)).isInstanceOf(OutOfStockException.class);
		}
		assertThat(inventory.count("/v1/inventory/reserve")).as("no retries").isEqualTo(20);
		assertThat(breaker.getState()).isEqualTo(CircuitBreaker.State.CLOSED);
	}

	@Test
	void transientFailuresAreRetriedWithBackoff() {
		// Fails twice, then recovers: the third attempt succeeds and the caller never notices.
		inventory.failNextReserves.set(2);
		HttpInventoryGateway gw = gateway(settings(3, 10, 10, 100), Duration.ofSeconds(2));

		gw.reserve("o1", "SKU", 1);
		assertThat(inventory.count("/v1/inventory/reserve")).isEqualTo(3);
	}

	@Test
	void givesUpAfterTheConfiguredAttempts() {
		inventory.failEverything = true;
		HttpInventoryGateway gw = gateway(settings(3, 100, 10, 100), Duration.ofSeconds(2));

		assertThatThrownBy(() -> gw.reserve("o1", "SKU", 1)).isInstanceOf(UnavailableException.class);
		assertThat(inventory.count("/v1/inventory/reserve")).isEqualTo(3);
	}

	@Test
	void breakerOpensAndFailsFastWithoutHittingTheDownstream() {
		inventory.failEverything = true;
		HttpInventoryGateway gw = gateway(settings(1, 10, 10, 100), Duration.ofSeconds(2)); // 1 attempt: simple counting

		for (int i = 0; i < 10; i++) {
			assertThatThrownBy(() -> gw.reserve("o" + System.nanoTime(), "SKU", 1)).isInstanceOf(UnavailableException.class);
		}
		assertThat(breaker.getState()).isEqualTo(CircuitBreaker.State.OPEN);

		int before = inventory.calls.size();
		long start = System.nanoTime();
		for (int i = 0; i < 50; i++) {
			assertThatThrownBy(() -> gw.reserve("later", "SKU", 1)).isInstanceOf(UnavailableException.class)
					.hasMessageContaining("circuit breaker is open");
		}
		assertThat(inventory.calls.size()).as("open breaker: no request reaches inventory").isEqualTo(before);
		assertThat(Duration.ofNanos(System.nanoTime() - start)).as("fail fast").isLessThan(Duration.ofMillis(500));
	}

	@Test
	void slowInventoryHitsTheTimeoutAndTripsTheBreaker() {
		inventory.delayMs = 600;
		HttpInventoryGateway gw = gateway(settings(1, 10, 20, 100), Duration.ofMillis(150));

		long start = System.nanoTime();
		assertThatThrownBy(() -> gw.reserve("o1", "SKU", 1)).isInstanceOf(UnavailableException.class);
		assertThat(Duration.ofNanos(System.nanoTime() - start)).as("the read timeout cuts the wait")
				.isLessThan(Duration.ofMillis(450));
	}

	/**
	 * Slow inventory must not exhaust the order service: the bulkhead admits only N calls at a time and the rest
	 * fail fast, so callers (and threads) are released quickly instead of piling up behind the slow dependency.
	 */
	@Test
	void bulkheadLimitsConcurrencyAndRejectsTheRestQuickly() throws Exception {
		inventory.delayMs = 400;
		int bulkhead = 5;
		HttpInventoryGateway gw = gateway(settings(1, 1000, bulkhead, 20), Duration.ofSeconds(2));

		int callers = 100;
		AtomicInteger rejected = new AtomicInteger();
		AtomicInteger ok = new AtomicInteger();
		long start = System.nanoTime();
		try (var executor = Executors.newVirtualThreadPerTaskExecutor()) {
			List<Future<?>> futures = new ArrayList<>();
			for (int i = 0; i < callers; i++) {
				int n = i;
				futures.add(executor.submit(() -> {
					try {
						gw.reserve("o" + n, "SKU", 1);
						ok.incrementAndGet();
					}
					catch (UnavailableException ex) {
						rejected.incrementAndGet();
					}
				}));
			}
			for (Future<?> f : futures) {
				f.get();
			}
		}
		Duration elapsed = Duration.ofNanos(System.nanoTime() - start);

		assertThat(inventory.maxInFlight.get()).as("never more than the bulkhead size in flight").isLessThanOrEqualTo(bulkhead);
		assertThat(ok.get()).isBetween(bulkhead, bulkhead * 2);
		assertThat(rejected.get()).isEqualTo(callers - ok.get());
		assertThat(elapsed).as("rejected callers did not wait for the slow dependency").isLessThan(Duration.ofSeconds(3));
	}
}
