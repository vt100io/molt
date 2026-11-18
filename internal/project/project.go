package project

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/vt100io/molt/internal/duckdb"
)

// Kinds are the materializations a model can have. A model is always a SELECT;
// the kind decides which statement molt wraps it in.
const (
	KindTable = "table"
	KindView  = "view"
)

const defaultSchema = "main"

// headerKeys lists the directives accepted in a model's header (`-- @kind: view`).
// Unknown directives are an error, so a typo never silently falls back to a default.
var headerKeys = map[string]bool{"kind": true}

type Model struct {
	Name   string // schema.table
	Schema string
	Table  string
	Path   string
	RawSQL string // file contents, header included
	SQL    string // the SELECT, without a trailing semicolon
	Kind   string
	// Hash records which version of the SQL produced a model's table. It is
	// stored in _molt.models and never used to skip work.
	Hash         string
	Dependencies []string
}

// Column is an output column of a model, as DuckDB binds it.
type Column struct {
	Name string
	Type string
}

// A Test is a query that passes when it returns zero rows.
type Test struct {
	Name         string // path relative to tests/, without .sql
	Path         string
	SQL          string
	Dependencies []string
}

type Project struct {
	Dir     string
	InitSQL string // init.sql: settings, secrets, variables and macros, run before anything else
	Models  map[string]*Model
	Tests   []*Test
}

// LoadProject reads init.sql, models/<schema>/<name>.sql (or models/<name>.sql
// for the main schema) and tests/**/*.sql, and resolves dependencies.
func LoadProject(dir string) (*Project, error) {
	p := &Project{Dir: dir, Models: make(map[string]*Model)}

	initSQL, err := os.ReadFile(filepath.Join(dir, "init.sql"))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	p.InitSQL = string(initSQL)

	modelsDir := filepath.Join(dir, "models")
	err = walkSQL(modelsDir, func(path, rel string) error {
		m, err := loadModel(path, rel)
		if err != nil {
			return err
		}
		if existing, ok := p.Models[m.Name]; ok {
			return fmt.Errorf("model %s is defined twice: %s and %s", m.Name, existing.Path, m.Path)
		}
		p.Models[m.Name] = m
		return nil
	})
	if err != nil {
		return nil, err
	}

	testsDir := filepath.Join(dir, "tests")
	err = walkSQL(testsDir, func(path, rel string) error {
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		p.Tests = append(p.Tests, &Test{
			Name: strings.TrimSuffix(rel, ".sql"),
			Path: path,
			SQL:  trimStatement(string(content)),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(p.Tests, func(i, j int) bool { return p.Tests[i].Name < p.Tests[j].Name })

	if err := p.analyzeDependencies(); err != nil {
		return nil, err
	}
	return p, nil
}

// walkSQL calls fn for every .sql file under dir, in lexical order. A missing dir is not an error.
func walkSQL(dir string, fn func(path, rel string) error) error {
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".sql") {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		return fn(path, filepath.ToSlash(rel))
	})
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func loadModel(path, rel string) (*Model, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	schema, table := defaultSchema, strings.TrimSuffix(rel, ".sql")
	if parts := strings.Split(table, "/"); len(parts) == 2 {
		schema, table = parts[0], parts[1]
	} else if len(parts) > 2 {
		return nil, fmt.Errorf("%s: models must be at models/<schema>/<name>.sql or models/<name>.sql", path)
	}

	raw := string(content)
	header, err := parseHeader(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	kind := KindTable
	if k, ok := header["kind"]; ok {
		kind = k
	}
	switch kind {
	case KindTable, KindView:
	default:
		return nil, fmt.Errorf("%s: unknown kind %q (expected %s or %s)", path, kind, KindTable, KindView)
	}

	return &Model{
		Name:   schema + "." + table,
		Schema: schema,
		Table:  table,
		Path:   path,
		RawSQL: raw,
		SQL:    trimStatement(raw),
		Kind:   kind,
		Hash:   fmt.Sprintf("%x", sha256.Sum256(content)),
	}, nil
}

// parseHeader reads `-- @key: value` directives from the comment lines at the
// top of a model, up to the first line that is neither blank nor a comment.
// Ordinary comments are ignored.
func parseHeader(sql string) (map[string]string, error) {
	header := make(map[string]string)
	for _, line := range strings.Split(sql, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "--") {
			break
		}
		directive, ok := strings.CutPrefix(strings.TrimSpace(strings.TrimPrefix(line, "--")), "@")
		if !ok {
			continue
		}
		key, value, ok := strings.Cut(directive, ":")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if !ok || !headerKeys[key] {
			return nil, fmt.Errorf("unknown header directive %q", line)
		}
		header[key] = value
	}
	return header, nil
}

// trimStatement removes trailing whitespace and a trailing semicolon, so the
// SQL can be wrapped in another statement. Leading lines are kept, so line
// numbers in DuckDB errors match the file.
func trimStatement(sql string) string {
	return strings.TrimSuffix(strings.TrimRight(sql, " \t\r\n"), ";")
}

func (p *Project) analyzeDependencies() error {
	names := p.ModelNames()
	for _, name := range names {
		m := p.Models[name]
		deps, err := duckdb.AnalyzeDependencies(m.SQL, names)
		if err != nil {
			return fmt.Errorf("failed to analyze %s: %w", m.Name, err)
		}
		m.Dependencies = deps
	}
	for _, t := range p.Tests {
		deps, err := duckdb.AnalyzeDependencies(t.SQL, names)
		if err != nil {
			return fmt.Errorf("failed to analyze test %s: %w", t.Name, err)
		}
		t.Dependencies = deps
	}
	return nil
}

// ModelNames returns all model names, sorted.
func (p *Project) ModelNames() []string {
	names := make([]string, 0, len(p.Models))
	for name := range p.Models {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Resolve finds a model by its full name, or by its table name when that is unambiguous.
func (p *Project) Resolve(name string) (*Model, error) {
	if m, ok := p.Models[name]; ok {
		return m, nil
	}
	var matches []*Model
	for _, m := range p.Models {
		if strings.EqualFold(m.Table, name) {
			matches = append(matches, m)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return nil, fmt.Errorf("model %s not found", name)
	default:
		return nil, fmt.Errorf("model name %s is ambiguous: use <schema>.%s", name, name)
	}
}

// Schemas returns the schemas used by models, sorted.
func (p *Project) Schemas() []string {
	seen := make(map[string]bool)
	for _, m := range p.Models {
		seen[m.Schema] = true
	}
	schemas := make([]string, 0, len(seen))
	for s := range seen {
		schemas = append(schemas, s)
	}
	sort.Strings(schemas)
	return schemas
}
