package com.flashsale.inventory;

import static org.assertj.core.api.Assertions.assertThat;
import static org.assertj.core.api.Assertions.assertThatThrownBy;
import static org.mockito.ArgumentMatchers.anyLong;
import static org.mockito.Mockito.inOrder;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.never;
import static org.mockito.Mockito.times;
import static org.mockito.Mockito.verify;
import static org.mockito.Mockito.when;

import com.flashsale.inventory.domain.DomainErrors;
import com.flashsale.inventory.domain.Records.StockRow;
import com.flashsale.inventory.repository.InventoryRepository;
import com.flashsale.inventory.strategy.StockStrategies;
import com.flashsale.inventory.strategy.StockStrategy;

import org.junit.jupiter.api.Test;

class StockStrategiesTests {

	private final InventoryRepository repo = mock(InventoryRepository.class);

	@Test
	void factoryBuildsEachStrategyAndRejectsUnknownNames() {
		for (String name : new String[] { "atomic", "pessimistic", "optimistic" }) {
			assertThat(StockStrategies.create(name, repo, 3).name()).isEqualTo(name);
		}
		assertThatThrownBy(() -> StockStrategies.create("magic", repo, 3)).hasMessageContaining("magic");
	}

	@Test
	void atomicIsASingleConditionalUpdate() {
		when(repo.decrementIfAvailable(1L, 2)).thenReturn(true);
		StockStrategy s = StockStrategies.create("atomic", repo, 3);

		assertThat(s.tryReserve(1L, 2, "SKU")).isTrue();
		verify(repo).decrementIfAvailable(1L, 2);
		verify(repo, never()).lockStock(anyLong());
	}

	@Test
	void pessimisticLocksBeforeUpdatingAndSkipsTheUpdateWhenShort() {
		when(repo.lockStock(1L)).thenReturn(new StockRow(5, 0));
		StockStrategy s = StockStrategies.create("pessimistic", repo, 3);
		assertThat(s.tryReserve(1L, 2, "SKU")).isTrue();
		var order = inOrder(repo);
		order.verify(repo).lockStock(1L);
		order.verify(repo).decrement(1L, 2);

		when(repo.lockStock(2L)).thenReturn(new StockRow(1, 0));
		assertThat(s.tryReserve(2L, 2, "SKU")).isFalse();
		verify(repo, never()).decrement(2L, 2);
	}

	@Test
	void optimisticRetriesAfterLosingTheRace() {
		when(repo.readStock(1L)).thenReturn(new StockRow(5, 7));
		when(repo.decrementIfVersion(1L, 1, 7)).thenReturn(false, false, true);
		StockStrategy s = StockStrategies.create("optimistic", repo, 5);

		assertThat(s.tryReserve(1L, 1, "SKU")).isTrue();
		verify(repo, times(3)).readStock(1L);
		verify(repo, times(3)).decrementIfVersion(1L, 1, 7);
	}

	@Test
	void optimisticGivesUpWithContention() {
		when(repo.readStock(1L)).thenReturn(new StockRow(5, 7));
		when(repo.decrementIfVersion(1L, 1, 7)).thenReturn(false);
		StockStrategy s = StockStrategies.create("optimistic", repo, 2);

		assertThatThrownBy(() -> s.tryReserve(1L, 1, "SKU-X")).isInstanceOf(DomainErrors.Contention.class);
		verify(repo, times(3)).decrementIfVersion(1L, 1, 7); // first attempt + 2 retries
	}

	@Test
	void optimisticReportsOutOfStockWithoutUpdating() {
		when(repo.readStock(1L)).thenReturn(new StockRow(0, 3));
		StockStrategy s = StockStrategies.create("optimistic", repo, 2);

		assertThat(s.tryReserve(1L, 1, "SKU")).isFalse();
		verify(repo, never()).decrementIfVersion(anyLong(), anyLong(), anyLong());
	}
}
