package com.flashsale.order.web;

import java.math.BigDecimal;
import java.time.Instant;
import java.util.List;

import com.fasterxml.jackson.annotation.JsonInclude;
import com.flashsale.order.OrderService;
import com.flashsale.order.OrderService.CreateResult;
import com.flashsale.order.domain.ApiErrors;
import com.flashsale.order.domain.Domain.ItemRequest;
import com.flashsale.order.domain.Domain.Order;
import com.flashsale.order.domain.Domain.OrderStatus;

import io.micrometer.core.instrument.Counter;
import io.micrometer.core.instrument.MeterRegistry;

import org.springframework.http.HttpStatus;
import org.springframework.http.ResponseEntity;
import org.springframework.web.bind.annotation.GetMapping;
import org.springframework.web.bind.annotation.PathVariable;
import org.springframework.web.bind.annotation.PostMapping;
import org.springframework.web.bind.annotation.RequestBody;
import org.springframework.web.bind.annotation.RequestHeader;
import org.springframework.web.bind.annotation.RestController;

/**
 * POST /v1/orders: 201 new confirmed order, 200 replay of an earlier request with the same key, 202 saga not
 * finished (PENDING), 409 {OUT_OF_STOCK, PAYMENT_DECLINED, IDEMPOTENCY_KEY_REUSED, REQUEST_IN_PROGRESS},
 * 503 INVENTORY_UNAVAILABLE. The authenticated user comes from X-User-Id (set by the gateway after JWT checks).
 */
@RestController
class OrderController {

	private final OrderService service;
	private final MeterRegistry registry;

	OrderController(OrderService service, MeterRegistry registry) {
		this.service = service;
		this.registry = registry;
		// Register the series up front so dashboards and alerts see zeros instead of missing metrics.
		for (String result : List.of("confirmed", "failed", "pending", "replay", "request_in_progress",
				"idempotency_key_reused", "invalid_request", "sku_not_found")) {
			Counter.builder("order.requests").tag("result", result).register(registry);
		}
	}

	record ItemBody(String sku, Long qty) {
	}

	record CreateOrderRequest(List<ItemBody> items) {
	}

	record ItemView(String sku, long qty, BigDecimal price) {
	}

	@JsonInclude(JsonInclude.Include.NON_NULL)
	record OrderView(String id, String status, BigDecimal total, Instant createdAt, String failureReason,
			List<ItemView> items) {

		static OrderView of(Order o) {
			return new OrderView(o.id(), o.status().name(), o.total(), o.createdAt(), o.failureReason(),
					o.items().stream().map(i -> new ItemView(i.sku(), i.qty(), i.price())).toList());
		}
	}

	@PostMapping("/v1/orders")
	ResponseEntity<OrderView> create(@RequestHeader(value = "Idempotency-Key", required = false) String idempotencyKey,
			@RequestHeader(value = "X-User-Id", required = false) String userId,
			@RequestBody CreateOrderRequest body) {
		List<ItemRequest> items = body.items() == null ? null : body.items().stream()
				.map(i -> new ItemRequest(i == null ? null : i.sku(), i == null || i.qty() == null ? 0 : i.qty())).toList();
		CreateResult result;
		try {
			result = service.create(userId, idempotencyKey, items);
		}
		catch (ApiErrors.ApiException ex) {
			count(ex.code().toLowerCase());
			throw ex;
		}
		Order order = result.order();
		if (result.replay()) {
			count("replay");
			return ResponseEntity.ok(OrderView.of(order));
		}
		return switch (order.status()) {
			case CONFIRMED -> {
				count("confirmed");
				yield ResponseEntity.status(HttpStatus.CREATED).header("Location", "/v1/orders/" + order.id())
						.body(OrderView.of(order));
			}
			case FAILED -> {
				count("failed");
				throw ApiErrors.failed(order.failureReason(), order.id());
			}
			default -> {
				count("pending");
				yield ResponseEntity.status(HttpStatus.ACCEPTED).header("Location", "/v1/orders/" + order.id())
						.body(OrderView.of(order));
			}
		};
	}

	@GetMapping("/v1/orders/{id}")
	OrderView get(@PathVariable String id, @RequestHeader(value = "X-User-Id", required = false) String userId) {
		Order order = service.find(id).orElseThrow(() -> ApiErrors.orderNotFound(id));
		if (userId != null && !userId.equals(order.userId())) {
			throw ApiErrors.orderNotFound(id); // do not reveal other users' orders
		}
		return OrderView.of(order);
	}

	private void count(String result) {
		Counter.builder("order.requests").tag("result", result).register(registry).increment();
	}
}
