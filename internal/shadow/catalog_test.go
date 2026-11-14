package shadow

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/vt100io/molt/internal/project"
)

func loadProject(t *testing.T, models map[string]string) *project.Project {
	t.Helper()
	dir := t.TempDir()
	modelsDir := filepath.Join(dir, "models")
	if err := os.MkdirAll(modelsDir, 0755); err != nil {
		t.Fatal(err)
	}
	for name, sql := range models {
		if err := os.WriteFile(filepath.Join(modelsDir, name+".sql"), []byte(sql), 0644); err != nil {
			t.Fatal(err)
		}
	}
	p, err := project.LoadProject(dir)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBuildResolvesRealSchemas(t *testing.T) {
	p := loadProject(t, map[string]string{
		"src": "SELECT 1::BIGINT AS id, 'a' AS name, '{\"premium\": 10}'::JSON AS data",
		// Columns that never existed in the old dummy tables.
		"mid": "SELECT s.id, upper(s.name) AS name_u, TRY_CAST(json_extract(s.data, '$.premium') AS INTEGER) AS premium FROM src s",
		"top": "-- trailing comment without a newline\nSELECT id, premium * 2 AS doubled FROM mid -- end",
	})

	c, err := Build(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if len(c.Errors) > 0 {
		t.Fatalf("unexpected errors: %v", c.Errors)
	}

	want := map[string][]Column{
		"main.src": {{"id", "BIGINT"}, {"name", "VARCHAR"}, {"data", "JSON"}},
		"main.mid": {{"id", "BIGINT"}, {"name_u", "VARCHAR"}, {"premium", "INTEGER"}},
		"main.top": {{"id", "BIGINT"}, {"doubled", "INTEGER"}},
	}
	if !reflect.DeepEqual(c.Schemas, want) {
		t.Errorf("Schemas = %v, want %v", c.Schemas, want)
	}
}

func TestBuildReportsBindErrorsAndSkipsDownstream(t *testing.T) {
	p := loadProject(t, map[string]string{
		"src":   "SELECT 1 AS id",
		"typo":  "SELECT idd FROM src",
		"child": "SELECT * FROM typo",
		"ok":    "SELECT id FROM src",
		// Not a model, so the parser ignores it, but binding must catch it.
		"missing": "SELECT * FROM does_not_exist",
	})

	c, err := Build(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if _, ok := c.Schemas["main.ok"]; !ok {
		t.Errorf("model ok should bind, got error %v", c.Errors["main.ok"])
	}
	if err := c.Errors["main.typo"]; err == nil || !strings.Contains(err.Error(), "idd") {
		t.Errorf("typo error = %v, want a binder error mentioning idd", err)
	}
	if err := c.Errors["main.child"]; err == nil || !strings.Contains(err.Error(), "upstream model main.typo failed") {
		t.Errorf("child error = %v, want upstream failure", err)
	}
	if err := c.Errors["main.missing"]; err == nil || !strings.Contains(err.Error(), "does_not_exist") {
		t.Errorf("missing error = %v, want a binder error mentioning does_not_exist", err)
	}
	if len(c.Errors) != 3 {
		t.Errorf("got %d errors, want 3: %v", len(c.Errors), c.Errors)
	}
}

func TestExplainUsesRealSchemas(t *testing.T) {
	p := loadProject(t, map[string]string{
		"src": "SELECT 1 AS policy_id, 2.5 AS premium",
		"agg": "SELECT policy_id, sum(premium) AS total FROM src GROUP BY policy_id",
	})

	c, err := Build(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	plan, err := c.Explain("main.agg")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "src") {
		t.Errorf("plan does not scan src:\n%s", plan)
	}
}
