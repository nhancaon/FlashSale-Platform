package com.flashsale.ratelimiter.limiter;

import java.time.Clock;

import org.springframework.data.redis.core.StringRedisTemplate;

/** Cheapest option, but allows up to 2x limit across a window boundary. */
final class FixedWindowLimiter extends AbstractScriptLimiter {

	FixedWindowLimiter(StringRedisTemplate redis, Clock clock) {
		super(RateLimiterFactory.FIXED_WINDOW, "fixed_window.lua", redis, clock);
	}
}
