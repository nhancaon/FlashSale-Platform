package com.flashsale.inventory.web;

import java.io.IOException;
import java.util.UUID;

import jakarta.servlet.FilterChain;
import jakarta.servlet.ServletException;
import jakarta.servlet.http.HttpServletRequest;
import jakarta.servlet.http.HttpServletResponse;

import org.slf4j.MDC;

import org.springframework.core.Ordered;
import org.springframework.core.annotation.Order;
import org.springframework.stereotype.Component;
import org.springframework.web.filter.OncePerRequestFilter;

/** Echoes or generates X-Request-Id and exposes it to logs as MDC key request_id. */
@Component
@Order(Ordered.HIGHEST_PRECEDENCE)
class RequestIdFilter extends OncePerRequestFilter {

	static final String HEADER = "X-Request-Id";

	@Override
	protected void doFilterInternal(HttpServletRequest request, HttpServletResponse response, FilterChain chain)
			throws ServletException, IOException {
		String id = request.getHeader(HEADER);
		if (id == null || id.isBlank()) {
			id = UUID.randomUUID().toString().replace("-", "").substring(0, 16);
		}
		response.setHeader(HEADER, id);
		MDC.put("request_id", id);
		try {
			chain.doFilter(request, response);
		}
		finally {
			MDC.remove("request_id");
		}
	}
}
