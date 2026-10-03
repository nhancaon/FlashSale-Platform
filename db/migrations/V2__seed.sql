-- V2: dữ liệu mẫu cho dev. Tồn kho nhỏ để dễ kiểm tra oversell.

INSERT INTO product (sku, name, price) VALUES ('SKU-IPHONE', 'Flash Sale Phone', 9990000);
INSERT INTO product (sku, name, price) VALUES ('SKU-LAPTOP', 'Flash Sale Laptop', 15990000);
INSERT INTO product (sku, name, price) VALUES ('SKU-HEADSET', 'Flash Sale Headset', 490000);

INSERT INTO stock (product_id, available) SELECT id, 100  FROM product WHERE sku = 'SKU-IPHONE';
INSERT INTO stock (product_id, available) SELECT id, 50   FROM product WHERE sku = 'SKU-LAPTOP';
INSERT INTO stock (product_id, available) SELECT id, 1000 FROM product WHERE sku = 'SKU-HEADSET';

COMMIT;
