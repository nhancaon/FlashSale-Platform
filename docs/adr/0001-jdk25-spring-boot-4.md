# ADR 0001: JDK 25 and Spring Boot 4.1

Status: accepted (Phase 1)

## Context
The spec first said Java 21 + Spring Boot 3.x. The project targets the future, so we moved to the
current LTS (JDK 25) and the newest stable Spring Boot offered by start.spring.io for it (4.1.1,
Spring Framework 7, Jackson 3, Tomcat 11).

## Decision
- Java: Eclipse Temurin 25 (LTS). Build: Maven via the Maven Wrapper (`./mvnw`), so no local Maven install.
- Spring Boot 4.1.1. Virtual threads on (`spring.threads.virtual.enabled=true`).
- Go stays on the installed toolchain (1.26).

## Consequences
- Verified in Phase 1 on this stack: Spring Data Redis (Lettuce), Micrometer/Prometheus, structured
  JSON logging, Testcontainers 2.x. Boot 4 moved packages and test support (e.g. `@LocalServerPort`
  is in `org.springframework.boot.test.web.server`; Jackson 3 lives in `tools.jackson`).
- Verified in Phase 2: Oracle JDBC (`ojdbc17`, the driver Initializr picks for JDK 25; the spec said
  `ojdbc11`), HikariCP, Spring JDBC `JdbcClient` and Spring Data Redis work on this stack against Oracle Free.
- Verified in Phase 3: Resilience4j 2.4.0 (core modules: circuit breaker, retry, bulkhead, micrometer) works on JDK 25 /
  Spring Boot 4.1 with Micrometer metrics. A `resilience4j-spring-boot4` module also exists; we configure in code instead.
- Virtual-thread pinning on `synchronized` was largely fixed in JDK 24, so the pinning experiment in
  the concurrency lab must be measured, not assumed (see spec appendix A).
