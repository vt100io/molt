// Package plan compares the project as it is now with what was last built,
// column by column, before anything runs.
//
// What was built is read from _molt: each model's file hash, and the type and
// fingerprint of each output column. What would be built comes from the shadow
// catalog. A fingerprint identifies what a column computes through every model
// upstream (see package lineage), so comparing fingerprints finds every column
// whose data would change, in the models that were edited and in everything
// downstream of them, without comparing any SQL text.
//
// A change is breaking when existing columns change or disappear, or the set
// of rows changes: whatever reads the model may see different data. A change
// that only adds columns is non-breaking.
package plan

import (
	"database/sql"
	"fmt"
	"sort"

	"github.com/vt100io/molt/internal/lineage"
	"github.com/vt100io/molt/internal/project"
	"github.com/vt100io/molt/internal/shadow"
)

// Built is the state recorded in _molt by the last runs.
type Built struct {
	Models map[string]*BuiltModel
}

type BuiltModel struct {
	Kind            string
	Hash            string
	RowsFingerprint string // empty when built before fingerprints were recorded
	Columns         []BuiltColumn
}

type BuiltColumn struct {
	Name        string
	Type        string
	Fingerprint string
}

// Status of a model in the plan.
const (
	Added     = "added"     // new model
	Removed   = "removed"   // model file deleted; its table is left in place
	Modified  = "modified"  // its SQL changed
	Affected  = "affected"  // its SQL is unchanged, but data upstream changes
	Unchanged = "unchanged" // nothing it computes changes
	Failed    = "failed"    // does not bind against the new upstream schemas
)

// Column changes.
const (
	ColumnAdded    = "added"
	ColumnRemoved  = "removed"
	ColumnModified = "modified"
)

type Plan struct {
	// Models lists every model, in dependency order; removed models last.
	Models []*ModelPlan
}

type ModelPlan struct {
	Name   string
	Status string
	// Breaking is set when existing columns change or disappear, or the rows
	// change. Only meaningful for Modified and Affected.
	Breaking    bool
	RowsChanged bool
	// OldKind is set when the model changes between table and view.
	OldKind string
	Kind    string
	Columns []ColumnChange
	// Note explains a Failed model, or why changes could not be narrowed down.
	Note string
}

type ColumnChange struct {
	Name    string
	Change  string
	OldType string // set when the type changes
	NewType string
	// Because lists the upstream columns whose change causes this one, for
	// Affected models.
	Because []lineage.Ref
}

// Changed reports whether the plan changes anything.
func (p *Plan) Changed() bool {
	for _, m := range p.Models {
		if m.Status != Unchanged {
			return true
		}
	}
	return false
}

// Compare plans the project against what was built. built may be empty.
func Compare(built *Built, p *project.Project, c *shadow.Catalog) *Plan {
	plan := &Plan{}
	// Changed columns so far, for the Because of downstream columns. A model
	// whose rows change counts as all its columns changing.
	changed := map[lineage.Ref]bool{}

	for _, name := range c.Order {
		m := p.Models[name]
		mp := &ModelPlan{Name: name}
		plan.Models = append(plan.Models, mp)
		old, wasBuilt := built.Models[name]

		if err, failed := c.Errors[name]; failed {
			mp.Status, mp.Breaking, mp.Note = Failed, true, err.Error()
			if old != nil {
				for _, col := range old.Columns {
					changed[lineage.Ref{Table: name, Column: col.Name}] = true
				}
			}
			continue
		}

		l, err := c.Lineage(name)
		if err != nil {
			mp.Note = err.Error()
		}

		if !wasBuilt {
			mp.Status = Added
			for _, col := range c.Schemas[name] {
				mp.Columns = append(mp.Columns, ColumnChange{Name: col.Name, Change: ColumnAdded, NewType: col.Type})
			}
			continue
		}

		if old.Kind != m.Kind {
			mp.OldKind, mp.Kind = old.Kind, m.Kind
		}
		compareColumns(mp, old, c.Schemas[name], l)

		for _, col := range mp.Columns {
			if col.Change != ColumnAdded {
				mp.Breaking = true
				changed[lineage.Ref{Table: name, Column: col.Name}] = true
			}
		}
		if mp.RowsChanged {
			mp.Breaking = true
			for _, col := range c.Schemas[name] {
				changed[lineage.Ref{Table: name, Column: col.Name}] = true
			}
		}

		switch {
		case old.Hash != m.Hash:
			mp.Status = Modified
		case len(mp.Columns) > 0 || mp.RowsChanged:
			mp.Status = Affected
			explain(mp, l, changed)
		default:
			mp.Status = Unchanged
		}
	}

	var removed []string
	for name := range built.Models {
		if _, ok := p.Models[name]; !ok {
			removed = append(removed, name)
		}
	}
	sort.Strings(removed)
	for _, name := range removed {
		mp := &ModelPlan{Name: name, Status: Removed, Breaking: true}
		for _, col := range built.Models[name].Columns {
			mp.Columns = append(mp.Columns, ColumnChange{Name: col.Name, Change: ColumnRemoved, OldType: col.Type})
		}
		plan.Models = append(plan.Models, mp)
	}
	return plan
}

