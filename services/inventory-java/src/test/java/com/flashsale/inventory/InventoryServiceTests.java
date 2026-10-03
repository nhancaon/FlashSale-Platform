package com.flashsale.inventory;

import static org.assertj.core.api.Assertions.assertThat;
import static org.assertj.core.api.Assertions.assertThatThrownBy;
import static org.mockito.ArgumentMatchers.any;
import static org.mockito.ArgumentMatchers.anyLong;
import static org.mockito.ArgumentMatchers.anyString;
import static org.mockito.ArgumentMatchers.eq;
import static org.mockito.Mockito.doThrow;
import static org.mockito.Mockito.lenient;
import static org.mockito.Mockito.never;
import static org.mockito.Mockito.verify;
import static org.mockito.Mockito.when;

import java.util.Optional;

import com.flashsale.inventory.cache.StockCache;
import com.flashsale.inventory.domain.DomainErrors;
import com.flashsale.inventory.domain.Records.Reservation;
import com.flashsale.inventory.repository.InventoryRepository;
import com.flashsale.inventory.service.InventoryService;
import com.flashsale.inventory.strategy.StockStrategy;

import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.extension.ExtendWith;
import org.mockito.Mock;
import org.mockito.junit.jupiter.MockitoExtension;
import org.springframework.dao.DuplicateKeyException;
import org.springframework.transaction.support.TransactionCallback;
import org.springframework.transaction.support.TransactionTemplate;

/** Service logic with a mocked repository: the Oracle behaviour is covered by the contract tests. */
@ExtendWith(MockitoExtension.class)
class InventoryServiceTests {

	@Mock
	InventoryRepository repo;

	@Mock
	StockStrategy strategy;

	@Mock
	StockCache cache;

	@Mock
	TransactionTemplate tx;

	InventoryService service;

	@BeforeEach
	@SuppressWarnings("unchecked")
	void setUp() {
		service = new InventoryService(repo, strategy, cache, tx);
		// Run the transactional callback inline; exceptions propagate like a rollback.
		lenient().when(tx.execute(any())).thenAnswer(
				inv -> ((TransactionCallback<Object>) inv.getArgument(0)).doInTransaction(null));
		lenient().when(repo.findProductId("SKU")).thenReturn(Optional.of(7L));
	}

	@Test
	void reserveStoresReservationThenDecrementsAndInvalidatesCache() {
		when(repo.findReservation("o1", 7L)).thenReturn(Optional.empty());
		when(strategy.tryReserve(7L, 2, "SKU")).thenReturn(true);

		String id = service.reserve("o1", "SKU", 2);

		assertThat(id).isNotBlank();
		verify(repo).insertReservation(eq(id), eq("o1"), eq(7L), eq(2L));
		verify(cache).invalidate("SKU");
	}

	@Test
	void outOfStockPropagatesAndDoesNotTouchTheCache() {
		when(repo.findReservation("o1", 7L)).thenReturn(Optional.empty());
		when(strategy.tryReserve(7L, 5, "SKU")).thenReturn(false);

		assertThatThrownBy(() -> service.reserve("o1", "SKU", 5)).isInstanceOf(DomainErrors.OutOfStock.class);
		verify(cache, never()).invalidate(anyString());
	}

	@Test
	void unknownSku() {
		when(repo.findProductId("NOPE")).thenReturn(Optional.empty());
		assertThatThrownBy(() -> service.reserve("o1", "NOPE", 1)).isInstanceOf(DomainErrors.SkuNotFound.class);
	}

	@Test
	void replayReturnsTheOriginalReservationWithoutTouchingStock() {
		when(repo.findReservation("o1", 7L)).thenReturn(Optional.of(new Reservation("r-1", 7L, 2, "RESERVED")));

		assertThat(service.reserve("o1", "SKU", 2)).isEqualTo("r-1");
		verify(strategy, never()).tryReserve(anyLong(), anyLong(), anyString());
	}

