package com.flashsale.order.saga;

import java.util.ArrayList;
import java.util.List;

import com.flashsale.order.domain.Domain.Order;
import com.flashsale.order.domain.Domain.OrderItem;
import com.flashsale.order.domain.Domain.OrderStatus;
import com.flashsale.order.inventory.InventoryGateway;
import com.flashsale.order.inventory.InventoryGateway.OutOfStockException;
import com.flashsale.order.inventory.InventoryGateway.RejectedException;
import com.flashsale.order.inventory.InventoryGateway.UnavailableException;
import com.flashsale.order.payment.PaymentGateway;
import com.flashsale.order.payment.PaymentGateway.PaymentDeclinedException;
import com.flashsale.order.repository.OrderStore;

import io.micrometer.core.instrument.MeterRegistry;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

/**
 * Orchestration saga: reserve every item, charge, confirm every item.
 *
 * <ul>
 * <li>Before the payment succeeded, any failure is compensated by releasing what was reserved, and the order
 * becomes FAILED (all inventory calls are idempotent).</li>
 * <li>Payment is the pivot: once it succeeded there is no way back, so a later failure (inventory down while
 * confirming) leaves the order PENDING. The saga resumes forward when the client retries the same
 * Idempotency-Key, and every step is safe to repeat.</li>
 * <li>No database connection is held while calling other services: the order row is saved first, the saga
 * runs, then the result is saved in a second short transaction together with the outbox event.</li>
 * </ul>
 */
public class OrderSaga {

	private static final Logger log = LoggerFactory.getLogger(OrderSaga.class);

	private final InventoryGateway inventory;
	private final PaymentGateway payment;
	private final OrderStore store;
	private final MeterRegistry metrics;

	public OrderSaga(InventoryGateway inventory, PaymentGateway payment, OrderStore store, MeterRegistry metrics) {
		this.inventory = inventory;
		this.payment = payment;
		this.store = store;
		this.metrics = metrics;
	}

	/** Runs the saga for a PENDING order and returns it in its final (or still PENDING) state. */
	public Order run(Order order) {
		List<OrderItem> reserved = new ArrayList<>();
		boolean paid = false;
		try {
			for (OrderItem item : order.items()) { // items are sorted by sku: a stable order for every order
				inventory.reserve(order.id(), item.sku(), item.qty());
				reserved.add(item);
			}
			payment.charge(order.id(), order.total());
			paid = true;
			for (OrderItem item : order.items()) {
				inventory.confirm(order.id(), item.sku());
			}
			return store.finalizeOrder(order, OrderStatus.CONFIRMED, null);
		}
		catch (OutOfStockException ex) {
			return compensate(order, reserved, "OUT_OF_STOCK");
		}
		catch (RejectedException ex) {
			log.error("inventory rejected the request for order {}: {} {}", order.id(), ex.status(), ex.code());
			return compensate(order, reserved, "INVENTORY_REJECTED");
		}
		catch (PaymentDeclinedException ex) {
			return compensate(order, reserved, "PAYMENT_DECLINED");
		}
		catch (UnavailableException ex) {
			if (paid) {
				// Money is taken; releasing would be wrong. Leave PENDING so a retry resumes forward.
				metrics.counter("order.saga.stuck").increment();
				log.error("order {} paid but inventory confirm failed, left PENDING: {}", order.id(), ex.getMessage());
				return order;
			}
			return compensate(order, reserved, "INVENTORY_UNAVAILABLE");
		}
	}

	/** Releases what was reserved. If a release itself fails the order stays PENDING and a retry finishes the job. */
	private Order compensate(Order order, List<OrderItem> reserved, String reason) {
		for (OrderItem item : reserved) {
			try {
				inventory.release(order.id(), item.sku());
			}
			catch (RuntimeException ex) {
				metrics.counter("order.saga.compensation.failures").increment();
				log.error("order {} could not release {}: {}. Left PENDING for a retry.", order.id(), item.sku(), ex.toString());
				return order;
			}
		}
		return store.finalizeOrder(order, OrderStatus.FAILED, reason);
	}
}
