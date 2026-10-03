package com.flashsale.order.inventory;

import java.io.IOException;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.time.Duration;
import java.util.function.Supplier;

import io.github.resilience4j.bulkhead.Bulkhead;
import io.github.resilience4j.bulkhead.BulkheadFullException;
import io.github.resilience4j.circuitbreaker.CallNotPermittedException;
import io.github.resilience4j.circuitbreaker.CircuitBreaker;
import io.github.resilience4j.retry.Retry;
import org.slf4j.MDC;
import tools.jackson.databind.JsonNode;
import tools.jackson.databind.json.JsonMapper;

/**
 * Calls the inventory service over HTTP, wrapped in Timeout (HTTP client), Bulkhead, Circuit Breaker and Retry
 * with exponential backoff and jitter. Each retry attempt passes through the breaker, so failures are counted.
 */
public class HttpInventoryGateway implements InventoryGateway {

	private final HttpClient http;
	private final URI baseUri;
	private final Duration readTimeout;
	private final JsonMapper json;
	private final CircuitBreaker breaker;
	private final Retry retry;
	private final Bulkhead bulkhead;

	public HttpInventoryGateway(String baseUrl, Duration connectTimeout, Duration readTimeout, JsonMapper json,
			CircuitBreaker breaker, Retry retry, Bulkhead bulkhead) {
		this.http = HttpClient.newBuilder().connectTimeout(connectTimeout).version(HttpClient.Version.HTTP_1_1).build();
		this.baseUri = URI.create(baseUrl.endsWith("/") ? baseUrl.substring(0, baseUrl.length() - 1) : baseUrl);
		this.readTimeout = readTimeout;
		this.json = json;
		this.breaker = breaker;
		this.retry = retry;
		this.bulkhead = bulkhead;
	}

	@Override
	public void reserve(String orderId, String sku, long qty) {
		call("/v1/inventory/reserve", "{\"orderId\":%s,\"sku\":%s,\"qty\":%d}".formatted(quote(orderId), quote(sku), qty), sku);
	}

	@Override
	public void release(String orderId, String sku) {
		call("/v1/inventory/release", "{\"orderId\":%s,\"sku\":%s}".formatted(quote(orderId), quote(sku)), sku);
	}

	@Override
	public void confirm(String orderId, String sku) {
		call("/v1/inventory/confirm", "{\"orderId\":%s,\"sku\":%s}".formatted(quote(orderId), quote(sku)), sku);
	}

	private void call(String path, String body, String sku) {
		Supplier<Void> attempt = () -> {
			send(path, body, sku);
			return null;
		};
		// Innermost first: bulkhead limits concurrency, the breaker counts the outcome, retry wraps both.
		Supplier<Void> guarded = Retry.decorateSupplier(retry,
				CircuitBreaker.decorateSupplier(breaker, Bulkhead.decorateSupplier(bulkhead, attempt)));
		try {
			guarded.get();
		}
		catch (CallNotPermittedException ex) {
			throw new UnavailableException("inventory circuit breaker is open", ex);
		}
		catch (BulkheadFullException ex) {
			throw new UnavailableException("inventory bulkhead is full", ex);
		}
		catch (TransientException ex) {
			throw new UnavailableException("inventory failed after retries: " + ex.getMessage(), ex);
		}
	}

	private void send(String path, String body, String sku) {
		HttpRequest.Builder request = HttpRequest.newBuilder(URI.create(baseUri + path)).timeout(readTimeout)
				.header("Content-Type", "application/json").POST(HttpRequest.BodyPublishers.ofString(body));
		String requestId = MDC.get("request_id");
		if (requestId != null) {
			request.header("X-Request-Id", requestId);
		}
		HttpResponse<String> response;
		try {
			response = http.send(request.build(), HttpResponse.BodyHandlers.ofString());
		}
		catch (IOException ex) { // connect failures and timeouts
			throw new TransientException(ex.getClass().getSimpleName() + ": " + ex.getMessage(), ex);
		}
		catch (InterruptedException ex) {
			Thread.currentThread().interrupt();
			throw new TransientException("interrupted", ex);
		}
		int status = response.statusCode();
		if (status == 200) {
			return;
		}
		String code = errorCode(response.body());
		if (status == 409 && "OUT_OF_STOCK".equals(code)) {
			throw new OutOfStockException(sku);
		}
		if (status >= 500 || (status == 409 && "CONFLICT_RETRY".equals(code))) {
			throw new TransientException("inventory answered " + status + " " + code, null);
		}
		throw new RejectedException(status, code);
	}

	private String errorCode(String body) {
		try {
			JsonNode node = json.readTree(body);
			return node.path("code").asString("");
		}
		catch (RuntimeException ex) {
			return "";
		}
	}

	private String quote(String s) {
		return json.writeValueAsString(s);
	}
}
