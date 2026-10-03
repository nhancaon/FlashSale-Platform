package com.flashsale.order;

import java.io.IOException;
import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.util.List;
import java.util.Map;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.Executors;
import java.util.concurrent.atomic.AtomicInteger;

import com.sun.net.httpserver.HttpExchange;
import com.sun.net.httpserver.HttpServer;

/**
 * A scriptable stand-in for the inventory service: records every call, keeps idempotent reservations in memory,
 * and can answer slowly or with errors. Runs on virtual threads so slow answers do not limit its concurrency.
 */
final class FakeInventoryServer implements AutoCloseable {

	record Call(String path, String orderId, String sku) {
	}

	private final HttpServer server;
	final List<Call> calls = new CopyOnWriteArrayList<>();
	final Map<String, Long> reservations = new ConcurrentHashMap<>(); // "orderId|sku" -> qty
	final AtomicInteger inFlight = new AtomicInteger();
	final AtomicInteger maxInFlight = new AtomicInteger();

	/** Skus that answer 409 OUT_OF_STOCK on reserve. */
	final java.util.Set<String> outOfStock = ConcurrentHashMap.newKeySet();
	/** Paths (e.g. /v1/inventory/confirm) that answer 500. */
	final java.util.Set<String> failingPaths = ConcurrentHashMap.newKeySet();
	/** The next N reserve calls answer 500, then the server recovers. */
	final AtomicInteger failNextReserves = new AtomicInteger();
	volatile boolean failEverything;
	volatile long delayMs;

	FakeInventoryServer() throws IOException {
		server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 1024);
		server.setExecutor(Executors.newVirtualThreadPerTaskExecutor());
		server.createContext("/v1/inventory/", this::handle);
		server.start();
	}

	String url() {
		return "http://127.0.0.1:" + server.getAddress().getPort();
	}

	int count(String path) {
		return (int) calls.stream().filter(c -> c.path().equals(path)).count();
	}

	void reset() {
		calls.clear();
		reservations.clear();
		outOfStock.clear();
		failingPaths.clear();
		failEverything = false;
		failNextReserves.set(0);
		delayMs = 0;
		maxInFlight.set(0);
	}

	private void handle(HttpExchange ex) throws IOException {
		int now = inFlight.incrementAndGet();
		maxInFlight.accumulateAndGet(now, Math::max);
		try {
			String body = new String(ex.getRequestBody().readAllBytes(), StandardCharsets.UTF_8);
			String path = ex.getRequestURI().getPath();
			String orderId = field(body, "orderId");
			String sku = field(body, "sku");
			calls.add(new Call(path, orderId, sku));
			if (delayMs > 0) {
				try {
					Thread.sleep(delayMs);
				}
				catch (InterruptedException e) {
					Thread.currentThread().interrupt();
				}
			}
			if (failEverything || failingPaths.contains(path)) {
				reply(ex, 500, "{\"code\":\"INTERNAL\",\"message\":\"boom\"}");
				return;
			}
			switch (path) {
				case "/v1/inventory/reserve" -> {
					if (failNextReserves.getAndUpdate(n -> n > 0 ? n - 1 : 0) > 0) {
						reply(ex, 500, "{\"code\":\"INTERNAL\",\"message\":\"flaky\"}");
						return;
					}
					if (outOfStock.contains(sku)) {
						reply(ex, 409, "{\"code\":\"OUT_OF_STOCK\",\"message\":\"no stock\"}");
						return;
					}
					reservations.putIfAbsent(orderId + "|" + sku, 1L);
					reply(ex, 200, "{\"reservationId\":\"r-" + orderId + "\"}");
				}
				case "/v1/inventory/release" -> {
					reservations.remove(orderId + "|" + sku);
					reply(ex, 200, "{\"status\":\"RELEASED\"}");
				}
				case "/v1/inventory/confirm" -> reply(ex, 200, "{\"status\":\"CONFIRMED\"}");
				default -> reply(ex, 404, "{\"code\":\"NOT_FOUND\"}");
			}
		}
		finally {
			inFlight.decrementAndGet();
		}
	}

	private static String field(String json, String name) {
		int i = json.indexOf("\"" + name + "\"");
		if (i < 0) {
			return "";
		}
		int start = json.indexOf('"', json.indexOf(':', i) + 1) + 1;
		return json.substring(start, json.indexOf('"', start));
	}

	private static void reply(HttpExchange ex, int status, String json) throws IOException {
		byte[] bytes = json.getBytes(StandardCharsets.UTF_8);
		ex.getResponseHeaders().add("Content-Type", "application/json");
		ex.sendResponseHeaders(status, bytes.length);
		try (OutputStream os = ex.getResponseBody()) {
			os.write(bytes);
		}
	}

	@Override
	public void close() {
		server.stop(0);
	}
}
