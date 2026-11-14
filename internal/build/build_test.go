package build

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/vt100io/molt/internal/compile"
	"github.com/vt100io/molt/internal/project"
)

func writeProject(t *testing.T, dir string, files map[string]string) *project.Project {
	t.Helper()
	for path, content := range files {
		full := filepath.Join(dir, path)
		if content == "" {
			os.Remove(full)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	p, err := project.LoadProject(dir)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// run compiles and executes the selected models, as `molt run` does.
func run(t *testing.T, p *project.Project, dbPath string, selectors []string, vars map[string]string) error {
	t.Helper()
	db, err := sql.Open("duckdb", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	models, err := Select(p, selectors)
	if err != nil {
		t.Fatal(err)
	}
	existing, err := compile.ExistingObjects(db)
	if err != nil {
		t.Fatal(err)
	}
	script, err := compile.Compile(p, compile.Options{Models: models, Vars: vars, Existing: existing})
	if err != nil {
		t.Fatal(err)
	}
	return Execute(context.Background(), db, script, nil)
}

func query(t *testing.T, dbPath, q string) [][]any {
	t.Helper()
	db, err := sql.Open("duckdb", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(q)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var result [][]any
	for rows.Next() {
		row := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range row {
			ptrs[i] = &row[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		result = append(result, row)
	}
	return result
}

func TestSelect(t *testing.T) {
	p := writeProject(t, t.TempDir(), map[string]string{
		"models/a.sql":   "SELECT 1 AS id",
		"models/b.sql":   "SELECT * FROM a",
		"models/c.sql":   "SELECT * FROM b",
		"models/d.sql":   "SELECT * FROM a",
		"models/x/e.sql": "SELECT 1",
	})

	tests := []struct {
		selectors []string
		want      []string
	}{
		{nil, []string{"main.a", "main.b", "main.c", "main.d", "x.e"}},
		{[]string{"b"}, []string{"main.b"}},
		{[]string{"b+"}, []string{"main.b", "main.c"}},
		{[]string{"main.a+"}, []string{"main.a", "main.b", "main.c", "main.d"}},
		{[]string{"c", "e"}, []string{"main.c", "x.e"}},
	}
	for _, tt := range tests {
		got, err := Select(p, tt.selectors)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("Select(%v) = %v, want %v", tt.selectors, got, tt.want)
		}
	}
}

func TestRunBuildsInOneTransaction(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "molt.duckdb")
	p := writeProject(t, dir, map[string]string{
		// Macros persist in the database, so views may use them. Variables only
		// live in the session, so they belong in table models.
		"init.sql": `SET VARIABLE factor = 10;
			CREATE OR REPLACE MACRO double(x) AS x * 2;`,
		"models/raw/numbers.sql":         "SELECT range * getvariable('factor')::INTEGER AS n FROM range(3)",
		"models/marts/doubled.sql":       "-- @kind: view\nSELECT double(n) AS v FROM raw.numbers",
		"tests/doubled_non_negative.sql": "SELECT * FROM marts.doubled WHERE v < 0",
	})

	if err := run(t, p, dbPath, nil, nil); err != nil {
		t.Fatal(err)
	}
	// Read from a new session, as an analyst would.
	if got := query(t, dbPath, "SELECT sum(v)::INTEGER FROM marts.doubled"); got[0][0] != int32(60) {
		t.Errorf("sum(v) = %v, want 60", got[0][0])
	}

	// Bookkeeping is written in the same transaction as the models.
	got := query(t, dbPath, "SELECT name, kind, row_count FROM _molt.models ORDER BY name")
	want := [][]any{{"marts.doubled", "view", nil}, {"raw.numbers", "table", int64(3)}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("_molt.models = %v, want %v", got, want)
	}

	// --var overrides init.sql. It sets a VARCHAR, hence the cast in the model.
	if err := run(t, p, dbPath, nil, map[string]string{"factor": "100"}); err != nil {
		t.Fatal(err)
	}
	if got := query(t, dbPath, "SELECT sum(v)::INTEGER FROM marts.doubled"); got[0][0] != int32(600) {
		t.Errorf("sum(v) after --var = %v, want 600", got[0][0])
	}
	if got := query(t, dbPath, "SELECT count(*) FROM _molt.runs"); got[0][0] != int64(2) {
		t.Errorf("runs = %v, want 2", got[0][0])
	}
}

func TestFailingTestRollsBack(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "molt.duckdb")
	p := writeProject(t, dir, map[string]string{
		"models/numbers.sql":     "SELECT 1 AS n",
		"tests/no_negatives.sql": "SELECT * FROM numbers WHERE n < 0",
	})
	if err := run(t, p, dbPath, nil, nil); err != nil {
		t.Fatal(err)
	}

	// A change that breaks the test must leave the database as it was.
	p = writeProject(t, dir, map[string]string{"models/numbers.sql": "SELECT -1 AS n UNION ALL SELECT 2"})
	err := run(t, p, dbPath, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "test no_negatives failed: 1 rows") {
		t.Fatalf("run error = %v, want the failing test", err)
	}
	if got := query(t, dbPath, "SELECT n FROM numbers"); !reflect.DeepEqual(got, [][]any{{int32(1)}}) {
		t.Errorf("numbers = %v, want the previous build [[1]]", got)
	}
	if got := query(t, dbPath, "SELECT count(*) FROM _molt.runs"); got[0][0] != int64(1) {
		t.Errorf("runs = %v, want 1: the failed run must not be recorded", got[0][0])
	}
}

func TestFailingModelRollsBack(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "molt.duckdb")
	p := writeProject(t, dir, map[string]string{
		"models/a.sql": "SELECT 1 AS n",
		"models/b.sql": "SELECT n FROM a",
	})
	if err := run(t, p, dbPath, nil, nil); err != nil {
		t.Fatal(err)
	}

	p = writeProject(t, dir, map[string]string{
		"models/a.sql": "SELECT 2 AS n",
		"models/b.sql": "SELECT missing FROM a",
	})
	err := run(t, p, dbPath, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "model main.b") {
		t.Fatalf("run error = %v, want it to name main.b", err)
	}
	// a was rebuilt before b failed, but the whole transaction rolled back.
	if got := query(t, dbPath, "SELECT n FROM a"); !reflect.DeepEqual(got, [][]any{{int32(1)}}) {
		t.Errorf("a = %v, want the previous build [[1]]", got)
	}
}

func TestKindChange(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "molt.duckdb")
	files := map[string]string{"models/m.sql": "-- @kind: view\nSELECT 1 AS n"}

	for _, kind := range []string{"view", "table", "view"} {
		files["models/m.sql"] = "-- @kind: " + kind + "\nSELECT 1 AS n"
		p := writeProject(t, dir, files)
		if err := run(t, p, dbPath, nil, nil); err != nil {
			t.Fatalf("switching to %s: %v", kind, err)
		}
		got := query(t, dbPath, "SELECT kind FROM _molt.models WHERE name = 'main.m'")
		if got[0][0] != kind {
			t.Errorf("kind = %v, want %s", got[0][0], kind)
		}
	}
}

func TestSelectedRunOnlyRunsRelatedTests(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "molt.duckdb")
	p := writeProject(t, dir, map[string]string{
		"models/a.sql":    "SELECT 1 AS n",
		"models/b.sql":    "SELECT 1 AS n",
		"tests/a_ok.sql":  "SELECT * FROM a WHERE n < 0",
		"tests/b_bad.sql": "SELECT * FROM b",
	})

	// b's test fails, but it is not related to a.
	if err := run(t, p, dbPath, []string{"a"}, nil); err != nil {
		t.Fatalf("run a: %v", err)
	}
	if err := run(t, p, dbPath, nil, nil); err == nil {
		t.Fatal("full run should fail on b_bad")
	}
}
