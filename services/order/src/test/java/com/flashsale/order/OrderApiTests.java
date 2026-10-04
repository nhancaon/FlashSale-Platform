package com.flashsale.order;

import static org.assertj.core.api.Assertions.assertThat;

import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.util.ArrayList;
import java.util.HashSet;
import java.util.List;
import java.util.Set;
import java.util.UUID;
import java.util.concurrent.Executors;
import java.util.concurrent.Future;

import com.flashsale.order.repository.OrderStore;
import com.flashsale.order.saga.PendingOrderRecovery;

import io.github.resilience4j.circuitbreaker.CircuitBreakerRegistry;
import org.junit.jupiter.api.AfterAll;
import org.junit.jupiter.api.BeforeAll;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;
import tools.jackson.databind.JsonNode;
import tools.jackson.databind.json.JsonMapper;

import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.boot.test.context.SpringBootTest;
import org.springframework.boot.test.web.server.LocalServerPort;
import org.springframework.jdbc.core.simple.JdbcClient;
import org.springframework.test.context.DynamicPropertyRegistry;
import org.springframework.test.context.DynamicPropertySource;

/**
 * End to end through HTTP with real Oracle and a scriptable fake inventory: idempotency, saga compensation,
 * the transactional outbox, and what happens when inventory breaks.
 */
@SpringBootTest(webEnvironment = SpringBootTest.WebEnvironment.RANDOM_PORT, properties = {
		"order.payment.latency-ms=0", "order.stale-after-ms=1000", "order.inventory.retry-max-attempts=2",
		"order.inventory.retry-wait-ms=5", "order.inventory.breaker-window=10", "order.inventory.read-timeout-ms=500",
		"order.recovery.interval-ms=3600000", // tests call recoverOnce() themselves
		"management.defaults.metrics.export.enabled=true", "management.prometheus.metrics.export.enabled=true" })
class OrderApiTests {

	private static final JsonMapper JSON = JsonMapper.builder().build();
	private static FakeInventoryServer inventory;

	@BeforeAll
	static void startFake() throws Exception {
		inventory = new FakeInventoryServer();
	}

	@AfterAll
	static void stopFake() {
		inventory.close();
	}

	@DynamicPropertySource
	static void properties(DynamicPropertyRegistry registry) {
		registry.add("spring.datasource.url", () -> OracleSupport.URL);
		registry.add("spring.datasource.username", () -> OracleSupport.USER);
		registry.add("spring.datasource.password", () -> OracleSupport.PASSWORD);
		registry.add("order.inventory.url", () -> inventory.url());
	}

	@LocalServerPort
	int port;

	@Autowired
	JdbcClient jdbc;

	@Autowired
	CircuitBreakerRegistry breakers;

	@Autowired
	PendingOrderRecovery recovery;

	@Autowired
	OrderStore store;

	private final HttpClient http = HttpClient.newHttpClient();

	@BeforeEach
	void reset() {
		inventory.reset();
		breakers.circuitBreaker("inventory").reset();
	}

	// ---- helpers ----

	private String newSku(String price) {
		String sku = "T-" + UUID.randomUUID().toString().substring(0, 8);
		jdbc.sql("INSERT INTO product (sku, name, price) VALUES (:sku, :name, :price)").param("sku", sku)
				.param("name", "test " + sku).param("price", new java.math.BigDecimal(price)).update();
		return sku;
	}

	record Resp(int status, JsonNode body) {
	}

	private Resp post(String user, String key, String json) throws Exception {
		HttpRequest.Builder b = HttpRequest.newBuilder(URI.create("http://localhost:" + port + "/v1/orders"))
				.header("Content-Type", "application/json").POST(HttpRequest.BodyPublishers.ofString(json));
		if (user != null) {
			b.header("X-User-Id", user);
		}
		if (key != null) {
			b.header("Idempotency-Key", key);
		}
		HttpResponse<String> r = http.send(b.build(), HttpResponse.BodyHandlers.ofString());
		return new Resp(r.statusCode(), JSON.readTree(r.body()));
	}

	private Resp get(String path) throws Exception {
		HttpResponse<String> r = http.send(HttpRequest.newBuilder(URI.create("http://localhost:" + port + path)).GET().build(),
				HttpResponse.BodyHandlers.ofString());
		return new Resp(r.statusCode(), r.body().startsWith("{") ? JSON.readTree(r.body()) : null);
	}

	private static String order(String... skuQty) {
		StringBuilder sb = new StringBuilder("{\"items\":[");
		for (int i = 0; i < skuQty.length; i += 2) {
			sb.append(i > 0 ? "," : "").append("{\"sku\":\"").append(skuQty[i]).append("\",\"qty\":").append(skuQty[i + 1]).append("}");
		}
		return sb.append("]}").toString();
	}

