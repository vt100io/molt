package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for path, content := range files {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestLoadProject(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"init.sql":                "SET VARIABLE x = 1;",
		"models/offers.sql":       "SELECT 1 AS id;\n",
		"models/sales/claims.sql": "-- One row per claim.\n-- TODO: add reserves\n-- @kind: view\n\nSELECT * FROM offers",
		"tests/claims_unique.sql": "SELECT id FROM sales.claims GROUP BY id HAVING count(*) > 1",
	})

	p, err := LoadProject(dir)
	if err != nil {
		t.Fatal(err)
	}

	if p.InitSQL != "SET VARIABLE x = 1;" {
		t.Errorf("InitSQL = %q", p.InitSQL)
	}

	offers := p.Models["main.offers"]
	if offers == nil || offers.Kind != KindTable || offers.SQL != "SELECT 1 AS id" {
		t.Fatalf("main.offers = %+v, want a table with the trailing semicolon trimmed", offers)
	}

	claims := p.Models["sales.claims"]
	if claims == nil || claims.Schema != "sales" || claims.Table != "claims" || claims.Kind != KindView {
		t.Fatalf("sales.claims = %+v, want a view in schema sales", claims)
	}
	if got := claims.Dependencies; len(got) != 1 || got[0] != "main.offers" {
		t.Errorf("sales.claims dependencies = %v, want [main.offers]", got)
	}
	if !strings.HasPrefix(claims.SQL, "-- One row per claim.") {
		t.Errorf("leading lines must be kept so error line numbers match the file, got %q", claims.SQL)
	}
	if claims.Hash == "" || claims.Hash == offers.Hash {
		t.Errorf("hashes must be set and differ per file")
	}

	if len(p.Tests) != 1 || p.Tests[0].Name != "claims_unique" || p.Tests[0].Dependencies[0] != "sales.claims" {
		t.Errorf("tests = %+v", p.Tests)
	}

	if m, err := p.Resolve("claims"); err != nil || m.Name != "sales.claims" {
		t.Errorf("Resolve(claims) = %v, %v", m, err)
	}
}

func TestLoadProjectErrors(t *testing.T) {
	tests := []struct {
		name    string
		files   map[string]string
		wantErr string
	}{
		{
			name:    "unknown directive",
			files:   map[string]string{"models/a.sql": "-- @knd: view\nSELECT 1"},
			wantErr: "unknown header directive",
		},
		{
			name:    "unknown kind",
			files:   map[string]string{"models/a.sql": "-- @kind: incremental\nSELECT 1"},
			wantErr: `unknown kind "incremental"`,
		},
		{
			name:    "nested too deep",
			files:   map[string]string{"models/a/b/c.sql": "SELECT 1"},
			wantErr: "models must be at",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadProject(writeFiles(t, tt.files))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("LoadProject() error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestResolveAmbiguous(t *testing.T) {
	p, err := LoadProject(writeFiles(t, map[string]string{
		"models/a/claims.sql": "SELECT 1",
		"models/b/claims.sql": "SELECT 1",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Resolve("claims"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("Resolve(claims) error = %v, want ambiguous", err)
	}
}
