package com.flashsale.ratelimiter.limiter;

import java.time.Clock;
import java.time.Duration;
import java.util.List;

import org.springframework.core.io.ClassPathResource;
import org.springframework.data.redis.core.StringRedisTemplate;
import org.springframework.data.redis.core.script.DefaultRedisScript;
import org.springframework.scripting.support.ResourceScriptSource;

/**
 * Shared plumbing: runs one Lua script atomically in Redis. The scripts are
 * byte-identical to the Go service so both languages behave the same.
 */
abstract class AbstractScriptLimiter implements RateLimiter {

	private final String name;
	private final DefaultRedisScript<List> script;
	private final StringRedisTemplate redis;
	private final Clock clock;

	@SuppressWarnings("rawtypes")
	AbstractScriptLimiter(String name, String scriptFile, StringRedisTemplate redis, Clock clock) {
		this.name = name;
		this.redis = redis;
		this.clock = clock;
		this.script = new DefaultRedisScript<>();
		this.script.setScriptSource(new ResourceScriptSource(new ClassPathResource("scripts/" + scriptFile)));
		this.script.setResultType(List.class);
	}

	@Override
	public String name() {
		return name;
	}

	/** Extra script arguments appended after now/limit/window. */
	Object[] extraArgs() {
		return new Object[0];
	}

	@Override
	public RateLimitResult allow(String key, long limit, Duration window) {
		if (limit <= 0 || window.toMillis() < 1) {
			throw new IllegalArgumentException("limit must be > 0 and window >= 1ms (got %d, %s)".formatted(limit, window));
		}
		Object[] extra = extraArgs();
		Object[] args = new Object[3 + extra.length];
		args[0] = String.valueOf(clock.millis());
		args[1] = String.valueOf(limit);
		args[2] = String.valueOf(window.toMillis());
		System.arraycopy(extra, 0, args, 3, extra.length);

		@SuppressWarnings("unchecked")
		List<Long> raw = redis.execute(script, List.of("rl:" + name + ":" + key), args);
		if (raw == null || raw.size() != 3) {
			throw new IllegalStateException(name + " script returned an unexpected result: " + raw);
		}
		return new RateLimitResult(raw.get(0) == 1L, raw.get(1), raw.get(2));
	}
}
