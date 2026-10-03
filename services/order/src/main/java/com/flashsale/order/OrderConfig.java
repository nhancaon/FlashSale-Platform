package com.flashsale.order;

import java.time.Duration;

import com.flashsale.order.inventory.HttpInventoryGateway;
import com.flashsale.order.inventory.InventoryGateway;
import com.flashsale.order.inventory.ResilienceFactory;
import com.flashsale.order.inventory.ResilienceFactory.Settings;
import com.flashsale.order.payment.PaymentGateway;
import com.flashsale.order.payment.SimulatedPaymentGateway;
import com.flashsale.order.repository.OrderStore;
import com.flashsale.order.saga.OrderSaga;

import io.github.resilience4j.bulkhead.Bulkhead;
import io.github.resilience4j.bulkhead.BulkheadRegistry;
import io.github.resilience4j.circuitbreaker.CircuitBreaker;
import io.github.resilience4j.circuitbreaker.CircuitBreakerRegistry;
import io.github.resilience4j.micrometer.tagged.TaggedBulkheadMetrics;
import io.github.resilience4j.micrometer.tagged.TaggedCircuitBreakerMetrics;
import io.github.resilience4j.micrometer.tagged.TaggedRetryMetrics;
import io.github.resilience4j.retry.Retry;
import io.github.resilience4j.retry.RetryRegistry;
import io.micrometer.core.instrument.MeterRegistry;
import io.micrometer.core.instrument.binder.MeterBinder;
import tools.jackson.databind.json.JsonMapper;

import org.springframework.beans.factory.annotation.Value;
import org.springframework.context.annotation.Bean;
import org.springframework.context.annotation.Configuration;

@Configuration(proxyBeanMethods = false)
class OrderConfig {

	private static final String INVENTORY = "inventory";

	@Bean
	Settings inventorySettings(@Value("${order.inventory.retry-max-attempts:3}") int retryMaxAttempts,
			@Value("${order.inventory.retry-wait-ms:50}") long retryWaitMs,
			@Value("${order.inventory.breaker-window:20}") int breakerWindow,
			@Value("${order.inventory.breaker-failure-rate:50}") float breakerFailureRate,
			@Value("${order.inventory.breaker-open-ms:10000}") long breakerOpenMs,
			@Value("${order.inventory.bulkhead-max-concurrent:50}") int bulkheadMax,
			@Value("${order.inventory.bulkhead-max-wait-ms:200}") long bulkheadWaitMs) {
		return new Settings(retryMaxAttempts, retryWaitMs, breakerWindow, breakerFailureRate, breakerOpenMs, bulkheadMax,
				bulkheadWaitMs);
	}

	@Bean
	CircuitBreakerRegistry circuitBreakerRegistry(Settings s) {
		CircuitBreakerRegistry registry = CircuitBreakerRegistry.ofDefaults();
		registry.circuitBreaker(INVENTORY, ResilienceFactory.circuitBreaker(s));
		return registry;
	}

	@Bean
	RetryRegistry retryRegistry(Settings s) {
		RetryRegistry registry = RetryRegistry.ofDefaults();
		registry.retry(INVENTORY, ResilienceFactory.retry(s));
		return registry;
	}

	@Bean
	BulkheadRegistry bulkheadRegistry(Settings s) {
		BulkheadRegistry registry = BulkheadRegistry.ofDefaults();
		registry.bulkhead(INVENTORY, ResilienceFactory.bulkhead(s));
		return registry;
	}

	// Micrometer picks MeterBinder beans up: resilience4j_circuitbreaker_*, _retry_*, _bulkhead_* in /metrics.
	@Bean
	MeterBinder circuitBreakerMetrics(CircuitBreakerRegistry registry) {
		return TaggedCircuitBreakerMetrics.ofCircuitBreakerRegistry(registry);
	}

	@Bean
	MeterBinder retryMetrics(RetryRegistry registry) {
		return TaggedRetryMetrics.ofRetryRegistry(registry);
	}

	@Bean
	MeterBinder bulkheadMetrics(BulkheadRegistry registry) {
		return TaggedBulkheadMetrics.ofBulkheadRegistry(registry);
	}

	@Bean
	InventoryGateway inventoryGateway(@Value("${order.inventory.url:http://localhost:8083}") String url,
			@Value("${order.inventory.connect-timeout-ms:500}") long connectTimeoutMs,
			@Value("${order.inventory.read-timeout-ms:2000}") long readTimeoutMs, JsonMapper json,
			CircuitBreakerRegistry breakers, RetryRegistry retries, BulkheadRegistry bulkheads) {
		CircuitBreaker breaker = breakers.circuitBreaker(INVENTORY);
		Retry retry = retries.retry(INVENTORY);
		Bulkhead bulkhead = bulkheads.bulkhead(INVENTORY);
		return new HttpInventoryGateway(url, Duration.ofMillis(connectTimeoutMs), Duration.ofMillis(readTimeoutMs), json,
				breaker, retry, bulkhead);
	}

	@Bean
	PaymentGateway paymentGateway(@Value("${order.payment.latency-ms:20}") long latencyMs,
			@Value("${order.payment.fail-rate:0}") double failRate) {
		return new SimulatedPaymentGateway(latencyMs, failRate);
	}

	@Bean
	OrderSaga orderSaga(InventoryGateway inventory, PaymentGateway payment, OrderStore store, MeterRegistry metrics) {
		return new OrderSaga(inventory, payment, store, metrics);
	}
}
