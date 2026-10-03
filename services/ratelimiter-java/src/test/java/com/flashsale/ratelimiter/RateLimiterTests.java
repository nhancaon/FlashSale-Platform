package com.flashsale.ratelimiter;

import static org.assertj.core.api.Assertions.assertThat;
import static org.assertj.core.api.Assertions.assertThatThrownBy;

import java.time.Clock;
import java.time.Duration;
import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.Executors;
import java.util.concurrent.Future;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.concurrent.atomic.AtomicLong;

import com.flashsale.ratelimiter.limiter.RateLimitResult;
import com.flashsale.ratelimiter.limiter.RateLimiter;
import com.flashsale.ratelimiter.limiter.RateLimiterFactory;

import org.junit.jupiter.api.Test;
import org.junit.jupiter.params.ParameterizedTest;
import org.junit.jupiter.params.provider.ValueSource;
import org.springframework.data.redis.core.StringRedisTemplate;

/** Mirrors the Go limiter tests: same scenarios, same expected numbers. */
class RateLimiterTests {

	private static final StringRedisTemplate REDIS = RedisSupport.TEMPLATE;
	private static final AtomicLong SEQ = new AtomicLong();

	private static RateLimiter limiter(String algorithm, Clock clock) {
		return RateLimiterFactory.create(algorithm, REDIS, clock);
	}

	private static String uniqueKey(String name) {
		return name + "-" + System.nanoTime() + "-" + SEQ.incrementAndGet();
	}

	@Test
	void factoryRejectsUnknownAlgorithm() {
		assertThatThrownBy(() -> limiter("leaky_bucket", Clock.systemUTC()))
				.isInstanceOf(IllegalArgumentException.class).hasMessageContaining("leaky_bucket");
	}

	@Test
	void rejectsInvalidParameters() {
		RateLimiter l = limiter(RateLimiterFactory.FIXED_WINDOW, Clock.systemUTC());
		assertThatThrownBy(() -> l.allow("k", 0, Duration.ofSeconds(1))).isInstanceOf(IllegalArgumentException.class);
		assertThatThrownBy(() -> l.allow("k", 5, Duration.ZERO)).isInstanceOf(IllegalArgumentException.class);
	}

	@ParameterizedTest
	@ValueSource(strings = { "token_bucket", "sliding_window", "fixed_window" })
	void commonBehaviour(String algorithm) {
		FakeClock clock = new FakeClock();
		RateLimiter l = limiter(algorithm, clock);
		String key = uniqueKey("common-" + algorithm);
		long limit = 5;
		Duration window = Duration.ofSeconds(10);

		for (int i = 1; i <= limit; i++) {
			RateLimitResult r = l.allow(key, limit, window);
			assertThat(r.allowed()).as("request %d should pass", i).isTrue();
			assertThat(r.remaining()).isEqualTo(limit - i);
			assertThat(r.retryAfterMs()).isZero();
		}

		RateLimitResult rejected = l.allow(key, limit, window);
		assertThat(rejected.allowed()).isFalse();
		assertThat(rejected.remaining()).isZero();
		assertThat(rejected.retryAfterMs()).isPositive().isLessThanOrEqualTo(window.toMillis());

		// Waiting the advertised retry-after must let a request through.
		clock.advance(Duration.ofMillis(rejected.retryAfterMs()));
		assertThat(l.allow(key, limit, window).allowed()).as("request after retryAfter should pass").isTrue();

		// Other keys are unaffected.
		assertThat(l.allow(uniqueKey("other"), limit, window).allowed()).isTrue();
	}

	/** 1000 virtual threads race for one key: exactly `limit` may pass, never more. */
	@ParameterizedTest
	@ValueSource(strings = { "token_bucket", "sliding_window", "fixed_window" })
	void concurrencyNeverExceedsLimit(String algorithm) throws Exception {
		int workers = 1000;
		long limit = 100;
		RateLimiter l = limiter(algorithm, new FakeClock()); // frozen clock: no refill during the test
		String key = uniqueKey("concurrency-" + algorithm);
		AtomicInteger allowed = new AtomicInteger();
		AtomicInteger rejected = new AtomicInteger();
		CountDownLatch start = new CountDownLatch(1);

		try (var executor = Executors.newVirtualThreadPerTaskExecutor()) {
			List<Future<?>> futures = new ArrayList<>();
			for (int i = 0; i < workers; i++) {
				futures.add(executor.submit(() -> {
					start.await();
					(l.allow(key, limit, Duration.ofMinutes(1)).allowed() ? allowed : rejected).incrementAndGet();
					return null;
				}));
			}
			start.countDown();
			for (Future<?> f : futures) {
				f.get();
			}
		}
		assertThat(allowed.get()).isEqualTo((int) limit);
		assertThat(rejected.get()).isEqualTo(workers - (int) limit);
	}

	@Test
	void fixedWindowAllowsBurstAcrossBoundary() {
		FakeClock clock = new FakeClock();
		RateLimiter l = limiter(RateLimiterFactory.FIXED_WINDOW, clock);
		String key = uniqueKey("fw-burst");
		Duration window = Duration.ofSeconds(10);

		clock.advance(Duration.ofSeconds(9));
		for (int i = 0; i < 3; i++) {
			assertThat(l.allow(key, 3, window).allowed()).isTrue();
		}
		clock.advance(Duration.ofMillis(1100));
		for (int i = 0; i < 3; i++) {
			assertThat(l.allow(key, 3, window).allowed()).as("known weakness: 2x limit across a boundary").isTrue();
		}
	}

	@Test
	void slidingWindowHasNoBoundaryBurst() {
		FakeClock clock = new FakeClock();
		RateLimiter l = limiter(RateLimiterFactory.SLIDING_WINDOW, clock);
		String key = uniqueKey("sw-noburst");
		Duration window = Duration.ofSeconds(10);

		clock.advance(Duration.ofSeconds(9));
		for (int i = 0; i < 3; i++) {
			assertThat(l.allow(key, 3, window).allowed()).isTrue();
		}
		clock.advance(Duration.ofMillis(1100));
		RateLimitResult r = l.allow(key, 3, window);
		assertThat(r.allowed()).as("earlier requests are still inside the window").isFalse();
		assertThat(r.retryAfterMs()).isEqualTo(8900);
	}

	@Test
	void tokenBucketRefillsGradually() {
		FakeClock clock = new FakeClock();
		RateLimiter l = limiter(RateLimiterFactory.TOKEN_BUCKET, clock);
		String key = uniqueKey("tb-refill");
		long limit = 10;
		Duration window = Duration.ofSeconds(10); // refill: 1 token per second

		for (int i = 0; i < limit; i++) {
			assertThat(l.allow(key, limit, window).allowed()).isTrue();
		}
		RateLimitResult r = l.allow(key, limit, window);
		assertThat(r.allowed()).isFalse();
		assertThat(r.retryAfterMs()).isEqualTo(1000);

		clock.advance(Duration.ofSeconds(3)); // 3 tokens back
		for (int i = 0; i < 3; i++) {
			assertThat(l.allow(key, limit, window).allowed()).as("refilled token %d", i + 1).isTrue();
		}
		assertThat(l.allow(key, limit, window).allowed()).isFalse();

		clock.advance(Duration.ofHours(1)); // never exceeds capacity
		r = l.allow(key, limit, window);
		assertThat(r.allowed()).isTrue();
		assertThat(r.remaining()).isEqualTo(limit - 1);
	}
}