	private int orderCount(String user, String key) {
		return jdbc.sql("SELECT COUNT(*) FROM orders WHERE user_id = :u AND idempotency_key = :k").param("u", user)
				.param("k", key).query(Integer.class).single();
	}

	private List<String> events(String orderId) {
		return jdbc.sql("SELECT event_type FROM outbox_events WHERE aggregate_id = :id ORDER BY id").param("id", orderId)
				.query(String.class).list();
	}

	private static String user() {
		return "u-" + UUID.randomUUID().toString().substring(0, 8);
	}

	// ---- happy path ----

	@Test
	void createsAConfirmedOrderAndWritesOutboxEventsInTheSameTransaction() throws Exception {
		String a = newSku("10.50");
		String b = newSku("2.00");
		String user = user(), key = UUID.randomUUID().toString();

		Resp r = post(user, key, order(b, "3", a, "2"));

		assertThat(r.status()).isEqualTo(201);
		assertThat(r.body().get("status").asString()).isEqualTo("CONFIRMED");
		assertThat(r.body().get("total").decimalValue()).isEqualByComparingTo("27.00"); // 2*10.50 + 3*2.00
		assertThat(r.body().get("items")).hasSize(2);
		String id = r.body().get("id").asString();

		// Items are processed in sku order, whatever order the client sent them in.
		String first = a.compareTo(b) < 0 ? a : b;
		String second = first.equals(a) ? b : a;
		assertThat(inventory.calls).extracting(c -> c.path() + ":" + c.sku()).containsExactly(
				"/v1/inventory/reserve:" + first, "/v1/inventory/reserve:" + second,
				"/v1/inventory/confirm:" + first, "/v1/inventory/confirm:" + second);
		assertThat(events(id)).containsExactly("ORDER_CREATED", "ORDER_CONFIRMED");

		Resp fetched = get("/v1/orders/" + id);
		assertThat(fetched.status()).isEqualTo(200);
		assertThat(fetched.body().get("status").asString()).isEqualTo("CONFIRMED");
	}

	// ---- idempotency ----

	@Test
	void sameKeyTwiceCreatesOneOrderAndReplaysTheResult() throws Exception {
		String sku = newSku("5");
		String user = user(), key = UUID.randomUUID().toString();

		Resp first = post(user, key, order(sku, "2"));
		Resp second = post(user, key, order(sku, "2"));

		assertThat(first.status()).isEqualTo(201);
		assertThat(second.status()).as("replay returns the earlier result").isEqualTo(200);
		assertThat(second.body().get("id").asString()).isEqualTo(first.body().get("id").asString());
		assertThat(orderCount(user, key)).isEqualTo(1);
		assertThat(inventory.count("/v1/inventory/reserve")).as("the saga ran once").isEqualTo(1);
		assertThat(events(first.body().get("id").asString())).containsExactly("ORDER_CREATED", "ORDER_CONFIRMED");
	}

	@Test
	void concurrentRequestsWithTheSameKeyProduceExactlyOneOrder() throws Exception {
		String sku = newSku("5");
		String user = user(), key = UUID.randomUUID().toString();
		inventory.delayMs = 150; // keep the first saga running while the others arrive

		Set<String> ids = new HashSet<>();
		int created = 0, replays = 0, inProgress = 0;
		try (var executor = Executors.newVirtualThreadPerTaskExecutor()) {
			List<Future<Resp>> futures = new ArrayList<>();
			for (int i = 0; i < 20; i++) {
				futures.add(executor.submit(() -> post(user, key, order(sku, "1"))));
			}
			for (Future<Resp> f : futures) {
				Resp r = f.get();
				switch (r.status()) {
					case 201 -> created++;
					case 200 -> replays++;
					case 409 -> {
						assertThat(r.body().get("code").asString()).isEqualTo("REQUEST_IN_PROGRESS");
						inProgress++;
					}
					default -> throw new AssertionError("unexpected " + r.status() + " " + r.body());
				}
				if (r.body().has("id")) {
					ids.add(r.body().get("id").asString());
				}
			}
		}
		assertThat(created).as("exactly one request creates the order").isEqualTo(1);
		assertThat(created + replays + inProgress).isEqualTo(20);
		assertThat(ids).hasSize(1);
		assertThat(orderCount(user, key)).isEqualTo(1);
		assertThat(inventory.count("/v1/inventory/reserve")).as("one saga, one reserve").isEqualTo(1);
	}

