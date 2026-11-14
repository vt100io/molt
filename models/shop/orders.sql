-- One row per order. items is a JSON array of order lines.
SELECT id, customer_id, ordered_at, status, items::JSON AS items
FROM read_csv(getvariable('DATA_DIR') || '/orders.csv')
