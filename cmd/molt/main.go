package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"

	_ "github.com/duckdb/duckdb-go/v2"
	"github.com/spf13/cobra"
	"github.com/vt100io/molt/internal/build"
	"github.com/vt100io/molt/internal/compile"
	"github.com/vt100io/molt/internal/duckdb"
	"github.com/vt100io/molt/internal/lineage"
	"github.com/vt100io/molt/internal/plan"
	"github.com/vt100io/molt/internal/project"
	"github.com/vt100io/molt/internal/shadow"
)

var (
	// dbPath is the DuckDB file that models are built into.
	dbPath string
	// varFlags holds raw KEY=VALUE strings from --var flags.
	varFlags []string
	// vars holds the parsed --var flags. They are set after init.sql, so they override it.
	vars        map[string]string
	fullRefresh bool
)

// parseVars converts ["KEY=VALUE", ...] into a map.
func parseVars(raw []string) (map[string]string, error) {
	result := make(map[string]string, len(raw))
	for _, kv := range raw {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid --var %q: expected KEY=VALUE", kv)
		}
		result[parts[0]] = parts[1]
	}
	return result, nil
}

var rootCmd = &cobra.Command{
	Use:   "molt",
	Short: "molt builds a folder of SQL files into a DuckDB database",
	Long: `molt builds a folder of SQL files into a DuckDB database, in dependency
order and in one transaction: either every model and test succeeds, or nothing changes.

  init.sql                       settings, secrets, variables and macros
  models/<schema>/<name>.sql     one SELECT per model (models/<name>.sql: schema main)
  tests/*.sql                    passes when the query returns zero rows`,
	SilenceUsage:  true,
	SilenceErrors: true,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		var err error
		vars, err = parseVars(varFlags)
		return err
	},
}

// openExisting opens the database read-only, or returns nil if it does not exist yet.
func openExisting() (*sql.DB, error) {
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}
	return sql.Open("duckdb", dbPath+"?access_mode=read_only")
}

func compileScript(p *project.Project, db *sql.DB, selectors []string) (*compile.Script, error) {
	models, err := build.Select(p, selectors)
	if err != nil {
		return nil, err
	}
	existing := map[string]string{}
	if db != nil {
		if existing, err = compile.ExistingObjects(db); err != nil {
			return nil, err
		}
	}
	columns, lin, err := analyzeColumns(p, db, models)
	if err != nil {
		return nil, err
	}
	return compile.Compile(p, compile.Options{
		Models:      models,
		Vars:        vars,
		Existing:    existing,
		FullRefresh: fullRefresh,
		Columns:     columns,
		Lineage:     lin,
	})
}

// analyzeColumns binds the project in a shadow catalog and returns the output
// columns and lineage of the selected models, to be recorded in _molt. A model
// that fails to bind is left out: the build reports the real error. Without
// lineage (an unverified DuckDB version) the build still runs.
//
// Models that are not selected keep the fingerprints recorded when they were
// built, since the selected models are built from their data as it is.
func analyzeColumns(p *project.Project, db *sql.DB, models []string) (map[string][]shadow.Column, map[string]*lineage.Model, error) {
	catalog, err := shadow.Build(p, vars)
	if err != nil {
		return nil, nil, err
	}
	defer catalog.Close()

	if len(models) < len(p.Models) {
		built, err := plan.LoadBuilt(db)
		if err != nil {
			return nil, nil, err
		}
		selected := map[string]bool{}
		for _, name := range models {
			selected[name] = true
		}
		for name, b := range built.Models {
			if selected[name] || b.RowsFingerprint == "" {
				continue
			}
			l := &lineage.Model{RowsFingerprint: b.RowsFingerprint}
			for _, c := range b.Columns {
				l.Columns = append(l.Columns, lineage.Column{Name: c.Name, Fingerprint: c.Fingerprint})
			}
			catalog.Pin(name, l)
		}
	}

	columns := map[string][]shadow.Column{}
	lin := map[string]*lineage.Model{}
	for _, name := range models {
		if _, failed := catalog.Errors[name]; failed {
			continue
		}
		columns[name] = catalog.Schemas[name]
		if l, err := catalog.Lineage(name); err == nil {
			lin[name] = l
		}
	}
	return columns, lin, nil
}

