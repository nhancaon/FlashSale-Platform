package com.flashsale.inventory.cache;

import java.time.Duration;
import java.util.Optional;

import com.flashsale.inventory.domain.Records.StockView;

import io.micrometer.core.instrument.Counter;
import io.micrometer.core.instrument.MeterRegistry;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

import org.springframework.beans.factory.annotation.Value;
import org.springframework.data.redis.core.StringRedisTemplate;
import org.springframework.stereotype.Component;

/**
 * Cache-aside for stock reads. Value format "available:reserved". Redis is best effort:
 * any failure counts as a miss, so Oracle stays the source of truth. Entries are deleted
 * after every committed change and also expire quickly (a concurrent read can re-fill an
 * old value right after a delete, so staleness is bounded by the TTL).
 */
@Component
public class StockCache {

	private static final Logger log = LoggerFactory.getLogger(StockCache.class);

	private final StringRedisTemplate redis;
	private final boolean enabled;
	private final Duration ttl;
	private final Counter hit;
	private final Counter miss;

	StockCache(StringRedisTemplate redis, MeterRegistry registry,
			@Value("${inventory.cache.enabled:true}") boolean enabled,
			@Value("${inventory.cache.ttl-ms:2000}") long ttlMs) {
		this.redis = redis;
		this.enabled = enabled;
		this.ttl = Duration.ofMillis(ttlMs);
		this.hit = Counter.builder("inventory.cache").tag("result", "hit").register(registry);
		this.miss = Counter.builder("inventory.cache").tag("result", "miss").register(registry);
	}

	private static String key(String sku) {
		return "inv:stock:" + sku;
	}

	public Optional<StockView> get(String sku) {
		if (!enabled) {
			return Optional.empty();
		}
		try {
			String v = redis.opsForValue().get(key(sku));
			if (v != null) {
				int i = v.indexOf(':');
				hit.increment();
				return Optional.of(new StockView(sku, Long.parseLong(v.substring(0, i)), Long.parseLong(v.substring(i + 1))));
			}
		}
		catch (RuntimeException ex) {
			log.warn("stock cache read failed, falling back to Oracle: {}", ex.toString());
		}
		miss.increment();
		return Optional.empty();
	}

	public void put(StockView view) {
		if (!enabled) {
			return;
		}
		try {
			redis.opsForValue().set(key(view.sku()), view.available() + ":" + view.reserved(), ttl);
		}
		catch (RuntimeException ex) {
			log.warn("stock cache write failed: {}", ex.toString());
		}
	}

	public void invalidate(String sku) {
		if (!enabled) {
			return;
		}
		try {
			redis.delete(key(sku));
		}
		catch (RuntimeException ex) {
			log.warn("stock cache invalidate failed (entry expires after {}): {}", ttl, ex.toString());
		}
	}
}