// compareColumns fills in the column changes of a model that exists in both.
// Without fingerprints on either side, every column counts as modified.
func compareColumns(mp *ModelPlan, old *BuiltModel, cols []project.Column, l *lineage.Model) {
	fps := map[string]string{}
	newRows := ""
	if l != nil {
		newRows = l.RowsFingerprint
		for _, c := range l.Columns {
			fps[c.Name] = c.Fingerprint
		}
	}
	unknown := old.RowsFingerprint == "" || newRows == ""
	if unknown && mp.Note == "" {
		mp.Note = "no fingerprints recorded for the last build: every column counts as changed"
	}
	mp.RowsChanged = unknown || old.RowsFingerprint != newRows

	oldCols := map[string]BuiltColumn{}
	for _, c := range old.Columns {
		oldCols[c.Name] = c
	}
	newCols := map[string]bool{}
	for _, c := range cols {
		newCols[c.Name] = true
		o, existed := oldCols[c.Name]
		switch {
		case !existed:
			mp.Columns = append(mp.Columns, ColumnChange{Name: c.Name, Change: ColumnAdded, NewType: c.Type})
		case o.Type != c.Type:
			mp.Columns = append(mp.Columns, ColumnChange{Name: c.Name, Change: ColumnModified, OldType: o.Type, NewType: c.Type})
		case unknown || o.Fingerprint == "" || o.Fingerprint != fps[c.Name]:
			mp.Columns = append(mp.Columns, ColumnChange{Name: c.Name, Change: ColumnModified})
		}
	}
	for _, c := range old.Columns {
		if !newCols[c.Name] {
			mp.Columns = append(mp.Columns, ColumnChange{Name: c.Name, Change: ColumnRemoved, OldType: c.Type})
		}
	}
}

// explain fills in, for each changed column of an affected model, the changed
// upstream columns it depends on.
func explain(mp *ModelPlan, l *lineage.Model, changed map[lineage.Ref]bool) {
	if l == nil {
		return
	}
	var rows []lineage.Ref
	for _, r := range l.Rows {
		if changed[r] {
			rows = append(rows, r)
		}
	}
	for i := range mp.Columns {
		cc := &mp.Columns[i]
		for _, lc := range l.Columns {
			if lc.Name != cc.Name {
				continue
			}
			seen := map[lineage.Ref]bool{}
			for _, r := range append(append(append([]lineage.Ref{}, lc.Direct...), lc.Indirect...), rows...) {
				if changed[r] && !seen[r] {
					seen[r] = true
					cc.Because = append(cc.Because, r)
				}
			}
		}
	}
}

// LoadBuilt reads what was built from _molt. A database without _molt, or
// with bookkeeping from before fingerprints were recorded, is not an error.
func LoadBuilt(db *sql.DB) (*Built, error) {
	b := &Built{Models: map[string]*BuiltModel{}}
	if db == nil {
		return b, nil
	}

	var hasModels, hasColumns bool
	if err := db.QueryRow(`
		SELECT count(*) FILTER (table_name = 'models') > 0, count(*) FILTER (table_name = 'columns') > 0
		FROM duckdb_tables() WHERE schema_name = '_molt' AND database_name = current_database()`).
		Scan(&hasModels, &hasColumns); err != nil {
		return nil, fmt.Errorf("failed to read _molt: %w", err)
	}
	if !hasModels {
		return b, nil
	}

	query := "SELECT name, kind, sql_hash, rows_fingerprint FROM _molt.models"
	if !hasColumns {
		query = "SELECT name, kind, sql_hash, NULL FROM _molt.models"
	}
	rows, err := db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("failed to read _molt.models: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var rowsFP sql.NullString
		m := &BuiltModel{}
		if err := rows.Scan(&name, &m.Kind, &m.Hash, &rowsFP); err != nil {
			return nil, err
		}
		m.RowsFingerprint = rowsFP.String
		b.Models[name] = m
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !hasColumns {
		return b, nil
	}

	cols, err := db.Query("SELECT model, name, type, fingerprint FROM _molt.columns ORDER BY model, position")
	if err != nil {
		return nil, fmt.Errorf("failed to read _molt.columns: %w", err)
	}
	defer cols.Close()
	for cols.Next() {
		var model string
		var fp sql.NullString
		var c BuiltColumn
		if err := cols.Scan(&model, &c.Name, &c.Type, &fp); err != nil {
			return nil, err
		}
		c.Fingerprint = fp.String
		if m, ok := b.Models[model]; ok {
			m.Columns = append(m.Columns, c)
		}
	}
	return b, cols.Err()
}

// FromCatalog is the state a run of every model would record: what Compare
// sees after a successful build.
func FromCatalog(p *project.Project, c *shadow.Catalog) *Built {
	b := &Built{Models: map[string]*BuiltModel{}}
	for _, name := range c.Order {
		if _, failed := c.Errors[name]; failed {
			continue
		}
		m := &BuiltModel{Kind: p.Models[name].Kind, Hash: p.Models[name].Hash}
		fps := map[string]string{}
		if l, err := c.Lineage(name); err == nil {
			m.RowsFingerprint = l.RowsFingerprint
			for _, col := range l.Columns {
				fps[col.Name] = col.Fingerprint
			}
		}
		for _, col := range c.Schemas[name] {
			m.Columns = append(m.Columns, BuiltColumn{Name: col.Name, Type: col.Type, Fingerprint: fps[col.Name]})
		}
		b.Models[name] = m
	}
	return b
}
