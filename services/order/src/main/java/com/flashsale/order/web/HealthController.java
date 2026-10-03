package com.flashsale.order.web;

import java.util.Map;

import javax.sql.DataSource;

import org.springframework.http.HttpStatus;
import org.springframework.http.ResponseEntity;
import org.springframework.web.bind.annotation.GetMapping;
import org.springframework.web.bind.annotation.RestController;

@RestController
class HealthController {

	private final DataSource dataSource;

	HealthController(DataSource dataSource) {
		this.dataSource = dataSource;
	}

	@GetMapping("/healthz")
	Map<String, String> healthz() {
		return Map.of("status", "ok");
	}

	/** Ready only when Oracle answers; Redis is optional (cache-aside). */
	@GetMapping("/readyz")
	ResponseEntity<?> readyz() {
		try (var c = dataSource.getConnection(); var st = c.createStatement(); var rs = st.executeQuery("SELECT 1 FROM dual")) {
			rs.next();
			return ResponseEntity.ok(Map.of("status", "ready"));
		}
		catch (Exception ex) {
			return ResponseEntity.status(HttpStatus.SERVICE_UNAVAILABLE)
					.body(new ApiExceptionHandler.ErrorBody("NOT_READY", "oracle unreachable", null));
		}
	}
}
