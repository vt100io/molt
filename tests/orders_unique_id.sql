-- Each order appears once.
SELECT id
FROM shop.orders
GROUP BY id
HAVING count(*) > 1
