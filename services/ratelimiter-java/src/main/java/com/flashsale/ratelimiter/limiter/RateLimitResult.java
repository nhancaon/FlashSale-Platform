package com.flashsale.ratelimiter.limiter;

public record RateLimitResult(boolean allowed, long remaining, long retryAfterMs) {
}