	@Test
	void replayWithDifferentQuantityOrReleasedStateIsRejected() {
		when(repo.findReservation("o1", 7L)).thenReturn(Optional.of(new Reservation("r-1", 7L, 2, "RESERVED")));
		assertThatThrownBy(() -> service.reserve("o1", "SKU", 3)).isInstanceOf(DomainErrors.IdempotencyConflict.class);

		when(repo.findReservation("o2", 7L)).thenReturn(Optional.of(new Reservation("r-2", 7L, 2, "RELEASED")));
		assertThatThrownBy(() -> service.reserve("o2", "SKU", 2)).isInstanceOf(DomainErrors.InvalidState.class);
	}

	@Test
	void losingTheUniqueConstraintRaceReturnsTheWinnersReservation() {
		when(repo.findReservation("o1", 7L)).thenReturn(Optional.empty())
				.thenReturn(Optional.of(new Reservation("winner", 7L, 2, "RESERVED")));
		doThrow(new DuplicateKeyException("ORA-00001")).when(repo)
				.insertReservation(anyString(), eq("o1"), eq(7L), eq(2L));

		assertThat(service.reserve("o1", "SKU", 2)).isEqualTo("winner");
	}

	@Test
	void releaseRestocksOnceAndIsANoOpWhenUnknown() {
		when(repo.findReservation("o1", 7L)).thenReturn(Optional.of(new Reservation("r-1", 7L, 4, "RESERVED")));
		when(repo.transitionFromReserved("r-1", "RELEASED")).thenReturn(true);
		assertThat(service.release("o1", "SKU")).isEqualTo("RELEASED");
		verify(repo).restock(7L, 4);

		when(repo.findReservation("ghost", 7L)).thenReturn(Optional.empty());
		assertThat(service.release("ghost", "SKU")).isEqualTo("NOT_FOUND");
	}

	@Test
	void releaseOfConfirmedReservationIsInvalid() {
		when(repo.findReservation("o1", 7L)).thenReturn(Optional.of(new Reservation("r-1", 7L, 4, "CONFIRMED")));
		assertThatThrownBy(() -> service.release("o1", "SKU")).isInstanceOf(DomainErrors.InvalidState.class);
		verify(repo, never()).restock(anyLong(), anyLong());
	}

	@Test
	void releaseLosingARaceAgainstConfirmIsInvalid() {
		when(repo.findReservation("o1", 7L))
				.thenReturn(Optional.of(new Reservation("r-1", 7L, 4, "RESERVED")))
				.thenReturn(Optional.of(new Reservation("r-1", 7L, 4, "CONFIRMED")));
		when(repo.transitionFromReserved("r-1", "RELEASED")).thenReturn(false);

		assertThatThrownBy(() -> service.release("o1", "SKU")).isInstanceOf(DomainErrors.InvalidState.class);
		verify(repo, never()).restock(anyLong(), anyLong());
	}

	@Test
	void confirmMarksSoldOnceAndRejectsReleased() {
		when(repo.findReservation("o1", 7L)).thenReturn(Optional.of(new Reservation("r-1", 7L, 2, "RESERVED")));
		when(repo.transitionFromReserved("r-1", "CONFIRMED")).thenReturn(true);
		assertThat(service.confirm("o1", "SKU")).isEqualTo("CONFIRMED");
		verify(repo).markSold(7L, 2);

		when(repo.findReservation("o2", 7L)).thenReturn(Optional.of(new Reservation("r-2", 7L, 2, "RELEASED")));
		assertThatThrownBy(() -> service.confirm("o2", "SKU")).isInstanceOf(DomainErrors.InvalidState.class);

		when(repo.findReservation("ghost", 7L)).thenReturn(Optional.empty());
		assertThatThrownBy(() -> service.confirm("ghost", "SKU")).isInstanceOf(DomainErrors.ReservationNotFound.class);
	}

	@Test
	void confirmTwiceDoesNotMarkSoldTwice() {
		when(repo.findReservation("o1", 7L)).thenReturn(Optional.of(new Reservation("r-1", 7L, 2, "CONFIRMED")));
		when(repo.transitionFromReserved("r-1", "CONFIRMED")).thenReturn(false);

		assertThat(service.confirm("o1", "SKU")).isEqualTo("CONFIRMED");
		verify(repo, never()).markSold(anyLong(), anyLong());
	}
}
