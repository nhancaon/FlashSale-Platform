package com.flashsale.order.web;

import com.fasterxml.jackson.annotation.JsonInclude;
import com.flashsale.order.domain.ApiErrors.ApiException;

import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

import org.springframework.dao.CannotAcquireLockException;
import org.springframework.dao.DataAccessException;
import org.springframework.dao.DeadlockLoserDataAccessException;
import org.springframework.dao.QueryTimeoutException;
import org.springframework.http.HttpStatus;
import org.springframework.http.ResponseEntity;
import org.springframework.http.converter.HttpMessageNotReadableException;
import org.springframework.jdbc.CannotGetJdbcConnectionException;
import org.springframework.transaction.TransactionException;
import org.springframework.web.bind.annotation.ExceptionHandler;
import org.springframework.web.bind.annotation.RestControllerAdvice;
import org.springframework.web.bind.MissingRequestHeaderException;

/** Maps failures to {"code": "...", "message": "...", "orderId"?: "..."}. */
@RestControllerAdvice
class ApiExceptionHandler {

	private static final Logger log = LoggerFactory.getLogger(ApiExceptionHandler.class);

	@JsonInclude(JsonInclude.Include.NON_NULL)
	record ErrorBody(String code, String message, String orderId) {
	}

	private static ResponseEntity<ErrorBody> body(HttpStatus status, String code, String message) {
		return ResponseEntity.status(status).body(new ErrorBody(code, message, null));
	}

	@ExceptionHandler(ApiException.class)
	ResponseEntity<ErrorBody> api(ApiException ex) {
		ResponseEntity.BodyBuilder response = ResponseEntity.status(ex.status());
		if ("REQUEST_IN_PROGRESS".equals(ex.code())) {
			response.header("Retry-After", "1");
		}
		return response.body(new ErrorBody(ex.code(), ex.getMessage(), ex.orderId()));
	}

	@ExceptionHandler({ HttpMessageNotReadableException.class, MissingRequestHeaderException.class })
	ResponseEntity<ErrorBody> unreadable(Exception ex) {
		return body(HttpStatus.BAD_REQUEST, "INVALID_REQUEST", "body must be valid JSON with known fields");
	}

	@ExceptionHandler({ DeadlockLoserDataAccessException.class, CannotAcquireLockException.class })
	ResponseEntity<ErrorBody> lockConflict(DataAccessException ex) {
		log.warn("lock conflict: {}", ex.getMessage());
		return body(HttpStatus.CONFLICT, "CONFLICT_RETRY", "concurrent update, retry the request");
	}

	@ExceptionHandler({ CannotGetJdbcConnectionException.class, QueryTimeoutException.class, TransactionException.class })
	ResponseEntity<ErrorBody> dbTimeout(RuntimeException ex) {
		log.error("database unavailable or timed out", ex);
		return body(HttpStatus.SERVICE_UNAVAILABLE, "DB_UNAVAILABLE", "database unavailable or timed out");
	}

	@ExceptionHandler(DataAccessException.class)
	ResponseEntity<ErrorBody> database(DataAccessException ex) {
		log.error("database error", ex);
		return body(HttpStatus.INTERNAL_SERVER_ERROR, "INTERNAL", "unexpected database error");
	}
}
