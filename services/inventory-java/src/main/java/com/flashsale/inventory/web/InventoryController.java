package com.flashsale.inventory.web;

import java.util.Map;

import com.flashsale.inventory.domain.DomainErrors;
import com.flashsale.inventory.domain.Records.StockView;
import com.flashsale.inventory.service.InventoryService;

import io.micrometer.core.instrument.Counter;
import io.micrometer.core.instrument.MeterRegistry;

import org.springframework.web.bind.annotation.GetMapping;
import org.springframework.web.bind.annotation.PathVariable;
import org.springframework.web.bind.annotation.PostMapping;
import org.springframework.web.bind.annotation.RequestBody;
import org.springframework.web.bind.annotation.RestController;

@RestController
class InventoryController {

	private static final int MAX_ID_LENGTH = 64;
	private static final long MAX_QTY = 1000;

	private final InventoryService service;
	private final MeterRegistry registry;

	InventoryController(InventoryService service, MeterRegistry registry) {
		this.service = service;
		this.registry = registry;
	}

	record ReserveRequest(String orderId, String sku, Long qty) {
	}

	record OrderSkuRequest(String orderId, String sku) {
	}

	record StockResponse(String sku, long available, long reserved) {
	}

	@GetMapping("/v1/inventory/{sku}")
	StockResponse get(@PathVariable String sku) {
		StockView v = service.getStock(sku);
		return new StockResponse(v.sku(), v.available(), v.reserved());
	}

	@PostMapping("/v1/inventory/reserve")
	Map<String, String> reserve(@RequestBody ReserveRequest r) {
		requireId("orderId", r.orderId());
		requireId("sku", r.sku());
		if (r.qty() == null || r.qty() < 1 || r.qty() > MAX_QTY) {
			throw new DomainErrors.InvalidRequest("qty must be between 1 and " + MAX_QTY);
		}
		String result = "ok";
		try {
			return Map.of("reservationId", service.reserve(r.orderId(), r.sku(), r.qty()));
		}
		catch (DomainErrors.OutOfStock ex) {
			result = "out_of_stock";
			throw ex;
		}
		catch (RuntimeException ex) {
			result = "error";
			throw ex;
		}
		finally {
			// Same metric name and labels as the Go service: inventory_reserve_total{result}.
			Counter.builder("inventory.reserve").tag("result", result).register(registry).increment();
		}
	}

	@PostMapping("/v1/inventory/release")
	Map<String, String> release(@RequestBody OrderSkuRequest r) {
		requireId("orderId", r.orderId());
		requireId("sku", r.sku());
		return Map.of("status", service.release(r.orderId(), r.sku()));
	}

	@PostMapping("/v1/inventory/confirm")
	Map<String, String> confirm(@RequestBody OrderSkuRequest r) {
		requireId("orderId", r.orderId());
		requireId("sku", r.sku());
		return Map.of("status", service.confirm(r.orderId(), r.sku()));
	}

	private static void requireId(String field, String value) {
		if (value == null || value.isEmpty()) {
			throw new DomainErrors.InvalidRequest(field + " is required");
		}
		if (value.length() > MAX_ID_LENGTH) {
			throw new DomainErrors.InvalidRequest(field + " is too long");
		}
	}
}
