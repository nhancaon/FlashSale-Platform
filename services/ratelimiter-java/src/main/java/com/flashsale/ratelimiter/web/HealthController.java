package com.flashsale.ratelimiter.web;

import java.util.Map;

import org.springframework.data.redis.connection.RedisConnectionFactory;
import org.springframework.http.HttpStatus;
import org.springframework.http.ResponseEntity;
import org.springframework.web.bind.annotation.GetMapping;
import org.springframework.web.bind.annotation.RestController;

@RestController
class HealthController {

	private final RedisConnectionFactory redis;

	HealthController(RedisConnectionFactory redis) {
		this.redis = redis;
	}

	@GetMapping("/healthz")
	Map<String, String> healthz() {
		return Map.of("status", "ok");
	}

	@GetMapping("/readyz")
	ResponseEntity<?> readyz() {
		try (var connection = redis.getConnection()) {
			connection.ping();
			return ResponseEntity.ok(Map.of("status", "ready"));
		}
		catch (RuntimeException ex) {
			return ResponseEntity.status(HttpStatus.SERVICE_UNAVAILABLE)
					.body(new ApiExceptionHandler.ErrorBody("NOT_READY", "redis unreachable"));
		}
	}
}
