package com.flashsale.order;

import java.math.BigDecimal;
import java.util.ArrayList;
import java.util.Comparator;
import java.util.HashSet;
import java.util.List;
import java.util.Optional;
import java.util.Set;
import java.util.UUID;

import com.flashsale.order.domain.ApiErrors;
import com.flashsale.order.domain.Domain.ItemRequest;
import com.flashsale.order.domain.Domain.Order;
import com.flashsale.order.domain.Domain.OrderItem;
import com.flashsale.order.domain.Domain.OrderStatus;
import com.flashsale.order.domain.Domain.Product;
import com.flashsale.order.repository.OrderRepository;
import com.flashsale.order.repository.OrderStore;
import com.flashsale.order.saga.OrderSaga;

import org.springframework.beans.factory.annotation.Value;
import org.springframework.dao.DuplicateKeyException;
import org.springframework.stereotype.Service;

@Service
public class OrderService {

	static final int MAX_ITEMS = 20;
	static final long MAX_QTY = 1000;
	static final int MAX_ID_LENGTH = 128;

	/** @param replay true when the answer is the stored result of an earlier request with the same key */
	public record CreateResult(Order order, boolean replay) {
	}

	private final OrderRepository repo;
	private final OrderStore store;
	private final OrderSaga saga;
	private final long staleAfterSeconds;

	OrderService(OrderRepository repo, OrderStore store, OrderSaga saga,
			@Value("${order.stale-after-ms:30000}") long staleAfterMs) {
		this.repo = repo;
		this.store = store;
		this.saga = saga;
		this.staleAfterSeconds = Math.max(1, staleAfterMs / 1000);
	}

	public CreateResult create(String userId, String idempotencyKey, List<ItemRequest> requested) {
		List<ItemRequest> items = normalize(userId, idempotencyKey, requested);

		Optional<Order> existing = repo.findByUserAndKey(userId, idempotencyKey);
		if (existing.isPresent()) {
			return replay(existing.get(), items);
		}

		List<OrderItem> orderItems = new ArrayList<>();
		BigDecimal total = BigDecimal.ZERO;
		for (ItemRequest item : items) {
			Product p = repo.findProduct(item.sku()).orElseThrow(() -> ApiErrors.unknownSku(item.sku()));
			orderItems.add(new OrderItem(p.sku(), p.id(), item.qty(), p.price()));
			total = total.add(p.price().multiply(BigDecimal.valueOf(item.qty())));
		}
		Order order = new Order(UUID.randomUUID().toString(), userId, OrderStatus.PENDING, total, idempotencyKey,
				null, null, orderItems);
		try {
			store.createPending(order);
		}
		catch (DuplicateKeyException race) {
			// A concurrent request with the same key inserted first (ORA-00001 on uq_order_idem).
			Order winner = repo.findByUserAndKey(userId, idempotencyKey).orElseThrow(() -> race);
			return replay(winner, items);
		}
		return new CreateResult(saga.run(order), false);
	}

	public Optional<Order> find(String orderId) {
		return repo.findById(orderId);
	}

	private CreateResult replay(Order existing, List<ItemRequest> requested) {
		if (!sameItems(existing, requested)) {
			throw ApiErrors.keyReused();
		}
		if (existing.status() != OrderStatus.PENDING) {
			return new CreateResult(existing, true);
		}
		if (!store.isStale(existing.id(), staleAfterSeconds)) {
			throw ApiErrors.inProgress(existing.id());
		}
		// Abandoned: the first request died or the saga was left half done. Resume it (all steps are idempotent).
		return new CreateResult(saga.run(existing), true);
	}

	private static boolean sameItems(Order existing, List<ItemRequest> requested) {
		if (existing.items().size() != requested.size()) {
			return false;
		}
		for (int i = 0; i < requested.size(); i++) {
			OrderItem a = existing.items().get(i);
			ItemRequest b = requested.get(i);
			if (!a.sku().equals(b.sku()) || a.qty() != b.qty()) {
				return false;
			}
		}
		return true;
	}

	/** Validates the request and sorts items by sku, the order used everywhere (storage, saga, comparison). */
	private static List<ItemRequest> normalize(String userId, String key, List<ItemRequest> items) {
		if (userId == null || userId.isBlank()) {
			throw ApiErrors.invalid("X-User-Id header is required");
		}
		if (key == null || key.isBlank()) {
			throw ApiErrors.invalid("Idempotency-Key header is required");
		}
		if (userId.length() > 64 || key.length() > MAX_ID_LENGTH) {
			throw ApiErrors.invalid("X-User-Id (max 64) or Idempotency-Key (max 128) is too long");
		}
		if (items == null || items.isEmpty() || items.size() > MAX_ITEMS) {
			throw ApiErrors.invalid("items must contain between 1 and " + MAX_ITEMS + " entries");
		}
		Set<String> seen = new HashSet<>();
		for (ItemRequest item : items) {
			if (item == null || item.sku() == null || item.sku().isEmpty() || item.sku().length() > 64) {
				throw ApiErrors.invalid("each item needs a sku (max 64 characters)");
			}
			if (item.qty() < 1 || item.qty() > MAX_QTY) {
				throw ApiErrors.invalid("qty must be between 1 and " + MAX_QTY);
			}
			if (!seen.add(item.sku())) {
				throw ApiErrors.invalid("duplicate sku " + item.sku());
			}
		}
		return items.stream().sorted(Comparator.comparing(ItemRequest::sku)).toList();
	}
}
