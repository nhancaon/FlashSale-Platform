package com.flashsale.ratelimiter.web;

import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

import org.springframework.dao.DataAccessException;
import org.springframework.http.HttpStatus;
import org.springframework.http.ResponseEntity;
import org.springframework.http.converter.HttpMessageNotReadableException;
import org.springframework.web.bind.annotation.ExceptionHandler;
import org.springframework.web.bind.annotation.RestControllerAdvice;

/** Maps failures to the shared error shape {"code": "...", "message": "..."}. */
@RestControllerAdvice
class ApiExceptionHandler {

	private static final Logger log = LoggerFactory.getLogger(ApiExceptionHandler.class);

	record ErrorBody(String code, String message) {
	}

	@ExceptionHandler(InvalidRequestException.class)
	ResponseEntity<ErrorBody> invalid(InvalidRequestException ex) {
		return ResponseEntity.badRequest().body(new ErrorBody("INVALID_REQUEST", ex.getMessage()));
	}

	@ExceptionHandler(HttpMessageNotReadableException.class)
	ResponseEntity<ErrorBody> unreadable(HttpMessageNotReadableException ex) {
		return ResponseEntity.badRequest().body(new ErrorBody("INVALID_REQUEST", "body must be valid JSON"));
	}

	@ExceptionHandler(DataAccessException.class)
	ResponseEntity<ErrorBody> backend(DataAccessException ex) {
		log.error("limiter store failure", ex);
		return ResponseEntity.status(HttpStatus.SERVICE_UNAVAILABLE)
				.body(new ErrorBody("BACKEND_UNAVAILABLE", "rate limit store unavailable"));
	}
}
