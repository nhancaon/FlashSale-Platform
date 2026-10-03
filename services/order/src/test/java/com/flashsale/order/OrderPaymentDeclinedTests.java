package com.flashsale.order;

import static org.assertj.core.api.Assertions.assertThat;

import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.util.UUID;

import org.junit.jupiter.api.AfterAll;
import org.junit.jupiter.api.BeforeAll;
import org.junit.jupiter.api.Test;
import tools.jackson.databind.JsonNode;
import tools.jackson.databind.json.JsonMapper;

import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.boot.test.context.SpringBootTest;
import org.springframework.boot.test.web.server.LocalServerPort;
import org.springframework.jdbc.core.simple.JdbcClient;
import org.springframework.test.context.DynamicPropertyRegistry;
import org.springframework.test.context.DynamicPropertySource;

/** Saga compensation when the payment provider declines (every payment is declined in this context). */
@SpringBootTest(webEnvironment = SpringBootTest.WebEnvironment.RANDOM_PORT,
		properties = { "order.payment.latency-ms=0", "order.payment.fail-rate=100" })
class OrderPaymentDeclinedTests {

	private static FakeInventoryServer inventory;

	@BeforeAll
	static void start() throws Exception {
		inventory = new FakeInventoryServer();
	}

	@AfterAll
	static void stop() {
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

	@Test
	void declinedPaymentReleasesEveryReservationAndFailsTheOrder() throws Exception {
		String a = "T-" + UUID.randomUUID().toString().substring(0, 8);
		String b = "T-" + UUID.randomUUID().toString().substring(0, 8);
		for (String sku : new String[] { a, b }) {
			jdbc.sql("INSERT INTO product (sku, name, price) VALUES (:sku, 'p', 3)").param("sku", sku).update();
		}

		HttpRequest request = HttpRequest.newBuilder(URI.create("http://localhost:" + port + "/v1/orders"))
				.header("Content-Type", "application/json").header("X-User-Id", "u1")
				.header("Idempotency-Key", UUID.randomUUID().toString())
				.POST(HttpRequest.BodyPublishers.ofString(
						"{\"items\":[{\"sku\":\"%s\",\"qty\":1},{\"sku\":\"%s\",\"qty\":2}]}".formatted(a, b)))
				.build();
		HttpResponse<String> response = HttpClient.newHttpClient().send(request, HttpResponse.BodyHandlers.ofString());
		JsonNode body = JsonMapper.builder().build().readTree(response.body());

		assertThat(response.statusCode()).isEqualTo(409);
		assertThat(body.get("code").asString()).isEqualTo("PAYMENT_DECLINED");
		assertThat(inventory.count("/v1/inventory/reserve")).isEqualTo(2);
		assertThat(inventory.count("/v1/inventory/release")).as("both reservations are compensated").isEqualTo(2);
		assertThat(inventory.count("/v1/inventory/confirm")).isZero();
		assertThat(inventory.reservations).isEmpty();

		String orderId = body.get("orderId").asString();
		assertThat(jdbc.sql("SELECT status FROM orders WHERE id = :id").param("id", orderId).query(String.class).single())
				.isEqualTo("FAILED");
		assertThat(jdbc.sql("SELECT event_type FROM outbox_events WHERE aggregate_id = :id ORDER BY id")
				.param("id", orderId).query(String.class).list()).containsExactly("ORDER_CREATED", "ORDER_FAILED");
	}
}
