package main

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/vt100io/molt/internal/plan"
	"github.com/vt100io/molt/internal/project"
	"github.com/vt100io/molt/internal/shadow"
)

var planCmd = &cobra.Command{
	Use:   "plan",
	Short: "Show which models and columns would change compared to what is built",
	Long: `Compare the project with what was last built into the database, column by
column, without running anything. Each column's fingerprint identifies what it
computes through every model upstream, so plan finds the models you edited and
every column downstream whose data would change.

  breaking       existing columns change or disappear, or the rows change
  non-breaking   only new columns`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		p, err := project.LoadProject(".")
		if err != nil {
			return err
		}

		db, err := openExisting()
		if err != nil {
			return err
		}
		built, err := plan.LoadBuilt(db)
		if db != nil {
			db.Close()
		}
		if err != nil {
			return err
		}

		catalog, err := shadow.Build(p, vars)
		if err != nil {
			return err
		}
		defer catalog.Close()

		result := plan.Compare(built, p, catalog)
		printPlan(result, len(built.Models) == 0, nil)

		for _, m := range result.Models {
			if m.Status == plan.Failed {
				return fmt.Errorf("some models fail to bind: run molt check")
			}
		}
		return nil
	},
}

// printPlan prints the changed models of a plan. With only, it prints only
// those models (removed models are never part of a selection).
func printPlan(result *plan.Plan, nothingBuilt bool, only map[string]bool) {
	if nothingBuilt {
		fmt.Printf("Nothing built in %s yet: every model is new.\n", dbPath)
		return
	}
	if only != nil {
		filtered := &plan.Plan{}
		for _, m := range result.Models {
			if only[m.Name] {
				filtered.Models = append(filtered.Models, m)
			}
		}
		result = filtered
	}
	if !result.Changed() {
		if only != nil {
			fmt.Println("No changes in the selected models.")
		} else {
			fmt.Printf("No changes: %s matches the project.\n", dbPath)
		}
		return
	}

	symbols := map[string]string{
		plan.Added: "+", plan.Removed: "-", plan.Modified: "~", plan.Affected: "~", plan.Failed: "✗",
	}
	unchanged := 0
	for _, m := range result.Models {
		if m.Status == plan.Unchanged {
			unchanged++
			continue
		}

		label := m.Status
		switch {
		case m.Status == plan.Failed:
			label = "fails to bind"
		case m.Status == plan.Modified || m.Status == plan.Affected:
			switch {
			case m.Breaking:
				label += ", breaking"
			case len(m.Columns) > 0:
				label += ", non-breaking"
			default:
				label += ", no data change"
			}
		}
		if m.OldKind != "" {
			label += fmt.Sprintf(" (%s → %s)", m.OldKind, m.Kind)
		}
		fmt.Printf("%s %-36s %s\n", symbols[m.Status], m.Name, label)

		if m.Note != "" {
			fmt.Printf("    %s\n", m.Note)
		}
		if m.RowsChanged && m.Status != plan.Added {
			fmt.Printf("    ~ rows changed: every column downstream sees different data\n")
		}
		if m.Status == plan.Added || m.Status == plan.Removed {
			continue
		}
		for _, c := range m.Columns {
			line := fmt.Sprintf("    %s %-24s %s", map[string]string{
				plan.ColumnAdded: "+", plan.ColumnRemoved: "-", plan.ColumnModified: "~",
			}[c.Change], c.Name, c.Change)
			if c.OldType != "" && c.NewType != "" {
				line += fmt.Sprintf(" (%s → %s)", c.OldType, c.NewType)
			}
			if len(c.Because) > 0 {
				line += " ← " + joinRefs(c.Because)
			}
			fmt.Println(line)
		}
	}
	fmt.Printf("\n%s changed, %s unchanged\n", plural(len(result.Models)-unchanged, "model"), plural(unchanged, "model"))
}