var runCmd = &cobra.Command{
	Use:   "run [model | model+ ...]",
	Short: "Build all models, or the selected ones, and run their tests in one transaction",
	Long: `Build all models, or the selected ones, and run their tests in one transaction.
"model+" selects a model and everything downstream of it. If any model or test
fails, the transaction is rolled back and the database is unchanged.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		p, err := project.LoadProject(".")
		if err != nil {
			return err
		}

		db, err := sql.Open("duckdb", dbPath)
		if err != nil {
			return err
		}
		defer db.Close()

		script, err := compileScript(p, db, args)
		if err != nil {
			return err
		}

		start := time.Now()
		var models, tests int
		err = build.Execute(context.Background(), db, script, func(stmt compile.Statement, d time.Duration) {
			switch stmt.Kind {
			case compile.StmtModel:
				models++
				fmt.Printf("✓ %-5s %-40s %6.2fs\n", p.Models[stmt.Label].Kind, stmt.Label, d.Seconds())
			case compile.StmtTest:
				tests++
				fmt.Printf("✓ test  %-40s %6.2fs\n", stmt.Label, d.Seconds())
			}
		})
		if err != nil {
			return fmt.Errorf("✗ %w\nrolled back: %s is unchanged", err, dbPath)
		}

		fmt.Printf("\n%s, %s in %.2fs → %s\n", plural(models, "model"), plural(tests, "test"), time.Since(start).Seconds(), dbPath)
		return nil
	},
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

var compileCmd = &cobra.Command{
	Use:   "compile [model | model+ ...]",
	Short: "Print the SQL script that run would execute",
	Long: `Print the SQL script that run would execute. The script is plain SQL and can
be run with the DuckDB CLI: duckdb -bail molt.duckdb < build.sql`,
	RunE: func(cmd *cobra.Command, args []string) error {
		p, err := project.LoadProject(".")
		if err != nil {
			return err
		}

		db, err := openExisting()
		if err != nil {
			return err
		}
		if db != nil {
			defer db.Close()
		}

		script, err := compileScript(p, db, args)
		if err != nil {
			return err
		}
		fmt.Print(script.String())
		return nil
	},
}

var testCmd = &cobra.Command{
	Use:   "test",
	Short: "Run all tests against the existing database, without building",
	RunE: func(cmd *cobra.Command, args []string) error {
		p, err := project.LoadProject(".")
		if err != nil {
			return err
		}
		if _, err := os.Stat(dbPath); os.IsNotExist(err) {
			return fmt.Errorf("%s does not exist: run molt run first", dbPath)
		}

		db, err := sql.Open("duckdb", dbPath)
		if err != nil {
			return err
		}
		defer db.Close()
		db.SetMaxOpenConns(1)

		setup := duckdb.VarStatements(vars)
		if strings.TrimSpace(p.InitSQL) != "" {
			setup = append([]string{p.InitSQL}, setup...)
		}
		for _, stmt := range setup {
			if _, err := db.Exec(stmt); err != nil {
				return fmt.Errorf("init failed: %w", err)
			}
		}

		failed := 0
		for _, t := range p.Tests {
			var n int64
			if err := db.QueryRow(fmt.Sprintf("SELECT count(*) FROM (%s\n)", t.SQL)).Scan(&n); err != nil {
				failed++
				fmt.Printf("✗ %s: %v\n", t.Name, err)
				continue
			}
			if n > 0 {
				failed++
				fmt.Printf("✗ %s: %d rows\n", t.Name, n)
				continue
			}
			fmt.Printf("✓ %s\n", t.Name)
		}

		if failed > 0 {
			return fmt.Errorf("%d of %d tests failed", failed, len(p.Tests))
		}
		return nil
	},
}

func printTree(p *project.Project, modelName string, prefix string, isLast bool, visited map[string]bool) {
	fmt.Print(prefix)
	if isLast {
		fmt.Print("└── ")
		prefix += "    "
	} else {
		fmt.Print("├── ")
		prefix += "│   "
	}

	fmt.Println(modelName)

	if visited[modelName] {
		return
	}
	visited[modelName] = true

	m, ok := p.Models[modelName]
	if !ok {
		return
	}

	for i, dep := range m.Dependencies {
		isLastDep := i == len(m.Dependencies)-1
		printTree(p, dep, prefix, isLastDep, visited)
	}
}

var graphCmd = &cobra.Command{
	Use:   "graph [model]",
	Short: "Show a visual dependency tree for the project or a specific model",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		p, err := project.LoadProject(".")
		if err != nil {
			return err
		}

		var roots []string
		if len(args) == 1 {
			m, err := p.Resolve(args[0])
			if err != nil {
				return err
			}
			roots = []string{m.Name}
		} else {
			dependentOn := make(map[string]bool)
			for _, m := range p.Models {
				for _, dep := range m.Dependencies {
					dependentOn[dep] = true
				}
			}
			for _, name := range p.ModelNames() {
				if !dependentOn[name] {
					roots = append(roots, name)
				}
			}
		}

		for i, root := range roots {
			if i > 0 {
				fmt.Println()
			}
			visited := make(map[string]bool)
			fmt.Println(root)
			m := p.Models[root]
			for i, dep := range m.Dependencies {
				isLastDep := i == len(m.Dependencies)-1
				printTree(p, dep, "", isLastDep, visited)
			}
		}
		return nil
	},
}

var explainCmd = &cobra.Command{
	Use:   "explain <model>",
	Short: "Show the DuckDB execution plan for a model",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		p, err := project.LoadProject(".")
		if err != nil {
			return err
		}

		m, err := p.Resolve(args[0])
		if err != nil {
			return err
		}

		catalog, err := shadow.Build(p, vars)
		if err != nil {
			return err
		}
		defer catalog.Close()

		plan, err := catalog.Explain(m.Name)
		if err != nil {
			return err
		}

		fmt.Printf("Execution plan for %s:\n%s\n", m.Name, plan)
		return nil
	},
}

var checkCmd = &cobra.Command{
	Use:   "check",
	Short: "Bind every model against the real schemas of its upstream models, without building",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		p, err := project.LoadProject(".")
		if err != nil {
			return err
		}

		catalog, err := shadow.Build(p, vars)
		if err != nil {
			return err
		}
		defer catalog.Close()

		for _, name := range catalog.Order {
			if err, failed := catalog.Errors[name]; failed {
				fmt.Printf("✗ %s: %v\n", name, err)
				continue
			}
			note := ""
			// Lineage is informational: a model whose lineage cannot be traced still passes.
			if l, err := catalog.Lineage(name); err != nil {
				note = fmt.Sprintf(", no lineage: %v", err)
			} else if l.Approximate != "" {
				note = fmt.Sprintf(", approximate lineage: %s", l.Approximate)
			}
			fmt.Printf("✓ %s (%d columns%s)\n", name, len(catalog.Schemas[name]), note)
		}

		if n := len(catalog.Errors); n > 0 {
			return fmt.Errorf("%d of %d models failed to bind", n, len(catalog.Order))
		}
		return nil
	},
}

var schemaCmd = &cobra.Command{
	Use:   "schema [model]",
	Short: "Show the output columns and types of a model, or of all models",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		p, err := project.LoadProject(".")
		if err != nil {
			return err
		}

		var names []string
		if len(args) == 1 {
			m, err := p.Resolve(args[0])
			if err != nil {
				return err
			}
			names = []string{m.Name}
		}

		catalog, err := shadow.Build(p, vars)
		if err != nil {
			return err
		}
		defer catalog.Close()

		if len(names) == 0 {
			names = catalog.Order
		}

		for i, name := range names {
			if i > 0 {
				fmt.Println()
			}
			if err, failed := catalog.Errors[name]; failed {
				fmt.Printf("%s: failed to bind: %v\n", name, err)
				continue
			}
			fmt.Println(name)
			for _, col := range catalog.Schemas[name] {
				fmt.Printf("  %-32s %s\n", col.Name, col.Type)
			}
		}
		return nil
	},
}

func init() {
	rootCmd.PersistentFlags().StringVar(&dbPath, "db", "molt.duckdb", "DuckDB file to build into")
	rootCmd.PersistentFlags().StringArrayVar(&varFlags, "var", []string{},
		"Set a DuckDB variable (KEY=VALUE), after init.sql. Example: --var DATA_DIR=example_flow")

	for _, cmd := range []*cobra.Command{runCmd, compileCmd} {
		cmd.Flags().BoolVar(&fullRefresh, "full-refresh", false,
			"Rebuild models from scratch (table and view models always are)")
	}

	rootCmd.AddCommand(runCmd, compileCmd, planCmd, testCmd, checkCmd, schemaCmd, explainCmd, lineageCmd, graphCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
