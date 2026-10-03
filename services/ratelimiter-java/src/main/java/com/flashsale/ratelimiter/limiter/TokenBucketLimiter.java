package com.flashsale.ratelimiter.limiter;

import java.time.Clock;

import org.springframework.data.redis.core.StringRedisTemplate;

/** Capacity = limit, refilled at limit tokens per window. Allows bursts, smooth refill. */
final class TokenBucketLimiter extends AbstractScriptLimiter {

	TokenBucketLimiter(StringRedisTemplate redis, Clock clock) {
		super(RateLimiterFactory.TOKEN_BUCKET, "token_bucket.lua", redis, clock);
	}
}
