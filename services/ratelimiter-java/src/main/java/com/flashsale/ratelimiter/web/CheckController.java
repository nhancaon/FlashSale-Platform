package com.flashsale.ratelimiter.web;

import java.time.Duration;
import java.util.concurrent.TimeUnit;

import com.flashsale.ratelimiter.limiter.RateLimitResult;
import com.flashsale.ratelimiter.limiter.RateLimiter;

import io.micrometer.core.instrument.Counter;
import io.micrometer.core.instrument.MeterRegistry;
import io.micrometer.core.instrument.Timer;

import org.springframework.http.ResponseEntity;
import org.springframework.web.bind.annotation.PostMapping;
import org.springframework.web.bind.annotation.RequestBody;
import org.springframework.web.bind.annotation.RestController;

@RestController
class CheckController {

	private static final int MAX_KEY_LENGTH = 256;

	private final RateLimiter limiter;
	private final Counter allowed;
	private final Counter rejected;
	private final Counter errors;
	private final Timer latency;

	CheckController(RateLimiter limiter, MeterRegistry registry) {
		this.limiter = limiter;
		String algorithm = limiter.name();
		// Names match the Go service: ratelimit_allowed_total etc. in Prometheus.
		this.allowed = Counter.builder("ratelimit.allowed").description("Requests allowed by the rate limiter.")
				.tag("algorithm", algorithm).register(registry);
		this.rejected = Counter.builder("ratelimit.rejected").description("Requests rejected by the rate limiter.")
				.tag("algorithm", algorithm).register(registry);
		this.errors = Counter.builder("ratelimit.errors")
				.description("Checks that failed because the backing store errored.")
				.tag("algorithm", algorithm).register(registry);
		this.latency = Timer.builder("ratelimit.check.duration").description("Latency of limiter decisions.")
				.tag("algorithm", algorithm).publishPercentileHistogram().register(registry);
	}

	record CheckRequest(String key, Long limit, Long windowSec) {
	}

	record CheckResponse(boolean allowed, long remaining, long retryAfterMs) {
	}

	@PostMapping("/v1/check")
	ResponseEntity<CheckResponse> check(@RequestBody CheckRequest request) {
		String problem = validate(request);
		if (problem != null) {
			throw new InvalidRequestException(problem);
		}
		long start = System.nanoTime();
		RateLimitResult result;
		try {
			result = limiter.allow(request.key(), request.limit(), Duration.ofSeconds(request.windowSec()));
		}
		catch (RuntimeException ex) {
			errors.increment();
			throw ex;
		}
		finally {
			latency.record(System.nanoTime() - start, TimeUnit.NANOSECONDS);
		}
		(result.allowed() ? allowed : rejected).increment();
		return ResponseEntity.ok(new CheckResponse(result.allowed(), result.remaining(), result.retryAfterMs()));
	}

	private static String validate(CheckRequest r) {
		if (r.key() == null || r.key().isEmpty()) {
			return "key is required";
		}
		if (r.key().length() > MAX_KEY_LENGTH) {
			return "key is too long";
		}
		if (r.limit() == null || r.limit() <= 0) {
			return "limit must be greater than 0";
		}
		if (r.windowSec() == null || r.windowSec() <= 0) {
			return "windowSec must be greater than 0";
		}
		return null;
	}
}
