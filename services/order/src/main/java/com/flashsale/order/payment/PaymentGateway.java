package com.flashsale.order.payment;

import java.math.BigDecimal;

/** Port to the (simulated) payment provider. Must be idempotent per order id. */
public interface PaymentGateway {

	/** @throws PaymentDeclinedException when the payment is refused; the same order id always gets the same answer */
	void charge(String orderId, BigDecimal amount);

	class PaymentDeclinedException extends RuntimeException {

		public PaymentDeclinedException(String orderId) {
			super("payment declined for order " + orderId);
		}
	}
}
