package com.flashsale.ratelimiter.limiter;

import java.time.Duration;

/** Strategy interface: one implementation per rate limit algorithm. */
public interface RateLimiter {

	String name();

	/**
	 * Decides whether a request for {@code key} may proceed.
	 *
	 * @throws org.springframework.dao.DataAccessException when the Redis store fails
	 */
	RateLimitResult allow(String key, long limit, Duration window);
}
