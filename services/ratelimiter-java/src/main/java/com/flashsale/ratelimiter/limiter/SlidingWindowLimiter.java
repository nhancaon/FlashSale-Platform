package com.flashsale.ratelimiter.limiter;

import java.time.Clock;
import java.util.UUID;
import java.util.concurrent.atomic.AtomicLong;

import org.springframework.data.redis.core.StringRedisTemplate;

/** Exact sliding window log: no boundary burst, memory grows with the limit. */
final class SlidingWindowLimiter extends AbstractScriptLimiter {

	private final String instance = UUID.randomUUID().toString().substring(0, 12);
	private final AtomicLong sequence = new AtomicLong();

	SlidingWindowLimiter(StringRedisTemplate redis, Clock clock) {
		super(RateLimiterFactory.SLIDING_WINDOW, "sliding_window.lua", redis, clock);
	}

	/** Sorted-set members must be unique or same-millisecond requests would collapse into one. */
	@Override
	Object[] extraArgs() {
		return new Object[] { instance + "-" + Long.toString(sequence.incrementAndGet(), 36) };
	}
}
