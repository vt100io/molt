package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/vt100io/molt/internal/lineage"
	"github.com/vt100io/molt/internal/project"
	"github.com/vt100io/molt/internal/shadow"
)

var lineageCmd = &cobra.Command{
	Use:   "lineage <model> [column]",
	Short: "Show where a model's columns come from, or trace one column to its sources",
	Long: `Show where a model's columns come from, one model upstream, from DuckDB's bound
plan against the shadow catalog. Nothing is built or read.

With a column, trace it through every upstream model to its sources.

  ←   direct: the value flows into the column
  ~   indirect: decides which rows exist, or shapes a window or a filtered aggregate`,
	Args: cobra.RangeArgs(1, 2),
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

		l, err := catalog.Lineage(m.Name)
		if err != nil {
			return err
		}

		if len(args) == 1 {
			printModelLineage(m.Name, l)
			return nil
		}

		column := findColumn(l, args[1])
		if column == nil {
			return fmt.Errorf("model %s has no column %s", m.Name, args[1])
		}
		t := &tracer{project: p, catalog: catalog, cache: map[string]*lineage.Model{}}
		fmt.Println(m.Name + "." + column.Name)
		t.print(column, l.Rows, "")
		return nil
	},
}

func printModelLineage(name string, l *lineage.Model) {
	fmt.Println(name)
	if l.Approximate != "" {
		fmt.Printf("  (approximate: %s)\n", l.Approximate)
	}
	width := 0
	for _, c := range l.Columns {
		width = max(width, len(c.Name))
	}
	for _, c := range l.Columns {
		sources := "no upstream model"
		if len(c.Direct) > 0 {
			sources = joinRefs(c.Direct)
		}
		fmt.Printf("  %-*s ← %s\n", width, c.Name, sources)
		if len(c.Indirect) > 0 {
			fmt.Printf("  %-*s ~ %s\n", width, "", joinRefs(c.Indirect))
		}
	}
	if len(l.Rows) > 0 {
		fmt.Printf("\n  rows ~ %s\n", joinRefs(l.Rows))
	}
}

func joinRefs(refs []lineage.Ref) string {
	s := make([]string, len(refs))
	for i, r := range refs {
		s[i] = r.String()
	}
	return strings.Join(s, ", ")
}

func findColumn(l *lineage.Model, name string) *lineage.Column {
	for i := range l.Columns {
		if strings.EqualFold(l.Columns[i].Name, name) {
			return &l.Columns[i]
		}
	}
	return nil
}

// tracer follows a column upstream through models.
type tracer struct {
	project *project.Project
	catalog *shadow.Catalog
	cache   map[string]*lineage.Model
}

func (t *tracer) lineage(model string) (*lineage.Model, error) {
	if l, ok := t.cache[model]; ok {
		return l, nil
	}
	l, err := t.catalog.Lineage(model)
	t.cache[model] = l
	return l, err
}

// print writes the upstream tree of a column: direct sources, then the
// column's own indirect sources and the model's row sources, marked ~.
func (t *tracer) print(c *lineage.Column, rows []lineage.Ref, prefix string) {
	type edge struct {
		ref    lineage.Ref
		marker string
	}
	var edges []edge
	for _, r := range c.Direct {
		edges = append(edges, edge{r, "←"})
	}
	for _, r := range c.Indirect {
		edges = append(edges, edge{r, "~"})
	}
	for _, r := range rows {
		edges = append(edges, edge{r, "~"})
	}

	for i, e := range edges {
		branch, next := "├── ", "│   "
		if i == len(edges)-1 {
			branch, next = "└── ", "    "
		}
		line := prefix + branch + e.marker + " " + e.ref.String()

		upstream, isModel := t.project.Models[e.ref.Table]
		if !isModel {
			fmt.Println(line)
			continue
		}
		l, err := t.lineage(upstream.Name)
		if err != nil {
			fmt.Printf("%s  (%v)\n", line, err)
			continue
		}
		if l.Approximate != "" {
			line += "  (approximate)"
		}
		fmt.Println(line)
		// Only direct edges are followed further: an indirect source's own
		// history is a property of that column, not of this one.
		if e.marker == "←" {
			if col := findColumn(l, e.ref.Column); col != nil {
				t.print(col, nil, prefix+next)
			}
		}
	}
}
