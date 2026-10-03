package com.flashsale.inventory.web;

import java.sql.SQLException;

import com.flashsale.inventory.domain.DomainErrors.DomainException;

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

/**
 * Maps failures to {"code": "...", "message": "..."}. Oracle errors become meaningful codes:
 * ORA-00060 (deadlock) and lock timeouts are retryable conflicts, pool or query timeouts are 503.
 * ORA-00001 (unique violation) is handled in the service because it means "replay" there.
 */
@RestControllerAdvice
class ApiExceptionHandler {

	private static final Logger log = LoggerFactory.getLogger(ApiExceptionHandler.class);

	record ErrorBody(String code, String message) {
	}

	private static ResponseEntity<ErrorBody> body(HttpStatus status, String code, String message) {
		return ResponseEntity.status(status).body(new ErrorBody(code, message));
	}

	@ExceptionHandler(DomainException.class)
	ResponseEntity<ErrorBody> domain(DomainException ex) {
		return body(ex.status(), ex.code(), ex.getMessage());
	}

	@ExceptionHandler(HttpMessageNotReadableException.class)
	ResponseEntity<ErrorBody> unreadable(HttpMessageNotReadableException ex) {
		return body(HttpStatus.BAD_REQUEST, "INVALID_REQUEST", "body must be valid JSON with known fields");
	}

	@ExceptionHandler({ DeadlockLoserDataAccessException.class, CannotAcquireLockException.class })
	ResponseEntity<ErrorBody> lockConflict(DataAccessException ex) {
		log.warn("lock conflict (ORA-00060/00054): {}", ex.getMessage());
		return body(HttpStatus.CONFLICT, "CONFLICT_RETRY", "concurrent update, retry the request");
	}

	@ExceptionHandler({ CannotGetJdbcConnectionException.class, QueryTimeoutException.class, TransactionException.class })
	ResponseEntity<ErrorBody> dbTimeout(RuntimeException ex) {
		log.error("database unavailable or timed out", ex);
		return body(HttpStatus.SERVICE_UNAVAILABLE, "DB_UNAVAILABLE", "database unavailable or timed out");
	}

	@ExceptionHandler(DataAccessException.class)
	ResponseEntity<ErrorBody> database(DataAccessException ex) {
		Throwable root = ex.getMostSpecificCause();
		log.error("database error {}", root instanceof SQLException sql ? "ORA/SQL code " + sql.getErrorCode() : "", ex);
		return body(HttpStatus.INTERNAL_SERVER_ERROR, "INTERNAL", "unexpected database error");
	}
}
