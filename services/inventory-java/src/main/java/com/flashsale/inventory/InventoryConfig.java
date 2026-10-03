package com.flashsale.inventory;

import com.flashsale.inventory.repository.InventoryRepository;
import com.flashsale.inventory.strategy.StockStrategies;
import com.flashsale.inventory.strategy.StockStrategy;

import org.springframework.beans.factory.annotation.Value;
import org.springframework.context.annotation.Bean;
import org.springframework.context.annotation.Configuration;

@Configuration(proxyBeanMethods = false)
class InventoryConfig {

	/** The strategy is chosen by configuration; an unknown name fails startup. */
	@Bean
	StockStrategy stockStrategy(@Value("${inventory.stock-strategy:atomic}") String name,
			@Value("${inventory.optimistic-max-retries:50}") int maxRetries, InventoryRepository repo) {
		return StockStrategies.create(name, repo, maxRetries);
	}
}
