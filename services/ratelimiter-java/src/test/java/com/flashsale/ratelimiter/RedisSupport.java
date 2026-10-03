package com.flashsale.ratelimiter;

import org.springframework.data.redis.connection.lettuce.LettuceConnectionFactory;
import org.springframework.data.redis.core.StringRedisTemplate;
import org.testcontainers.containers.GenericContainer;
import org.testcontainers.utility.DockerImageName;

/**
 * One Redis per test JVM. TEST_REDIS_ADDR (host:port) reuses an existing Redis,
 * otherwise a throwaway container is started (cleaned up by Testcontainers).
 */
final class RedisSupport {

	static final StringRedisTemplate TEMPLATE;

	static {
		String host;
		int port;
		String addr = System.getenv("TEST_REDIS_ADDR");
		if (addr != null && !addr.isBlank()) {
			String[] parts = addr.split(":");
			host = parts[0];
			port = Integer.parseInt(parts[1]);
		}
		else {
			GenericContainer<?> container = new GenericContainer<>(DockerImageName.parse("redis:7-alpine"))
					.withExposedPorts(6379);
			container.start();
			host = container.getHost();
			port = container.getMappedPort(6379);
		}
		LettuceConnectionFactory factory = new LettuceConnectionFactory(host, port);
		factory.afterPropertiesSet();
		TEMPLATE = new StringRedisTemplate(factory);
		TEMPLATE.afterPropertiesSet();
	}

	private RedisSupport() {
	}
}