	@Test
	void sameKeyWithADifferentBodyIsRejected() throws Exception {
		String sku = newSku("5");
		String user = user(), key = UUID.randomUUID().toString();
		assertThat(post(user, key, order(sku, "1")).status()).isEqualTo(201);

		Resp r = post(user, key, order(sku, "2"));
		assertThat(r.status()).isEqualTo(409);
		assertThat(r.body().get("code").asString()).isEqualTo("IDEMPOTENCY_KEY_REUSED");
	}

	@Test
	void theSameKeyOfAnotherUserIsAnotherOrder() throws Exception {
		String sku = newSku("5");
		String key = UUID.randomUUID().toString();
		assertThat(post(user(), key, order(sku, "1")).status()).isEqualTo(201);
		assertThat(post(user(), key, order(sku, "1")).status()).isEqualTo(201);
	}

	// ---- saga compensation ----

	@Test
	void outOfStockReleasesWhatWasReservedAndFailsTheOrder() throws Exception {
		String x = newSku("1");
		String y = newSku("1");
		// Items are processed in sku order: the lower sku reserves fine, the higher one is out of stock.
		String a = x.compareTo(y) < 0 ? x : y;
		String b = a.equals(x) ? y : x;
		inventory.outOfStock.add(b);
		String user = user(), key = UUID.randomUUID().toString();

		Resp r = post(user, key, order(a, "1", b, "1"));

		assertThat(r.status()).isEqualTo(409);
		assertThat(r.body().get("code").asString()).isEqualTo("OUT_OF_STOCK");
		String id = r.body().get("orderId").asString();
		assertThat(inventory.calls).extracting(c -> c.path() + ":" + c.sku()).contains("/v1/inventory/release:" + a);
		assertThat(inventory.count("/v1/inventory/confirm")).isZero();
		assertThat(inventory.reservations).as("compensated: nothing stays reserved").isEmpty();
		assertThat(events(id)).containsExactly("ORDER_CREATED", "ORDER_FAILED");

		Resp replay = post(user, key, order(a, "1", b, "1"));
		assertThat(replay.status()).isEqualTo(200);
		assertThat(replay.body().get("status").asString()).isEqualTo("FAILED");
		assertThat(replay.body().get("failureReason").asString()).isEqualTo("OUT_OF_STOCK");
	}

	// ---- inventory trouble ----

	@Test
	void inventoryDownFailsTheOrderWith503AndTheBreakerOpensThenFailsFast() throws Exception {
		String sku = newSku("1");
		inventory.failEverything = true;

		for (int i = 0; i < 6; i++) {
			Resp r = post(user(), UUID.randomUUID().toString(), order(sku, "1"));
			assertThat(r.status()).isEqualTo(503);
			assertThat(r.body().get("code").asString()).isEqualTo("INVENTORY_UNAVAILABLE");
		}
		assertThat(breakers.circuitBreaker("inventory").getState().name()).isEqualTo("OPEN");

		int before = inventory.calls.size();
		long start = System.nanoTime();
		Resp r = post(user(), UUID.randomUUID().toString(), order(sku, "1"));
		assertThat(r.status()).isEqualTo(503);
		assertThat(inventory.calls.size()).as("breaker open: inventory is not called").isEqualTo(before);
		assertThat((System.nanoTime() - start) / 1_000_000).as("fails fast (ms)").isLessThan(1000);
	}

	@Test
	void paidButConfirmFailsLeavesThePendingOrderAndARetryResumesIt() throws Exception {
		String sku = newSku("5");
		String user = user(), key = UUID.randomUUID().toString();
		inventory.failingPaths.add("/v1/inventory/confirm");

		Resp first = post(user, key, order(sku, "1"));
		assertThat(first.status()).as("saga not finished: accepted, not confirmed").isEqualTo(202);
		assertThat(first.body().get("status").asString()).isEqualTo("PENDING");
		assertThat(inventory.count("/v1/inventory/release")).as("paid orders are never released").isZero();

		// Immediately retrying while it is young: still in progress.
		assertThat(post(user, key, order(sku, "1")).status()).isEqualTo(409);

		// Inventory recovers; after the stale window a retry with the same key resumes the saga forward.
		inventory.failingPaths.clear();
		breakers.circuitBreaker("inventory").reset();
		Thread.sleep(1500);
		Resp resumed = post(user, key, order(sku, "1"));
		assertThat(resumed.status()).isEqualTo(200);
		assertThat(resumed.body().get("status").asString()).isEqualTo("CONFIRMED");
		assertThat(events(first.body().get("id").asString())).containsExactly("ORDER_CREATED", "ORDER_CONFIRMED");
	}

