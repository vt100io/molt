-- One row per customer.
SELECT * FROM read_csv(getvariable('DATA_DIR') || '/customers.csv')
