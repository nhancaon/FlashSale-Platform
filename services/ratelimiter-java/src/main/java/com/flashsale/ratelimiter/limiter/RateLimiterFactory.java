package com.flashsale.ratelimiter.limiter;

import java.time.Clock;

import org.springframework.data.redis.core.StringRedisTemplate;

/** Factory: builds a {@link RateLimiter} from its configured name. */
public final class RateLimiterFactory {

	public static final String TOKEN_BUCKET = "token_bucket";
	public static final String SLIDING_WINDOW = "sliding_window";
	public static final String FIXED_WINDOW = "fixed_window";

	private RateLimiterFactory() {
	}

	public static RateLimiter create(String algorithm, StringRedisTemplate redis, Clock clock) {
		return switch (algorithm) {
			case TOKEN_BUCKET -> new TokenBucketLimiter(redis, clock);
			case SLIDING_WINDOW -> new SlidingWindowLimiter(redis, clock);
			case FIXED_WINDOW -> new FixedWindowLimiter(redis, clock);
			default -> throw new IllegalArgumentException(
					"unknown rate limit algorithm \"%s\" (want %s, %s or %s)".formatted(
							algorithm, TOKEN_BUCKET, SLIDING_WINDOW, FIXED_WINDOW));
		};
	}
}
