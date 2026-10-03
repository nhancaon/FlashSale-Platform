package com.flashsale.ratelimiter;

import static org.assertj.core.api.Assertions.assertThat;

import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.util.UUID;

import tools.jackson.databind.JsonNode;
import tools.jackson.databind.json.JsonMapper;

import org.junit.jupiter.api.Test;
import org.springframework.boot.test.context.SpringBootTest;
import org.springframework.boot.test.web.server.LocalServerPort;
import org.springframework.context.annotation.Import;

/** HTTP contract of POST /v1/check; mirrors the Go server tests. */
@Import(TestcontainersConfiguration.class)
@SpringBootTest(webEnvironment = SpringBootTest.WebEnvironment.RANDOM_PORT,
		properties = { "ratelimit.algorithm=fixed_window",
				// @SpringBootTest turns metric export off by default; this test checks /metrics.
				"management.defaults.metrics.export.enabled=true",
				"management.prometheus.metrics.export.enabled=true" })
class CheckApiTests {

	private static final JsonMapper JSON = JsonMapper.builder().build();
	private final HttpClient http = HttpClient.newHttpClient();

	@LocalServerPort
	int port;

	private HttpResponse<String> post(String body, String... headers) throws Exception {
		HttpRequest.Builder b = HttpRequest.newBuilder(URI.create("http://localhost:" + port + "/v1/check"))
				.header("Content-Type", "application/json").POST(HttpRequest.BodyPublishers.ofString(body));
		for (int i = 0; i + 1 < headers.length; i += 2) {
			b.header(headers[i], headers[i + 1]);
		}
		return http.send(b.build(), HttpResponse.BodyHandlers.ofString());
	}

	private HttpResponse<String> get(String path) throws Exception {
		return http.send(HttpRequest.newBuilder(URI.create("http://localhost:" + port + path)).GET().build(),
				HttpResponse.BodyHandlers.ofString());
	}

	private static String body(String key, long limit, long windowSec) {
		return "{\"key\":\"%s\",\"limit\":%d,\"windowSec\":%d}".formatted(key, limit, windowSec);
	}

	@Test
	void allowsThenRejects() throws Exception {
		String key = "api-" + UUID.randomUUID();
		HttpResponse<String> first = post(body(key, 1, 60));
		assertThat(first.statusCode()).isEqualTo(200);
		JsonNode ok = JSON.readTree(first.body());
		assertThat(ok.get("allowed").asBoolean()).isTrue();
		assertThat(ok.get("remaining").asLong()).isZero();
		assertThat(ok.get("retryAfterMs").asLong()).isZero();

		HttpResponse<String> second = post(body(key, 1, 60));
		assertThat(second.statusCode()).as("a rejection is a normal answer, not an HTTP error").isEqualTo(200);
		JsonNode no = JSON.readTree(second.body());
		assertThat(no.get("allowed").asBoolean()).isFalse();
		assertThat(no.get("retryAfterMs").asLong()).isPositive();
	}

	@Test
	void validatesInput() throws Exception {
		String[] bad = { "nope", "{\"limit\":5,\"windowSec\":60}", body("k", 0, 60), body("k", -1, 60),
				body("k", 5, 0), "{\"key\":\"k\",\"limit\":5,\"windowSec\":60,\"extra\":1}",
				body("a".repeat(257), 5, 60) };
		for (String payload : bad) {
			HttpResponse<String> r = post(payload);
			assertThat(r.statusCode()).as(payload).isEqualTo(400);
			assertThat(JSON.readTree(r.body()).get("code").asString()).isEqualTo("INVALID_REQUEST");
		}
	}

	@Test
	void requestIdIsEchoedOrGenerated() throws Exception {
		assertThat(post(body("rid", 5, 60), "X-Request-Id", "abc-123").headers().firstValue("X-Request-Id"))
				.contains("abc-123");
		assertThat(post(body("rid", 5, 60)).headers().firstValue("X-Request-Id").orElse("")).hasSize(16);
	}

	@Test
	void healthReadinessAndMetrics() throws Exception {
		assertThat(get("/healthz").statusCode()).isEqualTo(200);
		assertThat(get("/readyz").statusCode()).isEqualTo(200);

		post(body("metrics-" + UUID.randomUUID(), 1, 60));
		String metrics = get("/metrics").body();
		assertThat(metrics).contains("ratelimit_allowed_total{algorithm=\"fixed_window\"");
		assertThat(metrics).contains("ratelimit_rejected_total{algorithm=\"fixed_window\"");
		assertThat(metrics).contains("ratelimit_check_duration_seconds_bucket{algorithm=\"fixed_window\"");
	}
}
