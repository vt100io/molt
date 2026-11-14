-- Every order line has a positive quantity.
SELECT *
FROM shop.order_items
WHERE quantity IS NULL OR quantity <= 0
