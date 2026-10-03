package com.flashsale.order;

import java.nio.file.Files;
import java.nio.file.Path;
import java.sql.Connection;
import java.sql.DriverManager;
import java.sql.Statement;
import java.util.List;
import java.util.stream.Stream;

import org.testcontainers.oracle.OracleContainer;

/**
 * One Oracle per test JVM. With DB_PASSWORD set (make up + db-migrate) it reuses the compose Oracle on localhost;
 * otherwise it starts a throwaway Oracle Free container and applies db/migrations itself (CI).
 */
final class OracleSupport {

	static final String URL;
	static final String USER;
	static final String PASSWORD;

	static {
		String password = System.getenv("DB_PASSWORD");
		if (password != null && !password.isBlank()) {
			URL = "jdbc:oracle:thin:@//%s:%s/%s".formatted(env("DB_HOST", "localhost"), env("DB_PORT", "1521"),
					env("DB_SERVICE", "FREEPDB1"));
			USER = env("DB_USER", "flashsale");
			PASSWORD = password;
		}
		else {
			OracleContainer oracle = new OracleContainer("gvenzl/oracle-free:slim").withUsername("flashsale")
					.withPassword("test_password");
			oracle.start();
			URL = oracle.getJdbcUrl();
			USER = "flashsale";
			PASSWORD = "test_password";
			applyMigrations();
		}
	}

	private static String env(String key, String def) {
		String v = System.getenv(key);
		return v == null || v.isBlank() ? def : v;
	}

	/** Runs db/migrations/V*.sql (relative to services/order) in name order; statements end with ';'. */
	private static void applyMigrations() {
		Path dir = Path.of("..", "..", "db", "migrations");
		try (Stream<Path> files = Files.list(dir); Connection c = DriverManager.getConnection(URL, USER, PASSWORD)) {
			List<Path> sorted = files.filter(p -> p.getFileName().toString().matches("V\\d+__.*\\.sql")).sorted().toList();
			for (Path file : sorted) {
				StringBuilder statement = new StringBuilder();
				for (String line : Files.readAllLines(file)) {
					if (line.strip().startsWith("--")) {
						continue;
					}
					statement.append(line).append('\n');
					if (line.strip().endsWith(";")) {
						String sql = statement.toString().strip();
						statement.setLength(0);
						try (Statement st = c.createStatement()) {
							st.execute(sql.substring(0, sql.length() - 1));
						}
					}
				}
			}
		}
		catch (Exception ex) {
			throw new IllegalStateException("could not apply migrations", ex);
		}
	}

	private OracleSupport() {
	}
}
