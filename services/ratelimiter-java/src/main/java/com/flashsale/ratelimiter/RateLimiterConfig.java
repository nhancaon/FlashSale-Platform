package com.flashsale.ratelimiter;

import java.time.Clock;

import com.flashsale.ratelimiter.limiter.RateLimiter;
import com.flashsale.ratelimiter.limiter.RateLimiterFactory;

import org.springframework.beans.factory.annotation.Value;
import org.springframework.context.annotation.Bean;
import org.springframework.context.annotation.Configuration;
import org.springframework.data.redis.core.StringRedisTemplate;

@Configuration(proxyBeanMethods = false)
class RateLimiterConfig {

	@Bean
	Clock clock() {
		return Clock.systemUTC();
	}

	/** The algorithm is chosen by configuration; an unknown name fails startup. */
	@Bean
	RateLimiter rateLimiter(@Value("${ratelimit.algorithm:token_bucket}") String algorithm,
			StringRedisTemplate redis, Clock clock) {
		return RateLimiterFactory.create(algorithm, redis, clock);
	}
}
