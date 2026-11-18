package lineage

import (
	"database/sql"
	"reflect"
	"strings"
	"testing"

	_ "github.com/duckdb/duckdb-go/v2"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	for _, stmt := range []string{
		"CREATE SCHEMA shop",
		"CREATE TABLE shop.orders (id INT, customer_id INT, amount DECIMAL(10,2), status VARCHAR, items JSON, tags INT[])",
		"CREATE TABLE shop.customers (id INT, name VARCHAR, country VARCHAR)",
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func analyze(t *testing.T, db *sql.DB, query string) *Model {
	t.Helper()
	var plan string
	if err := db.QueryRow("SELECT json_serialize_plan(?, optimize := false)::VARCHAR", query).Scan(&plan); err != nil {
		t.Fatal(err)
	}
	// Column names as the shadow catalog sees them: CREATE TABLE AS makes
	// duplicate names unique (id, id_1).
	if _, err := db.Exec("CREATE OR REPLACE TEMP TABLE _out AS SELECT * FROM (" + query + ") LIMIT 0"); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query("DESCRIBE _out")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		var rest [5]sql.NullString
		if err := rows.Scan(&name, &rest[0], &rest[1], &rest[2], &rest[3], &rest[4]); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	m, err := Analyze([]byte(plan), names)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// refs parses "shop.orders.id, shop.customers.name".
func refs(s string) []Ref {
	out := []Ref{}
	if s == "" {
		return out
	}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		i := strings.LastIndex(part, ".")
		out = append(out, Ref{part[:i], part[i+1:]})
	}
	return out
}

type want struct {
	direct   map[string]string // column → direct refs
	indirect map[string]string // column → column-specific indirect refs
	rows     string
}

func check(t *testing.T, m *Model, w want) {
	t.Helper()
	if m.Approximate != "" {
		t.Fatalf("approximate: %s", m.Approximate)
	}
	if len(m.Columns) != len(w.direct) {
		t.Fatalf("got %d columns, want %d: %+v", len(m.Columns), len(w.direct), m.Columns)
	}
	for _, c := range m.Columns {
		d, ok := w.direct[c.Name]
		if !ok {
			t.Errorf("unexpected column %s", c.Name)
			continue
		}
		if got := append([]Ref{}, c.Direct...); !reflect.DeepEqual(got, refs(d)) {
			t.Errorf("%s direct = %v, want %v", c.Name, c.Direct, refs(d))
		}
		if got := append([]Ref{}, c.Indirect...); !reflect.DeepEqual(got, refs(w.indirect[c.Name])) {
			t.Errorf("%s indirect = %v, want %v", c.Name, c.Indirect, refs(w.indirect[c.Name]))
		}
	}
	if got := append([]Ref{}, m.Rows...); !reflect.DeepEqual(got, refs(w.rows)) {
		t.Errorf("rows = %v, want %v", m.Rows, refs(w.rows))
	}
}

func TestLineage(t *testing.T) {
	db := testDB(t)
	// The right side of a left join: the condition shapes its values, not the rows.
	const on = "shop.customers.id, shop.orders.customer_id, shop.orders.status"

	tests := []struct {
		name  string
		query string
		want  want
	}{
		{
			name: "join and aggregate",
			query: `SELECT c.name, sum(o.amount) AS total, count(*) AS n
				FROM shop.orders o JOIN shop.customers c ON o.customer_id = c.id
				WHERE o.status <> 'cancelled'
				GROUP BY c.name`,
			want: want{
				direct: map[string]string{"name": "shop.customers.name", "total": "shop.orders.amount", "n": ""},
				rows:   "shop.customers.id, shop.customers.name, shop.orders.customer_id, shop.orders.status",
			},
		},
		{
			name:  "expressions and case",
			query: `SELECT id, CASE WHEN amount > 100 THEN status ELSE 'small' END AS label, 1 AS one FROM shop.orders`,
			want: want{
				direct: map[string]string{"id": "shop.orders.id", "label": "shop.orders.amount, shop.orders.status", "one": ""},
			},
		},
		{
			name:  "cte",
			query: `WITH x AS (SELECT id, amount * 2 AS a2 FROM shop.orders) SELECT a2 AS doubled, id FROM x`,
			want: want{
				direct: map[string]string{"doubled": "shop.orders.amount", "id": "shop.orders.id"},
			},
		},
		{
			name:  "window",
			query: `SELECT id, sum(amount) OVER (PARTITION BY customer_id ORDER BY id) AS running, row_number() OVER (ORDER BY status) AS rn FROM shop.orders`,
			want: want{
				direct:   map[string]string{"id": "shop.orders.id", "running": "shop.orders.amount", "rn": ""},
				indirect: map[string]string{"running": "shop.orders.customer_id, shop.orders.id", "rn": "shop.orders.status"},
			},
		},
		{
			name:  "qualify",
			query: `SELECT id FROM shop.orders QUALIFY row_number() OVER (PARTITION BY customer_id ORDER BY amount) = 1`,
			want: want{
				direct: map[string]string{"id": "shop.orders.id"},
				rows:   "shop.orders.amount, shop.orders.customer_id",
			},
		},
		{
			name:  "union",
			query: `SELECT id, name AS label FROM shop.customers UNION ALL SELECT id, status FROM shop.orders`,
			want: want{
				direct: map[string]string{"id": "shop.customers.id, shop.orders.id", "label": "shop.customers.name, shop.orders.status"},
			},
		},
		{
			name:  "in subquery",
			query: `SELECT name FROM shop.customers WHERE id IN (SELECT customer_id FROM shop.orders WHERE status = 'paid')`,
			want: want{
				direct: map[string]string{"name": "shop.customers.name"},
				rows:   "shop.customers.id, shop.orders.customer_id, shop.orders.status",
			},
		},
		{
			name:  "correlated scalar subquery",
			query: `SELECT id, (SELECT max(o.amount) FROM shop.orders o WHERE o.customer_id = c.id) AS biggest FROM shop.customers c`,
			want: want{
				direct:   map[string]string{"id": "shop.customers.id", "biggest": "shop.orders.amount"},
				indirect: map[string]string{"biggest": "shop.customers.id, shop.orders.customer_id"},
			},
		},
		{
			name:  "lateral unnest",
			query: `SELECT o.id, json_extract_string(item, '$.sku') AS sku FROM shop.orders o, unnest(json_extract(o.items, '$[*]')) AS t(item)`,
			want: want{
				direct: map[string]string{"id": "shop.orders.id", "sku": "shop.orders.items"},
				rows:   "shop.orders.items",
			},
		},
		{
			name:  "unnest in select",
			query: `SELECT id, unnest(tags) AS tag FROM shop.orders`,
			want: want{
				direct: map[string]string{"id": "shop.orders.id", "tag": "shop.orders.tags"},
				rows:   "shop.orders.tags",
			},
		},
		{
			name:  "lambda",
			query: `SELECT list_transform(tags, lambda x: x + customer_id) AS shifted FROM shop.orders`,
			want: want{
				direct: map[string]string{"shifted": "shop.orders.customer_id, shop.orders.tags"},
			},
		},
		{
			name:  "grouping sets",
			query: `SELECT country, count(DISTINCT id) FILTER (WHERE name <> '') AS n, grouping(country) AS g FROM shop.customers GROUP BY GROUPING SETS ((country), ())`,
			want: want{
				direct:   map[string]string{"country": "shop.customers.country", "n": "shop.customers.id", "g": ""},
				indirect: map[string]string{"n": "shop.customers.name"},
				rows:     "shop.customers.country",
			},
		},
		{
			name:  "distinct on",
			query: `SELECT DISTINCT ON (customer_id) customer_id, amount FROM shop.orders`,
			want: want{
				direct: map[string]string{"customer_id": "shop.orders.customer_id", "amount": "shop.orders.amount"},
				rows:   "shop.orders.customer_id",
			},
		},
		{
			name:  "values",
			query: `SELECT x, y FROM (VALUES (1, 'a'), (2, 'b')) v(x, y)`,
			want:  want{direct: map[string]string{"x": "", "y": ""}},
		},
		{
			name: "recursive cte",
			query: `WITH RECURSIVE r(n, label) AS (
					SELECT id, name FROM shop.customers
					UNION ALL
					SELECT n + 1, label FROM r WHERE n < 3)
				SELECT n, label FROM r`,
			want: want{
				direct: map[string]string{"n": "shop.customers.id", "label": "shop.customers.name"},
				rows:   "shop.customers.id",
			},
		},
		{
			name:  "left join and star",
			query: `SELECT * FROM shop.customers c LEFT JOIN shop.orders o ON o.customer_id = c.id AND o.status = 'paid'`,
			want: want{
				direct: map[string]string{
					"id": "shop.customers.id", "name": "shop.customers.name", "country": "shop.customers.country",
					"id_1": "shop.orders.id", "customer_id": "shop.orders.customer_id", "amount": "shop.orders.amount",
					"status": "shop.orders.status", "items": "shop.orders.items", "tags": "shop.orders.tags",
				},
				indirect: map[string]string{
					"id_1": on, "customer_id": on, "amount": on, "status": on, "items": on, "tags": on,
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			check(t, analyze(t, db, tt.query), tt.want)
		})
	}
}

func TestUnsupportedOperatorIsApproximate(t *testing.T) {
	plan := `{"error":false,"plans":[{"type":"LOGICAL_SOMETHING_NEW","children":[
		{"type":"LOGICAL_GET","children":[],"table_index":0,"names":["id","name"],
		 "function_data":{"catalog":"memory","schema":"shop","table":"customers"},
		 "column_indexes":[{"index":1}]}]}]}`
	m, err := Analyze([]byte(plan), []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(m.Approximate, "LOGICAL_SOMETHING_NEW") {
		t.Errorf("Approximate = %q", m.Approximate)
	}
	for _, c := range m.Columns {
		if !reflect.DeepEqual(c.Direct, refs("shop.customers.name")) {
			t.Errorf("%s direct = %v", c.Name, c.Direct)
		}
	}
}

func TestPlanError(t *testing.T) {
	_, err := Analyze([]byte(`{"error":true,"error_message":"CSVReaderSerialize not implemented"}`), nil)
	if _, ok := err.(*PlanError); !ok {
		t.Fatalf("err = %v, want *PlanError", err)
	}
}

// The plan walker depends on DuckDB's serialized plan format, which is not a
// stable API. When this fails after a DuckDB upgrade, check that the other
// tests pass and then add the new version to SupportedVersions.
func TestDuckDBVersionIsSupported(t *testing.T) {
	var version string
	if err := testDB(t).QueryRow("SELECT version()").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := CheckVersion(version); err != nil {
		t.Fatal(err)
	}
	if CheckVersion("v1.5.6") == nil {
		t.Error("v1.5.6 should not be supported")
	}
}