	@Test
	void anAbandonedPendingOrderIsFinishedByTheRecoveryJobWithoutAnyClientRetry() throws Exception {
		String sku = newSku("5");
		inventory.failingPaths.add("/v1/inventory/confirm");
		Resp first = post(user(), UUID.randomUUID().toString(), order(sku, "1"));
		assertThat(first.status()).isEqualTo(202);
		String id = first.body().get("id").asString();

		// The client gave up. While the order is young the job leaves it alone (its request may still be running).
		inventory.failingPaths.clear();
		breakers.circuitBreaker("inventory").reset();
		recovery.recoverOnce();
		assertThat(status(id)).isEqualTo("PENDING");

		Thread.sleep(1500); // past order.stale-after-ms
		// Oldest first, one batch per call: on a shared dev database older abandoned orders may come before ours.
		for (int i = 0; i < 20 && status(id).equals("PENDING"); i++) {
			recovery.recoverOnce();
		}
		assertThat(status(id)).isEqualTo("CONFIRMED");
		assertThat(events(id)).containsExactly("ORDER_CREATED", "ORDER_CONFIRMED");
		recovery.recoverOnce();
		assertThat(events(id)).as("a final order is never resumed again").hasSize(2);
	}

	@Test
	void onlyOneCallerClaimsAStaleOrder() throws Exception {
		String sku = newSku("5");
		inventory.failingPaths.add("/v1/inventory/confirm");
		String id = post(user(), UUID.randomUUID().toString(), order(sku, "1")).body().get("id").asString();
		Thread.sleep(1500);

		List<Future<Boolean>> claims = new ArrayList<>();
		try (var pool = Executors.newFixedThreadPool(8)) {
			for (int i = 0; i < 8; i++) {
				claims.add(pool.submit(() -> store.claimStale(id, 1)));
			}
		}
		int won = 0;
		for (Future<Boolean> c : claims) {
			won += c.get() ? 1 : 0;
		}
		assertThat(won).as("client retries and recovery jobs of every replica race on the same row").isEqualTo(1);

		// Leave nothing PENDING behind on the shared dev database.
		inventory.failingPaths.clear();
		Thread.sleep(1100);
		for (int i = 0; i < 20 && status(id).equals("PENDING"); i++) {
			recovery.recoverOnce();
		}
		assertThat(status(id)).isEqualTo("CONFIRMED");
	}

	private String status(String orderId) {
		return jdbc.sql("SELECT status FROM orders WHERE id = :id").param("id", orderId).query(String.class).single();
	}

	// ---- validation ----

	@Test
	void validatesHeadersAndBody() throws Exception {
		String sku = newSku("5");
		assertThat(post(null, "k", order(sku, "1")).status()).isEqualTo(400);
		assertThat(post("u", null, order(sku, "1")).status()).isEqualTo(400);
		assertThat(post("u", "k", "{\"items\":[]}").status()).isEqualTo(400);
		assertThat(post("u", "k", "{}").status()).isEqualTo(400);
		assertThat(post("u", "k", "nope").status()).isEqualTo(400);
		assertThat(post("u", "k", order(sku, "0")).status()).isEqualTo(400);
		assertThat(post("u", "k", order(sku, "1", sku, "2")).status()).as("duplicate sku").isEqualTo(400);
		Resp unknown = post("u", "k" + UUID.randomUUID(), order("NO-SUCH-SKU", "1"));
		assertThat(unknown.status()).isEqualTo(400);
		assertThat(unknown.body().get("code").asString()).isEqualTo("SKU_NOT_FOUND");
		assertThat(get("/v1/orders/" + UUID.randomUUID()).status()).isEqualTo(404);
	}

	@Test
	void usersCannotReadEachOthersOrders() throws Exception {
		String sku = newSku("5");
		String owner = user();
		String id = post(owner, UUID.randomUUID().toString(), order(sku, "1")).body().get("id").asString();

		HttpResponse<String> other = http.send(HttpRequest.newBuilder(URI.create("http://localhost:" + port + "/v1/orders/" + id))
				.header("X-User-Id", "someone-else").GET().build(), HttpResponse.BodyHandlers.ofString());
		assertThat(other.statusCode()).isEqualTo(404);
	}

	@Test
	void exposesHealthAndResilienceMetrics() throws Exception {
		assertThat(get("/healthz").status()).isEqualTo(200);
		assertThat(get("/readyz").status()).isEqualTo(200);
		HttpResponse<String> metrics = http.send(
				HttpRequest.newBuilder(URI.create("http://localhost:" + port + "/metrics")).GET().build(),
				HttpResponse.BodyHandlers.ofString());
		assertThat(metrics.body()).contains("order_requests_total").contains("resilience4j_circuitbreaker_state");
	}
}
