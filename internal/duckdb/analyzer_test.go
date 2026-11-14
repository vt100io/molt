package duckdb

import (
	"reflect"
	"testing"
)

func TestAnalyzeDependencies(t *testing.T) {
	tests := []struct {
		name    string
		sql     string
		models  []string
		want    []string
		wantErr bool
	}{
		{
			name:   "simple dependency",
			sql:    "SELECT * FROM users",
			models: []string{"main.users", "main.orders"},
			want:   []string{"main.users"},
		},
		{
			name:   "multiple dependencies, sorted",
			sql:    "SELECT * FROM users JOIN orders ON users.id = orders.user_id",
			models: []string{"main.users", "main.orders", "main.products"},
			want:   []string{"main.orders", "main.users"},
		},
		{
			name:   "no dependencies",
			sql:    "SELECT 1",
			models: []string{"main.users"},
			want:   nil,
		},
		{
			name:   "external tables are ignored",
			sql:    "SELECT * FROM raw_data JOIN users USING (id)",
			models: []string{"main.users"},
			want:   []string{"main.users"},
		},
		{
			// Previously required -- @depends: because the dummy tables lacked these columns.
			name: "arbitrary columns and JSON extraction",
			sql: `SELECT o.id AS order_id, c.name,
			        json_extract_string(o.items, '$[0].sku') AS first_sku
			      FROM orders o LEFT JOIN customers c ON c.id = o.customer_id`,
			models: []string{"main.orders", "main.customers", "main.events"},
			want:   []string{"main.customers", "main.orders"},
		},
		{
			name:   "unnest over a model column",
			sql:    "SELECT o.id, unnest(from_json(o.policies, '[\"json\"]')) AS policy FROM offers o",
			models: []string{"main.offers"},
			want:   []string{"main.offers"},
		},
		{
			name:   "CTE name is not a dependency",
			sql:    "WITH recent AS (SELECT * FROM orders) SELECT * FROM recent",
			models: []string{"main.orders", "main.recent"},
			want:   []string{"main.orders"},
		},
		{
			name:   "CTE shadowing a model still depends on it inside its own body",
			sql:    "WITH orders AS (SELECT * FROM orders WHERE amount > 0) SELECT * FROM orders",
			models: []string{"main.orders"},
			want:   []string{"main.orders"},
		},
		{
			name:   "later CTE referencing earlier CTE",
			sql:    "WITH a AS (SELECT * FROM users), b AS (SELECT * FROM a) SELECT * FROM b",
			models: []string{"main.users", "main.a", "main.b"},
			want:   []string{"main.users"},
		},
		{
			name:   "recursive CTE",
			sql:    "WITH RECURSIVE r(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM r WHERE n < 3) SELECT * FROM r, users",
			models: []string{"main.users", "main.r"},
			want:   []string{"main.users"},
		},
		{
			name:   "subqueries in FROM and WHERE",
			sql:    "SELECT * FROM (SELECT * FROM users) u WHERE u.id IN (SELECT user_id FROM orders)",
			models: []string{"main.users", "main.orders"},
			want:   []string{"main.orders", "main.users"},
		},
		{
			name:   "schema-qualified and case-insensitive",
			sql:    "SELECT * FROM main.Users",
			models: []string{"main.users"},
			want:   []string{"main.users"},
		},
		{
			name:   "schemas distinguish models with the same table name",
			sql:    "SELECT * FROM sales.claims JOIN claims USING (id)",
			models: []string{"sales.claims", "main.claims", "staging.claims"},
			want:   []string{"main.claims", "sales.claims"},
		},
		{
			name:   "unqualified name resolves to main, not other schemas",
			sql:    "SELECT * FROM claims",
			models: []string{"sales.claims"},
			want:   nil,
		},
		{
			name:   "qualified name is never a CTE",
			sql:    "WITH raw_claims AS (SELECT 1) SELECT * FROM sales.raw_claims, raw_claims",
			models: []string{"sales.raw_claims", "main.raw_claims"},
			want:   []string{"sales.raw_claims"},
		},
		{
			name:   "table functions are not dependencies",
			sql:    "SELECT * FROM read_csv_auto(getvariable('DATA_DIR') || '/offers.csv')",
			models: []string{"main.offers"},
			want:   nil,
		},
		{
			name:    "syntax error",
			sql:     "SELECT * FRM users",
			models:  []string{"main.users"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := AnalyzeDependencies(tt.sql, tt.models)
			if (err != nil) != tt.wantErr {
				t.Errorf("AnalyzeDependencies() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("AnalyzeDependencies() = %v, want %v", got, tt.want)
			}
		})
	}
}
