package com.flashsale.ratelimiter.web;

class InvalidRequestException extends RuntimeException {

	InvalidRequestException(String message) {
		super(message);
	}
}
