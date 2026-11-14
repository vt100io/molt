-- One row per customer: paid and shipped orders and their revenue.
SELECT
    c.id                                       AS customer_id,
    c.name,
    c.country,
    count(DISTINCT o.id)                       AS orders,
    coalesce(sum(i.quantity * i.unit_price), 0) AS revenue
FROM shop.customers c
LEFT JOIN shop.orders o ON o.customer_id = c.id AND o.status <> 'cancelled'
LEFT JOIN shop.order_items i ON i.order_id = o.id
GROUP BY ALL
