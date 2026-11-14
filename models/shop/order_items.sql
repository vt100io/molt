-- One row per order line, unpacked from orders.items.
-- @kind: view
SELECT
    o.id                                                AS order_id,
    json_extract_string(item, '$.sku')                  AS sku,
    json_extract_string(item, '$.name')                 AS product_name,
    json_extract(item, '$.quantity')::INTEGER           AS quantity,
    json_extract(item, '$.unit_price')::DECIMAL(10, 2)  AS unit_price
FROM shop.orders o,
UNNEST(json_extract(o.items, '$[*]')) AS t(item)
